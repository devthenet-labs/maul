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
// (<pv>-ledger, owned by pv) of entries, and its verified index in status
// (toolledger.Index's fields): a seq no entry has is the relay's own call,
// which never counts.
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
		ConfigMap: cm.Name, Entries: indexOf(count, entries)}
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// indexOf is the verified index of a ledger of count entries holding
// entries, as toolledger.Index writes it; a seq no entry has is a relay
// http call.
func indexOf(count int64, entries []toolwire.LedgerEntry) []v1alpha1.LedgerIndexEntry {
	out := make([]v1alpha1.LedgerIndexEntry, 0, count)
	for seq := int64(1); seq <= count; seq++ {
		ix := v1alpha1.LedgerIndexEntry{Seq: int32(seq), Kind: v1alpha1.LedgerKindRelay, Verb: v1alpha1.LedgerVerbHTTP,
			ResultSHA256: strings.Repeat("c", 64)}
		if i := slices.IndexFunc(entries, func(en toolwire.LedgerEntry) bool { return en.Seq == seq }); i >= 0 {
			en := entries[i]
			ix = v1alpha1.LedgerIndexEntry{Seq: int32(seq), Kind: v1alpha1.LedgerKind(en.Kind),
				Verb: v1alpha1.LedgerVerb(en.Verb), Exit: int32(en.Exit), ResultSHA256: en.OutputSHA256,
				Refused: en.Refused != "", Side: v1alpha1.TreeSide(en.Side.Recorded())}
		}
		out = append(out, ix)
	}
	return out
}

// httpEntry is the evaluator's `maul-tool http` call at seq, sending
// method and path and getting output.
func httpEntry(seq int64, method, path, output string) toolwire.LedgerEntry {
	args, _ := json.Marshal(toolwire.HTTPRequest{Method: method, Path: path})
	sum := sha256.Sum256([]byte(output))
	return toolwire.LedgerEntry{Seq: seq, TS: time.Unix(seq, 0).UTC(), Kind: toolwire.KindAgent,
		Verb: toolwire.VerbHTTP, Args: args, Output: output, OutputSHA256: hex.EncodeToString(sum[:])}
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

// failRound drives name's first evaluation round to an observed failure
// of the version outcome: the evaluator reports it, citing the ledger's
// entries (the evaluator's own calls), and the round's ledger holds
// entries and the index they verify to.
func (e *env) failRound(name, report string, entries []toolwire.LedgerEntry) {
	e.t.Helper()
	e.evaluateReporting(report)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.ledgerOfEntries(pv.Name, entries...)
	e.settleReviews(name)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing {
		e.t.Fatalf("verification = %s %+v, want Failing", v.State, v.Outcomes)
	}
}

// TestFixRoundGetsTheReproduction: an evaluation's verify-fix round is
// handed maul's reproduction of the failure: the approved outcome, the call
// the evaluator's fail cites (what it sent and what came back, from the
// round's ledger), the evaluator's own calls before it (what each sent),
// and the log lines of the round's component from that call's window;
// never the report's body, another outcome's calls, or a run's or the
// check store's name.
func TestFixRoundGetsTheReproduction(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.failRound(name, failingReport, []toolwire.LedgerEntry{
		logsEntry(1, "app", "boot ok\n"),
		httpEntry(2, "GET", "/version", `{"commit":"dev","note":"dev"}`),
		httpEntry(3, "GET", "/healthz", "never shown"),
		logsEntry(4, "app", "boot ok\nPOST /notes 201\nerror: note not stored\n"),
	})
	all := e.fixInput(name)
	for _, want := range []string{
		"GET /version answers with the commit it runs",
		"#### Outcome `version-shown`",
		"Call 2, an http request:",
		`{"commit":"dev","note":"dev"}`,
		"Call 1, ",
		"Log lines of component `app`",
		"error: note not stored",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the fix round's input lacks %q:\n%s", want, all)
		}
	}
	for _, f := range []string{"-checks-", "-ev-", "-cr-", "wrong note", "never shown", "/healthz", "boot ok"} {
		if strings.Contains(all, f) {
			t.Errorf("the fix round's input carries %q", f)
		}
	}
}

// TestFixRoundSeesTheEvaluatorsReading (owner decision 5): the evaluator's
// one-line note on the failing outcome reaches the fix round labelled as
// its reading, not checked by maul, fenced; the cited call's output reaches
// both the fix round and the person's verification comment, which says its
// rows were seen by maul's evaluator and names the call cited.
func TestFixRoundSeesTheEvaluatorsReading(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	const note = "The version endpoint names the dev build, not the commit."
	e.failRound(name, strings.Replace(failingReport, "    ledger: [2]\n",
		"    ledger: [2]\n    note: \""+note+"\"\n", 1), []toolwire.LedgerEntry{
		httpEntry(1, "GET", "/", "home"),
		httpEntry(2, "GET", "/version", `{"commit":"dev-build-7"}`),
		httpEntry(3, "GET", "/healthz", "ok"),
	})
	all := e.fixInput(name)
	label := "The evaluator's reading, not checked by maul:\n\n```text\n" + note + "\n```"
	if !strings.Contains(all, label) || !strings.Contains(all, "dev-build-7") {
		t.Errorf("the fix round's input lacks the labelled note or the cited output:\n%s", all)
	}
	comments := e.verifyComments(name, 1)
	if len(comments) == 0 {
		t.Fatal("no verification comment")
	}
	body := comments[len(comments)-1].Body
	for _, want := range []string{"seen not to hold by maul's evaluator", "it cites call 2", "dev-build-7",
		templates.ObservedLead} {
		if !strings.Contains(body, want) {
			t.Errorf("the verification comment lacks %q:\n%s", want, body)
		}
	}
}

// TestBuildHandoffCarriesOnlyCitedEvidence (restating
// TestBuildHandoffNeverReferencesEvaluator for the observed round): over
// random rounds, the fix round's input holds the cited call's args and
// output, the evaluator's call before it, the log line of its window and
// the failing outcome's note labelled as the evaluator's reading inside a
// fence; and no build or revise handoff of the intent holds the report's
// body, another outcome's note, an anomaly, a call no failure cites after
// the cited one, a line of the evaluate or the check review prompt, or the
// name of an evaluate (-ev-) or check review (-cr-) run or a check store
// (-checks-).
func TestBuildHandoffCarriesOnlyCitedEvidence(t *testing.T) {
	prompt := promptMarkers(t)
	for seed := range uint64(8) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 21))
			note, other, body := randomWords(rng, "note"), randomWords(rng, "other"), randomWords(rng, "body")
			anomaly, sent, got := randomWords(rng, "anomaly"), randomWords(rng, "sent"), randomWords(rng, "got")
			before, after, logLine := randomWords(rng, "before"), randomWords(rng, "after"), randomWords(rng, "log")
			report := fmt.Sprintf("---\nverdicts:\n  - id: version-shown\n    verdict: fail\n    ledger: [2]\n"+
				"    note: %q\n  - id: still-healthy\n    verdict: pass\n    ledger: [3]\n    note: %q\n"+
				"anomalies:\n  - summary: %q\n    route: /version\n    ledger: [3]\n---\n%s\n", note, other, anomaly, body)
			e, name := newEvaluatorEnv(t, 0.4)
			e.failRound(name, report, []toolwire.LedgerEntry{
				httpEntry(1, "POST", "/"+before, "home"),
				httpEntry(2, "GET", "/"+sent, got),
				httpEntry(3, "GET", "/"+after, "fine"),
				logsEntry(rng.Int64N(3)+4, "app", "error: "+logLine+"\n"),
			})
			all := e.fixInput(name)
			for _, want := range []string{sent, got, before, logLine} {
				if !strings.Contains(all, want) {
					t.Errorf("the reproduction lacks %q:\n%s", want, all)
				}
			}
			if label := "The evaluator's reading, not checked by maul:\n\n```text\n" + note + "\n```"; !strings.Contains(
				all, label) {
				t.Errorf("the reproduction lacks the note fenced under its label:\n%s", all)
			}
			for _, run := range e.runsOf(name, v1alpha1.IntentStageBuild) {
				var cm corev1.ConfigMap
				if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS,
					Name: run.Spec.Inputs.ConfigMap}, &cm); err == nil {
					all += joinedData(cm.Data)
				}
			}
			forbidden := append([]string{"-checks-", "-ev-", "-cr-", other, body, anomaly, after}, prompt...)
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

// promptMarkers are the evaluate prompt's own long lines, rendered over
// plain values: none may reach a build or revise handoff.
func promptMarkers(t *testing.T) []string {
	t.Helper()
	ev, err := templates.RenderEvaluatePrompt(templates.EvaluatePrompt{ReportPath: "report.md",
		ShotsDir: "shots", Round: 1, RunToken: "abcdefgh12345678", Outcomes: outcomesOf(t)})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(ev, "\n") {
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

// browserEntry is the evaluator's `maul-tool browser verb args...` call at
// seq, getting output.
func browserEntry(seq int64, verb, output string, args ...string) toolwire.LedgerEntry {
	raw, _ := json.Marshal(toolwire.BrowserRequest{Verb: verb, Args: args})
	sum := sha256.Sum256([]byte(output))
	return toolwire.LedgerEntry{Seq: seq, TS: time.Unix(seq, 0).UTC(), Kind: toolwire.KindAgent,
		Verb: toolwire.VerbBrowser, Args: raw, Output: output, OutputSHA256: hex.EncodeToString(sum[:])}
}

// TestFixRoundReproductionSaysWhatItLeavesOut (W13): the path to the cited
// call is never cut silently. In W13 the bound on it kept only the last
// eight calls and dropped call 3, the goto the reproduction starts from,
// saying nothing. Now the fix round is handed the last page the evaluator
// went to however far back it is, is told how many earlier calls were
// left out and which, which calls maul refused and whose request it did
// not keep, and the evaluator's own page scripts on the path are labelled
// so.
func TestFixRoundReproductionSaysWhatItLeavesOut(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	entries := []toolwire.LedgerEntry{browserEntry(1, "goto", "ok", "/app/"), httpEntry(2, "GET", "/", "home")}
	for seq := int64(3); seq <= 11; seq++ {
		entries = append(entries, browserEntry(seq, "eval", "1", fmt.Sprintf("window.step%d()", seq)))
	}
	refused := httpEntry(12, "GET", "/admin", "")
	refused.Refused = toolwire.RefusedInvalid
	entries = append(entries, refused, httpEntry(13, "GET", "/unkept", "x"),
		httpEntry(14, "GET", "/version", `{"commit":"dev"}`), httpEntry(15, "GET", "/healthz", "ok"))
	report := strings.NewReplacer("ledger: [2]", "ledger: [14]", "ledger: [3]", "ledger: [15]").Replace(failingReport)
	e.evaluateReporting(report)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.ledgerOfEntries(pv.Name, entries...)
	// Call 13's kept line is not the index's call: maul did not keep what
	// it sent.
	got := e.pvNamed(pv.Name)
	got.Status.Ledger.Entries[12].ResultSHA256 = strings.Repeat("d", 64)
	if err := e.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	e.settleReviews(name)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing {
		t.Fatalf("verification = %s %+v, want Failing", v.State, v.Outcomes)
	}
	all := e.fixInput(name)
	flatAll := strings.Join(strings.Fields(all), " ")
	for _, want := range []string{
		`Call 1, a browser command: {"verb":"goto","args":["/app/"]}`,
		"Call 4, a browser command, the evaluator's own page script: ",
		"maul left out 2 of the evaluator's earlier calls here to keep this short: calls 2 and 3. Call 1 is " +
			"kept: it is the last page the evaluator went to before the cited calls.",
		"maul refused 1 of the evaluator's calls before the ones it cites, not shown: call 12.",
		"maul did not keep what 1 of the evaluator's calls before the ones it cites sent, not shown: call 13.",
		"Call 14, an http request:",
	} {
		if !strings.Contains(flatAll, want) {
			t.Errorf("the fix round's input lacks %q:\n%s", want, all)
		}
	}
	for _, gone := range []string{"window.step3()", "/admin", "/unkept"} {
		if strings.Contains(all, gone) {
			t.Errorf("the fix round's input shows %q, which it says it left out:\n%s", gone, all)
		}
	}
}
