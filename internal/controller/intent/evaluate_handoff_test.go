// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"bytes"
	"context"
	"encoding/json"
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
