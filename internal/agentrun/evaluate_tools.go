// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/maultool"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// checkFilesMax bounds the files the evaluate stage reads under its checks
// directory to match a report's proposals against: a directory holding more
// is refused rather than half read.
const checkFilesMax = 256

// evaluationReport is an accepted evaluation report: the parsed frontmatter,
// each proposed check matched to a check file the agent wrote, and the
// proposals dropped because the file they name did not resolve.
type evaluationReport struct {
	*report.Evaluation
	dropped []droppedProposal
}

// droppedProposal is a proposal left out of an accepted report, and why.
type droppedProposal struct {
	id, file, reason string
}

// parseEvaluation parses an evaluation report (report.ParseEvaluation) and
// holds its proposals to the check files the agent wrote. A proposal by
// digest alone must be the digest (checks.Digest) of a check file under the
// checks directory, whose id and outcome are the proposal's; anything else
// refuses the report, so its repair (settleReport) can fix it in the
// agent's own session. A proposal naming its file is resolved from that
// file (resolveProposedFiles): one that does not resolve is dropped with
// its reason, never the whole report. The files are the agent's own claim
// either way: the tools container confirms only a digest it saw dry-run
// green, and the body it ran is the ledger's.
//
// A report the parser refuses has its frontmatter's key paths, never its
// values, added to the reason (S1 bug 5: the refused file is not kept), so
// a refusal is diagnosable from the run's detail alone.
func (a *Agent) parseEvaluation(raw []byte) (*evaluationReport, error) {
	e, err := report.ParseEvaluation(raw)
	if err != nil {
		if paths := report.FrontmatterKeyPaths(raw); len(paths) > 0 {
			err = fmt.Errorf("%w; frontmatter keys: %s", err, strings.Join(paths, ", "))
		}
		return nil, err
	}
	var byDigest []report.ProposedCheck
	for _, p := range e.Proposed {
		if p.File == "" {
			byDigest = append(byDigest, p)
		}
	}
	if err := checkProposed(a.cfg.checksDir(), byDigest); err != nil {
		return nil, fmt.Errorf("report: evaluation: %w", err)
	}
	dropped := resolveProposedFiles(a.cfg.scratchDir(), e)
	return &evaluationReport{Evaluation: e, dropped: dropped}, nil
}

// resolveProposedFiles resolves each of e's proposals that names a file,
// in place, to that file's digest, and drops each that does not resolve
// (resolveFile), returning what it dropped. A proposal by digest alone is
// kept as it is.
func resolveProposedFiles(scratch string, e *report.Evaluation) []droppedProposal {
	var (
		kept    []report.ProposedCheck
		dropped []droppedProposal
	)
	for _, p := range e.Proposed {
		if p.File == "" {
			kept = append(kept, p)
			continue
		}
		digest, err := resolveFile(scratch, p)
		if err != nil {
			dropped = append(dropped, droppedProposal{id: p.ID, file: p.File, reason: err.Error()})
			continue
		}
		p.Digest = digest
		kept = append(kept, p)
	}
	e.Proposed = kept
	return dropped
}

// resolveFile reads the check file p names, a regular file under scratch
// (absolute, or relative to scratch, where the stage runs) that no path or
// symlink leads out of, and returns its digest: the digest the dry run of
// exactly that file printed. The file must be a check whose id and outcome
// are p's, and, when p gives a digest too, have that digest. Whether it ran
// green is the tools container's to say: it confirms no digest it did not
// see dry-run green.
func resolveFile(scratch string, p report.ProposedCheck) (string, error) {
	rel := filepath.Clean(p.File)
	if filepath.IsAbs(rel) {
		r, err := filepath.Rel(scratch, rel)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s is not under %s", p.File, scratch)
		}
		rel = r
	}
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %w", p.File, err)
	}
	defer func() { _ = root.Close() }()
	data, err := readRootFile(root, filepath.ToSlash(rel), toolwire.MaxCheckBytes)
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %w", p.File, err)
	}
	c, err := checks.Parse(data)
	if err != nil {
		return "", fmt.Errorf("%s is not a check: %w", p.File, err)
	}
	digest := checks.Digest(c)
	switch {
	case c.ID != p.ID:
		return "", fmt.Errorf("the id of %s is %q, not the proposal's %q", p.File, c.ID, p.ID)
	case c.Outcome != p.Outcome:
		return "", fmt.Errorf("the outcome of %s is %q, not the proposal's %q", p.File, c.Outcome, p.Outcome)
	case p.Digest != "" && p.Digest != digest:
		return "", fmt.Errorf("the digest of %s is %s, not the proposal's %s", p.File, digest, p.Digest)
	}
	return digest, nil
}

// checkProposed matches each proposal to its check file in dir.
func checkProposed(dir string, proposed []report.ProposedCheck) error {
	if len(proposed) == 0 {
		return nil
	}
	files, err := readCheckFiles(dir)
	if err != nil {
		return fmt.Errorf("proposed checks: %w", err)
	}
	var errs []error
	for i, p := range proposed {
		field := fmt.Sprintf("proposed[%d]", i)
		f, ok := files[p.Digest]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s.digest %s is the digest of no check file under %s: propose a check "+
				"by the digest its green `maul-tool check run` printed, and leave its file as it ran", field, p.Digest,
				dir))
		case f.check.ID != p.ID:
			errs = append(errs, fmt.Errorf("%s.id %q is not the id of the check file %s with that digest (%q)", field,
				p.ID, f.name, f.check.ID))
		case f.check.Outcome != p.Outcome:
			errs = append(errs, fmt.Errorf("%s.outcome %q is not the outcome of the check file %s with that digest "+
				"(%q)", field, p.Outcome, f.name, f.check.Outcome))
		}
	}
	return errors.Join(errs...)
}

// checkFile is one parsed check file.
type checkFile struct {
	name  string
	check checks.Check
}

// readCheckFiles reads the regular files directly under dir, each at most
// toolwire.MaxCheckBytes, following no symlink, and keys each that parses
// as a check (checks.Parse) by its digest; the first of two alike wins. A
// file that does not parse, or is too large, is no check: it is skipped.
func readCheckFiles(dir string) (map[string]checkFile, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("there is no check file: %s does not exist", dir)
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	if len(entries) > checkFilesMax {
		return nil, fmt.Errorf("%s holds %d entries, over %d", dir, len(entries), checkFilesMax)
	}
	files := make(map[string]checkFile, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := readRootFile(root, e.Name(), toolwire.MaxCheckBytes)
		if err != nil {
			continue
		}
		c, err := checks.Parse(data)
		if err != nil {
			continue
		}
		d := checks.Digest(c)
		if _, seen := files[d]; !seen {
			files[d] = checkFile{name: path.Join(dir, e.Name()), check: c}
		}
	}
	return files, nil
}

// readRootFile reads name under root, a regular file of at most limit
// bytes.
func readRootFile(root *os.Root, name string, limit int) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s is over %d bytes", name, limit)
	}
	return data, nil
}

// closeTools ends the evaluator's use of its tool pod (SPEC Q18). With an
// accepted report that proposes checks it first asks the tools container to
// confirm them (POST /v1/confirm, one call per check): each run alone, under
// a fresh run token, recorded in the ledger. Then it finishes (POST /v1/finish), naming as
// evidence the screenshots of at most toolwire.MaxEvidence ledger entries
// the report cites (evidence). It returns the seqs it named in a finish the
// tools container accepted, nil otherwise.
//
// Neither call's outcome changes the stage's: the report is the model's
// claim either way, and what was confirmed and which bytes are evidence the
// controller reads from the ledger, never from here. A failed call is
// logged; the tool pod still finishes on its own deadline.
func (a *Agent) closeTools(ctx context.Context, tools *maultool.Client, accepted *evaluationReport) []int32 {
	if tools == nil {
		return nil
	}
	var refs []int32
	if accepted != nil {
		// One call per check: the tools container refuses a whole confirm
		// when any digest it names has no dry run of its expectation (green,
		// or red by assertion for a check proposed to show a defect), so a
		// batch would let one unconfirmable proposal cost every other its
		// confirmation.
		for _, p := range proposedDigests(accepted.Proposed) {
			req := &toolwire.ConfirmRequest{Digests: []string{p.digest}}
			if p.red {
				req.ExpectFail = []string{p.digest}
			}
			a.toolsCall(ctx, tools, toolwire.VerbConfirm, req)
		}
		refs = accepted.LedgerRefs()
	}
	evidence, cited := a.evidence(refs)
	if !a.toolsCall(ctx, tools, toolwire.VerbFinish, &toolwire.FinishRequest{Evidence: evidence}) {
		return nil
	}
	return cited
}

// toolsCall makes one of agent-runner's own tool calls and reports whether
// the tools container answered it unrefused.
func (a *Agent) toolsCall(ctx context.Context, tools *maultool.Client, verb toolwire.Verb,
	req toolwire.Request) bool {
	resp, err := tools.Call(ctx, verb, req)
	if err != nil {
		attrs := []any{"verb", verb, "error", oneLine(err.Error(), repairFailureBytes)}
		if resp != nil {
			attrs = append(attrs, "seq", resp.Seq, "refused", resp.Refused,
				"reason", oneLine(resp.Reason, repairFailureBytes))
		}
		a.cfg.Log.Warn("the tools container did not take agent-runner's call", attrs...)
		return false
	}
	a.cfg.Log.Info("the tools container took agent-runner's call", "verb", verb, "seq", resp.Seq)
	return true
}

// proposedDigest is a proposal as agent-runner confirms it: its digest as
// the tools container keys a check (bare hex), and whether it is proposed
// to show a defect.
type proposedDigest struct {
	digest string
	red    bool
}

// proposedDigests are the proposals' digests, in report order, none twice
// (the first proposal of a digest names its expectation).
func proposedDigests(proposed []report.ProposedCheck) []proposedDigest {
	var out []proposedDigest
	for _, p := range proposed {
		d := strings.TrimPrefix(p.Digest, "sha256:")
		if !slices.ContainsFunc(out, func(o proposedDigest) bool { return o.digest == d }) {
			out = append(out, proposedDigest{digest: d, red: p.ExpectsFail()})
		}
	}
	return out
}

// evidence picks, from the ledger seqs a report cites (ascending), those
// whose screenshot maul-tool wrote (shots/<seq>.png under scratch), at most
// toolwire.MaxEvidence, each named by its file's sha256. The file is the
// agent's to write too: the tools container emits only an image whose
// sha256 is an artifact of that entry, so a forged file names nothing.
func (a *Agent) evidence(refs []int32) ([]toolwire.EvidenceRef, []int32) {
	if len(refs) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(a.cfg.scratchDir())
	if err != nil {
		return nil, nil
	}
	defer func() { _ = root.Close() }()
	var (
		evidence []toolwire.EvidenceRef
		cited    []int32
	)
	for _, seq := range refs {
		if len(evidence) == toolwire.MaxEvidence {
			break
		}
		if seq < 1 || int64(seq) > toolwire.MaxEntries {
			continue
		}
		name := path.Join(maultool.ShotsDir, strconv.Itoa(int(seq))+".png")
		data, err := readRootFile(root, name, toolwire.MaxArtifactBytes)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		evidence = append(evidence, toolwire.EvidenceRef{Seq: int64(seq), SHA256: hex.EncodeToString(sum[:])})
		cited = append(cited, seq)
	}
	return evidence, cited
}
