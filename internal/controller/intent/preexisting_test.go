// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// G-B-BR-2 (defect gate 2): an anomaly that was there before the change
// must not hold the pull request, but only where maul can hold the claim
// to its own ledger: every call at the head the anomaly cites has the same
// call at the base, cited too, that printed exactly the same.

// execAt is a ledger entry of an exec of argv at side, its exit and output.
func execAt(seq int64, side toolwire.Side, exit int, out string, argv ...string) toolwire.LedgerEntry {
	args, _ := json.Marshal(toolwire.ExecRequest{Argv: argv, Side: side})
	return toolwire.LedgerEntry{Seq: seq, Kind: toolwire.KindAgent, Verb: toolwire.VerbExec, Side: side.Recorded(),
		Args: args, Exit: exit, Output: out, OutputSHA256: shaHex(out)}
}

func TestPreExistingCites(t *testing.T) {
	help := "usage: bb [--json]\n"
	head := execAt(4, "", 0, help, "bb", "--help")
	base := execAt(5, toolwire.SideBase, 0, help, "bb", "--help")
	with := func(e toolwire.LedgerEntry, f func(*toolwire.LedgerEntry)) toolwire.LedgerEntry {
		f(&e)
		return e
	}
	readAt := func(seq int64, side toolwire.Side, out string) toolwire.LedgerEntry {
		args, _ := json.Marshal(toolwire.FilesRequest{Side: side, Op: toolwire.OpRead, Path: "README.md"})
		return toolwire.LedgerEntry{Seq: seq, Kind: toolwire.KindAgent, Verb: toolwire.VerbFiles,
			Side: side.Recorded(), Args: args, Output: out, OutputSHA256: shaHex(out)}
	}
	for _, tc := range []struct {
		name    string
		cites   []int32
		entries []toolwire.LedgerEntry
		want    bool
	}{
		{"the same call, the same output at both sides", []int32{4, 5}, []toolwire.LedgerEntry{head, base}, true},
		{"a file read the same at both sides shows the text, not the behaviour", []int32{6, 7},
			[]toolwire.LedgerEntry{readAt(6, "", "# bb\n"), readAt(7, toolwire.SideBase, "# bb\n")}, false},
		{"a file read beside an exec twin", []int32{4, 5, 6, 7}, []toolwire.LedgerEntry{head, base,
			readAt(6, "", "# bb\n"), readAt(7, toolwire.SideBase, "# bb\n")}, false},
		{"a run the same at both sides", []int32{8, 9}, []toolwire.LedgerEntry{
			with(head, func(e *toolwire.LedgerEntry) { e.Seq, e.Verb = 8, toolwire.VerbRun }),
			with(base, func(e *toolwire.LedgerEntry) { e.Seq, e.Verb = 9, toolwire.VerbRun })}, true},
		{"the head alone", []int32{4}, []toolwire.LedgerEntry{head, base}, false},
		{"the base alone", []int32{5}, []toolwire.LedgerEntry{head, base}, false},
		{"no cites", nil, []toolwire.LedgerEntry{head, base}, false},
		{"no ledger", []int32{4, 5}, nil, false},
		{"other output at the base", []int32{4, 5}, []toolwire.LedgerEntry{head,
			execAt(5, toolwire.SideBase, 0, "usage: bb\n", "bb", "--help")}, false},
		{"another exit at the base", []int32{4, 5}, []toolwire.LedgerEntry{head,
			execAt(5, toolwire.SideBase, 1, help, "bb", "--help")}, false},
		{"another call at the base", []int32{4, 5}, []toolwire.LedgerEntry{head,
			execAt(5, toolwire.SideBase, 0, help, "bb", "-h")}, false},
		{"a second head call with no twin", []int32{4, 5, 6}, []toolwire.LedgerEntry{head, base,
			execAt(6, "", 2, "panic: shift\n", "bb", "shift")}, false},
		{"a base twin of the wrong verb", []int32{4, 5}, []toolwire.LedgerEntry{head,
			with(base, func(e *toolwire.LedgerEntry) { e.Verb = toolwire.VerbRun })}, false},
		{"refused at the base", []int32{4, 5}, []toolwire.LedgerEntry{head,
			with(base, func(e *toolwire.LedgerEntry) { e.Refused = toolwire.RefusedUnavailable })}, false},
		{"output dropped from the ledger", []int32{4, 5}, []toolwire.LedgerEntry{
			with(head, func(e *toolwire.LedgerEntry) { e.Output = toolledger.DroppedOutput }),
			with(base, func(e *toolwire.LedgerEntry) { e.Output = toolledger.DroppedOutput })}, false},
		{"truncated output", []int32{4, 5}, []toolwire.LedgerEntry{
			execAt(4, "", 0, help+toolwire.TruncationMarker, "bb", "--help"),
			execAt(5, toolwire.SideBase, 0, help+toolwire.TruncationMarker, "bb", "--help")}, false},
		{"a check run", []int32{4, 5}, []toolwire.LedgerEntry{
			with(head, func(e *toolwire.LedgerEntry) { e.CheckRun = &toolwire.CheckRunRecord{} }),
			with(base, func(e *toolwire.LedgerEntry) { e.CheckRun = &toolwire.CheckRunRecord{} })}, false},
		{"a browser call has no base", []int32{4, 5}, []toolwire.LedgerEntry{
			with(head, func(e *toolwire.LedgerEntry) { e.Verb = toolwire.VerbBrowser }), base}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := preExistingCites(tc.cites, tc.entries); got != tc.want {
				t.Errorf("preExistingCites = %v, want %v", got, tc.want)
			}
		})
	}
}

// preExistingFixture is a replay round ready on its own (the version
// outcome passed on its saved check), whose ledger holds an exec of
// bb --help at the head (seq 4) and at the base (seq 5).
func preExistingFixture(t *testing.T, baseOut string) (*v1alpha1.IntentAcceptance, *v1alpha1.PreviewVerification,
	[]v1alpha1.SavedCheck, []toolwire.LedgerEntry) {
	t.Helper()
	o := v1alpha1.AcceptanceOutcome{ID: oVersion, Outcome: "The build page shows the version"}
	version := savedCheck(t, versionCheck)
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o},
		Reviews: []v1alpha1.CheckReviewRecord{{CheckDigest: version.Digest, OutcomeDigest: outcomeDigest(o),
			Answer: v1alpha1.ReviewProves, Run: "r1", Check: version.ID, Reason: "yes"}}}
	pv := &v1alpha1.PreviewVerification{
		Spec: v1alpha1.PreviewVerificationSpec{Round: 2, HeadSetDigest: "sha256:" + strings.Repeat("a", 64),
			Checks: []v1alpha1.SavedCheck{version}},
		Status: v1alpha1.PreviewVerificationStatus{Replay: []v1alpha1.CheckRun{{CheckDigest: version.Digest,
			Result: v1alpha1.CheckRunPass, Token: "t1", Seq: 1}}, Ledger: &v1alpha1.LedgerSummary{Count: 9}},
	}
	help := "usage: bb [--json]\n"
	entries := []toolwire.LedgerEntry{execAt(4, "", 0, help, "bb", "--help"),
		execAt(5, toolwire.SideBase, 0, baseOut, "bb", "--help")}
	return acc, pv, []v1alpha1.SavedCheck{version}, entries
}

// oldHelp is an anomaly the evaluator says was there before the change.
func oldHelp(pre bool) *report.Evaluation {
	return &report.Evaluation{Anomalies: []report.Anomaly{{Summary: "the help text names no --count flag",
		Route: "bb --help", Ledger: []int32{4, 5}, PreExisting: pre}}}
}

// TestPreExistingAnomalyDoesNotHold: a pre-existing anomaly maul could
// hold to its ledger is recorded as such and neither keeps the round from
// ready nor its model round from clear; one the ledger does not bear out,
// or one the evaluator did not say was there before, holds as any other.
func TestPreExistingAnomalyDoesNotHold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseOut string
		claim   bool
		pre     bool
	}{
		{"claimed and the same at the base", "usage: bb [--json]\n", true, true},
		{"claimed, but the base printed otherwise", "usage: bb\n", true, false},
		{"the same at the base, but not claimed", "usage: bb [--json]\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc, pv, stored, entries := preExistingFixture(t, tc.baseOut)
			j := judgeHolding(acc, pv, stored, oldHelp(tc.claim), nil, entries)
			if len(j.anomalies) != 1 || j.anomalies[0].PreExisting != tc.pre {
				t.Fatalf("anomalies %+v, want one, pre-existing %v", j.anomalies, tc.pre)
			}
			ready := j.decision.Handoff == acceptance.HandoffReady
			if ready != tc.pre || j.clear != tc.pre {
				t.Errorf("hand-off %s, clear %v; want ready and clear only when pre-existing (%v)",
					j.decision.Handoff, j.clear, tc.pre)
			}
		})
	}
}

// TestPreExistingRecords: a pre-existing record is shown but never held,
// never accepted by /maul accept all, never crowds an open one out of the
// bounded record, and never turns an earlier round's open record of the
// same words into a pre-existing one (nor is an open one of a later round
// absorbed by an earlier pre-existing record).
func TestPreExistingRecords(t *testing.T) {
	pre := v1alpha1.AnomalyRecord{Summary: "old", Route: "bb", Ledger: []int32{4, 5}, PreExisting: true}
	open := v1alpha1.AnomalyRecord{Summary: "old", Route: "bb", Ledger: []int32{4}}
	if got := openAnomalies([]v1alpha1.AnomalyRecord{pre}); len(got) != 0 {
		t.Errorf("a pre-existing record is open: %+v", got)
	}

	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	heldOpen := open
	heldOpen.Round = 1
	got := carryAnomalies(nil, []v1alpha1.AnomalyRecord{heldOpen}, []v1alpha1.AnomalyRecord{pre}, 2, now)
	if len(openAnomalies(got)) != 1 {
		t.Errorf("round 2's pre-existing words released round 1's open record: %+v", got)
	}
	heldPre := pre
	heldPre.Round = 1
	got = carryAnomalies(nil, []v1alpha1.AnomalyRecord{heldPre}, []v1alpha1.AnomalyRecord{open}, 2, now)
	if len(openAnomalies(got)) != 1 {
		t.Errorf("round 2's open record absorbed by round 1's pre-existing one: %+v", got)
	}

	// Judged again, a round keeps its records' marking.
	got = carryAnomalies(got, []v1alpha1.AnomalyRecord{heldPre}, []v1alpha1.AnomalyRecord{open}, 2, now)
	if len(got) != 2 || !got[0].PreExisting || got[1].PreExisting {
		t.Errorf("judged again: %+v", got)
	}

	many := make([]v1alpha1.AnomalyRecord, 0, maxAnomalies+1)
	for range maxAnomalies {
		many = append(many, pre)
	}
	got = fitAnomalies(append(many, open))
	if len(got) != maxAnomalies || len(openAnomalies(got)) != 1 {
		t.Errorf("the open record crowded out by pre-existing ones: %d kept, %d open", len(got),
			len(openAnomalies(got)))
	}

	v := &v1alpha1.IntentVerification{HeadSetDigest: "h", OutcomesHeadSetDigest: "h",
		Anomalies: []v1alpha1.AnomalyRecord{pre}}
	v.Anomalies[0].RecordedAt = &metav1.Time{Time: now}
	if n := acceptAnomalies(v, verifyCommand{at: now.Add(time.Minute)}); n != 0 || v.Anomalies[0].Accepted != nil {
		t.Errorf("/maul accept all accepted %d pre-existing records", n)
	}
}

// shaHex is s's sha256, hex.
func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestLedgerOfIsPerVerification: a pass reads each verification's own
// ledger. The ledger now feeds a judgement (preExisting), so one read for
// one round must never stand in for another round's in the same pass: its
// seqs would twin against the wrong calls.
func TestLedgerOfIsPerVerification(t *testing.T) {
	ledgerCM := func(pv *v1alpha1.PreviewVerification, line string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: pv.Name + "-ledger", Namespace: testNS,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(),
				Kind: "PreviewVerification", Name: pv.Name, UID: pv.UID, Controller: new(true)}}},
			Data: map[string]string{keyLedgerLines: line}}
	}
	pvOf := func(name string) *v1alpha1.PreviewVerification {
		return &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS,
			UID: types.UID(name + "-uid")}, Status: v1alpha1.PreviewVerificationStatus{
			Ledger: &v1alpha1.LedgerSummary{Count: 1, ConfigMap: name + "-ledger"}}}
	}
	a, b := pvOf("pv-a"), pvOf("pv-b")
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).
		WithObjects(ledgerCM(a, "line of a"), ledgerCM(b, "line of b")).Build()
	p := &pass{r: &IntentReconciler{Client: c, APIReader: c}, in: &v1alpha1.Intent{}}
	if got := p.ledgerOf(context.Background(), a); !slices.Equal(got, []string{"line of a"}) {
		t.Fatalf("ledgerOf(a) = %q", got)
	}
	if got := p.ledgerOf(context.Background(), b); !slices.Equal(got, []string{"line of b"}) {
		t.Errorf("ledgerOf(b) after a = %q, want b's own ledger", got)
	}
	if got := p.ledgerOf(context.Background(), a); !slices.Equal(got, []string{"line of a"}) {
		t.Errorf("ledgerOf(a) again = %q, want a's own ledger", got)
	}
}
