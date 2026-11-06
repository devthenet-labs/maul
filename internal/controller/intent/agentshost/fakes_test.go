// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/runnerguard"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The fixtures: one intent of a cli Project, its open pull request, the
// head and base trees, the latest build run on the repository's accepted
// image, and the agents verification the intent reconciler created.
const (
	ns         = "maul"
	agentsNS   = "maul-agents"
	intentName = "render-7"
	pvName     = "render-7-v1-a1"
	repoURL    = "https://github.com/octo/render"
	headSHA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseSHA    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	headDigest = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	baseDigest = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	image      = "ghcr.io/octo/render-agent@sha256:" +
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	toolsImage = "ghcr.io/devthenet-labs/maul/preview-tools@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	searchPath = "/usr/local/go/bin:/usr/bin:/bin"
	buildRun   = "render-7-bld-r1-a1"
	pvUID      = types.UID("pv-uid-1")
	intentUID  = types.UID("intent-uid-1")
	checkBody  = `{"id":"steps.count","outcome":"steps","form":"exec","exec":{"argv":["{{bin}}","--count"],` +
		`"expect":{"exit":0,"stdoutContains":"16 steps"}}}`
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(batchv1.AddToScheme(s))
	utilruntime.Must(v1alpha1.AddToScheme(s))
	return s
}

func outcomes() []v1alpha1.AcceptanceOutcome {
	return []v1alpha1.AcceptanceOutcome{{ID: "steps", Outcome: "the sequencer counts 16 steps",
		Kind: v1alpha1.OutcomeKindChange}}
}

func outcomesDigest(t *testing.T) string {
	t.Helper()
	b, _ := json.Marshal(outcomes())
	var plan []acceptance.PlanOutcome
	if err := json.Unmarshal(b, &plan); err != nil {
		t.Fatal(err)
	}
	return acceptance.OutcomesDigest(plan)
}

func intentOwner() metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Intent", Name: intentName,
		UID: intentUID, Controller: new(true)}
}

func tree(name string, uid types.UID, sha, digest string) *v1alpha1.Repository {
	return &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: uid,
			Labels:          map[string]string{v1alpha1.LabelIntent: intentName},
			OwnerReferences: []metav1.OwnerReference{intentOwner()}},
		Spec: v1alpha1.RepositorySpec{URL: repoURL, Ref: v1alpha1.RepositoryRef{Commit: sha}},
		Status: v1alpha1.RepositoryStatus{ResolvedSHA: sha,
			Conditions: []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Ready", LastTransitionTime: metav1.NewTime(t0)}},
			Artifact: &v1alpha1.Artifact{URL: "http://maul-source.maul.svc:9790/artifacts/" + sha[:8] + ".tar.gz",
				Digest: digest}},
	}
}

// world is the objects a passing request is re-checked against.
type world struct {
	intent     *v1alpha1.Intent
	project    *v1alpha1.Project
	head, base *v1alpha1.Repository
	run        *v1alpha1.IntentRun
	build      *v1alpha1.Repository
	pv         *v1alpha1.PreviewVerification
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{}
	w.project = &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "render", Namespace: ns},
		Spec: v1alpha1.ProjectSpec{
			Repositories: []v1alpha1.ProjectRepository{{Name: "render", URL: repoURL}},
			Surfaces: &v1alpha1.ProjectSurfaces{CLI: &v1alpha1.CLISurface{
				Build: []string{"go", "build", "-o", "/work/bin/bb", "./cmd/bb"}, Binary: "/work/bin/bb"}},
		},
	}
	w.intent = &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: intentName, Namespace: ns, UID: intentUID},
		Spec:       v1alpha1.IntentSpec{Project: "render"},
		Status: v1alpha1.IntentStatus{
			PullRequests: []v1alpha1.IntentPullRequest{{Repository: repoURL, Number: 4, HeadSHA: headSHA,
				State: "open"}},
			Acceptance: &v1alpha1.IntentAcceptance{Outcomes: outcomes(), OutcomesDigest: outcomesDigest(t)},
		},
	}
	w.head = tree(intentName+"-tree-"+headSHA, "head-uid", headSHA, headDigest)
	w.base = tree(intentName+"-tree-"+baseSHA, "base-uid", baseSHA, baseDigest)
	w.run = &v1alpha1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: buildRun, Namespace: ns, UID: "run-uid",
			Labels:            map[string]string{v1alpha1.LabelIntent: intentName},
			OwnerReferences:   []metav1.OwnerReference{intentOwner()},
			CreationTimestamp: metav1.NewTime(t0.Add(-time.Hour))},
		Spec: v1alpha1.IntentRunSpec{IntentRef: v1alpha1.ObjectReference{Name: intentName, UID: intentUID},
			Stage: v1alpha1.IntentStageBuild, Round: 1, Attempt: 1,
			Repository: v1alpha1.IntentRunRepository{URL: repoURL,
				RepositoryRef: v1alpha1.LocalObjectReference{Name: buildRun + "-src"}}},
	}
	w.build = &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: buildRun + "-src", Namespace: ns, UID: "build-repo-uid",
			Labels: map[string]string{v1alpha1.LabelIntent: intentName},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "IntentRun",
				Name: buildRun, UID: "run-uid", Controller: new(true)}}},
		Spec: v1alpha1.RepositorySpec{URL: repoURL},
		Status: v1alpha1.RepositoryStatus{RunnerImage: &v1alpha1.RunnerImage{Image: image, SearchPath: searchPath,
			GoEnv: map[string]string{"GOMODCACHE": "/opt/go/pkg/mod", "GOROOT": "/usr/local/go"}}},
	}
	w.pv = &v1alpha1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{Name: pvName, Namespace: ns, UID: pvUID,
			CreationTimestamp: metav1.NewTime(t0), OwnerReferences: []metav1.OwnerReference{intentOwner()}},
		Spec: v1alpha1.PreviewVerificationSpec{
			Placement: v1alpha1.PlacementAgents,
			IntentRef: v1alpha1.ObjectReference{Name: intentName, UID: intentUID},
			Workspace: &v1alpha1.VerificationWorkspace{
				Repository: "render", HeadSHA: headSHA, BaseSHA: baseSHA,
				HeadRepositoryRef: v1alpha1.ObjectReference{Name: w.head.Name, UID: w.head.UID},
				BaseRepositoryRef: v1alpha1.ObjectReference{Name: w.base.Name, UID: w.base.UID},
				SandboxImage:      image, SandboxSearchPath: searchPath,
				Surface: v1alpha1.WorkspaceSurface{CLI: &v1alpha1.WorkspaceCLI{
					Build: []string{"go", "build", "-o", "/work/bin/bb", "./cmd/bb"}, Binary: "/work/bin/bb",
					BuildTimeout: metav1.Duration{Duration: 10 * time.Minute}}},
			},
			HeadSetDigest: "sha256:" + strings.Repeat("e", 64),
			Heads:         []v1alpha1.VerificationHead{{Component: "render", Repository: repoURL, Revision: headSHA}},
			Round:         1, Attempt: 1, Run: "r4nd0m",
			Outcomes:       outcomes(),
			OutcomesDigest: outcomesDigest(t),
			Rung:           v1alpha1.RungFull,
			Tools:          &v1alpha1.ToolsSpec{Sandbox: true, MaxToolCalls: 80},
			Timeout:        metav1.Duration{Duration: 45 * time.Minute},
		},
		Status: v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationPending},
	}
	return w
}

func (w *world) objects() []client.Object {
	return []client.Object{w.intent, w.project, w.head, w.base, w.run, w.build, w.pv}
}

// fakeTools is the agents namespace: what was created, the Job and pod the
// test sets, the tools container's log, the Secrets' keys.
type fakeTools struct {
	created  []jobs.ToolPodSpec
	env      []corev1.EnvVar
	keys     map[string]string // job -> key
	keyUIDs  map[string]string // job -> verification label
	job      *batchv1.Job
	pod      *corev1.Pod
	log      []byte
	logErr   error
	deleted  []string
	listed   []batchv1.Job
	envErr   error
	createFn func(jobs.ToolPodSpec) error
}

func newFakeTools() *fakeTools {
	return &fakeTools{keys: map[string]string{}, keyUIDs: map[string]string{}, env: []corev1.EnvVar{
		{Name: "HOME", Value: "/work/home"}, {Name: "TMPDIR", Value: "/tmp"}, {Name: "PATH", Value: searchPath},
		{Name: "GOCACHE", Value: "/work/cache/go-build"}, {Name: "NPM_CONFIG_CACHE", Value: "/work/cache/npm"},
		{Name: "GOTOOLCHAIN", Value: "local"}, {Name: "GOPROXY", Value: "off"},
	}}
}

func (f *fakeTools) Create(_ context.Context, spec jobs.ToolPodSpec) error {
	if f.createFn != nil {
		if err := f.createFn(spec); err != nil {
			return err
		}
	}
	f.created = append(f.created, spec)
	f.keys[spec.Name], f.keyUIDs[spec.Name] = spec.Key, spec.Verification
	if f.job == nil {
		f.job = toolJob(spec.Name, types.UID(spec.Verification))
	}
	return nil
}

func (f *fakeTools) SandboxEnv(jobs.ToolPodSpec) ([]corev1.EnvVar, error) {
	return slices.Clone(f.env), f.envErr
}

func (f *fakeTools) Key(_ context.Context, job string) (string, string, bool, error) {
	k, ok := f.keys[job]
	return k, f.keyUIDs[job], ok, nil
}

func (f *fakeTools) Get(_ context.Context, name string) (*batchv1.Job, *corev1.Pod, error) {
	if f.job == nil || f.job.Name != name {
		return nil, nil, nil
	}
	return f.job, f.pod, nil
}

func (f *fakeTools) Log(context.Context, *corev1.Pod) ([]byte, error) { return f.log, f.logErr }

func (f *fakeTools) Delete(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	if f.job != nil && f.job.Name == name {
		f.job, f.pod = nil, nil
	}
	delete(f.keys, name)
	return nil
}

func (f *fakeTools) List(context.Context) ([]batchv1.Job, error) {
	out := slices.Clone(f.listed)
	if f.job != nil {
		out = append(out, *f.job)
	}
	return out, nil
}

func toolJob(name string, uid types.UID) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentsNS,
		CreationTimestamp: metav1.NewTime(t0),
		Labels: map[string]string{v1alpha1.LabelRole: v1alpha1.RoleTools,
			v1alpha1.LabelPreviewVerification: string(uid)}}}
}

// finish marks the fake Job complete at at.
func finish(j *batchv1.Job, at time.Time) {
	j.Status.Conditions = append(j.Status.Conditions, batchv1.JobCondition{Type: batchv1.JobComplete,
		Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)})
}

// readyPod is the tool pod with tools and both sandboxes Ready.
func readyPod() *corev1.Pod {
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: pvName + "-x7k2p", Namespace: agentsNS},
		Status: corev1.PodStatus{PodIP: "10.42.0.17",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "prepare", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "Completed"}}},
				{Name: "sandbox-head", Ready: true, State: running},
				{Name: "sandbox-base", Ready: true, State: running},
			},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "tools", Ready: true, State: running,
				ImageID: "ghcr.io/devthenet-labs/maul/preview-tools@sha256:1111"}},
		},
	}
}

// testWriter sends the reconciler's log to the test's.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// harness is a reconciler over w with tools.
type harness struct {
	r     *Reconciler
	c     client.Client
	tools *fakeTools
	now   time.Time
}

func newHarness(t *testing.T, w *world) *harness {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(w.objects()...).
		WithStatusSubresource(&v1alpha1.PreviewVerification{}, &v1alpha1.Repository{}, &v1alpha1.Intent{}).Build()
	h := &harness{c: c, tools: newFakeTools(), now: t0.Add(time.Minute)}
	h.r = &Reconciler{Client: c, APIReader: c, Tools: h.tools,
		Images: runnerguard.Guard{Enabled: true, Breaker: runnerguard.NewBreaker("test", nil)},
		Settings: Settings{Namespace: ns, ToolsImage: toolsImage, MaxToolPods: 2,
			BuildTimeoutMax: 20 * time.Minute, PollInterval: 30 * time.Second, Harness: "claude"},
		Now: func() time.Time { return h.now }, Log: slog.New(slog.NewTextHandler(testWriter{t}, nil))}
	return h
}

func (h *harness) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: ns, Name: pvName}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (h *harness) pv(t *testing.T) *v1alpha1.PreviewVerification {
	t.Helper()
	var pv v1alpha1.PreviewVerification
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvName}, &pv); err != nil {
		t.Fatal(err)
	}
	return &pv
}

// ledgerOpts shape the tools container's ledger.
type ledgerOpts struct {
	headBuild, baseBuild v1alpha1.BuildResult
	noEvaluator          bool
	extraOutput          string
}

// toolsLog is what a workspace relay writes for a full round: the two
// builds, a replay of the saved check at head and at base, the agent's
// call, the runner's finish, then the summary.
func toolsLog(t *testing.T, o ledgerOpts) ([]byte, string) {
	t.Helper()
	var out bytes.Buffer
	w := toolwire.NewWriter(&out)
	add := func(e toolwire.LedgerEntry) {
		t.Helper()
		e.TS = t0
		if _, err := w.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	build := func(side toolwire.Side, res v1alpha1.BuildResult) {
		exit := 0
		if res == v1alpha1.BuildFailed {
			exit = 2
		}
		args, _ := json.Marshal(previewtools.BuildArgs{Argv: []string{"go", "build"}, TimeoutMs: 600_000,
			Result: res, DurationMs: 4200})
		add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbBuild, Side: side.Recorded(), Args: args,
			Exit: exit, Output: "built"})
	}
	build(toolwire.SideHead, o.headBuild)
	build(toolwire.SideBase, o.baseBuild)
	c, err := checks.Parse([]byte(checkBody))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(checks.Digest(c), "sha256:")
	add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbCheckRun, Args: json.RawMessage(`{}`),
		CheckRun: &toolwire.CheckRunRecord{Digest: digest, CheckID: c.ID, Mode: toolwire.ModeReplay,
			Result: toolwire.RunPass, Attempts: []toolwire.Attempt{{Run: "0123456789abcdef",
				Result: toolwire.ResultPass}}}})
	if o.baseBuild == v1alpha1.BuildBuilt {
		add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbCheckRun, Side: toolwire.SideBase,
			Args: json.RawMessage(`{}`), CheckRun: &toolwire.CheckRunRecord{Digest: digest, CheckID: c.ID,
				Mode: toolwire.ModeReplay, Result: toolwire.RunFail, Attempts: []toolwire.Attempt{
					redAttempt("fedcba9876543210"), redAttempt("fedcba9876543211")}}})
	}
	if !o.noEvaluator {
		add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbExec,
			Args: json.RawMessage(`{"argv":["{{bin}}","--help"]}`), Output: "usage" + o.extraOutput})
		add(toolwire.LedgerEntry{Kind: toolwire.KindRunner, Verb: toolwire.VerbFinish, Args: json.RawMessage(`{}`)})
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), sum.TerminationMessage()
}

// redAttempt is an attempt at base red by assertion at its one step.
func redAttempt(run string) toolwire.Attempt {
	return toolwire.Attempt{Run: run, Result: toolwire.ResultFail, Steps: []toolwire.StepRecord{{Index: 0,
		Result: toolwire.ResultFail, Side: toolwire.SideBase, Assertion: true, AssertionID: "marker steps"}}}
}

// terminated is pod with the tools container terminated with msg.
func terminated(pod *corev1.Pod, msg string) *corev1.Pod {
	pod.Status.ContainerStatuses[0].Ready = false
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: "Completed", Message: msg}}
	return pod
}
