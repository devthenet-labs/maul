// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestIntentEvaluateCodex (case 11): a Project that runs on codex. Under
// --intent-evaluate-codex its outcomes are evaluated on the codex harness:
// the REAL agent-runner drives hack/fake-agent/codex, whose one model
// request goes through the shipped egress broker to a fake OpenAI upstream
// (TokenReview of a caller token bound to the Job's pod), and which drives
// the real maul-tool against the tool pod as the claude script does; the
// round passes. maul has verified no codex evaluator live (SPEC Q16), so
// without the flag the Project says so (EvaluatorUnverified) and its
// intents are not evaluated: their hand-off is early, at once, and no tool
// pod or evaluator runs.
func TestIntentEvaluateCodex(t *testing.T) {
	codex := func(spec *v1alpha1.ProjectSpec) { spec.Harness = v1alpha1.ProjectHarnessCodex }

	t.Run("scripted pass with --intent-evaluate-codex", func(t *testing.T) {
		up := newOpenAIUpstream(t)
		e := startCodexEvaluate(t, up.URL, codex, "--intent-evaluate-codex")
		_, name, pr := e.toReview(t)
		e.serve(t, name, pr.HeadSHA)
		in := e.waitEval(t, name, "the codex evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
			return v.State == v1alpha1.VerificationPassed
		})
		if v := in.Status.Verification; v.Passed != 2 || v.Handoff == nil || v.Handoff.Reason != v1alpha1.HandoffReasonReady {
			t.Errorf("verification %+v, want both outcomes confirmed and handed over ready", v)
		}
		for _, stage := range []v1alpha1.IntentStage{v1alpha1.IntentStageEvaluate, v1alpha1.IntentStageCheckReview} {
			runs := e.stageRuns(t, name, stage)
			if len(runs) != 1 || runs[0].Spec.Harness != v1alpha1.ProjectHarnessCodex {
				t.Errorf("%s runs %d, harness %v; want one, on codex", stage, len(runs), runs)
			}
		}
		job := e.kubelet.waitRun(t, "the evaluator's Job", phaseIs("evaluate"))
		if job.Job.Spec.Template.Labels[v1alpha1.LabelHarness] != "codex" || job.ExitCode != 0 {
			t.Errorf("evaluator Job harness %q exit %d, want codex and 0",
				job.Job.Spec.Template.Labels[v1alpha1.LabelHarness], job.ExitCode)
		}
		reqs := up.requests()
		if len(reqs) == 0 {
			t.Error("no codex model request reached the upstream through the broker")
		}
		for _, r := range reqs {
			if r.CallerToken != "" {
				t.Errorf("the upstream saw a caller token: %+v", r)
			}
		}
		e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes confirmed")
	})

	t.Run("unverified without it", func(t *testing.T) {
		up := newOpenAIUpstream(t)
		e := startCodexEvaluate(t, up.URL, codex)
		eventually(t, "the project to say its evaluator is unverified", func() bool {
			return meta.IsStatusConditionTrue(e.project(t).Status.Conditions, v1alpha1.ConditionEvaluatorUnverified)
		})
		_, name, pr := e.toReview(t)
		e.serve(t, name, pr.HeadSHA)
		in := e.waitEval(t, name, "the hand-off released early, unverified", func(v *v1alpha1.IntentVerification) bool {
			return v.Handoff != nil && v.Handoff.Released
		})
		v := in.Status.Verification
		if v.State != v1alpha1.VerificationOff || v.Handoff.Mode != v1alpha1.HandoffEarly ||
			v.Handoff.Reason != v1alpha1.HandoffReasonEvaluatorUnverified {
			t.Errorf("verification %+v handoff %+v; want Off, released early as evaluator-unverified", v, v.Handoff)
		}
		eventually(t, "the preview linked at once", func() bool {
			return strings.Contains(e.previewComment(name, pr.Number), name+previewHostSuffix)
		})
		consistently(t, "no evaluation at all", func() bool {
			return len(e.verifications(t, name)) == 0 && len(e.kubelet.ToolRuns()) == 0 &&
				len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)) == 0
		})
	})
}

// startCodexEvaluate is startEvaluate for a codex Project: intent-controller
// with --intent-codex, the codex runner and the shipped egress broker's
// openai route to upstream, the kubelet minting each Job's caller token.
func startCodexEvaluate(t *testing.T, upstream string, mutate func(*v1alpha1.ProjectSpec),
	extra ...string) *evalEnv {
	t.Helper()
	var brokerURL string
	e := startEvaluate(t, evalOptions{mutate: mutate, intentArgs: append([]string{"--intent-codex",
		"--codex-agent-image", codexRunnerImage, "--broker-token-audience", brokerAudience}, extra...),
		beforeIntents: func(cl *cluster) []string {
			ensureAgentSA(t, cl.clientset(t))
			brokerURL = startOpenAIBroker(t, cl, upstream)
			return []string{"--broker-url", brokerURL}
		}})
	e.kubelet.useBrokerTokens(e.cl.clientset(t))
	return e
}
