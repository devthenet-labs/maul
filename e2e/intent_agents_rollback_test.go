// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// restartAgentsIntents stops intent-controller and starts it again as an
// operator's values change rolls it: the evaluator on, and the agents host
// on or off (intentController.verification.agents.enabled).
func (e *evalEnv) restartAgentsIntents(t *testing.T, agents bool) {
	t.Helper()
	e.stopIntents()
	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver 10.96.0.10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{"--namespace", namespace}, intentArgs...), evaluatorArgs...)
	if agents {
		args = append(args, agentsArgs...)
	}
	_, e.stopIntents = e.cl.serveBinaryEnv(t, "intent-controller", buildTagged(t, "intent-controller", "e2edial"),
		[]string{"MAUL_E2E_RESOLV_CONF=" + resolv}, args...)
}

// TestIntentAgentsRollback (S2-B4, SPEC-S2 S2-26 step (i)): with the agents
// host switched off while a round is open, its evaluator launched, the round
// settles Unavailable, never a pass or a fail, the hand-off released as
// couldn't confirm and the issue told; the unsettled verification is
// deleted, the evaluator cancelled and the tool Job swept. Back on, an
// approver's /maul verify starts the next round, which passes.
func TestIntentAgentsRollback(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBase},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
	})
	release := e.kubelet.hold(t, "evaluate")
	number, name, pr := e.agentsToEvaluating(t)
	within(t, roundTimeout, "the evaluator launched", func() bool {
		runs := e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)
		return len(runs) == 1 && runs[0].Status.JobRef != nil
	})
	pv := e.agentsVerification(t, name)

	e.restartAgentsIntents(t, false)
	in := e.waitEval(t, name, "the open round to settle unavailable", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationUnavailable
	})
	v := in.Status.Verification
	if h := v.Handoff; h == nil || !h.Released || h.Reason != v1alpha1.HandoffReasonUnconfirmed {
		t.Errorf("hand-off %+v, want released as couldn't confirm", h)
	}
	if v.Passed != 0 || v.Failed != 0 {
		t.Errorf("verification %+v, want no judgement", v)
	}
	within(t, roundTimeout, "the stranded verification deleted", func() bool {
		var cur v1alpha1.PreviewVerification
		return apierrors.IsNotFound(e.cl.client.Get(context.Background(), client.ObjectKeyFromObject(&pv), &cur))
	})
	within(t, roundTimeout, "the evaluator cancelled", func() bool {
		runs := e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)
		return len(runs) == 1 && runs[0].Status.Phase == v1alpha1.RunFailed &&
			strings.Contains(runs[0].Status.Detail, "evaluation round ended")
	})
	within(t, roundTimeout, "the tool Job swept", func() bool {
		var list batchv1.JobList
		err := e.cl.client.List(context.Background(), &list, client.InNamespace(agentsNS),
			client.MatchingLabels{v1alpha1.LabelRole: v1alpha1.RoleTools})
		return err == nil && len(list.Items) == 0
	})
	eventually(t, "the issue told the round is unavailable", func() bool {
		got := e.own(number, notice(name, templates.VerifyKey(1)))
		return len(got) == 1 && strings.Contains(got[0].Body, "unavailable")
	})
	if len(e.verifyRuns(t, name)) != 0 {
		t.Errorf("a fix round started: %s", e.describeRuns(t, name))
	}
	if st, ok := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus); ok && st.State == "success" {
		t.Errorf("maul/acceptance %+v: a stranded round is no pass", st)
	}
	release()

	e.restartAgentsIntents(t, true)
	e.gh.CommentAs(number, "/maul verify", approver)
	e.waitEval(t, name, "the request's round to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.Round == 2 && v.State == v1alpha1.VerificationPassed
	})
}
