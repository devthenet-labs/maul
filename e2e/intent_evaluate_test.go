// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/e2e/fakegithub"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The agent evaluator e2e (SPEC S1-24): the shipped intent-controller with
// --intent-evaluator, preview-controller with --preview-evaluator, and
// source-controller, against the fake GitHub. The fake kubelet runs the
// intent agent Jobs, the slot tool pod's tools container as the real
// preview-tools (built with e2edial), and the evaluator and check-reviewer
// Jobs on the REAL agent-runner driving hack/fake-agent/claude, which drives
// the real maul-tool against that tool pod as a model following the
// evaluate prompt would (MAUL_FAKE_CLAUDE_EVALUATE picks what it does). The
// application is a fake component the test serves in the previewed one's
// place. preview-controller collects the tools ledger through pods/log;
// intent-controller reviews the confirmed checks, judges the round and
// gates the preview hand-off.

const (
	// The tool pod's three images, digest-pinned, never run: the kubelet
	// runs the relay binary and no sidecar.
	toolsImage = "registry.example/maul/preview-tools@sha256:" +
		"7171717171717171717171717171717171717171717171717171717171717171"
	browserImage = "registry.example/maul/preview-browser@sha256:" +
		"7272727272727272727272727272727272727272727272727272727272727272"
	toolboxImage = "registry.example/maul/preview-toolbox@sha256:" +
		"7373737373737373737373737373737373737373737373737373737373737373"
	// previewHostSuffix follows an intent's name in its preview host,
	// linked once the preview is handed over.
	previewHostSuffix = ".preview.maul.example.com"
	// heldLink is what a held hand-off says in place of the link.
	heldLink = "maul holds its link until its evaluator has looked at the approved outcomes"
	// readyLabel is the label of a link released ready.
	readyLabel = "Ready: maul's evaluator saw every approved outcome hold, citing calls maul recorded."
)

// versionOutcomes is the plan's two approved outcomes under the evaluator,
// as the fake planner declares them: the application answers GET /version
// with the version the build wrote, and it still does what it did.
const versionOutcomes = `acceptance:
  - id: version-served
    outcome: GET /version answers with the version the build wrote
  - id: still-serving
    outcome: the application still answers its version endpoint
    kind: keep`

// oneOutcome is a plan stating version-served alone.
const oneOutcome = `acceptance:
  - id: version-served
    outcome: GET /version answers with the version the build wrote`

// evalApp stands in for the previewed application: GET /version answers
// with the status and version the test sets.
type evalApp struct {
	mu      sync.Mutex
	status  int
	version string
}

func (a *evalApp) set(status int, version string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status, a.version = status, version
}

func (a *evalApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	status, version := a.status, a.version
	a.mu.Unlock()
	// GET /version, or a POST under /version/ (a check writing its run's
	// own record there; under TestIntentEvaluateFixRound's planted defect
	// the record is still stored, and only the GET fails).
	if r.URL.Path != "/version" && (r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/version/")) {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost && status != http.StatusOK {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusOK {
		_, _ = fmt.Fprintf(w, `{"version":%q}`, version)
		return
	}
	_, _ = w.Write([]byte(`{"error":"internal error"}`))
}

// evalEnv is one running evaluator stack.
type evalEnv struct {
	*verifyEnv
	app *evalApp
}

// evalOptions shape one evaluator stack.
type evalOptions struct {
	// outcomes is the plan's acceptance (versionOutcomes when empty).
	outcomes string
	// noOutcomes: the plan states no acceptance at all (outcomes unused).
	noOutcomes bool
	// mode is MAUL_FAKE_CLAUDE_EVALUATE (empty: its default, pass).
	mode string
	// intentArgs and previewArgs are extra flags for each controller.
	intentArgs, previewArgs []string
	// mutate edits the Project spec before it is created.
	mutate func(*v1alpha1.ProjectSpec)
	// agentRunner are phases beside evaluate the real agent-runner runs.
	agentRunner []string
	// beforeIntents runs once the cluster is up, before intent-controller
	// starts, and returns more flags for it (a broker started first).
	beforeIntents func(*cluster) []string
	// component, when set, is served as the application in evalApp's place.
	component http.Handler
	// github, when set, shapes the fake GitHub before any controller
	// reads it (a permission the installation does not hold).
	github func(*fakegithub.Server)
}

// startEvaluate runs the evaluator stack for a Project whose one
// application repository is previewed and verified, the fake planner
// stating the outcomes, and the application answering version 0.1.0.
func startEvaluate(t *testing.T, o evalOptions) *evalEnv {
	t.Helper()
	cl, gh, registry := startSourceStack(t, appRepo)
	if o.github != nil {
		o.github(gh)
	}
	startEvaluatingPreviewController(t, cl, o.previewArgs...)
	spec := v1alpha1.ProjectSpec{
		IntentRepository: intentRepoURL,
		Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login, reader.Login}},
		Repositories: []v1alpha1.ProjectRepository{{Name: appRepo, URL: appRepoURL,
			Preview: previewAt(shopLeaf, "/healthz", "/")}},
		Verification: &v1alpha1.ProjectVerification{Enabled: true},
	}
	if o.mutate != nil {
		o.mutate(&spec)
	}
	args := o.intentArgs
	if o.beforeIntents != nil {
		args = append(slices.Clone(args), o.beforeIntents(cl)...)
	}
	e := withEvaluatingIntents(t, cl, gh, spec, args...)
	e.registry = registry
	gh.SetDefaultComparePatch("diff --git a/VERSION b/VERSION\n+0.1.0\n")
	app := &evalApp{status: http.StatusOK, version: "0.1.0"}
	slots := []string{"maul-preview-0", "maul-preview-1"}
	e.kubelet.runTools(t, slots...)
	var component http.Handler = app
	if o.component != nil {
		component = o.component
	}
	e.kubelet.serveComponent(t, appRepo, component)
	e.kubelet.useAgentRunner(t, append([]string{"evaluate"}, o.agentRunner...)...)
	outcomes := o.outcomes
	switch {
	case o.noOutcomes:
		outcomes = ""
	case outcomes == "":
		outcomes = versionOutcomes
	}
	e.kubelet.setAgentEnv("MAUL_FAKE_PLAN_ACCEPTANCE", outcomes)
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_CHECK_PATH", "/version")
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_CHECK_TEXT", "0.1.0")
	if o.mode != "" {
		e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_EVALUATE", o.mode)
	}
	return &evalEnv{verifyEnv: &verifyEnv{intentEnv: e}, app: app}
}

// startEvaluatingPreviewController runs preview-controller over two fixed
// slots with the evaluator's tool images (extra adds flags): outcome
// verifications run on the slot tool pod, and the evaluator alone turns
// verification on.
func startEvaluatingPreviewController(t *testing.T, cl *cluster, extra ...string) {
	t.Helper()
	for _, name := range []string{"maul-preview-0", "maul-preview-1"} {
		if err := cl.client.Create(context.Background(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	cl.controller(t, "preview-controller", append([]string{
		"--preview-slot-count", "2",
		"--preview-image-prefix", previewPrefix,
		"--preview-host-suffix", strings.TrimPrefix(previewHostSuffix, "."),
		"--preview-node-pool", "maul-preview", "--preview-node-class", "maul-preview",
		"--preview-taint-key", "maul.devthe.net/preview-only",
		"--preview-poll-interval", "1s",
		"--preview-evaluator",
		"--preview-tools-image", toolsImage,
		"--preview-browser-image", browserImage,
		"--preview-toolbox-image", toolboxImage,
		"--preview-agents-namespace", agentsNS,
	}, extra...)...)
}

// evaluatorArgs turn the evaluator, verification's one switch, on in
// intent-controller, beside previews. A role Job pins the broker in its hosts file
// (it runs with no resolver), so the broker's host must resolve here: this
// machine's localhost, as the DNS tests pin it.
var evaluatorArgs = []string{"--intent-previews-enabled", "--intent-evaluator",
	"--broker-url", "http://localhost:8080"}

// withEvaluatingIntents is withIntentsFor with intent-controller built with
// e2edial and run with the evaluator on: the evaluator's strict probe
// target is read from a resolver file of the test's own
// (MAUL_E2E_RESOLV_CONF), since the host's may name a loopback stub, which
// intent-controller rightly refuses.
func withEvaluatingIntents(t *testing.T, cl *cluster, gh *fakegithub.Server, spec v1alpha1.ProjectSpec,
	extra ...string) *intentEnv {
	t.Helper()
	gh.SetRole(approver.Login, "write")
	gh.SetRole(mallory.Login, "write")
	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver 10.96.0.10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := buildTagged(t, "intent-controller", "e2edial")
	args := append(append([]string{"--namespace", namespace}, intentArgs...), evaluatorArgs...)
	args = append(args, extra...)
	env := []string{"MAUL_E2E_RESOLV_CONF=" + resolv}
	_, stop := cl.serveBinaryEnv(t, "intent-controller", bin, env, args...)
	ie := &intentEnv{cl: cl, gh: gh, kubelet: startKubelet(t, cl, intentRunKind), stopIntents: stop}
	if err := cl.client.Create(context.Background(), &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
		Spec:       spec,
	}); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	eventually(t, "the project to be Ready", func() bool {
		var p v1alpha1.Project
		key := types.NamespacedName{Namespace: namespace, Name: projectName}
		if err := cl.client.Get(context.Background(), key, &p); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady)
	})
	return ie
}

// within polls cond for up to d, failing with why after it: an evaluation
// round takes several controller polls end to end.
func within(t *testing.T, d time.Duration, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, why)
}

// roundTimeout bounds one evaluation round end to end.
const roundTimeout = 90 * time.Second

// waitEval waits for the Intent's verification to satisfy cond within a
// round's time, and returns the Intent; on a timeout it says where the
// round stands.
func (e *evalEnv) waitEval(t *testing.T, name, why string,
	cond func(*v1alpha1.IntentVerification) bool) *v1alpha1.Intent {
	t.Helper()
	var in *v1alpha1.Intent
	deadline := time.Now().Add(roundTimeout)
	for {
		in = e.intent(t, name)
		if in.Status.Verification != nil && cond(in.Status.Verification) {
			return in
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for intent %s: %s\n%s", roundTimeout, name, why,
				e.describeEval(t, name))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// describeEval is where an intent's evaluation stands: the verification,
// the runs, the PreviewVerifications.
func (e *evalEnv) describeEval(t *testing.T, name string) string {
	t.Helper()
	in := e.intent(t, name)
	var b strings.Builder
	fmt.Fprintf(&b, "phase %s, verification %+v\n", in.Status.Phase, in.Status.Verification)
	if v := in.Status.Verification; v != nil && v.Handoff != nil {
		fmt.Fprintf(&b, "handoff %+v\n", *v.Handoff)
	}
	b.WriteString(e.describeRuns(t, name))
	for n, pv := range e.verifications(t, name) {
		fmt.Fprintf(&b, "\npv %s: %s %s tools %+v", n, pv.Status.Phase, pv.Status.Reason, pv.Status.Tools)
	}
	return b.String()
}

// stageRuns are the Intent's runs of stage, oldest first.
func (e *evalEnv) stageRuns(t *testing.T, name string, stage v1alpha1.IntentStage) []v1alpha1.IntentRun {
	t.Helper()
	var out []v1alpha1.IntentRun
	for _, run := range e.runs(t, name) {
		if run.Spec.Stage == stage {
			out = append(out, run)
		}
	}
	slices.SortFunc(out, func(a, b v1alpha1.IntentRun) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	return out
}

// toEvaluating files an intent, approves it, serves its pull request's head
// and waits for its first evaluation round to start; it returns the issue
// number, the Intent's name and the pull request.
func (e *evalEnv) toEvaluating(t *testing.T) (int, string, v1alpha1.IntentPullRequest) {
	t.Helper()
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitEval(t, name, "the first round to start", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationEvaluating
	})
	return number, name, pr
}

// previewComment is the intent's preview comment on the pull request,
// "" while there is none.
func (e *evalEnv) previewComment(name string, pr int64) string {
	got := e.own(int(pr), notice(name, templates.PreviewKey))
	if len(got) != 1 {
		return ""
	}
	return got[0].Body
}

// waitHeld waits for the pull request's preview comment to hold the link,
// and checks the issue's status comment holds it too.
func (e *evalEnv) waitLinkHeld(t *testing.T, name string, number int, pr int64) {
	t.Helper()
	host := name + previewHostSuffix
	eventually(t, "the preview comment to hold the link", func() bool {
		body := e.previewComment(name, pr)
		return strings.Contains(body, heldLink) && !strings.Contains(body, host)
	})
	e.waitStatusComment(t, name, number, "the status comment to hold the link", func(body string) bool {
		return strings.Contains(body, heldLink) && !strings.Contains(body, host)
	})
}

// evaluatorJobs are the evaluate Jobs the kubelet ran, in order.
func (e *evalEnv) evaluatorJobs() []agentRun {
	var out []agentRun
	for _, r := range e.kubelet.Runs() {
		if r.Env["MAUL_PHASE"] == "evaluate" {
			out = append(out, r)
		}
	}
	return out
}

// TestIntentEvaluatePasses (cases 1 and 10): a correct build. The plan
// states two outcomes. Once the pull request's head is live,
// preview-controller renders the slot tool pod; while its tools container
// is not Ready no evaluator exists, and the evaluator is created only once
// the tool pod's address is in status, its Job pinning exactly that
// address. The scripted evaluator sends one http call per outcome and, its
// answer showing the version, passes each outcome citing that call; maul
// counts each pass on its cited call in the verified ledger (no check is
// written, confirmed, reviewed or stored) and judges both outcomes seen to
// hold. The hand-off is gated: while maul checks, maul/acceptance is pending
// ("maul is evaluating this") and neither comment links the preview; once it
// passes, both link it as ready, each outcome's row naming the call it
// cites, and maul/acceptance is success.
func TestIntentEvaluatePasses(t *testing.T) {
	e := startEvaluate(t, evalOptions{})
	release := e.kubelet.holdToolsReady(t)
	number, name, pr := e.toEvaluating(t)

	// Case 10: the tool pod runs, not Ready: no evaluator.
	var pv v1alpha1.PreviewVerification
	within(t, roundTimeout, "the tool pod rendered and running", func() bool {
		for _, p := range e.verifications(t, name) {
			pv = p
		}
		return pv.Status.Phase == v1alpha1.VerificationRunning
	})
	consistently(t, "no evaluator while the tool pod's address is not in status", func() bool {
		var cur v1alpha1.PreviewVerification
		err := e.cl.client.Get(context.Background(), client.ObjectKeyFromObject(&pv), &cur)
		return err == nil && cur.Status.Tools == nil && len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)) == 0
	})
	e.waitStatus(t, pr.HeadSHA, "pending", "maul is evaluating this")
	e.waitLinkHeld(t, name, number, pr.Number)
	release()

	in := e.waitEval(t, name, "the evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	checkConfirmedReady(t, in.Status.Verification)
	if err := e.cl.client.Get(context.Background(), client.ObjectKeyFromObject(&pv), &pv); err != nil {
		t.Fatal(err)
	}
	tool := e.checkEvaluatorPinned(t, name, &pv)
	if pv.Status.Ledger == nil || pv.Status.Ledger.ConfigMap == "" {
		t.Errorf("ledger %+v; want the ledger stored", pv.Status.Ledger)
	}

	// The hand-off: ready, linked, each row naming the call it cites.
	e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes seen to hold")
	host := name + previewHostSuffix
	eventually(t, "the preview comment to link the preview as ready", func() bool {
		body := e.previewComment(name, pr.Number)
		return strings.Contains(body, readyLabel) && strings.Contains(body, host) &&
			strings.Count(body, "seen to hold by maul's evaluator; it cites call ") == 2
	})
	e.waitStatusComment(t, name, number, "the status comment to link the preview as ready", func(body string) bool {
		return strings.Contains(body, readyLabel) && strings.Contains(body, host)
	})
	eventually(t, "the settled tool Job deleted", func() bool {
		var cur batchv1.Job
		err := e.cl.client.Get(context.Background(), client.ObjectKeyFromObject(&tool.Job), &cur)
		return apierrors.IsNotFound(err) || (err == nil && !cur.DeletionTimestamp.IsZero())
	})
}

// TestIntentEvaluateTurnCap (S1 bug 3): every live evaluator ran out of
// turns before writing its report, and each round was thrown away. Here the
// operator sets the evaluate turn ceiling (--intent-evaluate-max-turns 40)
// and the scripted evaluator runs out: its first run, given the 34 turns
// left once agent-runner keeps 6 for the report (two repair rounds of 3),
// makes its call per outcome and ends on claude's error_max_turns with no
// report. agent-runner resumes its session once, on 3 of the 6 kept turns,
// and the report it then writes cites those calls: maul counts both
// outcomes from that one run, and the hand-off is ready.
func TestIntentEvaluateTurnCap(t *testing.T) {
	e := startEvaluate(t, evalOptions{intentArgs: []string{"--intent-evaluate-max-turns", "40"}})
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_CAPPED", "1")
	_, name, pr := e.toEvaluating(t)

	in := e.waitEval(t, name, "the evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	checkConfirmedReady(t, in.Status.Verification)
	evals := e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)
	if len(evals) != 1 || evals[0].Spec.Grant.MaxTurns != 40 {
		t.Fatalf("evaluate runs %d (grant %+v); want one, granted the controller's 40 turns", len(evals),
			evals[0].Spec.Grant)
	}
	job := e.kubelet.waitRun(t, "the evaluator's Job", phaseIs("evaluate"))
	calls := scriptedCalls(t, job)
	if len(calls) != 2 || calls[0].mode != "first" || calls[0].maxTurns != "34" || calls[1].mode != "resume" ||
		calls[1].session != calls[0].session || calls[1].maxTurns != "3" {
		t.Errorf("scripted calls = %+v; want the capped run on 34 turns, then its session resumed on 3 of the 6 kept",
			calls)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes seen to hold")
}

// checkConfirmedReady holds a passed first round: both outcomes seen to
// hold on the calls they cite, the gated hand-off released ready.
func checkConfirmedReady(t *testing.T, v *v1alpha1.IntentVerification) {
	t.Helper()
	if v.Round != 1 || v.Passed != 2 || len(v.Outcomes) != 2 || v.Handoff == nil || !v.Handoff.Released ||
		v.Handoff.Reason != v1alpha1.HandoffReasonReady || v.Handoff.Mode != v1alpha1.HandoffGated {
		t.Errorf("verification = %+v, handoff %+v; want round 1, 2 confirmed, a gated hand-off released ready", v,
			v.Handoff)
	}
	for _, o := range v.Outcomes {
		if o.Result != "passed" || o.Reason != "seen-to-hold" || len(o.Cites) == 0 {
			t.Errorf("outcome %+v, want seen to hold on a cited call", o)
		}
	}
}

// checkEvaluatorPinned holds the one round's evaluator to its tool pod: the
// evaluate run pins pv and the address preview-controller published, which
// is the address the tool pod was given; its Job's pod reaches maul-tools
// there and carries the evaluator role and pv's UID; both ran clean. It
// returns the tool pod's run.
func (e *evalEnv) checkEvaluatorPinned(t *testing.T, name string, pv *v1alpha1.PreviewVerification) toolRun {
	t.Helper()
	tools := e.kubelet.ToolRuns()
	evals := e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)
	if len(tools) != 1 || len(evals) != 1 || pv.Status.Tools == nil {
		t.Fatalf("tool runs %d, evaluate runs %d, tools status %+v; want one each", len(tools), len(evals),
			pv.Status.Tools)
	}
	if ev := evals[0].Spec.Evaluation; ev == nil || ev.ToolIP != tools[0].IP || pv.Status.Tools.PodIP != tools[0].IP ||
		ev.UID != pv.UID {
		t.Errorf("evaluate run pins %+v, tools status %+v, tool pod at %s: want all the same, this pv's",
			evals[0].Spec.Evaluation, pv.Status.Tools, tools[0].IP)
	}
	job := e.kubelet.waitRun(t, "the evaluator's Job", phaseIs("evaluate"))
	tmpl := job.Job.Spec.Template
	if !slices.ContainsFunc(tmpl.Spec.HostAliases, func(a corev1.HostAlias) bool {
		return a.IP == tools[0].IP && slices.Contains(a.Hostnames, jobs.ToolsHostName)
	}) || tmpl.Labels[v1alpha1.LabelRole] != v1alpha1.RoleEvaluator ||
		tmpl.Labels[v1alpha1.LabelPreviewVerification] != string(pv.UID) {
		t.Errorf("evaluator pod template aliases %+v labels %v; want maul-tools at %s, role evaluator, pv %s",
			tmpl.Spec.HostAliases, tmpl.Labels, tools[0].IP, pv.UID)
	}
	if job.ExitCode != 0 || !strings.Contains(string(tools[0].Stdout), "MAUL-TOOLS") || tools[0].ExitCode != 0 {
		t.Errorf("evaluator exit %d, tools exit %d; tools log:\n%s", job.ExitCode, tools[0].ExitCode, tools[0].Stdout)
	}
	return tools[0]
}

// requestRound asks for one more evaluation at the same heads, as an
// approver: /maul verify fix also arms automatic fixes.
func (e *evalEnv) requestRound(t *testing.T, number int) {
	t.Helper()
	e.gh.CommentAs(number, "/maul verify fix", approver)
}

// evaluatorLeak is what no build or fix handoff may carry (S1-21,
// TestBuildHandoffCarriesOnlyCitedEvidence, end to end): the evaluator's
// and reviewer's run names, the check store, the evaluate prompt and the
// evaluator's report body. Its one-line note on a failing outcome does
// reach the fix round, labelled as its reading (owner decision 5).
var evaluatorLeak = []string{"-ev-", "-cr-", "-checks-", "Write your report to", "### The approved outcomes",
	"maul-tool", "Scripted evaluation", "Scripted review"}

// TestIntentEvaluateFixRound (cases 2, 9 and d2): round 1 passes on a
// correct build. A defect is then planted (the application now answers 500,
// logging a panic beside a bearer token) and an approver asks for another
// round with /maul verify fix. Every round is a model round: round 2's
// evaluator sends its call, sees the version missing, reads the
// component's log with `maul-tool logs` and fails the outcomes citing the
// call. maul counts the fail on its cited call and starts one verify-fix
// round, handed a reproduction maul rendered from its own ledger: the cited
// call, what it sent and the 500 that came back, the panic from the
// captured log with the token redacted, and the evaluator's note labelled
// as its reading, never its report or its prompt. The fix pushes; at its
// head the application answers again and the next round passes: ready.
// The round after the fix is handed the previous round (its verdicts and
// the diff since). No build or fix handoff of the whole run names the
// evaluator (case 9).
func TestIntentEvaluateFixRound(t *testing.T) {
	e := startEvaluate(t, evalOptions{})
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitEval(t, name, "round 1 to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes seen to hold")

	// The planted defect.
	e.app.set(http.StatusInternalServerError, "")
	e.kubelet.setContainerLog("maul-preview-0", appRepo, []byte("listening on :8080\n"+
		"GET /version authorization: Bearer e2e-secret-token-0123456789abcdef\n"+
		"panic: runtime error: index out of range [3] with length 3\n"+
		"goroutine 7 [running]:\nmain.version()\n"))
	e.requestRound(t, number)

	fix := e.waitFixRun(t, name)
	if fix.Spec.Inputs.VerificationRef == nil || fix.Spec.Repository.URL != appRepoURL {
		t.Errorf("fix run inputs %+v on %s, want a verification ref on %s", fix.Spec.Inputs, fix.Spec.Repository.URL,
			appRepoURL)
	}
	in := e.intent(t, name)
	if v := in.Status.Verification; v.Round != 2 || v.Failed == 0 {
		t.Errorf("verification = %+v, want round 2 failed", v)
	}
	for _, o := range in.Status.Verification.Outcomes {
		if o.Result != v1alpha1.OutcomeFailed || o.Reason != "seen-not-to-hold" || len(o.Cites) == 0 {
			t.Errorf("outcome %+v, want seen not to hold on a cited call", o)
		}
	}
	if ref := fix.Spec.Inputs.VerificationRef; ref == nil || ref.Name != e.round(t, name, 2).Name {
		t.Errorf("fix run inputs %+v, want round 2", fix.Spec.Inputs)
	}
	e.waitStatus(t, pr.HeadSHA, "failure", "")
	job := e.kubelet.waitRun(t, "the fix round's Job", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(fix.Name, intentRunKind, 1)
	})
	checkReproduction(t, string(job.Investigation))
	evals := e.evaluatorJobs()
	if len(evals) != 2 {
		t.Fatalf("evaluate Jobs = %d, want 2", len(evals))
	}
	calls, _ := os.ReadFile(filepath.Join(evals[1].Workspace, ".fake-claude", "tools"))
	if !strings.Contains(string(calls), "http GET /version\nlogs shop\n") {
		t.Errorf("round 2's evaluator did not read the log after its failing call:\n%s", calls)
	}

	// The fix pushed: the application answers at its head, and the next
	// round sees it.
	in = e.waitFor(t, name, "the fix pushed and back in review", func(in *v1alpha1.Intent) bool {
		run := e.run(t, fix.Name)
		return in.Status.Phase == v1alpha1.IntentInReview && run.Status.PushedCommit != "" &&
			in.Status.PullRequests[0].HeadSHA == run.Status.PushedCommit
	})
	head := in.Status.PullRequests[0].HeadSHA
	e.app.set(http.StatusOK, "0.1.0")
	e.serve(t, name, head)
	in = e.waitEval(t, name, "the round at the fix's head to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Heads[0].Revision == head
	})
	if v := in.Status.Verification; v.Round != 3 || in.Status.VerifyFixes != 1 || len(e.verifyRuns(t, name)) != 1 {
		t.Errorf("verification %+v, verify fixes %d, fix rounds %d; want round 3 passed after one fix", v,
			in.Status.VerifyFixes, len(e.verifyRuns(t, name)))
	}
	e.waitStatus(t, head, "success", "2/2 outcomes seen to hold")

	// The round after the fix is handed the round before it.
	evals = e.evaluatorJobs()
	checkPriorRound(t, evals[len(evals)-1].Issue)
	checkFocusedPrompts(t, evals)

	// Case 9: no build or fix handoff of the whole run names the evaluator.
	for _, r := range e.kubelet.Runs() {
		if r.Env["MAUL_PHASE"] != "build" {
			continue
		}
		for _, leak := range evaluatorLeak {
			if strings.Contains(string(r.Issue), leak) || strings.Contains(string(r.Investigation), leak) {
				t.Errorf("build Job %s's handoff carries %q", r.Job.Name, leak)
			}
		}
	}
}

// checkReproduction holds a fix round's handoff to the reproduction maul
// renders from its ledger: the failing outcome, the call it cites (what it
// sent and the 500 that came back), the panic from the component's
// captured log, its bearer token redacted, and the evaluator's note
// labelled as its reading.
func checkReproduction(t *testing.T, repro string) {
	t.Helper()
	for _, want := range []string{"version-served", "The calls it cites", "GET /version", "500",
		"panic: runtime error: index out of range", "The evaluator's reading, not checked by maul",
		"The page does not show what the outcome states"} {
		if !strings.Contains(repro, want) {
			t.Errorf("the fix handoff lacks %q:\n%s", want, repro)
		}
	}
	if strings.Contains(repro, "e2e-secret-token") {
		t.Errorf("the fix handoff carries the bearer token unredacted:\n%s", repro)
	}
}

// checkFocusedPrompts holds the prompts the scripted evaluator received to
// the change-focused round: only the round after the fix (the one handed
// its diff and the round before it, whole) is told what the diff changes
// and that an untouched outcome that passed needs one look; round 1 and
// round 2 (at unmoved heads: no diff) are first-round prompts.
func checkFocusedPrompts(t *testing.T, evals []agentRun) {
	t.Helper()
	const heading = "### What this round covers"
	for i, r := range evals {
		prompt, err := os.ReadFile(filepath.Join(r.Workspace, ".fake-claude", "prompt-evaluate"))
		if err != nil {
			t.Fatalf("read evaluate Job %d's prompt: %v", i+1, err)
		}
		if got, want := strings.Contains(string(prompt), heading), i == len(evals)-1; got != want {
			t.Errorf("evaluate Job %d's prompt has the change-focused section = %v, want %v", i+1, got, want)
		}
	}
}

// checkPriorRound holds the evaluate handoff of the round after a fix to
// the round before it (SPEC Q15, S1-29): round 2's number, its verdicts on
// both outcomes, no saved check's replay (none is saved), and the diff
// since.
func checkPriorRound(t *testing.T, handoff []byte) {
	t.Helper()
	var h struct {
		PriorRound    int               `json:"prior_round"`
		PriorVerdicts []json.RawMessage `json:"prior_verdicts"`
		PriorReplay   []json.RawMessage `json:"prior_replay"`
		FixDiff       string            `json:"fix_diff"`
	}
	if err := json.Unmarshal(handoff, &h); err != nil {
		t.Fatalf("decode round 3's evaluate handoff: %v", err)
	}
	if h.PriorRound != 2 || len(h.PriorVerdicts) != 2 || len(h.PriorReplay) != 0 || h.FixDiff == "" {
		t.Errorf("round 3's handoff: prior round %d, %d prior verdicts, %d prior replays, fix diff %q; want round "+
			"2's, both outcomes, no replay and the diff since", h.PriorRound, len(h.PriorVerdicts),
			len(h.PriorReplay), h.FixDiff)
	}
}

// crashPreview plays the kubelet seeing the intent's served preview Pod
// crash during the round: its container exited non-zero just now and waits
// in CrashLoopBackOff, not Ready.
func (e *evalEnv) crashPreview(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	var pods corev1.PodList
	if err := e.cl.client.List(ctx, &pods, client.InNamespace("maul-preview-0")); err != nil {
		t.Fatal(err)
	}
	crashed := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if !strings.HasPrefix(p.Name, "preview-"+name+"-") || len(p.Status.ContainerStatuses) == 0 {
			continue
		}
		cs := &p.Status.ContainerStatuses[0]
		now := metav1.Now()
		cs.Ready, cs.RestartCount = false, cs.RestartCount+1
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
		cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2,
			Reason: "Error", StartedAt: now, FinishedAt: now}}
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		if err := e.cl.client.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		crashed++
	}
	if crashed == 0 {
		t.Fatalf("no served preview Pod of %s to crash", name)
	}
}

// TestIntentEvaluateEnvFailed: the application crashes while its round is
// evaluated (its component crash-looping, a failure of the change). The
// round is the application's failure (EnvFailed): every outcome is unsure,
// env-failed, whatever its evaluator saw, no outcome fails and no fix
// round starts, and the hand-off says the change's environment failed.
func TestIntentEvaluateEnvFailed(t *testing.T) {
	e := startEvaluate(t, evalOptions{})
	release := e.kubelet.hold(t, "evaluate")
	_, name, pr := e.toEvaluating(t)
	within(t, roundTimeout, "the evaluator launched and held", func() bool {
		return len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)) == 1
	})
	time.Sleep(2 * time.Second) // the crash comes after the attempt began
	e.crashPreview(t, name)
	release()
	in := e.waitEval(t, name, "the round judged on the application's failure", func(v *v1alpha1.IntentVerification) bool {
		return v.OutcomesRound == 1 && v.State != v1alpha1.VerificationEvaluating
	})
	v := in.Status.Verification
	if v.State != v1alpha1.VerificationUnconfirmed || v.OutcomesEnvironment != "AppFailure" {
		t.Fatalf("verification = %s on %q, want Unconfirmed on the application's failure", v.State,
			v.OutcomesEnvironment)
	}
	for _, o := range v.Outcomes {
		if o.Result != v1alpha1.OutcomeUnsure || o.Reason != "env-failed" {
			t.Errorf("outcome %+v, want unsure, env-failed", o)
		}
	}
	if pv := e.round(t, name, 1); pv.Status.Phase != v1alpha1.VerificationEnvFailed {
		t.Errorf("round 1 = %s, want EnvFailed", pv.Status.Phase)
	}
	e.waitStatus(t, pr.HeadSHA, "error", "maul can't call this ready")
	consistently(t, "no fix round on the application's crash", func() bool {
		return len(e.verifyRuns(t, name)) == 0
	})
}

// TestIntentEvaluateUnverifiedLedger: a round whose tools log does not hold
// to its summary (here a forged entry the relay never wrote, which the
// termination summary does not count) is maul's trouble, never evidence:
// the verification is Infra ledger-mismatch with no index in status, no
// outcome passes or fails whatever the evaluator reported, no fix round
// starts, and after its attempts the verification is Unavailable.
func TestIntentEvaluateUnverifiedLedger(t *testing.T) {
	e := startEvaluate(t, evalOptions{})
	e.kubelet.forgeToolLedgers()
	_, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	in := e.waitEval(t, name, "every attempt unverified", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationUnavailable
	})
	v := in.Status.Verification
	for _, o := range v.Outcomes {
		if o.Result == v1alpha1.OutcomePassed || o.Result == v1alpha1.OutcomeFailed {
			t.Errorf("outcome %+v on a ledger that does not hold", o)
		}
	}
	if v.Passed != 0 || v.Failed != 0 || len(e.verifyRuns(t, name)) != 0 {
		t.Errorf("verification %+v, %d fix rounds; want nothing passed or failed and no fix", v,
			len(e.verifyRuns(t, name)))
	}
	pvs := e.verifications(t, name)
	if len(pvs) != int(v1alpha1.MaxVerificationAttempts) {
		t.Errorf("%d attempts, want %d", len(pvs), v1alpha1.MaxVerificationAttempts)
	}
	for _, pv := range pvs {
		if pv.Status.Phase != v1alpha1.VerificationInfra ||
			pv.Status.Reason != v1alpha1.VerificationReasonLedgerMismatch || pv.Status.Ledger != nil {
			t.Errorf("attempt %s = %s %s, ledger %+v; want Infra ledger-mismatch with no index", pv.Name,
				pv.Status.Phase, pv.Status.Reason, pv.Status.Ledger)
		}
	}
	if st, ok := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus); ok && (st.State == "success" ||
		st.State == "failure") {
		t.Errorf("maul/acceptance %+v on a ledger that does not hold", st)
	}
}

// TestIntentEvaluateNoValidCitation (case 4, the canary's successor): an
// evaluator that passes every outcome citing only calls maul does not
// count (its own read of a log, and a seq the round's ledger does not
// hold) passes nothing: every outcome is unsure on no valid evidence, the
// verification Unconfirmed, no round follows by itself, the hand-off is
// released as "maul can't call this ready", maul/acceptance an error, and
// the comment shows the evaluator's note labelled as its words.
func TestIntentEvaluateNoValidCitation(t *testing.T) {
	e := startEvaluate(t, evalOptions{mode: "cite-logs"})
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	in := e.waitEval(t, name, "the evaluation to stay unconfirmed", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationUnconfirmed
	})
	v := in.Status.Verification
	for _, o := range v.Outcomes {
		if o.Result != v1alpha1.OutcomeUnsure || o.Reason != "no-valid-evidence" || len(o.Cites) != 0 {
			t.Errorf("outcome %+v, want unsure on no valid evidence", o)
		}
	}
	if v.Handoff == nil || !v.Handoff.Released || v.Handoff.Reason != v1alpha1.HandoffReasonUnconfirmed {
		t.Errorf("handoff %+v, want released unconfirmed", v.Handoff)
	}
	e.waitStatus(t, pr.HeadSHA, "error", "maul can't call this ready")
	eventually(t, "the verification comment to show the note as the evaluator's words", func() bool {
		body := strings.Join(e.gh.Comments(number), "")
		return strings.Contains(body, "cites no call of its own in this evaluation that maul counts") &&
			strings.Contains(body, "(its words, not checked by maul): The log says it works.")
	})
	consistently(t, "no round 2: nothing asks for one", func() bool {
		return len(e.verifications(t, name)) == 1
	})
}

// TestIntentEvaluateEarlyHandoff (case 5): under --intent-default-handoff
// early the preview is linked as soon as it serves, before the evaluator
// has run (the hand-off released early at once); the round still runs and
// passes.
func TestIntentEvaluateEarlyHandoff(t *testing.T) {
	e := startEvaluate(t, evalOptions{intentArgs: []string{"--intent-default-handoff", "early"}})
	release := e.kubelet.hold(t, "evaluate")
	_, name, pr := e.toEvaluating(t)
	host := name + previewHostSuffix
	eventually(t, "the preview linked while the evaluator waits", func() bool {
		return strings.Contains(e.previewComment(name, pr.Number), host)
	})
	in := e.intent(t, name)
	if h := in.Status.Verification.Handoff; h == nil || !h.Released || h.Mode != v1alpha1.HandoffEarly ||
		h.Reason != v1alpha1.HandoffReasonEarly {
		t.Errorf("handoff %+v, want released early", h)
	}
	if n := len(e.evaluatorJobs()); n != 0 {
		t.Errorf("evaluate Jobs run = %d before the link, want 0", n)
	}
	release()
	e.waitEval(t, name, "the evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes seen to hold")
}

// TestIntentEvaluateHandoffMax (case 6): a gated hand-off whose evaluator
// has not finished by handoffMax from CI's first green is released "out of
// time", still checking. The clock is moved, not waited for: the test sets
// status.verification.firstGreenAt back past handoffMax's 10m floor.
func TestIntentEvaluateHandoffMax(t *testing.T) {
	e := startEvaluate(t, evalOptions{})
	release := e.kubelet.hold(t, "evaluate")
	number, name, pr := e.toEvaluating(t)
	e.waitLinkHeld(t, name, number, pr.Number)

	in := e.intent(t, name)
	if in.Status.Verification.FirstGreenAt == nil {
		t.Fatalf("no handoffMax clock: %+v", in.Status.Verification)
	}
	past := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	patch, _ := json.Marshal(map[string]any{"status": map[string]any{"verification": map[string]any{
		"firstGreenAt": past}}})
	err := e.cl.client.Status().Patch(context.Background(), in, client.RawPatch(types.MergePatchType, patch))
	if err != nil {
		t.Fatalf("move the handoffMax clock: %v", err)
	}
	in = e.waitEval(t, name, "the link handed over out of time", func(v *v1alpha1.IntentVerification) bool {
		return v.Handoff != nil && v.Handoff.Released
	})
	if h := in.Status.Verification.Handoff; h.Reason != v1alpha1.HandoffReasonOutOfTime {
		t.Errorf("handoff %+v, want released out of time", h)
	}
	eventually(t, "the preview linked, out of time", func() bool {
		return strings.Contains(e.previewComment(name, pr.Number), name+previewHostSuffix)
	})
	release()
}

// TestIntentEvaluateHumanMidRound (case 7): an approver's /maul revise on
// the pull request while the evaluator is still to run releases the held
// link for the human and halts automatic fixes; their round is next.
func TestIntentEvaluateHumanMidRound(t *testing.T) {
	e := startEvaluate(t, evalOptions{})
	release := e.kubelet.hold(t, "evaluate")
	number, name, pr := e.toEvaluating(t)
	e.waitLinkHeld(t, name, number, pr.Number)
	within(t, roundTimeout, "the evaluator launched and held", func() bool {
		return len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)) == 1
	})

	e.gh.CommentAs(int(pr.Number), "/maul revise Please also log the version at startup.", approver)
	in := e.waitEval(t, name, "the link released and fixes halted for the human",
		func(v *v1alpha1.IntentVerification) bool {
			return v.Handoff != nil && v.Handoff.Released && v.AutoFix.State == v1alpha1.AutoFixHalted
		})
	v := in.Status.Verification
	if v.Handoff.Reason != v1alpha1.HandoffReasonHuman && v.Handoff.Reason != v1alpha1.HandoffReasonCommand {
		t.Errorf("handoff %+v, want released for the human", v.Handoff)
	}
	if v.AutoFix.Reason != v1alpha1.AutoFixHumanCommand {
		t.Errorf("auto fix %+v, want halted for the human's command", v.AutoFix)
	}
	eventually(t, "the preview linked for the human", func() bool {
		return strings.Contains(e.previewComment(name, pr.Number), name+previewHostSuffix)
	})
	release()
	e.runOfTrigger(t, name, v1alpha1.IntentRunTriggerCommand, "the approver's round", func(v1alpha1.IntentRun) bool {
		return true
	})
}

// TestIntentEvaluateCeiling (case 8): an intent whose evaluations spent
// the evaluation ceiling is not evaluated further: its one round, whose
// evaluator cited nothing maul counts, is Unconfirmed, released "it spent
// its evaluation budget", never Blocked, and its fix budget untouched
// (automatic fixes still armed, none spent).
func TestIntentEvaluateCeiling(t *testing.T) {
	e := startEvaluate(t, evalOptions{mode: "cite-logs", intentArgs: []string{"--intent-evaluation-cost-max", "1"}})
	_, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	in := e.waitEval(t, name, "the evaluation unconfirmed at the ceiling", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationUnconfirmed && v.Handoff != nil && v.Handoff.Released
	})
	v := in.Status.Verification
	if in.Status.Phase != v1alpha1.IntentInReview || in.Status.VerifyFixes != 0 ||
		v.AutoFix.State != v1alpha1.AutoFixArmed || len(e.verifyRuns(t, name)) != 0 {
		t.Errorf("phase %s, verify fixes %d, auto fix %+v; want in review, no fix spent, fixes armed",
			in.Status.Phase, in.Status.VerifyFixes, v.AutoFix)
	}
	e.waitStatus(t, pr.HeadSHA, "error", "maul can't call this ready")
	eventually(t, "the preview linked, out of budget", func() bool {
		body := e.previewComment(name, pr.Number)
		return strings.Contains(body, "it spent its evaluation budget") && strings.Contains(body, name+previewHostSuffix)
	})
}

// ---- the browser surface ---------------------------------------------------

// browserOutcome is a plan stating one outcome only a browser shows.
const browserOutcome = `acceptance:
  - id: vote-button
    outcome: the battle page shows a 32 px square Vote button a person can see and click`

// ownScriptLabel is how a comment labels a call that ran the evaluator's
// own page script (owner decision 1).
const ownScriptLabel = "a browser command, the evaluator's own page script, and what it returned"

// browserApp stands in for a previewed web application: its page, as the
// relay's scripted browser reads it (previewtools.ScriptedPage), is a Vote
// button whose width the test sets and a heading.
type browserApp struct {
	mu    sync.Mutex
	width float64
}

func (a *browserApp) setWidth(w float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.width = w
}

func (a *browserApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	width := a.width
	a.mu.Unlock()
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"title": "Beat Battle", "elements": []map[string]any{
		{"role": "button", "name": "Vote", "testid": "vote", "text": "Vote", "x": 600, "y": 400, "width": width,
			"height": 32},
		{"role": "heading", "name": "Beat Battle", "text": "Beat Battle", "x": 20, "y": 20, "width": 400,
			"height": 40},
	}})
}

// startBrowserEvaluate runs the evaluator stack for a Project with the
// browser surface (entry /), the plan stating browserOutcome, the
// application a browserApp whose Vote button is a 32 px square, and the
// scripted evaluator in mode.
func startBrowserEvaluate(t *testing.T, mode string) (*evalEnv, *browserApp) {
	t.Helper()
	app := &browserApp{width: 32}
	e := startEvaluate(t, evalOptions{outcomes: browserOutcome, mode: mode, component: app,
		mutate: func(s *v1alpha1.ProjectSpec) {
			s.Surfaces = &v1alpha1.ProjectSurfaces{Browser: &v1alpha1.BrowserSurface{Entry: "/"}}
		}})
	return e, app
}

// round is the Intent's PreviewVerification of round n.
func (e *evalEnv) round(t *testing.T, name string, n int32) v1alpha1.PreviewVerification {
	t.Helper()
	for _, pv := range e.verifications(t, name) {
		if pv.Spec.Round == n {
			return pv
		}
	}
	t.Fatalf("no verification of round %d:\n%s", n, e.describeEval(t, name))
	return v1alpha1.PreviewVerification{}
}

// toolRunOf is the tool pod run of pv's round.
func (e *evalEnv) toolRunOf(t *testing.T, pv v1alpha1.PreviewVerification) toolRun {
	t.Helper()
	for _, r := range e.kubelet.ToolRuns() {
		if r.Job.Labels[v1alpha1.LabelPreviewVerification] == string(pv.UID) ||
			r.Job.Spec.Template.Labels[v1alpha1.LabelPreviewVerification] == string(pv.UID) {
			return r
		}
	}
	t.Fatalf("no tool run of %s among %d", pv.Name, len(e.kubelet.ToolRuns()))
	return toolRun{}
}

// checkBrowserLedger holds a round's tools ledger to every browser action
// its evaluator took: one agent browser entry per `maul-tool browser`
// call, in order and with its arguments, each run (none refused) after the
// relay opened its session, each screenshot an artifact of its entry; and
// the verified index on pv to the same count. It returns the entries.
func (e *evalEnv) checkBrowserLedger(t *testing.T, pv v1alpha1.PreviewVerification,
	eval agentRun) []toolwire.LedgerEntry {
	t.Helper()
	calls, err := os.ReadFile(filepath.Join(eval.Workspace, ".fake-claude", "tools"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, line := range strings.Split(string(calls), "\n") {
		if rest, ok := strings.CutPrefix(line, "browser "); ok {
			want = append(want, rest)
		}
	}
	var entries []toolwire.LedgerEntry
	var got []string
	session := false
	for _, line := range strings.Split(string(e.toolRunOf(t, pv).Stdout), "\n") {
		body, ok := strings.CutPrefix(line, toolwire.EntryPrefix)
		if !ok {
			continue
		}
		var wire struct {
			Entry *toolwire.LedgerEntry `json:"entry"`
		}
		if err := json.Unmarshal([]byte(body), &wire); err != nil {
			t.Fatalf("decode a ledger line: %v", err)
		}
		if wire.Entry == nil {
			continue // the summary
		}
		entry := *wire.Entry
		entries = append(entries, entry)
		if entry.Verb == toolwire.VerbSession && entry.Exit == 0 {
			session = true
		}
		if entry.Verb != toolwire.VerbBrowser || entry.Kind != toolwire.KindAgent {
			continue
		}
		var req toolwire.BrowserRequest
		if err := json.Unmarshal(entry.Args, &req); err != nil || entry.Refused != "" || entry.Exit != 0 || !session {
			t.Errorf("browser entry %d: args %s exit %d refused %q (session open %v)", entry.Seq, entry.Args,
				entry.Exit, entry.Refused, session)
		}
		got = append(got, strings.Join(append([]string{req.Verb}, req.Args...), " "))
		if req.Verb == "screenshot" && len(screenshots(entry)) != 1 {
			t.Errorf("screenshot entry %d artifacts %+v, want its PNG", entry.Seq, entry.Artifacts)
		}
	}
	if len(want) == 0 || !slices.Equal(want, got) {
		t.Errorf("the evaluator's browser calls %q, the ledger's %q; want every one recorded, in order", want, got)
	}
	indexed := 0
	if pv.Status.Ledger != nil {
		for _, x := range pv.Status.Ledger.Entries {
			if x.Verb == v1alpha1.LedgerVerbBrowser && x.Kind == v1alpha1.LedgerKindAgent && !x.Refused {
				indexed++
			}
		}
	}
	if indexed != len(want) {
		t.Errorf("the verified ledger index holds %d browser entries, want %d", indexed, len(want))
	}
	return entries
}

// screenshots are an entry's PNG artifacts (a snapshot's YAML is one too).
func screenshots(e toolwire.LedgerEntry) []toolwire.Artifact {
	var out []toolwire.Artifact
	for _, a := range e.Artifacts {
		if toolwire.IsScreenshot(a) {
			out = append(out, a)
		}
	}
	return out
}

// TestIntentEvaluateBrowserPasses (browser case a): a Project with the
// browser surface. The scripted evaluator opens the page in the tool pod's
// browser (the relay's scripted one, through the preview front), takes a
// snapshot, clicks, takes a screenshot, and measures the Vote button with
// its own page script (`browser eval`): a 32 px square, so it passes the
// outcome citing the screenshot and the script. maul counts the pass on
// those cited calls: every browser action is in the ledger, the comment
// shows the script labelled as the evaluator's own, and no check is saved.
// Another round an approver asks for at the same heads is a model round
// too: its evaluator runs again and passes.
func TestIntentEvaluateBrowserPasses(t *testing.T) {
	e, _ := startBrowserEvaluate(t, "browser")
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	in := e.waitEval(t, name, "the browser evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	pv := e.round(t, name, 1)
	if o := in.Status.Verification.Outcomes; len(o) != 1 || o[0].Result != v1alpha1.OutcomePassed ||
		o[0].Reason != "seen-to-hold" || len(o[0].Cites) != 2 {
		t.Errorf("outcomes %+v, want vote-button seen to hold on the screenshot and the script", o)
	}
	entries := e.checkBrowserLedger(t, pv, e.evaluatorJobs()[0])
	if !slices.ContainsFunc(entries, func(x toolwire.LedgerEntry) bool {
		return x.Verb == toolwire.VerbBrowser && slices.Equal(x.Locators, []string{"getByTestId('vote')"})
	}) {
		t.Errorf("no browser entry recorded the clicked element's locator")
	}
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes seen to hold")
	eventually(t, "the issue's comment to show the script as the evaluator's own", func() bool {
		body := strings.Join(e.gh.Comments(number), "")
		return strings.Contains(body, ownScriptLabel) && strings.Contains(body, `{"width":32,"height":32}`)
	})

	// Another round at the same heads: a model round, never a replay.
	e.requestRound(t, number)
	in = e.waitEval(t, name, "round 2 to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Round == 2
	})
	e.round(t, name, 2)
	if len(e.evaluatorJobs()) != 2 {
		t.Errorf("%d evaluate Jobs; want a model round's two evaluators", len(e.evaluatorJobs()))
	}
}

// TestIntentEvaluateBrowserFixRound (browser case b, the demo's step (d)):
// round 1 passes. The Vote button is then 30 px wide where the outcome
// says 32: an approver's /maul verify fix asks for round 2, whose evaluator
// measures the button with its own page script and fails the outcome
// citing it. maul counts the fail on that cited call and starts a
// verify-fix round whose reproduction shows the script and the 30 px it
// returned. The fix pushes; at its head the button is 32 px again and the
// next round passes.
func TestIntentEvaluateBrowserFixRound(t *testing.T) {
	e, app := startBrowserEvaluate(t, "browser")
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitEval(t, name, "round 1 to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})

	app.setWidth(30)
	e.requestRound(t, number)
	fix := e.waitFixRun(t, name)
	in := e.intent(t, name)
	if o := in.Status.Verification.Outcomes; in.Status.Verification.Round != 2 || len(o) != 1 ||
		o[0].Result != v1alpha1.OutcomeFailed || o[0].Reason != "seen-not-to-hold" {
		t.Errorf("verification %+v, want round 2 failed on the cited script", in.Status.Verification)
	}
	pv := e.round(t, name, 2)
	e.checkBrowserLedger(t, pv, e.evaluatorJobs()[1])
	e.waitStatus(t, pr.HeadSHA, "failure", "")
	job := e.kubelet.waitRun(t, "the fix round's Job", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(fix.Name, intentRunKind, 1)
	})
	for _, want := range []string{"vote-button", ownScriptLabel, `{"width":30,"height":32}`,
		"The vote button is not a 32 px square."} {
		if !strings.Contains(string(job.Investigation), want) {
			t.Errorf("the fix handoff lacks %q:\n%s", want, job.Investigation)
		}
	}

	in = e.waitFor(t, name, "the fix pushed and back in review", func(in *v1alpha1.Intent) bool {
		run := e.run(t, fix.Name)
		return in.Status.Phase == v1alpha1.IntentInReview && run.Status.PushedCommit != "" &&
			in.Status.PullRequests[0].HeadSHA == run.Status.PushedCommit
	})
	head := in.Status.PullRequests[0].HeadSHA
	app.setWidth(32)
	e.serve(t, name, head)
	in = e.waitEval(t, name, "the round at the fix's head to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Heads[0].Revision == head
	})
	if in.Status.VerifyFixes != 1 {
		t.Errorf("verify fixes %d, want one", in.Status.VerifyFixes)
	}
	e.waitStatus(t, head, "success", "1/1 outcomes seen to hold")
}

// TestIntentEvaluateBrowserScreenshotOnly (browser case c, DESIGN 7.5,
// D12): the evaluator looks at the page, takes a screenshot and passes the
// outcome citing the screenshot alone. A browser command that ran is a call
// maul counts: the outcome is seen to hold on it, the screenshot kept as
// evidence, the hand-off ready. Whether the screenshot shows the outcome is
// the reviewers' to judge from it.
func TestIntentEvaluateBrowserScreenshotOnly(t *testing.T) {
	e, _ := startBrowserEvaluate(t, "browser-look")
	_, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	in := e.waitEval(t, name, "the evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	if o := in.Status.Verification.Outcomes; len(o) != 1 || o[0].Result != v1alpha1.OutcomePassed ||
		o[0].Reason != "seen-to-hold" || len(o[0].Cites) != 1 {
		t.Errorf("outcomes %+v, want vote-button seen to hold on the screenshot", o)
	}
	pv := e.round(t, name, 1)
	entries := e.checkBrowserLedger(t, pv, e.evaluatorJobs()[0])
	if pv.Status.Ledger == nil || pv.Status.Ledger.Evidence == "" ||
		!slices.ContainsFunc(entries, func(x toolwire.LedgerEntry) bool {
			return x.Verb == toolwire.VerbBrowser && len(screenshots(x)) == 1
		}) {
		t.Errorf("ledger %+v, want the cited screenshot kept as evidence", pv.Status.Ledger)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes seen to hold")
}
