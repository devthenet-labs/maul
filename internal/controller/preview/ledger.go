// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// Collecting the tool pod (DESIGN 2.2 step 10; SPEC Q7, Q8): preview-
// controller reads the tools container's log (that container only) and
// termination message, holds the entry lines to the summary both carry
// (toolwire.Verify), and writes what the ledger shows onto the
// verification's status: an index of every entry, the replay and
// confirmation runs, and the confirmed checks' bodies read from the
// ledger's own dry-run records, never from the evaluator's report. The full
// ledger goes to <pv>-ledger, a ConfigMap the verification owns in the
// release namespace, its oldest outputs dropped past maxLedgerConfigMap; the
// index on status is never cut. A log that does not hold is maul's
// trouble (Infra ledger-mismatch), never a verdict.

const (
	// ledgerSuffix and evidenceSuffix name the verification's ConfigMaps:
	// <pv>-ledger and <pv>-evidence.
	ledgerSuffix   = "-ledger"
	evidenceSuffix = "-evidence"
	// ledgerKey holds the ledger's entries, one JSON line each; ledgerSummaryKey
	// the summary they were verified against.
	ledgerKey        = "ledger.jsonl"
	ledgerSummaryKey = "summary.json"
	// maxLedgerConfigMap bounds what a ledger or evidence ConfigMap holds
	// (SPEC Q7, Q8), under the API server's 1 MiB.
	maxLedgerConfigMap = 900 << 10
	// droppedOutput replaces an output dropped to fit the ConfigMap; the
	// entry keeps its outputSHA256.
	droppedOutput = "[maul: output dropped to fit; its sha256 is kept]"
	// droppedArgs replaces args dropped to fit, once every output is gone.
	droppedArgs = `{"maul":"args dropped to fit"}`
)

// toolOutput is what parseToolOutput read: the verified ledger, the
// evidence lines that were well formed, how many were not, and the reason
// the log does not hold ("" when it does).
type toolOutput struct {
	ledger   toolwire.Ledger
	evidence []toolwire.Evidence
	dropped  int
	reason   v1alpha1.PreviewVerificationReason
}

// evidenceLine is an evidence line's JSON (toolwire's wire, read here so
// that one bad image drops alone instead of failing the ledger).
type evidenceLine struct {
	V      int    `json:"v"`
	Seq    int64  `json:"seq"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}

// parseToolOutput reads the tools container's log against its termination
// message. Evidence lines are set apart first (they are not part of the
// summary's hash): one whose bytes do not hash to its sha256, of another
// version, or malformed, is dropped and counted. The rest must verify.
func parseToolOutput(log []byte, msg string) toolOutput {
	if len(log) > toolwire.MaxLogBytes {
		return toolOutput{reason: v1alpha1.VerificationReasonLedgerMismatch}
	}
	var rest bytes.Buffer
	var out toolOutput
	for _, line := range bytes.SplitAfter(log, []byte("\n")) {
		body, ok := bytes.CutPrefix(line, []byte(toolwire.EvidencePrefix))
		if !ok {
			rest.Write(line)
			continue
		}
		var ev evidenceLine
		if err := json.Unmarshal(bytes.TrimSuffix(body, []byte("\n")), &ev); err != nil ||
			ev.V != toolwire.WireVersion || len(ev.Data) > toolwire.MaxEvidenceBytes || ev.SHA256 != sha256Hex(ev.Data) {
			out.dropped++
			continue
		}
		out.evidence = append(out.evidence, toolwire.Evidence{Seq: ev.Seq, SHA256: ev.SHA256, Data: ev.Data})
	}
	l, err := toolwire.Verify(rest.Bytes(), msg)
	switch {
	case errors.Is(err, toolwire.ErrNoSummary):
		out.reason = v1alpha1.VerificationReasonDeadlineNoSummary
	case err != nil:
		out.reason = v1alpha1.VerificationReasonLedgerMismatch
	}
	out.ledger = l
	return out
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// clampInt32 is n within int32.
func clampInt32[T int | int64](n T) int32 {
	return int32(max(min(int64(n), math.MaxInt32), math.MinInt32))
}

// ledgerIndex indexes every entry, in order: its facts without its output.
func ledgerIndex(entries []toolwire.LedgerEntry) []v1alpha1.LedgerIndexEntry {
	out := make([]v1alpha1.LedgerIndexEntry, 0, len(entries))
	for _, e := range entries {
		ie := v1alpha1.LedgerIndexEntry{Seq: clampInt32(e.Seq), Kind: v1alpha1.LedgerKind(e.Kind),
			Verb: v1alpha1.LedgerVerb(e.Verb), Exit: clampInt32(e.Exit), ResultSHA256: e.OutputSHA256,
			Refused: e.Refused != ""}
		for _, a := range e.Artifacts {
			ie.Artifacts = append(ie.Artifacts, a.SHA256)
		}
		out = append(out, ie)
	}
	return out
}

// runResults maps a check run's result onto the status vocabulary.
var runResults = map[toolwire.RunResult]v1alpha1.CheckRunResult{
	toolwire.RunPass: v1alpha1.CheckRunPass, toolwire.RunFail: v1alpha1.CheckRunFail,
	toolwire.RunError: v1alpha1.CheckRunError, toolwire.RunQuarantined: v1alpha1.CheckRunQuarantined,
}

// checkRunOf is entry e's check run as status records it; ok is false for
// a run the status cannot hold.
func checkRunOf(e toolwire.LedgerEntry) (v1alpha1.CheckRun, bool) {
	r := e.CheckRun
	result, known := runResults[r.Result]
	if !known || len(r.Attempts) == 0 || !runToken.MatchString(r.Attempts[0].Run) {
		return v1alpha1.CheckRun{}, false
	}
	run := v1alpha1.CheckRun{CheckDigest: "sha256:" + r.Digest, Result: result, Token: r.Attempts[0].Run,
		Seq: clampInt32(e.Seq)}
	if step, ok := r.AssertionStep(); ok {
		run.AssertionStep = clampInt32(step + 1)
	}
	return run, true
}

// expectsFail reports a relay confirmation entry of a check the evaluator
// proposed to show a defect: its args, which the relay writes, record the
// expectation (toolwire.ConfirmArgs).
func expectsFail(e toolwire.LedgerEntry) bool {
	var a toolwire.ConfirmArgs
	return json.Unmarshal(e.Args, &a) == nil && a.Expect == toolwire.ExpectFail
}

// redDryRun is the latest entry before seq recording the agent's dry run
// of the check of digest red by assertion at step, or nil; nil too when any
// dry run of it before seq passed (a check that ran green shows no defect).
func redDryRun(entries []toolwire.LedgerEntry, digest string, seq int64, step int) *toolwire.LedgerEntry {
	if greenDryRun(entries, digest, seq) != nil {
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := &entries[i]
		if e.Seq >= seq || e.Kind != toolwire.KindAgent || e.Verb != toolwire.VerbCheckRun || e.Refused != "" ||
			e.CheckRun == nil || e.CheckRun.Mode != toolwire.ModeDryRun || e.CheckRun.Digest != digest {
			continue
		}
		if at, ok := e.CheckRun.AssertionStep(); ok && at == step {
			return e
		}
	}
	return nil
}

// requiredDryRun is the dry run confirmation entry e required: the green
// one, or, for a check expected to fail whose confirmation is red by
// assertion, the red one at the same step; nil when there is none.
func requiredDryRun(entries []toolwire.LedgerEntry, e toolwire.LedgerEntry) *toolwire.LedgerEntry {
	if !expectsFail(e) {
		return greenDryRun(entries, e.CheckRun.Digest, e.Seq)
	}
	step, ok := e.CheckRun.AssertionStep()
	if !ok {
		return nil
	}
	return redDryRun(entries, e.CheckRun.Digest, e.Seq, step)
}

// runToken is the grammar of a run token status holds (CheckRun.Token).
var runToken = regexp.MustCompile(`^[A-Za-z0-9-]{1,40}$`)

// replayRuns are the relay's replays of the saved checks, in order.
func replayRuns(entries []toolwire.LedgerEntry) []v1alpha1.CheckRun {
	var out []v1alpha1.CheckRun
	for _, e := range entries {
		if e.Kind != toolwire.KindRelay || e.Verb != toolwire.VerbCheckRun || e.CheckRun == nil ||
			e.CheckRun.Mode != toolwire.ModeReplay || len(out) == v1alpha1.MaxSavedChecks {
			continue
		}
		if run, ok := checkRunOf(e); ok {
			out = append(out, run)
		}
	}
	return out
}

// greenDryRun is the latest entry before seq recording the agent's passing
// dry run of the check of digest (bare hex), or nil.
func greenDryRun(entries []toolwire.LedgerEntry, digest string, seq int64) *toolwire.LedgerEntry {
	for i := len(entries) - 1; i >= 0; i-- {
		e := &entries[i]
		if e.Seq < seq && e.Kind == toolwire.KindAgent && e.Verb == toolwire.VerbCheckRun && e.Refused == "" &&
			e.CheckRun != nil && e.CheckRun.Mode == toolwire.ModeDryRun && e.CheckRun.Digest == digest &&
			e.CheckRun.Result == toolwire.RunPass {
			return e
		}
	}
	return nil
}

// confirmationRuns are the relay's confirmation runs, in order, each with
// the dry run it required (requiredDryRun) and, for a check proposed to
// show a defect, its expectation.
func confirmationRuns(entries []toolwire.LedgerEntry) []v1alpha1.CheckRun {
	var out []v1alpha1.CheckRun
	for _, e := range entries {
		if e.Kind != toolwire.KindRelay || e.Verb != toolwire.VerbConfirm || e.CheckRun == nil ||
			e.CheckRun.Mode != toolwire.ModeConfirm || len(out) == v1alpha1.MaxSavedChecks {
			continue
		}
		run, ok := checkRunOf(e)
		if !ok {
			continue
		}
		if expectsFail(e) {
			run.Expect = v1alpha1.CheckExpectFail
		}
		if dry := requiredDryRun(entries, e); dry != nil {
			run.DryRunSeq = clampInt32(dry.Seq)
		}
		out = append(out, run)
	}
	return out
}

// confirmedChecks are the checks whose confirmation did what it was
// proposed to do, each read from the dry run that confirmation required:
// the check the relay itself validated and ran, by its own digest. A check
// expected to pass is confirmed by a passing confirmation after a green dry
// run; one proposed to show a defect (Expect fail) only by a confirmation
// red by assertion after a dry run red by assertion at the same step, with
// no green dry run of it. A check over MaxSavedCheckBytes, bound to no
// outcome under evaluation, or not reread exactly, is left out: what status
// holds is only what intent-controller may store.
func confirmedChecks(entries []toolwire.LedgerEntry, outcomes []v1alpha1.AcceptanceOutcome) []v1alpha1.SavedCheck {
	var out []v1alpha1.SavedCheck
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Kind != toolwire.KindRelay || e.Verb != toolwire.VerbConfirm || e.CheckRun == nil ||
			e.CheckRun.Mode != toolwire.ModeConfirm || seen[e.CheckRun.Digest] || len(out) == v1alpha1.MaxSavedChecks {
			continue
		}
		red := expectsFail(e)
		if !red && e.CheckRun.Result != toolwire.RunPass {
			continue
		}
		dry := requiredDryRun(entries, e)
		if dry == nil {
			continue
		}
		c, ok := checkOf(*dry, e.CheckRun.Digest, outcomes)
		if !ok {
			continue
		}
		if red {
			c.Expect = v1alpha1.CheckExpectFail
		}
		seen[e.CheckRun.Digest] = true
		out = append(out, c)
	}
	return out
}

// checkOf rereads the check a dry-run entry recorded, held to digest.
func checkOf(dry toolwire.LedgerEntry, digest string, outcomes []v1alpha1.AcceptanceOutcome) (v1alpha1.SavedCheck,
	bool) {
	var req toolwire.CheckRunRequest
	if err := json.Unmarshal(dry.Args, &req); err != nil {
		return v1alpha1.SavedCheck{}, false
	}
	c, err := checks.Parse(req.Spec)
	if err != nil {
		return v1alpha1.SavedCheck{}, false
	}
	body := checks.Canonical(c)
	bound := slices.ContainsFunc(outcomes, func(o v1alpha1.AcceptanceOutcome) bool { return o.ID == c.Outcome })
	if checks.Digest(c) != "sha256:"+digest || len(body) > v1alpha1.MaxSavedCheckBytes || !bound {
		return v1alpha1.SavedCheck{}, false
	}
	return v1alpha1.SavedCheck{ID: c.ID, Outcome: c.Outcome, Form: v1alpha1.CheckForm(c.Form), Body: string(body),
		Digest: checks.Digest(c)}, true
}

// readinessOf is the relay's readiness wait, its first entry: a relay http
// entry whose args name the readiness and whose output is the waits.
func readinessOf(entries []toolwire.LedgerEntry) []acceptance.TargetWait {
	if len(entries) == 0 {
		return nil
	}
	e := entries[0]
	var args struct {
		Readiness json.RawMessage `json:"readiness"`
	}
	if e.Kind != toolwire.KindRelay || e.Verb != toolwire.VerbHTTP || json.Unmarshal(e.Args, &args) != nil ||
		args.Readiness == nil {
		return nil
	}
	var waits []acceptance.TargetWait
	if json.Unmarshal([]byte(e.Output), &waits) != nil {
		return nil
	}
	return waits
}

// ledgerMeta names a ConfigMap of the verification: a controller reference
// to it, and the Intent's label, which intent-controller's and the status
// page's label-confined, owner-checked reads of a ConfigMap require.
func ledgerMeta(pv *v1alpha1.PreviewVerification, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: pv.Namespace,
		Labels: map[string]string{v1alpha1.LabelIntent: pv.Spec.IntentRef.Name,
			v1alpha1.LabelPreviewVerification: string(pv.UID), labelManagedBy: managedBy},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(),
			Kind: "PreviewVerification", Name: pv.Name, UID: pv.UID, Controller: ptr(true)}}}
}

// ledgerConfigMap is the full ledger as <pv>-ledger: each entry one JSON
// line, in order, and the summary. Past maxLedgerConfigMap the oldest
// entries' outputs and inline text go first, then their args; every entry
// keeps its line, its seq and its outputSHA256.
func ledgerConfigMap(pv *v1alpha1.PreviewVerification, l toolwire.Ledger) (*corev1.ConfigMap, error) {
	entries := slices.Clone(l.Entries)
	sum, err := json.Marshal(l.Summary)
	if err != nil {
		return nil, err
	}
	lines := make([][]byte, len(entries))
	size := len(sum) + len(ledgerKey) + len(ledgerSummaryKey)
	for i := range entries {
		if lines[i], err = json.Marshal(entries[i]); err != nil {
			return nil, fmt.Errorf("encode ledger entry %d: %w", entries[i].Seq, err)
		}
		size += len(lines[i]) + 1
	}
	for _, drop := range []func(*toolwire.LedgerEntry){
		func(e *toolwire.LedgerEntry) { e.Output, e.Inline = droppedOutput, "" },
		func(e *toolwire.LedgerEntry) { e.Args = json.RawMessage(droppedArgs) },
	} {
		for i := 0; i < len(entries) && size > maxLedgerConfigMap; i++ {
			drop(&entries[i])
			line, err := json.Marshal(entries[i])
			if err != nil {
				return nil, err
			}
			size += len(line) - len(lines[i])
			lines[i] = line
		}
	}
	if size > maxLedgerConfigMap {
		return nil, errors.New("the ledger's index lines alone are over the ConfigMap bound")
	}
	var b bytes.Buffer
	for _, line := range lines {
		b.Write(line)
		b.WriteByte('\n')
	}
	return &corev1.ConfigMap{ObjectMeta: ledgerMeta(pv, pv.Name+ledgerSuffix), Immutable: ptr(true),
		Data: map[string]string{ledgerKey: b.String(), ledgerSummaryKey: string(sum)}}, nil
}
