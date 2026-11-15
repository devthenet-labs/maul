// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/templates"
)

// decodeEvaluateHandoff decodes raw strictly, as agent-runner does.
func decodeEvaluateHandoff(t *testing.T, raw string) agentrun.EvaluateHandoff {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var h agentrun.EvaluateHandoff
	if err := dec.Decode(&h); err != nil {
		t.Fatalf("handoff %s: %v", raw, err)
	}
	return h
}

// TestEvaluateHandoffCarriesThePreviousRound (S1-29, SPEC Q15): a round
// after a judged one hands the evaluator the previous round's results from
// what the controller stores: maul's verdicts per outcome (the Intent's
// status.verification.outcomes), how its saved checks replayed (that round's
// PreviewVerification), and the fix's diff since its heads, one compare per
// moved head, bounded and made visible. Round 1, and a round at the same
// heads, get no diff; round 1 no previous round at all.
func TestEvaluateHandoffCarriesThePreviousRound(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv1 := e.onlyVerification(name)
	check := savedCheck(t, versionCheck)
	pv1.Spec.Checks = []v1alpha1.SavedCheck{check}
	pv1.Status.Phase = v1alpha1.VerificationObserved
	pv1.Status.Replay = []v1alpha1.CheckRun{{CheckDigest: check.Digest, Result: v1alpha1.CheckRunFail,
		Token: "t1", Seq: 1}}
	if err := e.c.Update(context.Background(), &pv1); err != nil {
		t.Fatal(err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Outcomes = []v1alpha1.OutcomeResult{
			{ID: oVersion, Result: v1alpha1.OutcomeFailed, Reason: "the note is wrong\u202e"},
			{ID: oHealthy, Result: v1alpha1.OutcomePassed, Basis: check.Digest},
			{ID: "gone-outcome", Result: v1alpha1.OutcomePassed},
		}
	})
	e.gh.mu.Lock()
	e.gh.patch = "--- a/version.go\n+++ b/version.go\n-old\n+new" + strings.Repeat("x", 2*templates.FixDiffMaxBytes)
	e.gh.mu.Unlock()

	p := e.prVerifyPass(name)
	first := decodeEvaluateHandoff(t, mustHandoff(t, p, &pv1))
	if first.PriorRound != 0 || len(first.PriorVerdicts) != 0 || len(first.PriorReplay) != 0 || first.FixDiff != "" {
		t.Errorf("round 1 handoff = %+v, want no previous round", first)
	}

	pv2 := pv1.DeepCopy()
	pv2.Name, pv2.UID = pv1.Name+"-next", types.UID("pv2-uid")
	pv2.Spec.Round, pv2.Status = 2, v1alpha1.PreviewVerificationStatus{}
	moved := strings.Repeat("b", 40)
	pv2.Spec.Heads[0].Revision = moved
	checkPriorRound(t, decodeEvaluateHandoff(t, mustHandoff(t, p, pv2)), check)
	e.gh.mu.Lock()
	compared := append([]string(nil), e.gh.compared...)
	e.gh.mu.Unlock()
	want := "acme/app " + pv1.Spec.Heads[0].Revision + "..." + moved
	if len(compared) != 1 || compared[0] != want {
		t.Errorf("compared = %v, want [%s]", compared, want)
	}

	pv2.Spec.Heads[0].Revision = pv1.Spec.Heads[0].Revision
	same := decodeEvaluateHandoff(t, mustHandoff(t, p, pv2))
	if same.FixDiff != "" || same.PriorRound != 1 || len(same.PriorVerdicts) != 2 {
		t.Errorf("a round at the same heads = %+v, want the previous round and no diff", same)
	}
}

// checkPriorRound holds h, round 2's handoff after a judged round 1, to
// that round's results: maul's verdicts of the approved outcomes made
// visible, check's replay, and the moved head's diff, bounded.
func checkPriorRound(t *testing.T, h agentrun.EvaluateHandoff, check v1alpha1.SavedCheck) {
	t.Helper()
	if h.PriorRound != 1 {
		t.Errorf("prior round = %d, want 1", h.PriorRound)
	}
	if len(h.PriorVerdicts) != 2 || h.PriorVerdicts[0].Outcome != oVersion || h.PriorVerdicts[0].Result != "failed" ||
		strings.ContainsRune(h.PriorVerdicts[0].Reason, '\u202e') || !strings.Contains(h.PriorVerdicts[0].Reason,
		"the note is wrong") || h.PriorVerdicts[1] != (agentrun.PriorVerdict{Outcome: oHealthy, Result: "passed"}) {
		t.Errorf("prior verdicts = %+v, want maul's results of the approved outcomes, made visible", h.PriorVerdicts)
	}
	if len(h.PriorReplay) != 1 || h.PriorReplay[0] != (agentrun.PriorReplay{Check: check.ID, Outcome: check.Outcome,
		Result: "fail"}) {
		t.Errorf("prior replay = %+v, want the version check's fail", h.PriorReplay)
	}
	if !strings.Contains(h.FixDiff, "+new") || len(h.FixDiff) > templates.FixDiffMaxBytes {
		t.Errorf("fix diff = %d bytes, want the compare bounded to %d", len(h.FixDiff), templates.FixDiffMaxBytes)
	}
}

// mustHandoff renders pv's evaluate handoff on p.
func mustHandoff(t *testing.T, p *pass, pv *v1alpha1.PreviewVerification) string {
	t.Helper()
	raw, err := p.evaluateHandoff(context.Background(), pv)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestEvaluateHandoffCarriesTheBrowserEntry: a Project's surfaces.browser.entry
// reaches the evaluator as the path its browser opens first, only on a
// round whose tool pod has a browser.
func TestEvaluateHandoffCarriesTheBrowserEntry(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4, func(pr *v1alpha1.Project) {
		pr.Spec.Surfaces = &v1alpha1.ProjectSurfaces{Browser: &v1alpha1.BrowserSurface{Entry: "/battle?x=1"}}
	})
	e.review(name, 3)
	pv := e.onlyVerification(name)
	p := e.prVerifyPass(name)

	pv.Spec.Tools = &v1alpha1.ToolsSpec{Browser: true, MaxToolCalls: 100}
	if got := decodeEvaluateHandoff(t, mustHandoff(t, p, &pv)).Tools.BrowserEntry; got != "/battle?x=1" {
		t.Errorf("browser entry = %q, want /battle?x=1", got)
	}
	pv.Spec.Tools.Browser = false
	if got := decodeEvaluateHandoff(t, mustHandoff(t, p, &pv)).Tools.BrowserEntry; got != "" {
		t.Errorf("browser entry without a browser = %q, want none", got)
	}
}

// TestEvaluateHandoffCarriesRefusedChecks (coverage design C3): the next
// round's evaluator is handed each check the reviewer refused for an
// approved outcome's current text, by outcome, check id and the reviewer's
// reason made visible; a proven check, a refusal for text the outcome no
// longer has, and a refusal recorded with no id are left out or named by
// their digest.
func TestEvaluateHandoffCarriesRefusedChecks(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	text := map[string]string{}
	for _, o := range e.get(name).Status.Acceptance.Outcomes {
		text[o.ID] = outcomeDigest(o)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Reviews = []v1alpha1.CheckReviewRecord{
			{CheckDigest: version.Digest, OutcomeDigest: text[oVersion], Answer: v1alpha1.ReviewDoesNotProve,
				Run: "r1", Check: version.ID, Reason: "it never reads the commit\u202e"},
			{CheckDigest: health.Digest, OutcomeDigest: text[oHealthy], Answer: v1alpha1.ReviewProves, Run: "r1",
				Check: health.ID, Reason: "fine"},
			{CheckDigest: health.Digest, OutcomeDigest: digest([]byte("an older text")),
				Answer: v1alpha1.ReviewDoesNotProve, Run: "r0", Check: health.ID, Reason: "old"},
			{CheckDigest: health.Digest, OutcomeDigest: text[oHealthy], Answer: v1alpha1.ReviewUnclear, Run: "r0"},
			// A red review's refusal is about the failure, not the check:
			// it is not shown as a refused check.
			{CheckDigest: version.Digest, OutcomeDigest: text[oVersion], Answer: v1alpha1.ReviewDoesNotProve,
				Run: "r2", Check: version.ID, Reason: "not a defect", Expect: v1alpha1.CheckExpectFail,
				FailedStep: 2, FailedAssertion: "json /sha"},
		}
	})
	h := decodeEvaluateHandoff(t, mustHandoff(t, e.prVerifyPass(name), &pv))
	if len(h.PriorRefused) != 2 {
		t.Fatalf("refused = %+v, want the version check's refusal and the unclear one", h.PriorRefused)
	}
	got := h.PriorRefused[0]
	if got.Outcome != oVersion || got.Check != version.ID || !strings.Contains(got.Reason, "it never reads the commit") ||
		strings.ContainsRune(got.Reason, '\u202e') {
		t.Errorf("refused[0] = %+v, want the version check by id with its reason made visible", got)
	}
	if got := h.PriorRefused[1]; got.Outcome != oHealthy || got.Check != health.Digest || got.Reason != "" {
		t.Errorf("refused[1] = %+v, want the unclear review named by its digest", got)
	}
	// More refusals than the prompt shows: the newest are handed over, in
	// the order they were made.
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Reviews = nil
		for i := range templates.PriorRefusedMax + 10 {
			in.Status.Acceptance.Reviews = append(in.Status.Acceptance.Reviews, v1alpha1.CheckReviewRecord{
				CheckDigest: digest([]byte(fmt.Sprint(i))), OutcomeDigest: text[oVersion],
				Answer: v1alpha1.ReviewDoesNotProve, Run: "r1", Check: fmt.Sprintf("c-%d", i), Reason: "no"})
		}
	})
	h = decodeEvaluateHandoff(t, mustHandoff(t, e.prVerifyPass(name), &pv))
	if n := len(h.PriorRefused); n != templates.PriorRefusedMax {
		t.Fatalf("%d refusals handed over, want %d", n, templates.PriorRefusedMax)
	}
	if first, last := h.PriorRefused[0].Check, h.PriorRefused[len(h.PriorRefused)-1].Check; first != "c-10" ||
		last != fmt.Sprintf("c-%d", templates.PriorRefusedMax+9) {
		t.Errorf("refusals run %s..%s, want the newest in order", first, last)
	}
}

// TestEvaluateHandoffCarriesRefusedCheckBodies (G5R-B-CLI-1): beside each
// refusal the next round's evaluator is handed the refused check itself,
// read from maul's own record of it (an earlier round's
// status.confirmed, which tools read from the verified ledger) by the
// reviewed digest, so it can amend that check rather than write an
// unrelated one. A body that is not its digest, a check this round
// replays (a stored check refused on the green question), and a check no
// round still records are named without one.
func TestEvaluateHandoffCarriesRefusedCheckBodies(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv1 := e.onlyVerification(name)
	version, health, again := savedCheck(t, versionCheck), savedCheck(t, healthCheck), savedCheck(t, healthCheck2)
	tampered := again
	tampered.Body = strings.Replace(again.Body, "again=1", "again=2", 1)
	pv1.Status.Phase = v1alpha1.VerificationObserved
	pv1.Status.Confirmed = []v1alpha1.SavedCheck{version, health, tampered}
	if err := e.c.Update(context.Background(), &pv1); err != nil {
		t.Fatal(err)
	}
	text := map[string]string{}
	for _, o := range e.get(name).Status.Acceptance.Outcomes {
		text[o.ID] = outcomeDigest(o)
	}
	gone := digest([]byte("a check no round records"))
	refuse := func(c v1alpha1.SavedCheck, d string) v1alpha1.CheckReviewRecord {
		return v1alpha1.CheckReviewRecord{CheckDigest: d, OutcomeDigest: text[c.Outcome],
			Answer: v1alpha1.ReviewDoesNotProve, Run: "r1", Check: c.ID, Reason: "too weak"}
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Reviews = []v1alpha1.CheckReviewRecord{refuse(version, version.Digest),
			refuse(health, health.Digest), refuse(again, again.Digest), refuse(again, gone)}
	})
	pv2 := pv1.DeepCopy()
	pv2.Name, pv2.UID = pv1.Name+"-next", types.UID("pv2-uid")
	pv2.Spec.Round, pv2.Status = 2, v1alpha1.PreviewVerificationStatus{}
	pv2.Spec.Checks = []v1alpha1.SavedCheck{health}
	h := decodeEvaluateHandoff(t, mustHandoff(t, e.prVerifyPass(name), pv2))
	if len(h.PriorRefused) != 4 {
		t.Fatalf("refused = %+v, want all four refusals", h.PriorRefused)
	}
	for i, want := range []string{version.Body, "", "", ""} {
		if got := h.PriorRefused[i].Body; got != want {
			t.Errorf("refused[%d] (%s) body = %q, want %q", i, h.PriorRefused[i].Check, got, want)
		}
	}
}

// TestEvaluateHandoffDropsRefusedBodiesFirst (G5R-B-CLI-1): over
// agent-runner's bound the refused checks' bodies go before any refusal
// does, the oldest first, so every refusal keeps its reason; the caller's
// handoff is left as it was.
func TestEvaluateHandoffDropsRefusedBodiesFirst(t *testing.T) {
	h := agentrun.EvaluateHandoff{Round: 2, RunToken: "ev2a1-abcdef",
		Request: strings.Repeat("r", evaluateHandoffMaxBytes-30<<10)}
	for i := range 4 {
		h.PriorRefused = append(h.PriorRefused, agentrun.PriorRefusal{Outcome: "o", Check: fmt.Sprintf("c-%d", i),
			Reason: "weak", Body: strings.Repeat("b", templates.RefusedBodiesMaxBytes/4)})
	}
	raw, err := encodeEvaluateHandoff(h)
	if err != nil || len(raw) > evaluateHandoffMaxBytes {
		t.Fatalf("encode with refused bodies = %d bytes, %v", len(raw), err)
	}
	got := decodeEvaluateHandoff(t, raw)
	if len(got.PriorRefused) != 4 || got.PriorRefused[0].Body != "" || got.PriorRefused[3].Body == "" {
		t.Errorf("refused = %d, want all four, the oldest body dropped and the newest kept", len(got.PriorRefused))
	}
	if h.PriorRefused[0].Body == "" {
		t.Error("encoding dropped a body from the caller's handoff")
	}
}

// TestEvaluateHandoffBoundsRefusedCheckBodies (G5R-B-CLI-1): the refused
// checks' bodies are handed over within templates.RefusedBodiesMaxBytes in
// all, the newest refusals' first; an older one past the bound is named
// with its reason and without its body.
func TestEvaluateHandoffBoundsRefusedCheckBodies(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv1 := e.onlyVerification(name)
	text := map[string]string{}
	for _, o := range e.get(name).Status.Acceptance.Outcomes {
		text[o.ID] = outcomeDigest(o)
	}
	reviews := make([]v1alpha1.CheckReviewRecord, 0, templates.PriorRefusedMax)
	pv1.Status.Phase = v1alpha1.VerificationObserved
	for i := range templates.PriorRefusedMax {
		c := savedCheck(t, strings.Replace(versionCheck, `"/version?q={{run}}"`,
			fmt.Sprintf(`"/version?q={{run}}&n=%d&pad=%s"`, i, strings.Repeat("x", 3000)), 1))
		pv1.Status.Confirmed = append(pv1.Status.Confirmed, c)
		reviews = append(reviews, v1alpha1.CheckReviewRecord{CheckDigest: c.Digest, OutcomeDigest: text[c.Outcome],
			Answer: v1alpha1.ReviewDoesNotProve, Run: "r1", Check: c.ID, Reason: "too weak"})
	}
	if err := e.c.Update(context.Background(), &pv1); err != nil {
		t.Fatal(err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Acceptance.Reviews = reviews })
	pv2 := pv1.DeepCopy()
	pv2.Name, pv2.UID = pv1.Name+"-next", types.UID("pv2-uid")
	pv2.Spec.Round, pv2.Status = 2, v1alpha1.PreviewVerificationStatus{}
	h := decodeEvaluateHandoff(t, mustHandoff(t, e.prVerifyPass(name), pv2))
	if len(h.PriorRefused) != templates.PriorRefusedMax {
		t.Fatalf("%d refusals, want every one named", len(h.PriorRefused))
	}
	total, shown := 0, 0
	for i, r := range h.PriorRefused {
		if r.Reason == "" {
			t.Errorf("refused[%d] lost its reason", i)
		}
		if r.Body == "" {
			if shown > 0 {
				t.Errorf("refused[%d] has no body after a shown one: an older body kept over a newer", i)
			}
			continue
		}
		shown++
		total += len(r.Body)
	}
	if shown == 0 || h.PriorRefused[len(h.PriorRefused)-1].Body == "" || h.PriorRefused[0].Body != "" {
		t.Errorf("%d bodies shown, want the newest only", shown)
	}
	if total > templates.RefusedBodiesMaxBytes {
		t.Errorf("bodies total %d bytes, over %d", total, templates.RefusedBodiesMaxBytes)
	}
}

// TestEvaluateHandoffRefusedBodyIsOfItsOutcome (G5R-B-CLI-1 review): a
// review is cached by its outcome's text, so after a replan that renames
// outcomes a refusal can be named under another outcome id than the one
// its check is bound to. Its body is then left out: the evaluator is never
// shown a check of one outcome as the refused check of another.
func TestEvaluateHandoffRefusedBodyIsOfItsOutcome(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv1 := e.onlyVerification(name)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	pv1.Status.Phase = v1alpha1.VerificationObserved
	pv1.Status.Confirmed = []v1alpha1.SavedCheck{version, health}
	if err := e.c.Update(context.Background(), &pv1); err != nil {
		t.Fatal(err)
	}
	text := map[string]string{}
	for _, o := range e.get(name).Status.Acceptance.Outcomes {
		text[o.ID] = outcomeDigest(o)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Reviews = []v1alpha1.CheckReviewRecord{
			{CheckDigest: health.Digest, OutcomeDigest: text[version.Outcome], Answer: v1alpha1.ReviewDoesNotProve,
				Run: "r1", Check: health.ID, Reason: "too weak"},
			{CheckDigest: version.Digest, OutcomeDigest: text[version.Outcome], Answer: v1alpha1.ReviewDoesNotProve,
				Run: "r1", Check: version.ID, Reason: "too weak"}}
	})
	pv2 := pv1.DeepCopy()
	pv2.Name, pv2.UID = pv1.Name+"-next", types.UID("pv2-uid")
	pv2.Spec.Round, pv2.Status = 2, v1alpha1.PreviewVerificationStatus{}
	h := decodeEvaluateHandoff(t, mustHandoff(t, e.prVerifyPass(name), pv2))
	if len(h.PriorRefused) != 2 {
		t.Fatalf("refused = %+v, want both refusals", h.PriorRefused)
	}
	if h.PriorRefused[0].Body != "" {
		t.Errorf("a check of %s shown as the refused check of %s", health.Outcome, h.PriorRefused[0].Outcome)
	}
	if h.PriorRefused[1].Body != version.Body {
		t.Errorf("the refused check of its own outcome lost its body: %q", h.PriorRefused[1].Body)
	}
}

// TestEvaluateHandoffDropsRefusedBodiesBeforeChangedPaths (G5R-B-CLI-1
// review): the refused checks' bodies are only a hint to the evaluator, so
// over the bound they go before the changed paths a handoff carried
// before they were added.
func TestEvaluateHandoffDropsRefusedBodiesBeforeChangedPaths(t *testing.T) {
	h := agentrun.EvaluateHandoff{Round: 2, RunToken: "ev2a1-abcdef",
		Request:      strings.Repeat("r", evaluateHandoffMaxBytes-24<<10),
		ChangedPaths: []agentrun.ChangedPath{{Path: "a.go"}, {Path: "b.go"}}}
	for i := range 4 {
		h.PriorRefused = append(h.PriorRefused, agentrun.PriorRefusal{Outcome: "o", Check: fmt.Sprintf("c-%d", i),
			Reason: "weak", Body: strings.Repeat("b", templates.RefusedBodiesMaxBytes/4)})
	}
	raw, err := encodeEvaluateHandoff(h)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeEvaluateHandoff(t, raw)
	if len(got.ChangedPaths) != 2 {
		t.Errorf("changed paths = %d, want both kept while a refused body could go", len(got.ChangedPaths))
	}
	if len(got.PriorRefused) != 4 {
		t.Errorf("refused = %d, want all four", len(got.PriorRefused))
	}
}
