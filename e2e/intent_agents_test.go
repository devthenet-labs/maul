// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// The no-Preview evaluator e2e (SPEC-S2 S2-22): a Project whose one
// repository is a command-line program or a library, with no preview, on
// the shipped intent-controller with --intent-evaluator-agents and
// source-controller, against the fake GitHub serving each pushed commit's
// own tree. intent-controller's agents host renders the agents-namespace
// tool pod; the fake kubelet runs its prepare and seeds on the real engine,
// both sandboxes on the real workspace shim and its tools container as the
// real preview-tools (kubelet_workspace_test.go); the evaluator and check
// reviewer run on the real agent-runner driving hack/fake-agent/claude in
// its workspace modes, which drives the real maul-tool against the tool
// pod. The build is hack/fake-agent's, writing the program's main.go.

// The fixture: a Go program, tally, which echoes its argument and whose head
// gains --greet.
const (
	tallyGoMod = "module example.com/tally\n\ngo 1.22\n"
	// tallyBase echoes the argument of --echo, and prints its usage for
	// anything else.
	tallyBase = `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 2 && os.Args[1] == "--echo" {
		fmt.Println(os.Args[2])
		return
	}
	fmt.Println("usage: tally [--echo text]")
}
`
	// tallyGreets also answers --greet, as the outcome states.
	tallyGreets = `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--greet" {
		fmt.Println("hello from tally")
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "--echo" {
		fmt.Println(os.Args[2])
		return
	}
	fmt.Println("usage: tally [--greet] [--echo text]")
}
`
	// tallyGreetOutcome is the plan's one approved outcome.
	tallyGreetOutcome = `acceptance:
  - id: greets
    outcome: tally --greet prints hello from tally`
)

// agentsArgs turn the agents tool pod on in intent-controller, beside the
// evaluator.
var agentsArgs = []string{"--intent-evaluator-agents", "--intent-tools-image", toolsImage}

// agentsOptions shape one agents evaluator stack.
type agentsOptions struct {
	// files are the repository's tree at the base, beside the fake's own.
	files map[string]string
	// surfaces are the Project's.
	surfaces *v1alpha1.ProjectSurfaces
	// build is the main.go the build writes; outcomes the plan's.
	build, outcomes string
	// env is the scripted evaluator's (MAUL_FAKE_CLAUDE_*).
	env map[string]string
	// intentArgs are more flags for intent-controller.
	intentArgs []string
}

// tallyCLI is the cli surface of the tally fixture.
func tallyCLI() *v1alpha1.ProjectSurfaces {
	return &v1alpha1.ProjectSurfaces{CLI: &v1alpha1.CLISurface{
		Build: []string{"go", "build", "-o", "/work/bin/tally", "."}, Binary: "/work/bin/tally"}}
}

// startAgents runs the agents evaluator stack: the Project verifying the
// one repository on its surface, no preview.
func startAgents(t *testing.T, o agentsOptions) *evalEnv {
	t.Helper()
	cl, gh, registry := startSourceStack(t, appRepo)
	gh.ServeCommitTrees(true)
	for path, content := range o.files {
		gh.SetRepoFile(intentOwner, appRepo, path, content)
	}
	spec := v1alpha1.ProjectSpec{
		IntentRepository: intentRepoURL,
		Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login, reader.Login}},
		Repositories:     []v1alpha1.ProjectRepository{{Name: appRepo, URL: appRepoURL}},
		Verification:     &v1alpha1.ProjectVerification{Enabled: true},
		Surfaces:         o.surfaces,
	}
	e := withEvaluatingIntents(t, cl, gh, spec, append(append([]string(nil), agentsArgs...), o.intentArgs...)...)
	e.registry = registry
	gh.SetDefaultComparePatch("diff --git a/main.go b/main.go\n+hello from tally\n")
	e.kubelet.runWorkspaceTools(t)
	e.kubelet.useAgentRunner(t, "evaluate", "checkreview")
	e.kubelet.setAgentEnv("MAUL_FAKE_PLAN_ACCEPTANCE", o.outcomes)
	e.kubelet.setAgentEnv("MAUL_FAKE_BUILD_EXTRA_PATH", "main.go")
	e.kubelet.setAgentEnv("MAUL_FAKE_BUILD_EXTRA_CONTENT", o.build)
	for name, value := range o.env {
		e.kubelet.setAgentEnv(name, value)
	}
	t.Cleanup(func() {
		if t.Failed() {
			e.kubelet.logWorkspaceRuns(t)
		}
	})
	return &evalEnv{verifyEnv: &verifyEnv{intentEnv: e}}
}

// startTally is startAgents for the tally cli, its build greeting.
func startTally(t *testing.T, mode string, extra ...string) *evalEnv {
	t.Helper()
	return startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBase},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": mode, "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
		intentArgs: extra,
	})
}

// agentsToEvaluating files an intent, approves it and waits for its first
// agents round to start: no preview to serve.
func (e *evalEnv) agentsToEvaluating(t *testing.T) (int, string, v1alpha1.IntentPullRequest) {
	t.Helper()
	number, name, pr := e.toReview(t)
	e.waitEval(t, name, "the first round to start", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationEvaluating
	})
	return number, name, pr
}

// agentsVerification is the Intent's one agents PreviewVerification.
func (e *evalEnv) agentsVerification(t *testing.T, name string) v1alpha1.PreviewVerification {
	t.Helper()
	var out v1alpha1.PreviewVerification
	n := 0
	for _, pv := range e.verifications(t, name) {
		if pv.Spec.IsAgents() {
			out, n = pv, n+1
		}
	}
	if n != 1 {
		t.Fatalf("%d agents verifications, want one: %s", n, e.describeEval(t, name))
	}
	return out
}

// baseRedByAssertion reports a base run of a check failing at its
// assertion, every base run naming the base tree.
func baseRedByAssertion(runs []v1alpha1.CheckRun) bool {
	red := false
	for _, r := range runs {
		if r.Tree != v1alpha1.TreeBase {
			return false
		}
		red = red || (r.Result == v1alpha1.CheckRunFail && r.AssertionID != "")
	}
	return red
}

// checkNoPreview asserts the intent never had a Preview: no slot was ever
// taken for it.
func (e *evalEnv) checkNoPreview(t *testing.T) {
	t.Helper()
	var list v1alpha1.PreviewList
	if err := e.cl.client.List(context.Background(), &list, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d Previews, want none for a Project with no preview", len(list.Items))
	}
}

// TestIntentAgentsCLIPasses (case 1): a correct cli build. The plan states
// one outcome; the agents host builds the head and the merge base in their
// own sandboxes; the scripted evaluator runs the built program, writes an
// exec check, dry-runs it at the head (green) and the base (red: the base
// does not greet), and proposes it; the tools container confirms it at both
// sides, the reviewer answers proves, and maul judges the outcome
// confirmed. The hand-off is gated while maul checks and then ready, with
// no link and no slot: the Project has no preview.
func TestIntentAgentsCLIPasses(t *testing.T) {
	e := startTally(t, "cli-pass")
	number, name, pr := e.agentsToEvaluating(t)
	e.waitStatus(t, pr.HeadSHA, "pending", "maul is checking this")

	in := e.waitEval(t, name, "the evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	if acc := in.Status.Acceptance; acc.Checks == nil || acc.Checks.Count != 1 || len(acc.Reviews) != 1 {
		t.Errorf("acceptance checks %+v reviews %+v; want the check stored and reviewed", acc.Checks, acc.Reviews)
	}
	pv := e.agentsVerification(t, name)
	if ws := pv.Spec.Workspace; ws == nil || ws.HeadSHA != pr.HeadSHA || ws.BaseSHA == pr.HeadSHA {
		t.Errorf("workspace %+v, want the head %s and its merge base", ws, pr.HeadSHA)
	}
	if len(pv.Status.Confirmed) != 1 {
		t.Errorf("confirmed %d checks, want one", len(pv.Status.Confirmed))
	}
	// The base was built and the check ran there, red by its assertion:
	// the base does not greet, so the check tells the change apart.
	if b := pv.Status.Base; b == nil || b.Unbuildable || !baseRedByAssertion(b.Runs) {
		t.Errorf("base %+v, want it built and the check red by assertion there", b)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes confirmed")
	e.waitStatusComment(t, name, number, "the status comment to say ready with no link", func(body string) bool {
		return strings.Contains(body, "Ready: maul checked the approved outcomes.") &&
			!strings.Contains(body, previewHostSuffix)
	})
	e.checkNoPreview(t)

	runs := e.kubelet.WorkspaceRuns()
	if len(runs) != 1 {
		t.Fatalf("%d agents tool pods ran, want one", len(runs))
	}
	if got := runs[0].Requests["head"]; len(got) == 0 || got[0].Kind != "build" {
		t.Errorf("head sandbox requests %+v, want its build first", got)
	}
}

// tallyKeepOutcome is a plan stating that echoing stays: true at the base,
// so a defect there is a regression maul can show.
const tallyKeepOutcome = `acceptance:
  - id: echo-kept
    outcome: tally --echo still prints the text it is given
    kind: keep`

// tallyBreaksEcho is the planted defect: --greet answers, but --echo no
// longer prints its text.
const tallyBreaksEcho = `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--greet" {
		fmt.Println("hello from tally")
		return
	}
	fmt.Println("usage: tally [--greet]")
}
`

// TestIntentAgentsFixRound (case 2, wave 4): an agents fix round end to
// end. The build plants a defect: --echo no longer prints its text, as the
// base does. The scripted evaluator's exec check, run with its run's own
// token, is red by its assertion at the head; it fails the outcome on that
// run and proposes the check expecting a fail; maul confirms it red at the
// head and green at the base and, a defect present from the first round,
// holds it for a person (suspected defect, never an automatic fix). An
// approver's /maul verify fix starts one verify-fix round, whose build
// writes the program right. At the fix's head the next round's replay of
// the saved check passes: the outcome is confirmed and the hand-off ready.
func TestIntentAgentsFixRound(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBase},
		surfaces: tallyCLI(), build: tallyBreaksEcho, outcomes: tallyKeepOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-fail",
			"MAUL_FAKE_CLAUDE_CLI_ARGS": "--echo {{run}}", "MAUL_FAKE_CLAUDE_CHECK_TEXT": "{{run}}"},
	})
	number, name, pr := e.toReview(t)
	in := e.waitEval(t, name, "round 1 to hold a suspected defect", func(v *v1alpha1.IntentVerification) bool {
		return len(v.Outcomes) == 1 && v.Outcomes[0].Reason == acceptance.ReasonSuspectedDefect &&
			v.Suspected != nil
	})
	if len(e.verifyRuns(t, name)) != 0 {
		t.Errorf("a fix round started on a defect present from round 1: %s", e.describeRuns(t, name))
	}
	pv := e.agentsVerification(t, name)
	if b := pv.Status.Base; b == nil || len(b.Runs) == 0 || b.Runs[0].Result != v1alpha1.CheckRunPass {
		t.Errorf("base %+v, want the check green at the base", b)
	}

	// The fix's build waits until it is handed the right program.
	release := e.kubelet.hold(t, "build")
	// A person confirms the defect once maul has recorded it.
	afterSecond(in.Status.Verification.Suspected.RecordedAt.Time)
	e.requestRound(t, number)
	fix := e.waitFixRun(t, name)
	e.kubelet.setAgentEnv("MAUL_FAKE_BUILD_EXTRA_CONTENT", tallyGreets)
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_EVALUATE", "cli-pass")
	release()

	in = e.waitFor(t, name, "the fix pushed and back in review", func(in *v1alpha1.Intent) bool {
		run := e.run(t, fix.Name)
		return in.Status.Phase == v1alpha1.IntentInReview && run.Status.PushedCommit != "" &&
			in.Status.PullRequests[0].HeadSHA == run.Status.PushedCommit
	})
	head := in.Status.PullRequests[0].HeadSHA
	if head == pr.HeadSHA {
		t.Fatalf("the fix did not move the head from %s", head)
	}
	in = e.waitEval(t, name, "the round at the fix's head to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Heads[0].Revision == head
	})
	if in.Status.VerifyFixes != 1 || len(e.verifyRuns(t, name)) != 1 {
		t.Errorf("verify fixes %d, fix rounds %d; want one fix", in.Status.VerifyFixes, len(e.verifyRuns(t, name)))
	}
	e.waitStatus(t, head, "success", "1/1 outcomes confirmed")
	runs := e.kubelet.WorkspaceRuns()
	if len(runs) < 2 {
		t.Fatalf("%d agents tool pods ran, want one per round", len(runs))
	}
	if last := runs[len(runs)-1]; !strings.Contains(string(last.Stdout), `"mode":"replay"`) {
		t.Errorf("the last round's ledger replays no saved check:\n%s", last.Stdout)
	}
}
