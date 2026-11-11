// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/toolwire"
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
	// build is what the build writes to buildPath (main.go when empty);
	// outcomes the plan's.
	build, buildPath, outcomes string
	// env is the scripted evaluator's (MAUL_FAKE_CLAUDE_*).
	env map[string]string
	// intentArgs are more flags for intent-controller.
	intentArgs []string
	// previewController runs preview-controller (with the slot evaluator)
	// beside it, which must never touch an agents verification.
	previewController bool
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
	if o.previewController {
		startEvaluatingPreviewController(t, cl)
	}
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
	path := o.buildPath
	if path == "" {
		path = "main.go"
	}
	e.kubelet.setAgentEnv("MAUL_FAKE_BUILD_EXTRA_PATH", path)
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

// checkForeignKeyRefused calls the tool pod's relay at addr as a caller
// holding key (none when empty), not this verification's: refused with
// key-refused, nothing run.
func checkForeignKeyRefused(t *testing.T, addr, key string) {
	t.Helper()
	body := `{"argv":["/work/bin/tally","--foreign"]}`
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+toolwire.Path(toolwire.VerbExec),
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(toolwire.KeyHeader, key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call the relay: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var resp toolwire.Response
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode the relay's answer: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized || resp.Refused != "key-refused" {
		t.Errorf("a caller with key %q: status %d, %+v; want 401 key-refused", key, res.StatusCode, resp)
	}
}

// checkNoForeignRun asserts no sandbox of any agents tool pod ran what
// checkForeignKeyRefused asked.
func (e *evalEnv) checkNoForeignRun(t *testing.T) {
	t.Helper()
	for _, r := range e.kubelet.WorkspaceRuns() {
		for side, reqs := range r.Requests {
			for _, req := range reqs {
				if slices.Contains(req.Argv, "--foreign") {
					t.Errorf("the %s sandbox ran a refused caller's program: %q", side, req.Argv)
				}
			}
		}
	}
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

// TestIntentAgentsCLIPasses (cases 1, 6, 7 and 9): a correct cli build.
// The plan states one outcome; the agents host builds the head and the
// merge base in their own sandboxes; the scripted evaluator runs the built
// program, writes an exec check, dry-runs it at the head (green) and the
// base (red: the base does not greet), and proposes it; the tools container
// confirms it at both sides, the reviewer answers proves, and maul judges
// the outcome confirmed. The hand-off is gated while maul checks and then
// ready, with no link and no slot: the Project has no preview.
// preview-controller runs beside it and never writes the agents
// verification.
func TestIntentAgentsCLIPasses(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBase},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
		previewController: true,
	})
	release := e.kubelet.holdToolsReady(t)
	number, name, pr := e.agentsToEvaluating(t)
	e.waitStatus(t, pr.HeadSHA, "pending", "maul is checking this")
	pv := e.checkHeldToolPod(t, name)
	release()

	in := e.waitEval(t, name, "the evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	if err := e.cl.client.Get(context.Background(), client.ObjectKeyFromObject(&pv), &pv); err != nil {
		t.Fatal(err)
	}
	tool := e.checkEvaluatorPinned(t, name, &pv)
	if got := strings.Count(string(tool.Stdout), `"refused":"key-refused"`); got != 2 {
		t.Errorf("the ledger records %d key refusals, want the two foreign calls:\n%s", got, tool.Stdout)
	}
	checkNotPreviewControllers(t, &pv)
	if acc := in.Status.Acceptance; acc.Checks == nil || acc.Checks.Count != 1 || len(acc.Reviews) != 1 {
		t.Errorf("acceptance checks %+v reviews %+v; want the check stored and reviewed", acc.Checks, acc.Reviews)
	}
	checkConfirmedAtBoth(t, e.agentsVerification(t, name), pr.HeadSHA)
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes confirmed")
	e.waitStatusComment(t, name, number, "the status comment to say ready with no link", func(body string) bool {
		return strings.Contains(body, "Ready: maul checked the approved outcomes.") &&
			!strings.Contains(body, previewHostSuffix)
	})
	e.checkNoPreview(t)
	e.checkNoForeignRun(t)

	runs := e.kubelet.WorkspaceRuns()
	if len(runs) != 1 {
		t.Fatalf("%d agents tool pods ran, want one", len(runs))
	}
	if got := runs[0].Requests["head"]; len(got) == 0 || got[0].Kind != "build" {
		t.Errorf("head sandbox requests %+v, want its build first", got)
	}
}

// checkHeldToolPod (cases 6 and 7) waits for the round's tool pod to run,
// held un-Ready: no evaluator exists while its address is not in status,
// and a caller without this verification's key, none or another's, is
// refused. It returns the round's verification.
func (e *evalEnv) checkHeldToolPod(t *testing.T, name string) v1alpha1.PreviewVerification {
	t.Helper()
	var pv v1alpha1.PreviewVerification
	within(t, roundTimeout, "the agents tool pod rendered and running", func() bool {
		for _, p := range e.verifications(t, name) {
			pv = p
		}
		return pv.Status.Phase == v1alpha1.VerificationRunning && e.kubelet.toolAddr(toolPodIP(e.kubelet)) != ""
	})
	consistently(t, "no evaluator while the tool pod's address is not in status", func() bool {
		var cur v1alpha1.PreviewVerification
		err := e.cl.client.Get(context.Background(), client.ObjectKeyFromObject(&pv), &cur)
		return err == nil && cur.Status.Tools == nil && len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)) == 0
	})
	addr := e.kubelet.toolAddr(toolPodIP(e.kubelet))
	for _, key := range []string{"", strings.Repeat("ab", 32)} {
		checkForeignKeyRefused(t, addr, key)
	}
	return pv
}

// checkNotPreviewControllers (case 9): preview-controller, running beside
// the agents host, never wrote the agents verification.
func checkNotPreviewControllers(t *testing.T, pv *v1alpha1.PreviewVerification) {
	t.Helper()
	for _, mf := range pv.ManagedFields {
		if strings.Contains(mf.Manager, "preview") {
			t.Errorf("preview-controller wrote the agents verification: %+v", mf)
		}
	}
}

// checkConfirmedAtBoth asserts the round's workspace at head and its merge
// base, one check confirmed, and the base built with the check red by its
// assertion there: the base does not greet, so the check tells the change
// apart.
func checkConfirmedAtBoth(t *testing.T, pv v1alpha1.PreviewVerification, head string) {
	t.Helper()
	if ws := pv.Spec.Workspace; ws == nil || ws.HeadSHA != head || ws.BaseSHA == head {
		t.Errorf("workspace %+v, want the head %s and its merge base", ws, head)
	}
	if len(pv.Status.Confirmed) != 1 {
		t.Errorf("confirmed %d checks, want one", len(pv.Status.Confirmed))
	}
	if b := pv.Status.Base; b == nil || b.Unbuildable || !baseRedByAssertion(b.Runs) {
		t.Errorf("base %+v, want it built and the check red by assertion there", b)
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
		// One tool pod at a time: a settled round's lingering Job must not
		// hold the slot the next round needs (U26).
		intentArgs: []string{"--intent-max-tool-pods", "1"},
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
	e.checkLingering(t, runs[0])
}

// checkLingering asserts a settled round's finished tool Job lingers (wave
// 3: ten minutes, for the isolation probe): the Job is still there, not
// deleting, and its tools container's log, the ledger, is still readable
// through pods/log.
func (e *evalEnv) checkLingering(t *testing.T, run workspaceRun) {
	t.Helper()
	ctx := context.Background()
	var job batchv1.Job
	if err := e.cl.client.Get(ctx, client.ObjectKeyFromObject(&run.Job), &job); err != nil ||
		!job.DeletionTimestamp.IsZero() {
		t.Fatalf("the settled round's tool Job %s: %v, deleting %v; want it lingering", run.Job.Name, err,
			job.DeletionTimestamp)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", e.cl.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log, err := cs.CoreV1().Pods(agentsNS).GetLogs(run.Job.Name+"-e2e",
		&corev1.PodLogOptions{Container: jobs.ToolsContainer}).DoRaw(ctx)
	if err != nil || !bytes.Equal(log, run.Stdout) {
		t.Errorf("the lingering tool pod's log: %v, %d bytes; want its ledger", err, len(log))
	}
}

// The library fixture: a Go package, strs, whose head gains ErrTooLong.
const (
	strsGoMod = "module example.com/strs\n\ngo 1.22\n"
	strsBase  = `// Package strs trims strings.
package strs

import "strings"

// Trim trims s.
func Trim(s string) string { return strings.TrimSpace(s) }
`
	strsHead = `// Package strs trims strings.
package strs

import (
	"errors"
	"strings"
)

// ErrTooLong is returned for input over Max bytes.
var ErrTooLong = errors.New("strs: too long")

// Max bounds Check's input.
const Max = 64

// Trim trims s.
func Trim(s string) string { return strings.TrimSpace(s) }

// Check refuses input over Max bytes.
func Check(s string) error {
	if len(s) > Max {
		return ErrTooLong
	}
	return nil
}
`
	strsOutcome = `acceptance:
  - id: too-long
    outcome: strs exports ErrTooLong for input over its limit`
)

// TestIntentAgentsLibraryNewSymbol (case 3): a library outcome resting on
// a symbol the change adds. The scripted evaluator reads the tree's go.mod,
// runs a consumer program referring to strs.ErrTooLong at the head and the
// base, and saves it as a run check printing its run's own token. At the
// base the consumer does not compile: a compiler diagnostic in the
// consumer's own main.go (undefined: lib.ErrTooLong), so the base run is an
// error with the compile cause, which maul reads as "no base comparison"
// (n/a). The outcome passes on the other rules, and the base still built.
func TestIntentAgentsLibraryNewSymbol(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files: map[string]string{"go.mod": strsGoMod, "strs.go": strsBase},
		surfaces: &v1alpha1.ProjectSurfaces{Library: &v1alpha1.LibrarySurface{
			Language: v1alpha1.LibraryLanguageGo}},
		build: strsHead, buildPath: "strs.go", outcomes: strsOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "lib-newsymbol", "MAUL_FAKE_CLAUDE_SYMBOL": "ErrTooLong"},
	})
	_, name, pr := e.toReview(t)
	in := e.waitEval(t, name, "the evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	if o := in.Status.Verification.Outcomes; len(o) != 1 || o[0].Result != v1alpha1.OutcomePassed {
		t.Errorf("outcomes %+v, want too-long passed", o)
	}
	pv := e.agentsVerification(t, name)
	b := pv.Status.Base
	if b == nil || b.Unbuildable || len(b.Runs) == 0 {
		t.Fatalf("base %+v, want it built and the check run there", b)
	}
	for _, r := range b.Runs {
		if r.Tree != v1alpha1.TreeBase || r.Result != v1alpha1.CheckRunError || r.Cause != v1alpha1.CheckRunCauseCompile {
			t.Errorf("base run %+v, want an error with the compile cause", r)
		}
	}
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes confirmed")
	runs := e.kubelet.WorkspaceRuns()
	if len(runs) != 1 {
		t.Fatalf("%d agents tool pods ran, want one", len(runs))
	}
	if !strings.Contains(string(runs[0].Stdout), `undefined: lib.ErrTooLong`) {
		t.Errorf("the ledger does not record the base compile's diagnostic:\n%s", runs[0].Stdout)
	}
}

// tallyDoesNotBuild is a head that does not compile.
const tallyDoesNotBuild = `package main

func main() { fmt.Println("hello from tally") }
`

// TestIntentAgentsHeadBuildFails (case 4): the head does not build in
// maul's sandbox, while its CI is green (the fake reports no checks). The
// relay finishes after its builds without ever becoming ready, so no
// evaluator is launched (S2-B5), the round can neither fail nor pass, and
// maul says it could not confirm the work because its own
// build did not finish: Unconfirmed, ToolsBuildFailed, the hand-off
// released, and no fix round, which only a failed outcome starts.
func TestIntentAgentsHeadBuildFails(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBase},
		surfaces: tallyCLI(), build: tallyDoesNotBuild, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
	})
	number, name, pr := e.toReview(t)
	in := e.waitEval(t, name, "the round to end unconfirmed", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationUnconfirmed &&
			v.Cause == v1alpha1.SettleToolsBuildFailed
	})
	if v := in.Status.Verification; v.Failed != 0 || v.Passed != 0 {
		t.Errorf("verification %+v, want nothing failed or passed", v)
	}
	e.waitStatusComment(t, name, number, "the status comment to say maul's build did not finish",
		func(body string) bool {
			return strings.Contains(body, "its own build of the pull request did not finish")
		})
	consistently(t, "no fix round on a build maul could not run", func() bool {
		return len(e.verifyRuns(t, name)) == 0
	})
	if evals := e.stageRuns(t, name, v1alpha1.IntentStageEvaluate); len(evals) != 0 {
		t.Errorf("%d evaluate runs launched on a head that did not build, want none", len(evals))
	}
	if st, ok := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus); ok && st.State == "failure" {
		t.Errorf("maul/acceptance %+v: a build maul could not run is no failure", st)
	}
	runs := e.kubelet.WorkspaceRuns()
	if len(runs) == 0 {
		t.Fatal("no agents tool pod ran")
	}
	for _, r := range runs {
		if !strings.Contains(string(r.Stdout), `"result":"failed"`) {
			t.Errorf("the ledger records no failed head build:\n%s", r.Stdout)
		}
	}
}

// tallyBaseNetError is a base whose --greet names a connection error:
// whatever the base prints when it cannot reach what it dials.
const tallyBaseNetError = `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--greet" {
		fmt.Println("dial tcp 127.0.0.1:7: connect: connection refused")
		os.Exit(1)
	}
	fmt.Println("usage: tally")
}
`

// TestIntentAgentsBaseNetError (case 5, wave 4): the check passes at the
// head, but at the base its program's output names a connection error. On a
// pod whose loopback both sides share, the other side's leftover can cause
// one, so the base run is an error with no cause: no base evidence. The
// check cannot tell the change apart, so the outcome is unsure
// (no-base-evidence), never a pass; and no fix round, which only a failed
// outcome starts.
func TestIntentAgentsBaseNetError(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBaseNetError},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
	})
	_, name, pr := e.toReview(t)
	in := e.waitEval(t, name, "the round to end unconfirmed", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationUnconfirmed && len(v.Outcomes) == 1
	})
	if o := in.Status.Verification.Outcomes[0]; o.Result != v1alpha1.OutcomeUnsure ||
		o.Reason != acceptance.ReasonNoBaseEvidence {
		t.Errorf("outcome %+v, want unsure, no-base-evidence", o)
	}
	pv := e.agentsVerification(t, name)
	if b := pv.Status.Base; b == nil || len(b.Runs) == 0 || b.Runs[0].Result != v1alpha1.CheckRunError ||
		b.Runs[0].Cause != "" {
		t.Errorf("base %+v, want an error run with no cause", b)
	}
	if st, ok := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus); ok && st.State == "success" {
		t.Errorf("maul/acceptance %+v: no base evidence is no pass", st)
	}
	if len(e.verifyRuns(t, name)) != 0 {
		t.Errorf("a fix round started on an unsure outcome: %s", e.describeRuns(t, name))
	}
}

// tallyAlreadyGreets is a base that already answers --greet as the outcome
// states: the change outcome's check cannot tell the head from it.
const tallyAlreadyGreets = `package main

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

// tallyBaseImportsMissing is a base whose build fails for a reason outside
// its own files: it imports a package no module provides, which the module
// loader, not the compiler, refuses.
const tallyBaseImportsMissing = `package main

import (
	"fmt"

	"example.com/missing/greeting"
)

func main() { fmt.Println(greeting.Text) }
`

// ledgerBuild is the result of the agents tool pod's build of side, ""
// when its ledger records none.
func ledgerBuild(pv v1alpha1.PreviewVerification, side v1alpha1.TreeSide) v1alpha1.BuildResult {
	if pv.Status.Ledger == nil {
		return ""
	}
	for _, b := range pv.Status.Ledger.Builds {
		if b.Side == side {
			return b.Result
		}
	}
	return ""
}

// waitUnsureNoBaseEvidence waits for intent name's one outcome to end
// unsure with no base evidence, never passed: maul/acceptance says it could
// not confirm, and no fix round starts, which only a failed outcome does.
func (e *evalEnv) waitUnsureNoBaseEvidence(t *testing.T, name, head string) v1alpha1.PreviewVerification {
	t.Helper()
	in := e.waitEval(t, name, "the round to end unconfirmed", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationUnconfirmed && len(v.Outcomes) == 1
	})
	if o := in.Status.Verification.Outcomes[0]; o.Result != v1alpha1.OutcomeUnsure ||
		o.Reason != acceptance.ReasonNoBaseEvidence {
		t.Errorf("outcome %+v, want unsure, no-base-evidence", o)
	}
	if v := in.Status.Verification; v.Passed != 0 || v.Failed != 0 {
		t.Errorf("verification %+v, want nothing passed or failed", v)
	}
	e.waitStatus(t, head, "error", "maul couldn't confirm this: 0/1 outcomes confirmed")
	if len(e.verifyRuns(t, name)) != 0 {
		t.Errorf("a fix round started on an unsure outcome: %s", e.describeRuns(t, name))
	}
	return e.agentsVerification(t, name)
}

// TestIntentAgentsGreenAtBase (review T2, rule 5): the base already greets,
// so the change outcome's check passes at the head and at the base alike.
// The scripted evaluator passes the outcome on its head run and proposes
// the check; maul confirms it green at both sides and, since it does not
// tell the change apart, never passes the outcome: unsure, no base
// evidence.
func TestIntentAgentsGreenAtBase(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyAlreadyGreets},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
	})
	_, name, pr := e.toReview(t)
	pv := e.waitUnsureNoBaseEvidence(t, name, pr.HeadSHA)
	if got := ledgerBuild(pv, v1alpha1.TreeBase); got != v1alpha1.BuildBuilt {
		t.Errorf("base build %q, want built", got)
	}
	b := pv.Status.Base
	if b == nil || b.Unbuildable || len(b.Runs) == 0 {
		t.Fatalf("base %+v, want it built and the check run there", b)
	}
	for _, r := range b.Runs {
		if r.Tree != v1alpha1.TreeBase || r.Result != v1alpha1.CheckRunPass {
			t.Errorf("base run %+v, want the check green at the base", r)
		}
	}
}

// TestIntentAgentsBaseUnbuildable (review T2): the base does not compile
// (undefined: fmt), a refusal of the base tree itself, so the base is
// unbuildable and every base run is an error with the base-unbuildable
// cause: no base comparison, which rule 5 admits. The check passes at the
// head on every other rule, so the outcome is confirmed.
func TestIntentAgentsBaseUnbuildable(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyDoesNotBuild},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
	})
	_, name, pr := e.toReview(t)
	in := e.waitEval(t, name, "the evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	o := in.Status.Verification.Outcomes
	if len(o) != 1 || o[0].Result != v1alpha1.OutcomePassed ||
		!strings.HasSuffix(o[0].Line, "; no base comparison: the base did not build") {
		t.Errorf("outcomes %+v, want greets passed with no base comparison, the base not built", o)
	}
	pv := e.agentsVerification(t, name)
	if got := ledgerBuild(pv, v1alpha1.TreeBase); got != v1alpha1.BuildFailed {
		t.Errorf("base build %q, want failed", got)
	}
	if b := pv.Status.Base; b == nil || !b.Unbuildable {
		t.Errorf("base %+v, want it unbuildable", b)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes confirmed")
	runs := e.kubelet.WorkspaceRuns()
	if len(runs) != 1 {
		t.Fatalf("%d agents tool pods ran, want one", len(runs))
	}
	ledger := string(runs[0].Stdout)
	for _, want := range []string{"undefined: fmt", `"cause":"base-unbuildable"`} {
		if !strings.Contains(ledger, want) {
			t.Errorf("the ledger does not record %s:\n%s", want, ledger)
		}
	}
}

// TestIntentAgentsBaseBuildError (review T2, VC-1): the base's build fails
// for a reason outside its own files (it imports a package no module
// provides), which maul cannot tell is the tree's: a build error, not a
// failed build, so the base is not unbuildable and every base run is an
// error with no cause. That is no base evidence: unsure, never a pass on
// the head alone.
func TestIntentAgentsBaseBuildError(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBaseImportsMissing},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
	})
	_, name, pr := e.toReview(t)
	pv := e.waitUnsureNoBaseEvidence(t, name, pr.HeadSHA)
	if got := ledgerBuild(pv, v1alpha1.TreeBase); got != v1alpha1.BuildError {
		t.Errorf("base build %q, want error", got)
	}
	if b := pv.Status.Base; b == nil || b.Unbuildable {
		t.Errorf("base %+v, want it not unbuildable", b)
	}
	runs := e.kubelet.WorkspaceRuns()
	if len(runs) != 1 {
		t.Fatalf("%d agents tool pods ran, want one", len(runs))
	}
	ledger := string(runs[0].Stdout)
	for _, want := range []string{"cannot find module providing package example.com/missing/greeting",
		"base-build-error: the base tree's build did not finish"} {
		if !strings.Contains(ledger, want) {
			t.Errorf("the ledger does not record %s:\n%s", want, ledger)
		}
	}
	if strings.Contains(ledger, `"cause":"base-unbuildable"`) {
		t.Errorf("the ledger counts a base build error as unbuildable:\n%s", ledger)
	}
}

// TestIntentAgentsRunningToolPodDeleted (wave 3): a round whose heads move
// while its tool pod still runs settles, and its tool Job, unfinished, is
// deleted at once, never kept for the linger a finished one gets. A person
// pushes to the pull request while the tool pod is up but not yet Ready.
func TestIntentAgentsRunningToolPodDeleted(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBase},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
	})
	e.kubelet.holdToolsReady(t)
	_, name, pr := e.agentsToEvaluating(t)
	var job batchv1.Job
	within(t, roundTimeout, "the round's tool Job running", func() bool {
		var list batchv1.JobList
		if err := e.cl.client.List(context.Background(), &list, client.InNamespace(agentsNS),
			client.MatchingLabels{v1alpha1.LabelRole: v1alpha1.RoleTools}); err != nil || len(list.Items) != 1 {
			return false
		}
		job = list.Items[0]
		return toolPodIP(e.kubelet) != ""
	})
	head := e.gh.HumanPush(intentOwner, appRepo, "maul-intent/"+name, "NOTES.md", "a person's note\n",
		"docs: a note")
	if head == pr.HeadSHA {
		t.Fatal("the push did not move the head")
	}
	within(t, roundTimeout, "the running tool Job deleted once its round settled", func() bool {
		var cur batchv1.Job
		err := e.cl.client.Get(context.Background(), client.ObjectKeyFromObject(&job), &cur)
		return apierrors.IsNotFound(err) || (err == nil && (!cur.DeletionTimestamp.IsZero() || cur.UID != job.UID))
	})
	if len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)) != 0 {
		t.Errorf("an evaluator ran on a tool pod that was never Ready: %s", e.describeRuns(t, name))
	}
}
