// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// logsEntry is a `maul-tool logs component` ledger entry at seq showing
// output.
func logsEntry(seq int64, component, output string) toolwire.LedgerEntry {
	args, _ := json.Marshal(toolwire.LogsRequest{Component: component})
	sum := sha256.Sum256([]byte(output))
	return toolwire.LedgerEntry{Seq: seq, TS: time.Unix(seq, 0).UTC(), Kind: toolwire.KindAgent,
		Verb: toolwire.VerbLogs, Args: args, Output: output, OutputSHA256: hex.EncodeToString(sum[:])}
}

// checkRunEntry is a replay ledger entry at seq of check digest, failing
// on step step with detail.
func checkRunEntry(seq int64, digest, id string, step int, detail string) toolwire.LedgerEntry {
	return toolwire.LedgerEntry{Seq: seq, TS: time.Unix(seq, 0).UTC(), Kind: toolwire.KindRelay,
		Verb: toolwire.VerbCheckRun, Args: json.RawMessage(`{}`), CheckRun: &toolwire.CheckRunRecord{
			Digest: digest, CheckID: id, Mode: toolwire.ModeReplay, Result: toolwire.RunFail,
			Attempts: []toolwire.Attempt{
				{Run: "r1", Result: toolwire.ResultFail, Steps: []toolwire.StepRecord{
					{Index: 0, Result: toolwire.ResultPass}, {Index: step, Result: toolwire.ResultFail, Detail: detail}}},
				{Run: "r2", Result: toolwire.ResultFail, Steps: []toolwire.StepRecord{
					{Index: step, Result: toolwire.ResultFail, Detail: detail}}}}}}
}

// TestLogWindow: a failing run's window is what a component logged between
// the evaluator's last read of its log before the run and its first read
// after it (the capture grows from the verification's start, so the later
// read less the earlier one); none without a read after the run; an entry
// whose output was dropped to fit, refused or of another component never
// counts; the excerpt starts at the first error or panic line, else is the
// newest lines, at most the reproduction's 40.
func TestLogWindow(t *testing.T) {
	early := "boot ok\nGET /healthz 200\n"
	late := early + "POST /notes 201\npanic: nil map\ngoroutine 1\nGET /healthz 200\n"
	var many strings.Builder
	for i := range 60 {
		fmt.Fprintf(&many, "GET /x/%d 200\n", i)
	}
	dropped := logsEntry(9, "app", late)
	dropped.Output = "[maul: output dropped to fit; its sha256 is kept]"
	refused := logsEntry(9, "app", late)
	refused.Refused = toolwire.RefusedInvalid
	for _, tt := range []struct {
		name    string
		entries []toolwire.LedgerEntry
		want    []string
	}{
		{"read before and after", []toolwire.LedgerEntry{logsEntry(3, "app", early), logsEntry(9, "app", late)},
			[]string{"panic: nil map", "goroutine 1", "GET /healthz 200"}},
		{"no read before", []toolwire.LedgerEntry{logsEntry(9, "app", late)},
			[]string{"panic: nil map", "goroutine 1", "GET /healthz 200"}},
		{"no read after", []toolwire.LedgerEntry{logsEntry(3, "app", late)}, nil},
		{"first read after only", []toolwire.LedgerEntry{logsEntry(9, "app", early),
			logsEntry(12, "app", late)}, []string{"boot ok", "GET /healthz 200"}},
		{"another component", []toolwire.LedgerEntry{logsEntry(9, "web", late)}, nil},
		{"output dropped", []toolwire.LedgerEntry{dropped}, nil},
		{"refused", []toolwire.LedgerEntry{refused}, nil},
		{"rolled tail", []toolwire.LedgerEntry{logsEntry(3, "app", "old line\nboot ok\nGET /healthz 200\n"),
			logsEntry(9, "app", late)}, []string{"panic: nil map", "goroutine 1", "GET /healthz 200"}},
		{"no error line: newest", []toolwire.LedgerEntry{logsEntry(9, "app", many.String())},
			strings.Split(strings.TrimSuffix(many.String(), "\n"), "\n")[20:]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := logWindow(tt.entries, 5, "app"); !slices.Equal(got, tt.want) {
				t.Errorf("logWindow = %q, want %q", got, tt.want)
			}
		})
	}
}

// ledgerOfEntries plays preview-controller writing pv's full ledger
// (<pv>-ledger, owned by pv) of entries.
func (e *env) ledgerOfEntries(pvName string, entries ...toolwire.LedgerEntry) {
	e.t.Helper()
	pv := e.pvNamed(pvName)
	var b strings.Builder
	for _, en := range entries {
		line, err := json.Marshal(en)
		if err != nil {
			e.t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: pv.Name + "-ledger", Namespace: testNS,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(),
			Kind: "PreviewVerification", Name: pv.Name, UID: pv.UID, Controller: new(true)}}},
		Data: map[string]string{keyLedgerLines: b.String()}}
	if err := e.c.Create(context.Background(), cm); err != nil {
		e.t.Fatal(err)
	}
	var count int64
	for _, en := range entries {
		count = max(count, en.Seq)
	}
	pv.Status.Ledger = &v1alpha1.LedgerSummary{Count: int32(count), SHA256: strings.Repeat("a", 64),
		ConfigMap: cm.Name}
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// fixInput waits for name's first revise run's input and returns its data
// joined.
func (e *env) fixInput(name string) string {
	e.t.Helper()
	e.reviewUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStageRevise)) > 0 })
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	for range 8 {
		var cm corev1.ConfigMap
		if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: run.Spec.Inputs.ConfigMap},
			&cm); err == nil {
			return joinedData(cm.Data)
		}
		e.readyRepositories(repoImage)
		e.runRuns()
		e.review(name, 1)
	}
	e.t.Fatal("the fix round has no input")
	return ""
}

// joinedData is a ConfigMap's values, in key order.
func joinedData(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(data[k])
		b.WriteByte('\n')
	}
	return b.String()
}

// failRound drives name's first evaluation round to a reproduced failure of
// the version outcome: the stored version check fails its replay at seq 2
// (step 2, with detail), the evaluator reports fail citing it with note,
// and the ledger holds entries.
func (e *env) failRound(name, report string, entries func(versionDigest string) []toolwire.LedgerEntry) {
	e.t.Helper()
	e.evaluateReporting(report)
	version, health := savedCheck(e.t, versionCheck), savedCheck(e.t, healthCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.replayed(pv.Name, v1alpha1.CheckRun{CheckDigest: version.Digest, Result: v1alpha1.CheckRunFail, Token: "t1",
		Seq: 2}, v1alpha1.CheckRun{CheckDigest: health.Digest, Result: v1alpha1.CheckRunPass, Token: "t2", Seq: 3})
	e.ledgerOfEntries(pv.Name, entries(version.Digest)...)
	e.settleReviews(name)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing {
		e.t.Fatalf("verification = %s %+v, want Failing", v.State, v.Outcomes)
	}
}

// TestFixRoundGetsTheReproduction: an evaluation's verify-fix round is
// handed maul's reproduction of the failure: the approved outcome, the
// failing check's steps in maul's words with what each expects, the check
// runner's observation of the failing run from the tools ledger, and the
// log lines of the component the check targets from that run's window;
// never the check's body or digest, the store, the other check, or the
// evaluator's words.
func TestFixRoundGetsTheReproduction(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.failRound(name, failingReport, func(version string) []toolwire.LedgerEntry {
		return []toolwire.LedgerEntry{
			logsEntry(1, "app", "boot ok\n"),
			checkRunEntry(2, version, "version-shown", 1, `/note: expected "t1-v", got "dev"`),
			checkRunEntry(3, savedCheck(t, healthCheck).Digest, "still-healthy", 0, "never shown"),
			logsEntry(4, "app", "boot ok\nPOST /notes 201\nerror: note not stored\n"),
		}
	})
	all := e.fixInput(name)
	for _, want := range []string{
		"GET /version answers with the commit it runs",
		"#### Outcome `version-shown`",
		"1. Send `POST /notes` with JSON body `{\"name\":\"{{run}}-v\"}`. Expected: status `201`.",
		"2. Send `GET /version?q={{run}}`. Expected: status `200`; JSON body at `/note`: `\"{{run}}-v\"`.",
		"attempt 1, step 2: failed: `/note: expected \"t1-v\", got \"dev\"`",
		"Log lines of component `app`",
		"error: note not stored",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the fix round's input lacks %q:\n%s", want, all)
		}
	}
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	for _, f := range []string{version.Digest, health.Digest, "-checks-", "-ev-", "-cr-", version.Body,
		"wrong note", "never shown", "/healthz", "boot ok"} {
		if strings.Contains(all, f) {
			t.Errorf("the fix round's input carries %q", f)
		}
	}
}

// TestBuildHandoffNeverReferencesEvaluator: over random rounds (the
// evaluator's verdict notes, report body and a cited anomaly, the
// failing step's detail and the log lines), no build or revise handoff of
// the intent names the check store (-checks-), an evaluate (-ev-) or check
// review (-cr-) run, a check digest, a line of the evaluate or the check
// review prompt, or anything the evaluator wrote; the reproduction's own
// application data is the only round data a fix round sees.
func TestBuildHandoffNeverReferencesEvaluator(t *testing.T) {
	prompt := promptMarkers(t)
	for seed := range uint64(8) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 21))
			note, body := randomWords(rng, "note"), randomWords(rng, "body")
			anomaly := randomWords(rng, "anomaly")
			report := fmt.Sprintf("---\nverdicts:\n  - id: version-shown\n    verdict: fail\n    ledger: [2]\n"+
				"    note: %q\n  - id: still-healthy\n    verdict: pass\n    ledger: [3]\nanomalies:\n"+
				"  - summary: %q\n    route: /version\n    ledger: [4]\n---\n%s\n", note, anomaly, body)
			detail, logLine := randomWords(rng, "detail"), randomWords(rng, "log")
			e, name := newEvaluatorEnv(t, 0.4)
			e.failRound(name, report, func(version string) []toolwire.LedgerEntry {
				return []toolwire.LedgerEntry{
					checkRunEntry(2, version, "version-shown", 1, detail),
					logsEntry(rng.Int64N(3)+4, "app", "error: "+logLine+"\n"),
				}
			})
			// The fix round may or may not run: an anomaly releases the
			// hand-off but a reproduced failure still asks for a fix.
			all := e.fixInput(name)
			if !strings.Contains(all, detail) || !strings.Contains(all, logLine) {
				t.Errorf("the reproduction lacks the observed detail or the log line:\n%s", all)
			}
			for _, run := range e.runsOf(name, v1alpha1.IntentStageBuild) {
				var cm corev1.ConfigMap
				if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS,
					Name: run.Spec.Inputs.ConfigMap}, &cm); err == nil {
					all += joinedData(cm.Data)
				}
			}
			forbidden := append([]string{"-checks-", "-ev-", "-cr-", note, body, anomaly,
				savedCheck(t, versionCheck).Digest, savedCheck(t, healthCheck).Digest}, prompt...)
			for _, f := range forbidden {
				if strings.Contains(all, f) {
					t.Errorf("a build or revise handoff carries %q", f)
				}
			}
		})
	}
}

// randomWords is a random sentence tagged kind, distinct from any text
// maul writes.
func randomWords(rng *rand.Rand, kind string) string {
	words := []string{"zeta", "quill", "marrow", "vortex", "plinth", "saffron", "gimbal", "tundra"}
	n := 4 + rng.IntN(4)
	out := make([]string, 0, n+2)
	out = append(out, "sentinel", kind)
	for range n {
		out = append(out, words[rng.IntN(len(words))])
	}
	return strings.Join(out, "-") + fmt.Sprintf("-%d", rng.Uint32())
}

// promptMarkers are the evaluate and check review prompts' own long lines,
// rendered over plain values: none may reach a build or revise handoff.
func promptMarkers(t *testing.T) []string {
	t.Helper()
	ev, err := templates.RenderEvaluatePrompt(templates.EvaluatePrompt{ReportPath: "report.md", ChecksDir: "checks",
		ShotsDir: "shots", Round: 1, RunToken: "abcdefgh12345678", Outcomes: outcomesOf(t)})
	if err != nil {
		t.Fatal(err)
	}
	cr, err := templates.RenderCheckReviewPrompt(templates.CheckReviewPrompt{ReportPath: "report.md",
		Outcome: "o", Check: parsedCheck(t, healthCheck)})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(ev+"\n"+cr, "\n") {
		if line = strings.TrimSpace(line); len(line) > 60 && !strings.ContainsAny(line, "`{") {
			out = append(out, line)
		}
	}
	if len(out) < 10 {
		t.Fatalf("only %d prompt markers", len(out))
	}
	return out
}

// outcomesOf is one plain outcome for rendering a prompt.
func outcomesOf(t *testing.T) []acceptance.PlanOutcome {
	t.Helper()
	return []acceptance.PlanOutcome{{ID: "o", Outcome: "the page loads"}}
}

// parsedCheck parses raw, failing t on a bad body.
func parsedCheck(t *testing.T, raw string) checks.Check {
	t.Helper()
	c, err := checks.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
