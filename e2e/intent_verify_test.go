// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
)

// The acceptance-verification e2e (slice 1): the shipped intent-controller
// with --intent-verification, preview-controller with a tester image, and
// source-controller, against the fake GitHub, with the fake kubelet
// running both the intent agent Jobs and the real preview-tester (built
// with the e2edial tag) for each tester Job preview-controller renders into
// a slot. The tester's requests reach a fake component the test serves in
// the previewed application's place; the tester's output reaches
// preview-controller through pods/log, as on a cluster; intent-controller
// judges it and posts maul/acceptance on the pull request's head.

const (
	// testerImage is the digest-pinned preview-tester image
	// preview-controller renders (never run: the kubelet runs the binary).
	testerImage = "registry.example/maul/preview-tester@sha256:" +
		"5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	// acceptanceStatus is the commit status context maul posts.
	acceptanceStatus = "maul/acceptance"
	// shopLeaf is the application's preview image leaf.
	shopLeaf = "acme-shop"
)

// versionCriterion is the plan's one http acceptance criterion, as the
// fake planner declares it: the application answers GET /version with the
// version the build wrote.
const versionCriterion = `acceptance:
  - id: version-served
    kind: http
    statement: GET /version names the version the build wrote
    target: shop
    request: {method: GET, path: /version}
    expect: {status: 200, bodyContains: "0.1.0"}`

// manualCriterion follows versionCriterion in a plan that also asks a human.
const manualCriterion = `
  - id: notes-read-well
    kind: manual
    statement: the release notes read well
    reason: only a human can judge the wording`

// fakeComponent stands in for the previewed application: GET /version
// answers with the status the test sets, and every request is counted.
type fakeComponent struct {
	mu     sync.Mutex
	status int
	paths  []string
}

func (c *fakeComponent) set(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = status
}

func (c *fakeComponent) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.paths)
}

func (c *fakeComponent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.paths = append(c.paths, r.Method+" "+r.URL.Path)
	status := c.status
	c.mu.Unlock()
	if r.URL.Path != "/version" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusOK {
		_, _ = w.Write([]byte(`{"version":"0.1.0"}`))
		return
	}
	_, _ = w.Write([]byte(`{"error":"no version yet"}`))
}

// verifyEnv is one running verification stack.
type verifyEnv struct {
	*intentEnv
	app *fakeComponent
}

// startVerify runs the intent stack with verification on for a Project
// whose one application repository is previewed, the fake planner
// declaring acceptance (none when empty), and the application answering
// status.
func startVerify(t *testing.T, acceptance string, status int) *verifyEnv {
	t.Helper()
	cl, gh, registry := startSourceStack(t, appRepo)
	startVerifyingPreviewController(t, cl)
	e := withIntentsFor(t, cl, gh, v1alpha1.ProjectSpec{
		IntentRepository: intentRepoURL,
		Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login, reader.Login}},
		Repositories: []v1alpha1.ProjectRepository{{Name: appRepo, URL: appRepoURL,
			Preview: previewAt(shopLeaf, "/healthz", "/")}},
		Verification: &v1alpha1.ProjectVerification{Enabled: true},
	}, "--intent-previews-enabled", "--intent-verification")
	e.registry = registry
	// Every round's handoff carries the pull request's patch; the heads
	// maul's rounds push are not known in advance.
	gh.SetDefaultComparePatch("diff --git a/VERSION b/VERSION\n+0.1.0\n")
	app := &fakeComponent{status: status}
	e.kubelet.runTesters(t, "maul-preview-0", "maul-preview-1")
	e.kubelet.serveComponent(t, appRepo, app)
	if acceptance != "" {
		e.kubelet.setAgentEnv("MAUL_FAKE_PLAN_ACCEPTANCE", acceptance)
	}
	return &verifyEnv{intentEnv: e, app: app}
}

// startVerifyingPreviewController runs preview-controller over two fixed
// slots, as startPreviewController does, with the tester image set:
// verification on.
func startVerifyingPreviewController(t *testing.T, cl *cluster) {
	t.Helper()
	for _, name := range []string{"maul-preview-0", "maul-preview-1"} {
		if err := cl.client.Create(context.Background(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	cl.controller(t, "preview-controller",
		"--preview-slot-count", "2",
		"--preview-image-prefix", previewPrefix,
		"--preview-host-suffix", "preview.maul.example.com",
		"--preview-node-pool", "maul-preview", "--preview-node-class", "maul-preview",
		"--preview-taint-key", "maul.devthe.net/preview-only",
		"--preview-poll-interval", "1s",
		"--preview-tester-image", testerImage)
}

// shopComponent is the intent's one preview component at head.
func shopComponent(head string) v1alpha1.PreviewComponent {
	return v1alpha1.PreviewComponent{Name: appRepo, ImageRepository: previewPrefix + shopLeaf, Revision: head,
		Port: 8080, ReadinessPath: "/healthz"}
}

// toReview files an intent, approves its plan and waits for its pull
// request, returning the issue number, the Intent's name and the pull
// request's record.
func (e *verifyEnv) toReview(t *testing.T) (int, string, v1alpha1.IntentPullRequest) {
	t.Helper()
	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	if len(in.Status.PullRequests) != 1 {
		t.Fatalf("pull requests = %+v, want one", in.Status.PullRequests)
	}
	return number, name, in.Status.PullRequests[0]
}

// serve plays the Deployment controller and the kubelet for the intent's
// one-component preview at head, which envtest has neither of: its
// Deployment in slot 0 renders head and has rolled out, and one Pod of
// exactly its current template (image and reset annotation) is Ready with
// an image ID. Any other Pod of it is deleted, as a rollout replaces it.
func (e *verifyEnv) serve(t *testing.T, name, head string) {
	t.Helper()
	ctx := context.Background()
	c := shopComponent(head)
	e.waitPreview(t, name, "the preview at "+head, []v1alpha1.PreviewComponent{c})
	image := c.ImageRepository + ":sha-" + c.Revision
	key := types.NamespacedName{Namespace: "maul-preview-0", Name: "preview-" + name}
	var dep appsv1.Deployment
	eventually(t, "the "+c.Name+" Deployment to roll out", func() bool {
		if e.cl.client.Get(ctx, key, &dep) != nil || dep.Spec.Template.Spec.Containers[0].Image != image {
			return false
		}
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1, 1
		return e.cl.client.Status().Update(ctx, &dep) == nil
	})
	tmpl := dep.Spec.Template
	var pods corev1.PodList
	if err := e.cl.client.List(ctx, &pods, client.InNamespace(key.Namespace),
		client.MatchingLabels(tmpl.Labels)); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.Containers[0].Image == image && maps.Equal(p.Annotations, tmpl.Annotations) {
			return // already served
		}
		if err := e.cl.client.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      fmt.Sprintf("%s-%s-g%d", key.Name, c.Revision[len(c.Revision)-8:], len(pods.Items)),
		Namespace: key.Namespace, Labels: tmpl.Labels, Annotations: tmpl.Annotations,
	}, Spec: tmpl.Spec}
	// Bound to the fake kubelet's Node, so its log (setContainerLog) is what
	// pods/log serves preview-controller's capture.
	pod.Spec.NodeName = kubeletNode
	if err := e.cl.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: c.Name, Ready: true,
		ImageID: c.ImageRepository + "@sha256:" + strings.Repeat(c.Revision[:1], 64)}}
	if err := e.cl.client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

// waitVerification waits for the Intent's verification to satisfy cond,
// and returns the Intent.
func (e *verifyEnv) waitVerification(t *testing.T, name, why string,
	cond func(*v1alpha1.IntentVerification) bool) *v1alpha1.Intent {
	t.Helper()
	return e.waitFor(t, name, why, func(in *v1alpha1.Intent) bool {
		return in.Status.Verification != nil && cond(in.Status.Verification)
	})
}

// waitStatus waits for the newest maul/acceptance on head to be state,
// posted by maul, with a description containing says, and returns it.
func (e *verifyEnv) waitStatus(t *testing.T, head, state, says string) fakegithub.CommitStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, ok := e.gh.LatestStatus(head, acceptanceStatus)
		if ok && st.State == state && st.Creator == fakegithub.Bot && strings.Contains(st.Description, says) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for maul/acceptance %s (%q) on %s; latest is %+v (posted %v)", state, says,
				head, st, ok)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// verifications lists the Intent's PreviewVerifications, by name.
func (e *verifyEnv) verifications(t *testing.T, name string) map[string]v1alpha1.PreviewVerification {
	t.Helper()
	var list v1alpha1.PreviewVerificationList
	if err := e.cl.client.List(context.Background(), &list, client.InNamespace(namespace),
		client.MatchingLabels{v1alpha1.LabelIntent: name}); err != nil {
		t.Fatalf("list verifications: %v", err)
	}
	out := map[string]v1alpha1.PreviewVerification{}
	for _, pv := range list.Items {
		out[pv.Name] = pv
	}
	return out
}

// verifyRuns are the Intent's acceptance-fix runs (trigger verify), by
// name.
func (e *verifyEnv) verifyRuns(t *testing.T, name string) map[string]v1alpha1.IntentRun {
	t.Helper()
	out := map[string]v1alpha1.IntentRun{}
	for n, run := range e.runs(t, name) {
		if run.Spec.Trigger == v1alpha1.IntentRunTriggerVerify {
			out[n] = run
		}
	}
	return out
}

// waitFixRun waits for the Intent's one acceptance-fix round to launch or
// complete and returns it, failing at once, with what the runs say, if it
// ended any other way.
func (e *verifyEnv) waitFixRun(t *testing.T, name string) v1alpha1.IntentRun {
	t.Helper()
	var fix v1alpha1.IntentRun
	eventually(t, "an acceptance-fix round to launch", func() bool {
		for _, run := range e.verifyRuns(t, name) {
			fix = run
		}
		switch fix.Status.Phase {
		case v1alpha1.RunRunning, v1alpha1.RunComplete:
			return true
		case v1alpha1.RunFailed:
			t.Fatalf("acceptance-fix round %s failed: %s", fix.Name, e.describeRuns(t, name))
		}
		return false
	})
	return fix
}

// checkOneTesterRun asserts the one tester run so far: rendered into slot
// 0 with no ServiceAccount token on the tester image, handed only the http
// criterion with the component's Service as its target, exiting 0 with its
// wire lines, its request reaching the application; and its Job deleted
// once the verification settled.
func (e *verifyEnv) checkOneTesterRun(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	runs := e.kubelet.TesterRuns()
	if len(runs) != 1 {
		t.Fatalf("tester runs = %d, want 1", len(runs))
	}
	tr := runs[0]
	var preview v1alpha1.Preview
	if err := e.cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &preview); err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Targets  map[string]string `json:"targets"`
		Criteria []struct {
			ID string `json:"id"`
		} `json:"criteria"`
	}
	if err := json.Unmarshal([]byte(tr.Spec), &spec); err != nil {
		t.Fatalf("tester spec: %v", err)
	}
	// The tester is handed the component Service's ClusterIP, as the API
	// server allocated it, never a name it would have to resolve.
	var svc corev1.Service
	if err := e.cl.client.Get(ctx, types.NamespacedName{Namespace: "maul-preview-0",
		Name: v1alpha1.PreviewComponentServiceName(&preview, 0)}, &svc); err != nil {
		t.Fatal(err)
	}
	if net.ParseIP(svc.Spec.ClusterIP) == nil {
		t.Fatalf("component Service ClusterIP %q", svc.Spec.ClusterIP)
	}
	wantTarget := net.JoinHostPort(svc.Spec.ClusterIP, "80")
	if spec.Targets[appRepo] != wantTarget || len(spec.Criteria) != 1 || spec.Criteria[0].ID != "version-served" {
		t.Errorf("tester spec targets %v criteria %+v, want %s at %s and only the http criterion", spec.Targets,
			spec.Criteria, appRepo, wantTarget)
	}
	if tr.Job.Namespace != "maul-preview-0" || tr.ExitCode != 0 ||
		!strings.Contains(string(tr.Stdout), "MAUL-VERIFY:") {
		t.Errorf("tester ran in %s, exit %d, stdout %q; want slot 0, 0, its wire lines", tr.Job.Namespace,
			tr.ExitCode, tr.Stdout)
	}
	pod := tr.Job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		pod.Containers[0].Image != testerImage {
		t.Errorf("tester pod mounts a token or runs %s, want none and %s", pod.Containers[0].Image, testerImage)
	}
	if got := e.app.requests(); !slices.Contains(got, "GET /version") {
		t.Errorf("the application saw %v, want the tester's GET /version", got)
	}
	eventually(t, "the settled tester Job deleted", func() bool {
		job := tr.Job.DeepCopy()
		err := e.cl.client.Get(ctx, client.ObjectKeyFromObject(job), job)
		return apierrors.IsNotFound(err) || (err == nil && !job.DeletionTimestamp.IsZero())
	})
}

// checkOneVerification asserts the Intent's one PreviewVerification:
// Observed, controlled by the Intent, at head.
func (e *verifyEnv) checkOneVerification(t *testing.T, in *v1alpha1.Intent, head string) {
	t.Helper()
	pvs := e.verifications(t, in.Name)
	if len(pvs) != 1 {
		t.Fatalf("verifications = %d, want 1", len(pvs))
	}
	for _, pv := range pvs {
		owner := metav1.GetControllerOf(&pv)
		if pv.Status.Phase != v1alpha1.VerificationObserved || owner == nil || owner.UID != in.UID ||
			pv.Spec.Heads[0].Revision != head {
			t.Errorf("verification %s = %s, owner %v, heads %+v; want Observed, the Intent's, at %s", pv.Name,
				pv.Status.Phase, owner, pv.Spec.Heads, head)
		}
	}
}

// TestIntentVerifyPasses: a plan with one http criterion and one manual
// one is approved and built; once the preview serves the pull request's
// head, preview-controller runs the tester in the slot, which reaches the
// application by its Service name; intent-controller judges the
// observation a pass and posts maul/acceptance success on the head, naming
// the manual check left open, with one verification comment on the issue.
// Another account's maul/acceptance over it is answered by maul's own,
// posted again, and named in the pull request's verification section.
func TestIntentVerifyPasses(t *testing.T) {
	e := startVerify(t, versionCriterion+manualCriterion, http.StatusOK)
	number, name, pr := e.toReview(t)

	// intent-controller records the approved criteria on a pass after the
	// Intent reaches InReview, so wait for them rather than reading at once.
	in := e.waitFor(t, name, "the approved plan's acceptance recorded", func(in *v1alpha1.Intent) bool {
		return in.Status.Acceptance != nil
	})
	if acc := in.Status.Acceptance; acc == nil || acc.PlanRevision != 1 || len(acc.Criteria) != 2 ||
		acc.Criteria[0].ID != "version-served" || acc.Criteria[1].Kind != v1alpha1.AcceptanceManual {
		t.Fatalf("recorded acceptance = %+v, want the approved plan's two criteria", in.Status.Acceptance)
	}
	// Nothing is posted as passed before anything ran.
	if st, ok := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus); ok && st.State == "success" {
		t.Fatalf("maul/acceptance = %+v before any verification", st)
	}

	e.serve(t, name, pr.HeadSHA)
	in = e.waitVerification(t, name, "the verification to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	v := in.Status.Verification
	if v.Passed != 1 || v.Failed != 0 || v.ManualOpen != 1 || v.Round != 1 || v.Attempt != 1 ||
		v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Errorf("verification = %+v, want round 1 attempt 1: 1 passed, 1 manual open, fixes armed", v)
	}
	// A manual criterion is open: pending, never green, until a human
	// accepts it.
	e.waitStatus(t, pr.HeadSHA, "pending", "1/1 automated passed, 1 to accept by hand")
	e.checkOneTesterRun(t, name)
	e.checkOneVerification(t, in, pr.HeadSHA)
	eventually(t, "one verification comment on the issue", func() bool {
		got := e.own(number, notice(name, templates.VerifyKey(1)))
		return len(got) == 1 && strings.Contains(got[0].Body, "version-served")
	})
	if e.intent(t, name).Status.Phase != v1alpha1.IntentInReview {
		t.Errorf("intent left review: verification never moves the phase")
	}

	// Someone else posts maul/acceptance over maul's: maul posts its own
	// again and names them.
	mine, _ := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus)
	e.gh.SetCommitStatus(pr.HeadSHA, fakegithub.CommitStatus{Context: acceptanceStatus, State: "failure",
		Description: "not verified", Creator: mallory})
	again := e.waitStatus(t, pr.HeadSHA, "pending", "1/1 automated passed, 1 to accept by hand")
	if again.ID <= mine.ID {
		t.Errorf("maul's status %d is not newer than its first %d", again.ID, mine.ID)
	}
	e.waitPreviewComment(t, name, pr.Number, "the verification section naming the impostor", "`mallory`")
}

// TestIntentVerifyAgentPlansCriteria: the plan of a verifying Project runs
// on the REAL agent-runner, with hack/fake-agent/claude as the model, which
// declares criteria only when its prompt asks for them, as a model would.
// Live, intent-controller never told a plan Job its Project verifies, so the
// prompt never asked, no plan declared a criterion and verification never
// ran; every other test here injects criteria past the prompt. Here the plan
// Job carries the Project's previewed components, the prompt the agent
// receives teaches the acceptance field naming them, the posted plan comment
// shows the approver the split, and the planned criterion is recorded and
// passes against the preview.
func TestIntentVerifyAgentPlansCriteria(t *testing.T) {
	e := startVerify(t, versionCriterion, http.StatusOK)
	e.kubelet.useAgentRunner(t, "plan")
	number, name, pr := e.toReview(t)

	r := e.kubelet.waitRun(t, "the plan Job", func(r agentRun) bool { return r.Env["MAUL_PHASE"] == "plan" })
	if r.ExitCode != 0 {
		t.Fatalf("plan agent-runner exited %d:\n%s", r.ExitCode, r.Stdout)
	}
	if got, want := r.Env["MAUL_PLAN_VERIFICATION"],
		`{"components":[{"key":"shop","path":"/","readiness":"/healthz"}]}`; got != want {
		t.Errorf("plan Job MAUL_PLAN_VERIFICATION = %q, want %q", got, want)
	}
	prompt, err := os.ReadFile(filepath.Join(r.Workspace, ".fake-claude", "prompt-plan"))
	if err != nil {
		t.Fatalf("read the prompt the scripted claude received: %v", err)
	}
	for _, want := range []string{"### Acceptance criteria", "- `shop`: served under `/`, ready when `/healthz` answers"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("the plan prompt lacks %q:\n%s", want, prompt)
		}
	}

	in := e.waitFor(t, name, "the approved plan's acceptance recorded", func(in *v1alpha1.Intent) bool {
		return in.Status.Acceptance != nil
	})
	if acc := in.Status.Acceptance; len(acc.Criteria) != 1 || acc.Criteria[0].ID != "version-served" ||
		acc.Criteria[0].Target != appRepo {
		t.Fatalf("recorded acceptance = %+v, want the planner's version-served on %s", acc, appRepo)
	}
	pl := in.Status.Plan
	plans := e.own(number, templates.PlanMarker(namespace, name, pl.Revision, pl.Digest))
	if len(plans) != 1 || !strings.Contains(plans[0].Body, "**Acceptance:** 1 checked automatically.") {
		t.Errorf("plan comments = %+v, want one stating the acceptance split", plans)
	}

	e.serve(t, name, pr.HeadSHA)
	e.waitVerification(t, name, "the planned criterion to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Passed == 1
	})
	e.waitStatus(t, pr.HeadSHA, "success", "")
}

// TestIntentVerifyFixRound: the application fails the criterion. The
// first tester run dies before its summary (maul's own trouble): the
// round's next attempt runs and observes the failure, which starts one
// acceptance-fix round on the repository, handed the failure after the
// approved plan. The round pushes; the preview rolls to the new head, where
// the application now answers, and the next verification passes: success
// on the new head, one verify fix counted.
func TestIntentVerifyFixRound(t *testing.T) {
	e := startVerify(t, versionCriterion, http.StatusInternalServerError)
	e.kubelet.failNextTester(testerNoSummary)
	_, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)

	// The first attempt is maul's trouble, never the intent's: retried.
	in := e.waitVerification(t, name, "the second attempt to run", func(v *v1alpha1.IntentVerification) bool {
		return v.Attempt == 2
	})
	if v := in.Status.Verification; v.Round != 1 || v.InfraRetries != 1 {
		t.Errorf("verification = %+v, want round 1's second attempt after one infra retry", v)
	}

	// The observed failure starts one acceptance-fix round.
	fix := e.waitFixRun(t, name)
	if fix.Spec.Inputs.VerificationRef == nil || fix.Spec.Repository.URL != appRepoURL {
		t.Errorf("fix run inputs %+v on %s, want a verification ref on %s", fix.Spec.Inputs, fix.Spec.Repository.URL,
			appRepoURL)
	}
	e.waitStatus(t, pr.HeadSHA, "failure", "0/1 automated passed")
	job := e.kubelet.waitRun(t, "the fix round's Job", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(fix.Name, intentRunKind, 1)
	})
	handoff := string(job.Investigation)
	if !strings.Contains(handoff, "### Acceptance failures") || !strings.Contains(handoff, "version-served") {
		t.Errorf("fix handoff does not carry the failure:\n%s", handoff)
	}
	e.checkCredentialless(t, job)

	// The fix pushed: the preview rolls to its head, where the application
	// now answers, and the next verification passes there.
	in = e.waitFor(t, name, "the fix pushed and back in review", func(in *v1alpha1.Intent) bool {
		run := e.run(t, fix.Name)
		return in.Status.Phase == v1alpha1.IntentInReview && run.Status.PushedCommit != "" &&
			in.Status.PullRequests[0].HeadSHA == run.Status.PushedCommit
	})
	head := in.Status.PullRequests[0].HeadSHA
	if in.Status.VerifyFixes != 1 || in.Status.Revisions != 0 || in.Status.CheckFixes != 0 {
		t.Errorf("counters verify %d revisions %d check %d, want 1, 0, 0", in.Status.VerifyFixes,
			in.Status.Revisions, in.Status.CheckFixes)
	}
	e.app.set(http.StatusOK)
	e.serve(t, name, head)
	in = e.waitVerification(t, name, "the verification at the fix's head to pass",
		func(v *v1alpha1.IntentVerification) bool {
			return v.State == v1alpha1.VerificationPassed && v.Heads[0].Revision == head
		})
	e.waitStatus(t, head, "success", "1/1 automated passed")
	if v := in.Status.Verification; v.Round != 2 || v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Errorf("verification = %+v, want round 2 passed, fixes still armed", v)
	}
	if n := len(e.verifyRuns(t, name)); n != 1 {
		t.Errorf("acceptance-fix rounds = %d, want 1", n)
	}
}

// TestIntentVerifyRepeatedFailure: a fix round that does not change what
// fails halts automatic fixes (RepeatedFailure): the failing status says so
// and no second round starts. An approver's /maul verify, once the
// application answers, runs one more verification, which passes.
func TestIntentVerifyRepeatedFailure(t *testing.T) {
	e := startVerify(t, versionCriterion, http.StatusInternalServerError)
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)

	fix := e.waitFixRun(t, name)
	in := e.waitFor(t, name, "the fix pushed and back in review", func(in *v1alpha1.Intent) bool {
		run := e.run(t, fix.Name)
		return in.Status.Phase == v1alpha1.IntentInReview && run.Status.PushedCommit != "" &&
			in.Status.PullRequests[0].HeadSHA == run.Status.PushedCommit
	})
	head := in.Status.PullRequests[0].HeadSHA
	e.serve(t, name, head)
	in = e.waitVerification(t, name, "automatic fixes halted on the same failure",
		func(v *v1alpha1.IntentVerification) bool {
			return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixRepeatedFailure
		})
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != string(v1alpha1.AutoFixRepeatedFailure) {
		t.Errorf("VerifyFailing = %+v, want True RepeatedFailure", c)
	}
	e.waitStatus(t, head, "failure", "automatic fixes halted")
	consistently(t, "no second fix round and the intent still in review", func() bool {
		return len(e.verifyRuns(t, name)) == 1 && e.intent(t, name).Status.Phase == v1alpha1.IntentInReview
	})

	// The application answers now; the approver asks for a verification.
	e.app.set(http.StatusOK)
	e.gh.CommentAs(number, "/maul verify", approver)
	e.waitVerification(t, name, "the requested verification to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Heads[0].Revision == head
	})
	e.waitStatus(t, head, "success", "1/1 automated passed")
	if n := len(e.verifyRuns(t, name)); n != 1 {
		t.Errorf("acceptance-fix rounds = %d, want still 1", n)
	}
}

// TestIntentVerifyNotDeclared: a plan with no acceptance criteria is
// reported NotDeclared: no tester runs, and maul/acceptance is pending
// (never green) until an approver's /maul accept all, which turns it
// success naming them.
func TestIntentVerifyNotDeclared(t *testing.T) {
	e := startVerify(t, "", http.StatusOK)
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitVerification(t, name, "the verification NotDeclared", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationNotDeclared && v.HeadSetDigest != ""
	})
	e.waitStatus(t, pr.HeadSHA, "pending", "no acceptance criteria declared")
	consistently(t, "no tester and nothing green", func() bool {
		st, _ := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus)
		return st.State == "pending" && len(e.kubelet.TesterRuns()) == 0 && len(e.verifications(t, name)) == 0
	})
	e.gh.CommentAs(number, "/maul accept all", approver)
	e.waitStatus(t, pr.HeadSHA, "success", "accepted by "+approver.Login)
}

// TestIntentVerifyWithoutStatusesWrite: statuses write is never part of
// a Project's Ready proof (D5, narrowed): an installation without it leaves
// a verifying Project Ready, with AcceptanceStatusUnavailable naming the
// repository and the permission.
func TestIntentVerifyWithoutStatusesWrite(t *testing.T) {
	cl, gh, _ := startSourceStack(t, appRepo)
	gh.DenyPermission("statuses")
	gh.SetRole(approver.Login, "write")
	cl.controller(t, "intent-controller", intentArgs...)
	ctx := context.Background()
	if err := cl.client.Create(ctx, &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: intentRepoURL,
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login}},
			Repositories: []v1alpha1.ProjectRepository{{Name: appRepo, URL: appRepoURL,
				Preview: previewAt(shopLeaf, "/healthz", "/")}},
			Verification: &v1alpha1.ProjectVerification{Enabled: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	var unavailable *metav1.Condition
	eventually(t, "the project Ready, the status unavailable for want of statuses write", func() bool {
		var p v1alpha1.Project
		if err := cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: projectName}, &p); err != nil {
			return false
		}
		unavailable = meta.FindStatusCondition(p.Status.Conditions, "AcceptanceStatusUnavailable")
		return meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady) &&
			unavailable != nil && unavailable.Status == metav1.ConditionTrue
	})
	if !strings.Contains(unavailable.Message, appRepoURL) || !strings.Contains(unavailable.Message, "statuses: write") {
		t.Errorf("AcceptanceStatusUnavailable = %+v, want it naming statuses: write on %s", unavailable, appRepoURL)
	}
}

// runOfTrigger waits for one of the Intent's runs of trigger to satisfy
// cond, and returns it.
func (e *verifyEnv) runOfTrigger(t *testing.T, name string, trigger v1alpha1.IntentRunTrigger,
	why string, cond func(v1alpha1.IntentRun) bool) v1alpha1.IntentRun {
	t.Helper()
	var found v1alpha1.IntentRun
	eventually(t, why, func() bool {
		for _, run := range e.runs(t, name) {
			if run.Spec.Trigger == trigger && cond(run) {
				found = run
				return true
			}
		}
		return false
	})
	return found
}

// botSays reports whether any of maul's comments on number contains s.
func (e *verifyEnv) botSays(number int64, s string) bool {
	for _, c := range e.gh.IssueComments(int(number)) {
		if c.User == fakegithub.Bot && strings.Contains(c.Body, s) {
			return true
		}
	}
	return false
}

// TestIntentVerifyHumanCancelsPendingFix (H5): an approver's /maul revise
// on the pull request arrives while the acceptance-fix round its failure
// started still waits for an agent slot (another intent's plan holds it).
// The fix is cancelled before any Job exists, superseded by the human, and
// counts as no fix; automatic fixes halt for the human's command, the pull
// request is told, and the approver's round runs next.
func TestIntentVerifyHumanCancelsPendingFix(t *testing.T) {
	e := startVerify(t, versionCriterion, http.StatusInternalServerError)
	_, name, pr := e.toReview(t)

	// Another intent's plan takes the one agent slot and keeps it.
	release := e.kubelet.hold(t, "plan")
	_, other := e.fileIntent(t)
	otherPlan := v1alpha1.IntentRunName(other, v1alpha1.IntentStagePlan, 1, "", 1)
	eventually(t, "the other intent's plan to hold the slot", func() bool {
		return e.kubelet.held(jobs.NameFor(otherPlan, intentRunKind, 1))
	})

	e.serve(t, name, pr.HeadSHA)
	fix := e.runOfTrigger(t, name, v1alpha1.IntentRunTriggerVerify, "an acceptance-fix round waiting for a slot",
		func(v1alpha1.IntentRun) bool { return true })
	e.waitStatus(t, pr.HeadSHA, "failure", "0/1 automated passed")
	if run := e.run(t, fix.Name); run.Status.JobRef != nil {
		t.Fatalf("the fix launched (%+v) while the slot was held", run.Status.JobRef)
	}

	e.gh.CommentAs(int(pr.Number), "/maul revise Please also log the version at startup.", approver)
	eventually(t, "the pending fix cancelled for the human", func() bool {
		run := e.run(t, fix.Name)
		return run.Status.Phase == v1alpha1.RunFailed && run.Status.Outcome == "superseded_by_human"
	})
	if run := e.run(t, fix.Name); run.Status.JobRef != nil {
		t.Errorf("the cancelled fix has a Job %+v", run.Status.JobRef)
	}
	in := e.waitVerification(t, name, "automatic fixes halted for the human's command",
		func(v *v1alpha1.IntentVerification) bool {
			return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanCommand
		})
	if in.Status.VerifyFixes != 0 {
		t.Errorf("verify fixes = %d, want 0: a cancelled fix is no fix", in.Status.VerifyFixes)
	}
	eventually(t, "the pull request told the fix was cancelled", func() bool {
		return e.botSays(pr.Number, "cancelled for your review")
	})

	// The slot frees: the approver's round runs, on their feedback.
	release()
	human := e.runOfTrigger(t, name, v1alpha1.IntentRunTriggerCommand, "the approver's round to complete",
		func(run v1alpha1.IntentRun) bool { return run.Status.Phase == v1alpha1.RunComplete })
	job := e.kubelet.waitRun(t, "the approver's round's Job", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(human.Name, intentRunKind, 1)
	})
	if !strings.Contains(string(job.Investigation), "Please also log the version at startup.") {
		t.Errorf("the approver's round was not handed their feedback:\n%s", job.Investigation)
	}
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	if in.Status.VerifyFixes != 0 || in.Status.Revisions != 1 || len(e.verifyRuns(t, name)) != 1 {
		t.Errorf("verify fixes %d revisions %d fix rounds %d, want 0, 1, 1", in.Status.VerifyFixes,
			in.Status.Revisions, len(e.verifyRuns(t, name)))
	}
}

// TestIntentVerifyHumanDuringRunningFix (H6): an approver's /maul revise
// arrives while the acceptance-fix round's agent runs. The fix finishes and
// pushes; automatic fixes halt for the human; the approver's round runs
// next, told the automatic fix landed after their feedback.
func TestIntentVerifyHumanDuringRunningFix(t *testing.T) {
	e := startVerify(t, versionCriterion, http.StatusInternalServerError)
	_, name, pr := e.toReview(t)
	release := e.kubelet.hold(t, "build")

	e.serve(t, name, pr.HeadSHA)
	fix := e.runOfTrigger(t, name, v1alpha1.IntentRunTriggerVerify, "the acceptance-fix round's agent running",
		func(run v1alpha1.IntentRun) bool {
			return run.Status.JobRef != nil && e.kubelet.held(jobs.NameFor(run.Name, intentRunKind, 1))
		})
	cmd := e.gh.CommentAs(int(pr.Number), "/maul revise Please also log the version at startup.", approver)
	in := e.waitVerification(t, name, "automatic fixes halted for the human's command",
		func(v *v1alpha1.IntentVerification) bool {
			return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanCommand
		})
	if in.Status.Verification.CancelRun != "" {
		t.Errorf("cancelRun = %q: a launched fix is never cancelled", in.Status.Verification.CancelRun)
	}
	// The fix finishes after the feedback was given (GitHub's whole
	// seconds).
	for _, c := range e.gh.IssueComments(int(pr.Number)) {
		if c.ID == cmd {
			afterSecond(c.CreatedAt)
		}
	}
	release()
	eventually(t, "the fix to push", func() bool {
		run := e.run(t, fix.Name)
		return run.Status.Phase == v1alpha1.RunComplete && run.Status.PushedCommit != ""
	})
	pushed := e.run(t, fix.Name).Status.PushedCommit

	human := e.runOfTrigger(t, name, v1alpha1.IntentRunTriggerCommand, "the approver's round to run",
		func(run v1alpha1.IntentRun) bool { return run.Status.JobRef != nil })
	job := e.kubelet.waitRun(t, "the approver's round's Job", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(human.Name, intentRunKind, 1)
	})
	handoff := string(job.Investigation)
	if !strings.Contains(handoff, "An automatic acceptance fix landed after your review: `"+pushed+"`") ||
		!strings.Contains(handoff, "Please also log the version at startup.") {
		t.Errorf("the approver's round was not told of the fix %s, or not handed their feedback:\n%s", pushed,
			handoff)
	}
	if run := e.run(t, human.Name); run.Status.BaseSHA != pushed {
		t.Errorf("the approver's round starts from %s, want the fix's %s", run.Status.BaseSHA, pushed)
	}
	in = e.waitFor(t, name, "the approver's round done", func(in *v1alpha1.Intent) bool {
		return in.Status.Phase == v1alpha1.IntentInReview && in.Status.Revisions == 1
	})
	if in.Status.VerifyFixes != 1 || len(e.verifyRuns(t, name)) != 1 {
		t.Errorf("verify fixes %d, fix rounds %d, want 1 and 1", in.Status.VerifyFixes, len(e.verifyRuns(t, name)))
	}
}

// TestIntentVerifyHumanPushAndAccept: the plan's http criterion passes and
// an approver accepts its manual one with /maul accept, which turns the
// pending status success, naming them. A human then pushes to the
// pull request's branch: the combined change moved, so maul/acceptance on
// the new head goes back to pending, automatic fixes halt (HumanPush), and
// the acceptance shows as made at the earlier heads, not re-checked. The
// approver asks for a verification; the application now fails it, which is
// reported failing with no automatic fix.
func TestIntentVerifyHumanPushAndAccept(t *testing.T) {
	e := startVerify(t, versionCriterion+manualCriterion, http.StatusOK)
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitVerification(t, name, "the verification to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	e.waitStatus(t, pr.HeadSHA, "pending", "1 to accept by hand")

	accept := e.gh.CommentAs(number, "/maul accept notes-read-well", approver)
	in := e.waitFor(t, name, "the manual criterion accepted", func(in *v1alpha1.Intent) bool {
		return in.Status.Acceptance != nil && len(in.Status.Acceptance.Accepted) == 1
	})
	rec := in.Status.Acceptance.Accepted[0]
	if rec.ID != "notes-read-well" || rec.By != approver.Login || rec.ByID != approver.ID ||
		rec.CommentID != accept || rec.HeadSetDigest != in.Status.Verification.HeadSetDigest {
		t.Errorf("acceptance record = %+v, want the approver's at the verified head set", rec)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 automated passed; 1 accepted by hand by "+approver.Login)

	// A human pushes to the branch.
	branch := v1alpha1.IntentBranchPrefix + name
	pushed := e.gh.HumanPush(intentOwner, appRepo, branch, "NOTES.md", "Written by hand.\n", "docs: notes")
	e.waitVerification(t, name, "automatic fixes halted for the human's push",
		func(v *v1alpha1.IntentVerification) bool {
			return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanPush
		})
	e.waitStatus(t, pushed, "pending", "combined change moved: re-verifying")
	e.serve(t, name, pushed)
	eventually(t, "the acceptance shown as not re-checked", func() bool {
		return e.botSays(pr.Number, "not re-checked") || e.botSays(int64(number), "not re-checked")
	})

	// The approver asks; the application now fails: reported, not fixed.
	e.app.set(http.StatusInternalServerError)
	e.gh.CommentAs(number, "/maul verify", approver)
	e.waitVerification(t, name, "the requested verification to fail at the pushed head",
		func(v *v1alpha1.IntentVerification) bool {
			return v.State == v1alpha1.VerificationFailing && v.Heads[0].Revision == pushed
		})
	e.waitStatus(t, pushed, "failure", "automatic fixes halted")
	consistently(t, "no automatic fix after a human's push", func() bool {
		return len(e.verifyRuns(t, name)) == 0 && e.intent(t, name).Status.Phase == v1alpha1.IntentInReview
	})
}

// TestIntentVerifyFixAfterAHumanPush (S1 demo bug 2): automatic fixes are
// halted (/maul verify stop); the approver pushes to the pull request and
// re-arms them with /maul verify fix a second later, before maul need have
// seen the push. GitHub recorded the push first, so the re-arm stands once
// maul records the pushed head: the application now fails there, and an
// automatic fix round starts.
func TestIntentVerifyFixAfterAHumanPush(t *testing.T) {
	e := startVerify(t, versionCriterion, http.StatusOK)
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitVerification(t, name, "the verification to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	e.gh.CommentAs(number, "/maul verify stop", approver)
	e.waitVerification(t, name, "automatic fixes stopped", func(v *v1alpha1.IntentVerification) bool {
		return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanStop
	})

	e.app.set(http.StatusInternalServerError)
	branch := v1alpha1.IntentBranchPrefix + name
	pushed := e.gh.HumanPush(intentOwner, appRepo, branch, "NOTES.md", "Written by hand.\n", "docs: notes")
	afterSecond(time.Now().UTC().Truncate(time.Second))
	e.gh.CommentAs(number, "/maul verify fix", approver)
	e.serve(t, name, pushed)
	e.waitVerification(t, name, "the pushed head recorded with automatic fixes armed",
		func(v *v1alpha1.IntentVerification) bool {
			return len(v.Heads) == 1 && v.Heads[0].Revision == pushed && v.AutoFix.Rearm != nil &&
				v.AutoFix.State == v1alpha1.AutoFixArmed
		})
	fix := e.waitFixRun(t, name)
	if v := e.intent(t, name).Status.Verification; v.AutoFix.Reason == v1alpha1.AutoFixHumanPush {
		t.Errorf("auto-fix = %+v: the push halted it after the later re-arm", v.AutoFix)
	}
	e.waitFor(t, name, "the fix pushed on top of the human's commit", func(in *v1alpha1.Intent) bool {
		run := e.run(t, fix.Name)
		return run.Status.PushedCommit != "" && run.Status.BaseSHA == pushed &&
			in.Status.PullRequests[0].HeadSHA == run.Status.PushedCommit
	})
}

// TestIntentVerifyReset: /maul verify --reset bumps the intent's reset
// generation, which reaches the Preview; preview-controller restarts its
// parts, and once they run for the reset the verification runs again.
func TestIntentVerifyReset(t *testing.T) {
	e := startVerify(t, versionCriterion, http.StatusOK)
	ctx := context.Background()
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitVerification(t, name, "the verification to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})

	e.gh.CommentAs(number, "/maul verify --reset", approver)
	in := e.waitVerification(t, name, "the reset recorded", func(v *v1alpha1.IntentVerification) bool {
		return v.Reset.Generation == 1
	})
	if r := in.Status.Verification.Reset; r.By != approver.Login || r.ByID != approver.ID {
		t.Errorf("reset = %+v, want the approver's", r)
	}
	eventually(t, "the preview to carry the reset", func() bool {
		var p v1alpha1.Preview
		return e.cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &p) == nil &&
			p.Spec.ResetGeneration == 1
	})
	// The component restarts for the reset: its Pod template changes.
	eventually(t, "the Deployment's template to carry the reset", func() bool {
		var dep appsv1.Deployment
		key := types.NamespacedName{Namespace: "maul-preview-0", Name: "preview-" + name}
		return e.cl.client.Get(ctx, key, &dep) == nil && len(dep.Spec.Template.Annotations) > 0
	})
	e.serve(t, name, pr.HeadSHA)
	eventually(t, "the preview to observe the reset", func() bool {
		var p v1alpha1.Preview
		return e.cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &p) == nil &&
			p.Status.ObservedResetGeneration == 1
	})
	e.waitVerification(t, name, "the verification after the reset to pass",
		func(v *v1alpha1.IntentVerification) bool {
			return v.State == v1alpha1.VerificationPassed && len(e.kubelet.TesterRuns()) == 2
		})
	if n := len(e.verifications(t, name)); n != 2 {
		t.Errorf("verifications = %d, want 2", n)
	}
}
