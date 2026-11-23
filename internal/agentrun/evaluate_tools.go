// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/internal/maultool"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// parseEvaluation parses an evaluation report (report.ParseEvaluation). A
// report the parser refuses has its frontmatter's key paths, never its
// values, added to the reason (S1 bug 5: the refused file is not kept), so a
// refusal is diagnosable from the run's detail alone.
//
// A report an earlier evaluator shaped, still proposing checks, is accepted
// as the parser accepts it: maul confirms no check (closeTools), so no check
// file is read and a proposal is no reason to repair a report.
func parseEvaluation(raw []byte) (*report.Evaluation, error) {
	e, err := report.ParseEvaluation(raw)
	if err != nil {
		if paths := report.FrontmatterKeyPaths(raw); len(paths) > 0 {
			err = fmt.Errorf("%w; frontmatter keys: %s", err, strings.Join(paths, ", "))
		}
		return nil, err
	}
	return e, nil
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

// closeTools ends the evaluator's use of its tool pod (SPEC Q18): it
// finishes (POST /v1/finish), naming as evidence the screenshots of at most
// toolwire.MaxEvidence ledger entries the accepted report cites
// (evidenceRefs, evidence). It returns the seqs it named in a finish the
// tools container accepted, nil otherwise. It confirms nothing: maul judges
// the report's verdicts by the calls they cite in the round's verified
// ledger, and keeps no check.
//
// The finish's outcome changes nothing the stage claims: the report is the
// model's claim either way, and which bytes are evidence the controller
// reads from the ledger, never from here. A failed call is logged; the tool
// pod still finishes on its own deadline.
func (a *Agent) closeTools(ctx context.Context, tools *maultool.Client, accepted *report.Evaluation) []int32 {
	if tools == nil {
		return nil
	}
	evidence, cited := a.evidence(evidenceRefs(accepted))
	if !a.toolsCall(ctx, tools, toolwire.VerbFinish, &toolwire.FinishRequest{Evidence: evidence}) {
		return nil
	}
	return cited
}

// evidenceRefs are the ledger seqs e cites, in the order their screenshots
// are kept: the seqs its verdicts cite first, ascending, since a verdict's
// cited calls are what the reviewers audit, then the rest it cites,
// ascending. nil for no report.
func evidenceRefs(e *report.Evaluation) []int32 {
	if e == nil {
		return nil
	}
	var verdicts []int32
	for _, v := range e.Verdicts {
		verdicts = append(verdicts, v.Ledger...)
	}
	slices.Sort(verdicts)
	verdicts = slices.Compact(verdicts)
	refs := slices.Clone(verdicts)
	for _, seq := range e.LedgerRefs() {
		if _, found := slices.BinarySearch(verdicts, seq); !found {
			refs = append(refs, seq)
		}
	}
	return refs
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

// evidence picks, from the ledger seqs a report cites (in evidenceRefs'
// order), the first toolwire.MaxEvidence whose screenshot maul-tool wrote
// (shots/<seq>.png under scratch), each named by its file's sha256, and
// returns them in seq order. The file is the agent's to write too: the tools
// container emits only an image whose sha256 is an artifact of that entry,
// so a forged file names nothing.
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
	slices.SortFunc(evidence, func(a, b toolwire.EvidenceRef) int { return cmp.Compare(a.Seq, b.Seq) })
	slices.Sort(cited)
	return evidence, cited
}
