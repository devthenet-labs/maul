// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
	"github.com/devthenet-labs/maul/internal/web/auth"
)

// The evaluation fixture: alpha-7's evaluate run of round 1, its
// PreviewVerification, and the verification's ledger and evidence
// ConfigMaps, with private strings the tier 1 view must never carry. The
// round was judged under the observed rules: rate-battle passed on calls 1
// (an http call) and 2 (the evaluator's own page script, its screenshot
// stored); leaderboard is unsure, its verdict citing only call 3, which
// maul refused.
const (
	evalRun      = "alpha-7-ev-r1-a1"
	evalPV       = "alpha-7-v1-a1"
	evalPVUID    = types.UID("uid-pv-alpha-7")
	evalNote     = "The rating moved by 16 points, said the evaluator"
	evalOutput   = "HTTP/1.1 200 OK secret-app-output"
	evalArgs     = `{"argv":["GET","/battles"]}`
	evalScript   = `{"verb":"eval","args":["function () { return document.title + 'secret-script' }"]}`
	evalReturned = "### Result\n\"Battles secret-returned\""
	evalBodyText = "an evaluate report body"
	evalForged   = `{"argv":["GET","/forged-line"]}`
)

var (
	evalShaA    = strings.Repeat("a", 64)
	evalShaB    = strings.Repeat("b", 64)
	evalHeadSet = "sha256:" + strings.Repeat("c", 64)
)

func sha256Hex(s []byte) string {
	sum := sha256.Sum256(s)
	return hex.EncodeToString(sum[:])
}

// tinyPNG is a valid 2x2 PNG.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// evalReport is the evaluate run's report: a verdict per outcome, each
// citing the calls it rests on, and no retired key.
const evalReport = "---\nverdicts:\n  - id: rate-battle\n    verdict: pass\n    ledger: [1, 2]\n    note: \"" +
	evalNote + "\"\n  - id: leaderboard\n    verdict: pass\n    ledger: [3]\n---\n" + evalBodyText + "\n"

// evaluationFixture is the intents fixture plus the evaluation objects.
func evaluationFixture(t *testing.T) []client.Object {
	t.Helper()
	img := tinyPNG(t)
	imgSha := sha256Hex(img)

	objs := intentsFixture(t)
	in := findAlpha7(objs)
	released := metav1.NewTime(testClock.Add(-time.Minute))
	in.Status.Verification = &v1alpha1.IntentVerification{
		Round: 1, Attempt: 1, Ref: &v1alpha1.ObjectReference{Name: evalPV, UID: evalPVUID},
		Judge:         v1alpha1.JudgeObserved,
		HeadSetDigest: evalHeadSet, OutcomesRound: 1, OutcomesHeadSetDigest: evalHeadSet,
		Outcomes: []v1alpha1.OutcomeResult{
			{ID: "rate-battle", Result: v1alpha1.OutcomePassed, Reason: "seen-to-hold", Cites: []int32{1, 2}},
			{ID: "leaderboard", Result: v1alpha1.OutcomeUnsure, Reason: "no-valid-evidence"},
		},
		Anomalies: []v1alpha1.AnomalyRecord{{Summary: "Console error on /battles\u202e", Route: "/battles",
			Ledger: []int32{2}}},
		Handoff: &v1alpha1.HandoffState{Mode: v1alpha1.HandoffGated, Released: true, ReleasedAt: &released,
			Reason: v1alpha1.HandoffReasonUnconfirmed},
	}

	run := &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: evalRun, Namespace: "maul", UID: "uid-eval-run",
			CreationTimestamp: metav1.NewTime(testClock.Add(-5 * time.Minute))},
		Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "alpha-7", UID: "uid-alpha-7"},
			Stage:     v1alpha1.IntentStageEvaluate, Round: 1, Attempt: 1,
			Repository: v1alpha1.IntentRunRepository{URL: "https://github.com/acme/app"},
			Evaluation: &v1alpha1.IntentRunEvaluation{Verification: evalPV, UID: evalPVUID, ToolIP: "10.0.0.9",
				Rung: v1alpha1.VerificationRung("full")},
		},
		Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunComplete, Outcome: "ok", Report: evalReport},
	}

	cut := toolledger.DroppedOutput
	snapshot := "- heading \"Battles\""
	pv := &v1alpha1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{Name: evalPV, Namespace: "maul", UID: evalPVUID},
		Spec: v1alpha1.PreviewVerificationSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "alpha-7", UID: "uid-alpha-7"},
			Round:     1, Attempt: 1, Rung: v1alpha1.VerificationRung("full"), HeadSetDigest: evalHeadSet,
			Outcomes: []v1alpha1.AcceptanceOutcome{
				{ID: "rate-battle", Outcome: "A rated battle moves the rating"},
				{ID: "leaderboard", Outcome: "The leaderboard lists the battle", Kind: v1alpha1.OutcomeKindKeep},
			},
		},
		Status: v1alpha1.PreviewVerificationStatus{
			Phase: v1alpha1.VerificationObserved,
			Ledger: &v1alpha1.LedgerSummary{Count: 5, SHA256: evalShaA, ConfigMap: evalPV + "-ledger",
				Evidence: evalPV + "-evidence",
				Entries: []v1alpha1.LedgerIndexEntry{
					{Seq: 1, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbHTTP,
						ResultSHA256: sha256Hex([]byte(evalOutput))},
					{Seq: 2, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbBrowser,
						ResultSHA256: sha256Hex([]byte(evalReturned)), Artifacts: []string{imgSha, evalShaA}},
					{Seq: 3, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbHTTP, Refused: true,
						ResultSHA256: sha256Hex(nil)},
					{Seq: 4, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbBrowser,
						ResultSHA256: sha256Hex([]byte(snapshot))},
					{Seq: 5, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbHTTP,
						ResultSHA256: evalShaB},
				}},
			Capture: &v1alpha1.CaptureSummary{Events: 4,
				Components: []v1alpha1.ComponentCapture{{Name: "app", Bytes: 2048, Truncated: true}}},
		},
	}
	line := func(seq int64, verb toolwire.Verb, args, output, digest string) string {
		b, err := json.Marshal(toolwire.LedgerEntry{Seq: seq, Kind: toolwire.KindAgent, Verb: verb,
			Args: json.RawMessage(args), Output: output, OutputSHA256: digest})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	pvOwned := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "maul",
			Labels:          map[string]string{v1alpha1.LabelIntent: "alpha-7"},
			OwnerReferences: []metav1.OwnerReference{ctrlRef("PreviewVerification", evalPV, evalPVUID)}}
	}
	ledgerCM := &corev1.ConfigMap{ObjectMeta: pvOwned(evalPV + "-ledger"), Data: map[string]string{
		toolledger.Key: strings.Join([]string{
			line(1, toolwire.VerbHTTP, evalArgs, evalOutput, sha256Hex([]byte(evalOutput))),
			line(2, toolwire.VerbBrowser, evalScript, evalReturned, sha256Hex([]byte(evalReturned))),
			// Entry 4's output was cut to fit: its digest is kept, the
			// output no longer hashes to it.
			line(4, toolwire.VerbBrowser, `{"verb":"snapshot"}`, cut, sha256Hex([]byte(snapshot))),
			// Entry 5's line is not the call the index records.
			line(5, toolwire.VerbHTTP, evalForged, "forged", sha256Hex([]byte("forged"))),
		}, "\n") + "\n",
	}}
	evidenceCM := &corev1.ConfigMap{ObjectMeta: pvOwned(evalPV + "-evidence"), BinaryData: map[string][]byte{
		fmt.Sprintf("2-%s.png", imgSha):   img,
		fmt.Sprintf("2-%s.png", evalShaA): []byte("<svg onload=alert(1)>"),
	}}
	return append(objs, run, pv, ledgerCM, evidenceCM)
}

// findAlpha7 is the fixture's alpha-7 Intent, the one the evaluation run
// belongs to.
func findAlpha7(objs []client.Object) *v1alpha1.Intent {
	for _, o := range objs {
		if in, ok := o.(*v1alpha1.Intent); ok && in.Name == "alpha-7" {
			return in
		}
	}
	panic("no intent alpha-7")
}

func evaluationPath(run string) string { return "/api/intents/alpha-7/runs/" + run + "/evaluation" }

func TestRunEvaluationTierOne(t *testing.T) {
	s, _ := intentsServer(t, nil, evaluationFixture(t)...)
	ts := as(t, s, viewerAlpha)
	code, body := get(t, ts, evaluationPath(evalRun))
	if code != http.StatusOK {
		t.Fatalf("GET evaluation = %d %s", code, body)
	}
	for _, leak := range []string{evalNote, evalOutput, "argv", evalBodyText, ".png", "secret-script",
		"secret-returned", "\u202e", `"own"`, `"verdict"`, `"cited"`} {
		if strings.Contains(body, leak) {
			t.Errorf("tier 1 evaluation carries %q: %s", leak, body)
		}
	}
	// The saved-check views are retired: the page shows the calls each
	// verdict cites instead.
	for _, retired := range []string{`"checks"`, `"disputes"`, `"rung"`, `"noBaseComparison"`, `"basis"`,
		`"line"`} {
		if strings.Contains(body, retired) {
			t.Errorf("tier 1 evaluation carries the retired %s: %s", retired, body)
		}
	}
	ev := decode[RunEvaluation](t, body)
	if !ev.Current || ev.Verification != evalPV || ev.Round != 1 || ev.Phase != "Observed" || ev.Tier != "intents" {
		t.Fatalf("header = %+v", ev)
	}
	if ev.Lead != templates.ObservedLead || ev.Withdrawn != "" {
		t.Errorf("lead %q, withdrawn %q; want the comment's lead and no withdrawal", ev.Lead, ev.Withdrawn)
	}
	checkTierOneOutcomes(t, ev)
	checkTierOneLedger(t, ev)
}

// TestRunEvaluationUnjudgedRound (S2-B3): the current verification's
// round that settled without a judgement shows none of an earlier round's
// results, cited calls or anomalies as its own: the outcomes are not
// decided for it.
func TestRunEvaluationUnjudgedRound(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(v *v1alpha1.IntentVerification, pv *v1alpha1.PreviewVerification)
	}{
		{"a later round", func(v *v1alpha1.IntentVerification, pv *v1alpha1.PreviewVerification) {
			v.Round, pv.Spec.Round = 2, 2
		}},
		{"heads that moved", func(v *v1alpha1.IntentVerification, pv *v1alpha1.PreviewVerification) {
			moved := "sha256:" + strings.Repeat("d", 64)
			v.HeadSetDigest, pv.Spec.HeadSetDigest = moved, moved
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := evaluationFixture(t)
			pv := findPV(objs)
			pv.Status.Phase, pv.Status.Reason = v1alpha1.VerificationInfra,
				v1alpha1.VerificationReasonToolsBuildFailed
			tc.move(findAlpha7(objs).Status.Verification, pv)
			s, _ := intentsServer(t, nil, objs...)
			code, body := get(t, as(t, s, viewerAlpha), evaluationPath(evalRun))
			if code != http.StatusOK {
				t.Fatalf("GET evaluation = %d %s", code, body)
			}
			ev := decode[RunEvaluation](t, body)
			if !ev.Current || ev.Lead != "" {
				t.Fatalf("header = %+v", ev)
			}
			for _, o := range ev.Outcomes {
				if o.Result != "" || o.Reason != "" || o.Words != "" || len(o.Cites) != 0 {
					t.Errorf("outcome %+v shows an earlier round's result", o)
				}
			}
			if len(ev.Anomalies) != 0 {
				t.Errorf("anomalies %+v: an earlier round's", ev.Anomalies)
			}
		})
	}
}

// checkTierOneOutcomes holds the hand-off and the decided outcomes, each in
// maul's words with the calls maul counted for it, with none of the
// evaluator's claims.
func checkTierOneOutcomes(t *testing.T, ev RunEvaluation) {
	t.Helper()
	if ev.Handoff == nil || ev.Handoff.Reason != "unconfirmed" || !ev.Handoff.Released {
		t.Errorf("handoff = %+v", ev.Handoff)
	}
	if len(ev.Outcomes) != 2 {
		t.Fatalf("outcomes = %+v", ev.Outcomes)
	}
	pass, unsure := ev.Outcomes[0], ev.Outcomes[1]
	if pass.Result != "passed" || pass.Reason != "seen-to-hold" || !slices.Equal(pass.Cites, []int32{1, 2}) ||
		pass.Words != templates.ObservedOutcomeWords("passed", "seen-to-hold") || pass.Verdict != "" {
		t.Errorf("passed outcome = %+v", pass)
	}
	if unsure.Kind != "keep" || unsure.Result != "unsure" || len(unsure.Cites) != 0 ||
		!strings.Contains(unsure.Words, "cites no call of its own") || unsure.Cited != nil {
		t.Errorf("unsure outcome = %+v", unsure)
	}
}

// checkTierOneLedger holds the ledger index (no outputs, no own labels),
// the anomaly and the capture.
func checkTierOneLedger(t *testing.T, ev RunEvaluation) {
	t.Helper()
	if ev.Ledger == nil || ev.Ledger.Outputs || len(ev.Ledger.Entries) != 5 || !ev.Ledger.Entries[2].Refused ||
		len(ev.Ledger.Entries[1].Artifacts) != 2 {
		t.Errorf("ledger = %+v", ev.Ledger)
	}
	if len(ev.Anomalies) != 1 {
		t.Errorf("anomalies %+v", ev.Anomalies)
	}
	if ev.Capture == nil || ev.Capture.Events != 4 || len(ev.Capture.Components) != 1 {
		t.Errorf("capture = %+v", ev.Capture)
	}
}

// TestRunEvaluationTierTwo: a transcripts-tier reader also gets the
// evaluator's claims and, for each ledger entry the outcomes' cites name,
// what it sent and returned, its stored screenshot, and whether it ran the
// evaluator's own page script or program.
func TestRunEvaluationTierTwo(t *testing.T) {
	s, _ := intentsServer(t, nil, evaluationFixture(t)...)
	ts := as(t, s, readerAlpha)
	code, body := get(t, ts, evaluationPath(evalRun))
	if code != http.StatusOK {
		t.Fatalf("GET evaluation = %d %s", code, body)
	}
	ev := decode[RunEvaluation](t, body)
	checkTierTwoClaims(t, ev)
	checkTierTwoLedger(t, ev, body)
}

// checkTierTwoClaims holds the evaluator's claims beside maul's decision.
func checkTierTwoClaims(t *testing.T, ev RunEvaluation) {
	t.Helper()
	if o := ev.Outcomes[0]; o.Verdict != "pass" || o.Note != evalNote || !slices.Equal(o.Cited, []int32{1, 2}) {
		t.Errorf("claims = %+v", o)
	}
	// The evaluator cited call 3 for leaderboard; maul counted none of it.
	if o := ev.Outcomes[1]; o.Verdict != "pass" || !slices.Equal(o.Cited, []int32{3}) || len(o.Cites) != 0 {
		t.Errorf("claims = %+v", o)
	}
}

// checkTierTwoLedger holds what each entry shows from its stored line:
// an http call; the evaluator's own page script; a refused call with no
// line; a call whose output was cut to fit, what it sent still shown; and
// one whose line is not the index's call, which shows nothing of it.
func checkTierTwoLedger(t *testing.T, ev RunEvaluation, body string) {
	t.Helper()
	l := ev.Ledger
	if l == nil || !l.Outputs || len(l.Entries) != 5 {
		t.Fatalf("ledger = %+v", l)
	}
	want := []EvaluationLedgerEntry{
		{Args: evalArgs, Output: evalOutput},
		{Args: evalScript, Output: evalReturned, Own: true},
		{OutputDropped: true},
		{Args: `{"verb":"snapshot"}`, OutputDropped: true},
		{OutputDropped: true},
	}
	for i, w := range want {
		e := l.Entries[i]
		if e.Args != w.Args || e.Output != w.Output || e.OutputDropped != w.OutputDropped || e.Own != w.Own {
			t.Errorf("entry %d = %+v, want args %q output %q dropped %v own %v", i+1, e, w.Args, w.Output,
				w.OutputDropped, w.Own)
		}
	}
	if arts := l.Entries[1].Artifacts; arts[0].Image == "" || arts[1].Image != "" {
		t.Errorf("only the PNG may be named: %+v", arts)
	}
	if strings.Contains(body, "forged") {
		t.Errorf("a line that is not the index's call is shown: %s", body)
	}
}

// TestRunEvaluationIgnoresARetiredReport: a report writing a key the
// observed evaluator retired claims nothing (report.CarriesRetiredKeys, as
// the controller judges it), so the page shows none of its claims beside
// maul's decision; a report without the key shows them.
func TestRunEvaluationIgnoresARetiredReport(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		claims      bool
	}{
		{"no retired key", "", true},
		{"disputes", "disputes: []\n", false},
		{"proposed", "proposed: []\n", false},
		{"retire", "retire: []\n", false},
		{"contrary", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := evaluationFixture(t)
			report := strings.Replace(evalReport, "\n---\n", "\n"+tc.extra+"---\n", 1)
			if tc.name == "contrary" {
				report = strings.Replace(report, "ledger: [3]", "ledger: [3]\n    contrary: []", 1)
			}
			findRun(objs, evalRun).Status.Report = report
			s, _ := intentsServer(t, nil, objs...)
			code, body := get(t, as(t, s, readerAlpha), evaluationPath(evalRun))
			if code != http.StatusOK {
				t.Fatalf("GET evaluation = %d %s", code, body)
			}
			ev := decode[RunEvaluation](t, body)
			for _, o := range ev.Outcomes {
				if got := o.Verdict != "" || o.Note != "" || o.Cited != nil; got != tc.claims {
					t.Errorf("outcome %s shows claims %v, want %v: %+v", o.ID, got, tc.claims, o)
				}
			}
			if ev.Outcomes[0].Result != "passed" || !slices.Equal(ev.Outcomes[0].Cites, []int32{1, 2}) {
				t.Errorf("maul's decision is shown whatever the report: %+v", ev.Outcomes[0])
			}
		})
	}
}

// TestRunEvaluationShowsTheWithdrawal: while an earlier result is withdrawn
// (the upgrade's supersede, W5), the page says so in the comment's words,
// on the fresh round's run and on the earlier one's.
func TestRunEvaluationShowsTheWithdrawal(t *testing.T) {
	for _, current := range []bool{true, false} {
		objs := evaluationFixture(t)
		in := findAlpha7(objs)
		in.Status.Conditions = append(in.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionVerificationSuperseded, Status: metav1.ConditionTrue,
			Reason: "VerificationRulesChanged", LastTransitionTime: metav1.NewTime(testClock)})
		if !current {
			in.Status.Verification.Ref = nil
		}
		s, _ := intentsServer(t, nil, objs...)
		code, body := get(t, as(t, s, viewerAlpha), evaluationPath(evalRun))
		if code != http.StatusOK {
			t.Fatalf("GET evaluation = %d %s", code, body)
		}
		if ev := decode[RunEvaluation](t, body); ev.Withdrawn != templates.WithdrawnSentence || ev.Current != current {
			t.Errorf("current=%v: withdrawn %q, current %v", current, ev.Withdrawn, ev.Current)
		}
	}
}

// TestRunEvaluationOwnLabelWithoutTheLedger (FC 4): with no ledger
// ConfigMap of the verification's own to read, a browser call is still
// labelled the evaluator's own page script at the transcripts tier, since
// nothing shows it was not one; an http call is not.
func TestRunEvaluationOwnLabelWithoutTheLedger(t *testing.T) {
	objs := evaluationFixture(t)
	findConfigMap(objs, evalPV+"-ledger").OwnerReferences = []metav1.OwnerReference{
		ctrlRef("PreviewVerification", "other", "uid-other")}
	s, _ := intentsServer(t, nil, objs...)
	code, body := get(t, as(t, s, readerAlpha), evaluationPath(evalRun))
	if code != http.StatusOK {
		t.Fatalf("GET evaluation = %d %s", code, body)
	}
	l := decode[RunEvaluation](t, body).Ledger
	for i, want := range []bool{false, true, false, true, false} {
		if got := l.Entries[i].Own; got != want {
			t.Errorf("entry %d (%s): own %v, want %v", i+1, l.Entries[i].Verb, got, want)
		}
	}
}

// TestRunEvaluationRefusesForeignConfigMaps: a ledger or evidence ConfigMap
// of the right name controlled by anything but the run's own verification
// is never read into the view nor served.
func TestRunEvaluationRefusesForeignConfigMaps(t *testing.T) {
	objs := evaluationFixture(t)
	for _, name := range []string{evalPV + "-ledger", evalPV + "-evidence"} {
		cm := findConfigMap(objs, name)
		cm.OwnerReferences = []metav1.OwnerReference{ctrlRef("PreviewVerification", "other", "uid-other")}
	}
	s, _ := intentsServer(t, nil, objs...)
	ts := as(t, s, readerAlpha)
	code, body := get(t, ts, evaluationPath(evalRun))
	if code != http.StatusOK {
		t.Fatalf("GET evaluation = %d %s", code, body)
	}
	if strings.Contains(body, evalOutput) || strings.Contains(body, ".png") {
		t.Errorf("foreign ConfigMaps read into the view: %s", body)
	}
	ev := decode[RunEvaluation](t, body)
	if ev.Ledger.Outputs {
		t.Error("outputs marked shown from a foreign ConfigMap")
	}
	img := imagePath(t, objs)
	if code, _ := get(t, ts, img); code != http.StatusNotFound {
		t.Errorf("foreign evidence image = %d, want 404", code)
	}
}

// TestRunEvaluationPinsVerification: the verification must be the one the
// run pins, of this very Intent; a run that is not an evaluate run has no
// evaluation.
func TestRunEvaluationPinsVerification(t *testing.T) {
	for name, mutate := range map[string]func(objs []client.Object){
		"wrong uid": func(objs []client.Object) {
			findRun(objs, evalRun).Spec.Evaluation.UID = "uid-someone-else"
		},
		"other intent": func(objs []client.Object) {
			findPV(objs).Spec.IntentRef.UID = "uid-older-alpha-7"
		},
		"missing": func(objs []client.Object) {
			findRun(objs, evalRun).Spec.Evaluation.Verification = "alpha-7-v9-a1"
		},
	} {
		t.Run(name, func(t *testing.T) {
			objs := evaluationFixture(t)
			mutate(objs)
			s, _ := intentsServer(t, nil, objs...)
			ts := as(t, s, readerAlpha)
			if code, body := get(t, ts, evaluationPath(evalRun)); code != http.StatusNotFound {
				t.Errorf("GET = %d %s, want 404", code, body)
			}
		})
	}
	s, _ := intentsServer(t, nil, evaluationFixture(t)...)
	if code, _ := get(t, as(t, s, readerAlpha), evaluationPath("alpha-7-plan-r1-a1")); code != http.StatusNotFound {
		t.Errorf("plan run evaluation = %d, want 404", code)
	}
	if code, _ := get(t, as(t, s, viewerBeta), evaluationPath(evalRun)); code != http.StatusNotFound {
		t.Errorf("another project's viewer = %d, want 404", code)
	}
}

func findPV(objs []client.Object) *v1alpha1.PreviewVerification {
	for _, o := range objs {
		if pv, ok := o.(*v1alpha1.PreviewVerification); ok {
			return pv
		}
	}
	panic("no verification")
}

func imagePath(t *testing.T, objs []client.Object) string {
	t.Helper()
	for k, v := range findConfigMap(objs, evalPV+"-evidence").BinaryData {
		if bytes.HasPrefix(v, pngSignature) {
			return "/api/intents/alpha-7/runs/" + evalRun + "/evidence/" + k
		}
	}
	t.Fatal("no PNG in the fixture")
	return ""
}

// TestEvidenceImage: tier 2 only, served as image/png with nosniff under
// the strict CSP, and only bytes that start as a PNG's.
func TestEvidenceImage(t *testing.T) {
	objs := evaluationFixture(t)
	s, _ := intentsServer(t, nil, objs...)
	path := imagePath(t, objs)

	req, err := http.NewRequest(http.MethodGet, as(t, s, readerAlpha).URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !bytes.HasPrefix(got, pngSignature) {
		t.Fatalf("GET image = %d (%d bytes)", res.StatusCode, len(got))
	}
	h := res.Header
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") ||
		!strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Errorf("headers = %v", h)
	}

	notPNG := "/api/intents/alpha-7/runs/" + evalRun + "/evidence/2-" + evalShaA + ".png"
	cases := []struct {
		name   string
		caller *auth.Identity
		path   string
		want   int
	}{
		{"not a PNG", readerAlpha, notPNG, http.StatusNotFound},
		{"absent", readerAlpha, "/api/intents/alpha-7/runs/" + evalRun + "/evidence/9-" + evalShaB + ".png",
			http.StatusNotFound},
		{"bad key", readerAlpha, "/api/intents/alpha-7/runs/" + evalRun + "/evidence/..%2Fplan.md",
			http.StatusNotFound},
		{"tier 1", viewerAlpha, path, http.StatusForbidden},
		{"other project", viewerBeta, path, http.StatusNotFound},
		{"plan run", readerAlpha, strings.Replace(path, evalRun, "alpha-7-plan-r1-a1", 1), http.StatusNotFound},
	}
	for _, tc := range cases {
		if code, body := get(t, as(t, s, tc.caller), tc.path); code != tc.want {
			t.Errorf("%s: GET = %d %q, want %d", tc.name, code, body, tc.want)
		}
	}
}

// TestRunEvaluationEarlierReleasesRecord: a judgement an earlier release
// wrote (no judge marker; W5 leaves it until its Project can run again) is
// not worded as the observed rules word one: its result is shown, never as
// seen by the evaluator, and the page carries no lead over it.
func TestRunEvaluationEarlierReleasesRecord(t *testing.T) {
	objs := evaluationFixture(t)
	v := findAlpha7(objs).Status.Verification
	v.Judge = ""
	for i := range v.Outcomes {
		v.Outcomes[i].Cites = nil
	}
	s, _ := intentsServer(t, nil, objs...)
	code, body := get(t, as(t, s, viewerAlpha), evaluationPath(evalRun))
	if code != http.StatusOK {
		t.Fatalf("GET evaluation = %d %s", code, body)
	}
	ev := decode[RunEvaluation](t, body)
	if ev.Lead != "" || ev.Outcomes[0].Result != "passed" || ev.Outcomes[0].Words != "" {
		t.Errorf("lead %q, outcome %+v; want the result alone", ev.Lead, ev.Outcomes[0])
	}
}
