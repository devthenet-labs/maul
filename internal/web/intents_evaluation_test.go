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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
	"github.com/devthenet-labs/maul/internal/web/auth"
)

// The evaluation fixture: alpha-7's evaluate run of round 1, its
// PreviewVerification, and the verification's ledger and evidence
// ConfigMaps, with private strings the tier 1 view must never carry.
const (
	evalRun       = "alpha-7-ev-r1-a1"
	evalPV        = "alpha-7-v1-a1"
	evalPVUID     = types.UID("uid-pv-alpha-7")
	evalNote      = "The rating moved by 16 points, said the evaluator"
	evalOutput    = "HTTP/1.1 200 OK secret-app-output"
	evalArgs      = `{"argv":["GET","/battles"]}`
	evalBodyText  = "an evaluate report body"
	evalTokenSeen = "v1a1t1-abc123"
)

var (
	evalShaA = strings.Repeat("a", 64)
	evalShaB = strings.Repeat("b", 64)
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

// savedCheck builds a SavedCheck from a check's JSON as the controllers do:
// its canonical body and digest.
func savedCheck(t *testing.T, raw string) v1alpha1.SavedCheck {
	t.Helper()
	c, err := checks.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v1alpha1.SavedCheck{ID: c.ID, Outcome: c.Outcome, Form: v1alpha1.CheckForm(c.Form),
		Body: string(checks.Canonical(c)), Digest: checks.Digest(c)}
}

const (
	browserCheckJSON = `{"id":"rate-battle.name","outcome":"rate-battle","form":"browser","browser":{"steps":[
		{"goto":"/battles"},
		{"fill":{"locator":{"label":"Name"},"text":"Battle {{run}}"}},
		{"press":{"locator":{"label":"Name"},"key":"Enter"}},
		{"waitForURL":"/battles/*"},
		{"expect":{"assert":"text","locator":{"role":"heading","nth":0},"text":{"equals":"Battle {{run}}"}}}]}}`
	httpCheckJSON = `{"id":"leaderboard","outcome":"leaderboard","form":"http","http":{"target":"app","steps":[
		{"request":{"path":"/api/leaderboard?run={{run}}"},"expect":{"status":200,"json":{"$.rows[0].name":"{{run}}"}}}]}}`
)

// evaluationFixture is the intents fixture plus the evaluation objects.
func evaluationFixture(t *testing.T) []client.Object {
	t.Helper()
	objs := intentsFixture(t)
	browser := savedCheck(t, browserCheckJSON)
	proposed := savedCheck(t, httpCheckJSON)
	img := tinyPNG(t)
	imgSha := sha256Hex(img)

	in := findIntent(objs, "alpha-7")
	released := metav1.NewTime(testClock.Add(-time.Minute))
	in.Status.Verification = &v1alpha1.IntentVerification{
		Round: 1, Attempt: 1, Ref: &v1alpha1.ObjectReference{Name: evalPV, UID: evalPVUID},
		Outcomes: []v1alpha1.OutcomeResult{
			{ID: "rate-battle", Result: v1alpha1.OutcomePassed, Basis: browser.Digest,
				Line: "opens /battles, names a battle, sees its heading"},
			{ID: "leaderboard", Result: v1alpha1.OutcomeUnsure, Reason: "the replay failed on shared data"},
		},
		Anomalies: []v1alpha1.AnomalyRecord{{Summary: "Console error on /battles\u202e", Route: "/battles",
			Ledger: []int32{3}}},
		Disputed: []string{browser.Digest},
		Handoff: &v1alpha1.HandoffState{Mode: v1alpha1.HandoffGated, Released: true, ReleasedAt: &released,
			Reason: v1alpha1.HandoffReasonUnconfirmed},
	}

	report := "---\nverdicts:\n  - id: rate-battle\n    verdict: pass\n    ledger: [1, 2]\n    note: \"" +
		evalNote + "\"\n  - id: leaderboard\n    verdict: unsure\ndisputes:\n  - check_digest: \"" +
		browser.Digest + "\"\n    reason: \"The check counted rows other runs created.\"\n    ledger: [2]\n" +
		"---\n" + evalBodyText + "\n"
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
		Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunComplete, Outcome: "ok", Report: report},
	}

	pv := &v1alpha1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{Name: evalPV, Namespace: "maul", UID: evalPVUID},
		Spec: v1alpha1.PreviewVerificationSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "alpha-7", UID: "uid-alpha-7"},
			Round:     1, Attempt: 1, Rung: v1alpha1.VerificationRung("full"),
			Outcomes: []v1alpha1.AcceptanceOutcome{
				{ID: "rate-battle", Outcome: "A rated battle moves the rating"},
				{ID: "leaderboard", Outcome: "The leaderboard lists the battle", Kind: v1alpha1.OutcomeKindKeep},
			},
			Checks: []v1alpha1.SavedCheck{browser},
		},
		Status: v1alpha1.PreviewVerificationStatus{
			Phase: v1alpha1.VerificationObserved,
			Ledger: &v1alpha1.LedgerSummary{Count: 3, SHA256: evalShaA, ConfigMap: evalPV + "-ledger",
				Evidence: evalPV + "-evidence",
				Entries: []v1alpha1.LedgerIndexEntry{
					{Seq: 1, Kind: v1alpha1.LedgerKindRunner, Verb: v1alpha1.LedgerVerbCheckRun,
						ResultSHA256: sha256Hex([]byte(evalOutput))},
					{Seq: 2, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbBrowser,
						ResultSHA256: evalShaB, Artifacts: []string{imgSha, evalShaA}},
					{Seq: 3, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbHTTP, Refused: true,
						ResultSHA256: sha256Hex(nil)},
				}},
			Replay: []v1alpha1.CheckRun{{CheckDigest: browser.Digest, Result: v1alpha1.CheckRunFail,
				Token: evalTokenSeen, Seq: 1}},
			Confirmations: []v1alpha1.CheckRun{{CheckDigest: proposed.Digest, Result: v1alpha1.CheckRunPass,
				Token: "v1a1t2-def456", Seq: 3, DryRunSeq: 2}},
			Confirmed: []v1alpha1.SavedCheck{proposed},
			Capture: &v1alpha1.CaptureSummary{Events: 4,
				Components: []v1alpha1.ComponentCapture{{Name: "app", Bytes: 2048, Truncated: true}}},
		},
	}
	line := func(seq int64, args, output string) string {
		b, err := json.Marshal(toolwire.LedgerEntry{Seq: seq, Kind: toolwire.Kind("agent"), Args: json.RawMessage(args),
			Output: output, OutputSHA256: sha256Hex([]byte(output))})
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
		ledgerDataKey: line(1, evalArgs, evalOutput) + "\n" +
			// Entry 2's output was cut to fit: it no longer hashes to the index.
			line(2, `{"verb":"snapshot"}`, "[maul: output dropped to fit; its sha256 is kept]") + "\n",
	}}
	evidenceCM := &corev1.ConfigMap{ObjectMeta: pvOwned(evalPV + "-evidence"), BinaryData: map[string][]byte{
		fmt.Sprintf("2-%s.png", imgSha):   img,
		fmt.Sprintf("2-%s.png", evalShaA): []byte("<svg onload=alert(1)>"),
	}}
	return append(objs, run, pv, ledgerCM, evidenceCM)
}

func findIntent(objs []client.Object, name string) *v1alpha1.Intent {
	for _, o := range objs {
		if in, ok := o.(*v1alpha1.Intent); ok && in.Name == name {
			return in
		}
	}
	panic("no intent " + name)
}

func evaluationPath(run string) string { return "/api/intents/alpha-7/runs/" + run + "/evaluation" }

func TestRunEvaluationTierOne(t *testing.T) {
	s, _ := intentsServer(t, nil, evaluationFixture(t)...)
	ts := as(t, s, viewerAlpha)
	code, body := get(t, ts, evaluationPath(evalRun))
	if code != http.StatusOK {
		t.Fatalf("GET evaluation = %d %s", code, body)
	}
	for _, leak := range []string{evalNote, evalOutput, "argv", evalBodyText, ".png", "counted rows",
		"\u202e"} {
		if strings.Contains(body, leak) {
			t.Errorf("tier 1 evaluation carries %q: %s", leak, body)
		}
	}
	ev := decode[RunEvaluation](t, body)
	if !ev.Current || ev.Verification != evalPV || ev.Round != 1 || ev.Phase != "Observed" || ev.Tier != "intents" {
		t.Fatalf("header = %+v", ev)
	}
	checkTierOneOutcomes(t, ev)
	checkTierOneChecks(t, ev.Checks)
	checkTierOneLedger(t, ev)
}

// checkTierOneOutcomes holds the hand-off and the decided outcomes, with
// none of the evaluator's claims.
func checkTierOneOutcomes(t *testing.T, ev RunEvaluation) {
	t.Helper()
	if ev.Handoff == nil || ev.Handoff.Reason != "unconfirmed" || !ev.Handoff.Released {
		t.Errorf("handoff = %+v", ev.Handoff)
	}
	if len(ev.Outcomes) != 2 || ev.Outcomes[0].Result != "passed" || ev.Outcomes[0].Line == "" ||
		ev.Outcomes[1].Kind != "keep" || ev.Outcomes[1].Result != "unsure" || ev.Outcomes[0].Verdict != "" {
		t.Errorf("outcomes = %+v", ev.Outcomes)
	}
}

// checkTierOneLedger holds the ledger index (no outputs), the anomaly, the
// dispute without its reason, and the capture.
func checkTierOneLedger(t *testing.T, ev RunEvaluation) {
	t.Helper()
	if ev.Ledger == nil || ev.Ledger.Outputs || len(ev.Ledger.Entries) != 3 || !ev.Ledger.Entries[2].Refused ||
		len(ev.Ledger.Entries[1].Artifacts) != 2 {
		t.Errorf("ledger = %+v", ev.Ledger)
	}
	if len(ev.Anomalies) != 1 || len(ev.Disputes) != 1 || ev.Disputes[0].Reason != "" {
		t.Errorf("anomalies %+v, disputes %+v", ev.Anomalies, ev.Disputes)
	}
	if ev.Capture == nil || ev.Capture.Events != 4 || len(ev.Capture.Components) != 1 {
		t.Errorf("capture = %+v", ev.Capture)
	}
}

// checkTierOneChecks holds the fixture's two checks: the saved browser
// check replayed, disputed and compiled with the replay's token, and the
// proposed http check confirmed.
func checkTierOneChecks(t *testing.T, cs []EvaluationCheck) {
	t.Helper()
	if len(cs) != 2 {
		t.Fatalf("checks = %+v", cs)
	}
	b, h := cs[0], cs[1]
	if b.Source != "saved" || !b.Disputed || b.Replay == nil || b.Replay.Result != "fail" ||
		!strings.Contains(b.Steps, "\n  \"browser\"") {
		t.Errorf("saved check = %+v", b)
	}
	if b.CompiledToken != evalTokenSeen || !strings.Contains(b.Compiled, "Battle "+evalTokenSeen) {
		t.Errorf("compiled = %q with %q", b.Compiled, b.CompiledToken)
	}
	if h.Source != "proposed" || h.Confirmation == nil || h.Confirmation.DryRunSeq != 2 || h.Compiled != "" {
		t.Errorf("proposed check = %+v", h)
	}
}

func TestRunEvaluationTierTwo(t *testing.T) {
	s, _ := intentsServer(t, nil, evaluationFixture(t)...)
	ts := as(t, s, readerAlpha)
	code, body := get(t, ts, evaluationPath(evalRun))
	if code != http.StatusOK {
		t.Fatalf("GET evaluation = %d %s", code, body)
	}
	ev := decode[RunEvaluation](t, body)
	if ev.Outcomes[0].Verdict != "pass" || ev.Outcomes[0].Note != evalNote ||
		len(ev.Outcomes[0].Cited) != 2 {
		t.Errorf("claims = %+v", ev.Outcomes[0])
	}
	if len(ev.Disputes) != 1 || ev.Disputes[0].Reason == "" {
		t.Errorf("disputes = %+v", ev.Disputes)
	}
	l := ev.Ledger
	if l == nil || !l.Outputs {
		t.Fatalf("ledger = %+v", l)
	}
	if e := l.Entries[0]; e.Output != evalOutput || e.Args != evalArgs || e.OutputDropped {
		t.Errorf("entry 1 = %+v", e)
	}
	if e := l.Entries[1]; e.Output != "" || !e.OutputDropped {
		t.Errorf("entry 2 output should be dropped: %+v", e)
	}
	if e := l.Entries[2]; !e.OutputDropped {
		t.Errorf("entry 3 has no line, its output should be dropped: %+v", e)
	}
	arts := l.Entries[1].Artifacts
	if arts[0].Image == "" || arts[1].Image != "" {
		t.Errorf("only the PNG may be named: %+v", arts)
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
