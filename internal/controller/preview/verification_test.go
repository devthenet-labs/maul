// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/previewtester"
)

// fakeLogs serves canned container logs by namespace/pod/container.
type fakeLogs struct {
	logs  map[string][]byte
	reads []LogOptions
}

func (f *fakeLogs) ReadLog(_ context.Context, namespace, pod, container string, opts LogOptions) ([]byte, error) {
	f.reads = append(f.reads, opts)
	b, ok := f.logs[namespace+"/"+pod+"/"+container]
	if !ok {
		return nil, kerrors.NewNotFound(schema.GroupResource{Resource: "pods/log"}, pod)
	}
	if int64(len(b)) > opts.LimitBytes {
		b = b[:opts.LimitBytes]
	}
	return b, nil
}

// statusJSON is an expected HTTP status as the CRD stores it.
func statusJSON(code int) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(fmt.Sprint(code))}
}

// approvedCriteria are an approved plan's criteria: two http (one per
// component) and a manual one.
func approvedCriteria() []v1alpha1.AcceptanceCriterion {
	httpOn := func(id, target, path string) v1alpha1.AcceptanceCriterion {
		return v1alpha1.AcceptanceCriterion{ID: id, Kind: v1alpha1.AcceptanceHTTP, Statement: id + " answers",
			Target: target, Steps: []v1alpha1.AcceptanceStep{{
				Request: v1alpha1.AcceptanceRequest{Method: "GET", Path: path},
				Expect:  v1alpha1.AcceptanceExpect{Status: statusJSON(200)},
			}}}
	}
	return []v1alpha1.AcceptanceCriterion{
		httpOn("items", "api", "/api/items"),
		{ID: "feel", Kind: v1alpha1.AcceptanceManual, Statement: "it feels right", Reason: "taste"},
		httpOn("home", "web", "/"),
	}
}

func toAcceptance(t *testing.T, cs []v1alpha1.AcceptanceCriterion) []acceptance.Criterion {
	t.Helper()
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	var out []acceptance.Criterion
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type verifyEnv struct {
	*testEnv
	v    *VerificationReconciler
	logs *fakeLogs
	in   *v1alpha1.Intent
	p    *v1alpha1.Preview
	pv   *v1alpha1.PreviewVerification
}

// newVerifyEnv is a verification-capable shop Preview Ready in slot 0, its
// Intent's approved criteria, and a PreviewVerification of it as
// intent-controller writes one. funcs plays API server behaviour.
func newVerifyEnv(t *testing.T, funcs interceptor.Funcs) *verifyEnv {
	t.Helper()
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := verifyPreview(t, at)
	criteria := approvedCriteria()
	approved := toAcceptance(t, criteria)
	in.Status.Acceptance = &v1alpha1.IntentAcceptance{PlanRevision: 1, Digest: acceptance.Digest(approved),
		Criteria: criteria}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).
		WithStatusSubresource(&v1alpha1.Preview{}, &v1alpha1.Intent{}, &appsv1.Deployment{}, &corev1.Pod{},
			&v1alpha1.PreviewVerification{}, &batchv1.Job{}).
		WithObjects(verifyProject(), in, p).WithInterceptorFuncs(funcs).Build()
	te := &testEnv{c: c, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), t: t}
	te.r = &Reconciler{Client: c, Settings: verifySettings(), Now: func() time.Time { return te.now }}
	e := &verifyEnv{testEnv: te, logs: &fakeLogs{logs: map[string][]byte{}}, in: in}
	e.v = &VerificationReconciler{Client: c, Settings: verifySettings(), Logs: e.logs,
		Now: func() time.Time { return te.now }}
	e.r.BeforeRelease = e.v.BeforeRelease
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	for i := range p.Spec.Components {
		e.markReady(p, i)
	}
	e.p = e.step(p.Name)
	if e.p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("preview %s, want Ready", e.p.Status.Phase)
	}
	for i := range e.p.Spec.Components {
		e.setClusterIP(i, fmt.Sprintf("10.96.0.%d", 10+i))
	}
	e.pv = e.verification(1, criteria)
	return e
}

// setClusterIP plays the API server allocating component i's Service its
// ClusterIP.
func (e *verifyEnv) setClusterIP(i int, ip string) {
	e.t.Helper()
	var svc corev1.Service
	key := types.NamespacedName{Namespace: "maul-preview-0", Name: v1alpha1.PreviewComponentServiceName(e.p, i)}
	if err := e.c.Get(context.Background(), key, &svc); err != nil {
		e.t.Fatalf("component %d Service: %v", i, err)
	}
	svc.Spec.ClusterIP = ip
	if err := e.c.Update(context.Background(), &svc); err != nil {
		e.t.Fatal(err)
	}
}

// verification creates attempt `attempt` of round 1 of the Preview's
// verification, carrying `criteria` (the approved list) automatable subset.
func (e *verifyEnv) verification(attempt int32, criteria []v1alpha1.AcceptanceCriterion) *v1alpha1.PreviewVerification {
	e.t.Helper()
	var auto []v1alpha1.AcceptanceCriterion
	for _, c := range criteria {
		if c.Kind == v1alpha1.AcceptanceHTTP {
			auto = append(auto, c)
		}
	}
	heads := make([]v1alpha1.VerificationHead, 0, len(e.p.Spec.Components))
	urls := map[string]string{"web": "https://github.com/acme/acme.web_app", "api": "https://github.com/acme/api"}
	for _, c := range e.p.Spec.Components {
		heads = append(heads, v1alpha1.VerificationHead{Component: c.Name, Repository: urls[c.Name], Revision: c.Revision})
	}
	pv := &v1alpha1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PreviewVerificationName(e.in.Name, 1, attempt), Namespace: "maul",
			UID: types.UID(fmt.Sprintf("pv-%d", attempt)), CreationTimestamp: metav1.NewTime(e.now),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "maul.run/v1alpha1", Kind: "Intent",
				Name: e.in.Name, UID: e.in.UID, Controller: ptr(true)}}},
		Spec: v1alpha1.PreviewVerificationSpec{
			IntentRef:  v1alpha1.ObjectReference{Name: e.in.Name, UID: e.in.UID},
			PreviewRef: v1alpha1.ObjectReference{Name: e.p.Name, UID: e.p.UID}, PreviewGeneration: e.p.Generation,
			HeadSetDigest: "sha256:" + strings.Repeat("0", 64), Heads: heads, Round: 1, Attempt: attempt,
			Run: "a1b2c3", Criteria: auto, CriteriaDigest: e.in.Status.Acceptance.Digest,
			Timeout: metav1.Duration{Duration: 5 * time.Minute},
		},
	}
	if err := e.c.Create(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
	return pv
}

// verify reconciles the verification and returns it.
func (e *verifyEnv) verify() (*v1alpha1.PreviewVerification, ctrl.Result) {
	e.t.Helper()
	res, err := e.v.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.pv)})
	if err != nil {
		e.t.Fatalf("reconcile verification: %v", err)
	}
	return e.currentPV(), res
}

func (e *verifyEnv) currentPV() *v1alpha1.PreviewVerification {
	e.t.Helper()
	var pv v1alpha1.PreviewVerification
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(e.pv), &pv); err != nil {
		e.t.Fatal(err)
	}
	return &pv
}

func (e *verifyEnv) jobs() []batchv1.Job {
	e.t.Helper()
	var jobs batchv1.JobList
	if err := e.c.List(context.Background(), &jobs, client.InNamespace("maul-preview-0")); err != nil {
		e.t.Fatal(err)
	}
	return jobs.Items
}

// testerJob is the one tester Job in the slot.
func (e *verifyEnv) testerJob() *batchv1.Job {
	e.t.Helper()
	jobs := e.jobs()
	if len(jobs) != 1 {
		e.t.Fatalf("%d tester Jobs, want 1", len(jobs))
	}
	return &jobs[0]
}

// startTester plays the Job controller creating the tester's Pod and the
// kubelet running it.
func (e *verifyEnv) startTester(job *batchv1.Job) *corev1.Pod {
	e.t.Helper()
	pod := testerPod(job, job.Name+"-x7k2p")
	if err := e.c.Create(context.Background(), pod); err != nil {
		e.t.Fatal(err)
	}
	return pod
}

// finishTester plays the tester writing `log` and exiting with `exit` and
// the termination message `msg`, and the Job controller completing (exit
// 0) or failing the Job.
func (e *verifyEnv) finishTester(job *batchv1.Job, pod *corev1.Pod, log []byte, msg string, exit int32) {
	e.t.Helper()
	ctx := context.Background()
	e.logs.logs[pod.Namespace+"/"+pod.Name+"/"+testerContainer] = log
	reason := "Completed"
	if exit != 0 {
		reason = "Error"
	}
	pod.Status.Phase = corev1.PodSucceeded
	if pod.Status.StartTime == nil {
		pod.Status.StartTime = &metav1.Time{Time: e.now}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: testerContainer,
		ImageID: "ghcr.io/devthenet-labs/maul-preview-tester@sha256:" + strings.Repeat("ab", 32),
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Reason: reason,
			Message: msg, StartedAt: metav1.NewTime(e.now), FinishedAt: metav1.NewTime(e.now.Add(5 * time.Minute))}}}}
	if err := e.c.Status().Update(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	cond := batchv1.JobComplete
	if exit != 0 {
		cond = batchv1.JobFailed
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(e.now)}}
	if err := e.c.Status().Update(ctx, job); err != nil {
		e.t.Fatal(err)
	}
}

// testerWaits are the readiness waits of a run on spec: every target
// reached, in key order, those named in unreached after every attempt
// refused.
func testerWaits(spec previewtester.Spec, unreached ...string) []acceptance.TargetWait {
	waits := make([]acceptance.TargetWait, 0, len(spec.Targets))
	for _, key := range slices.Sorted(maps.Keys(spec.Targets)) {
		w := acceptance.TargetWait{Target: key, WaitMs: 1200, Attempts: 4, Refused: 3}
		if slices.Contains(unreached, key) {
			w = acceptance.TargetWait{Target: key, WaitMs: 60000, Attempts: 66, Refused: 66}
		}
		waits = append(waits, w)
	}
	return waits
}

// newTesterWriter is the tester's wire writer for a run on spec.
func newTesterWriter(t *testing.T, out *bytes.Buffer, waits []acceptance.TargetWait) *acceptance.WireWriter {
	t.Helper()
	w, err := acceptance.NewWireWriter(out, waits)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// unreachableOutput is what the tester writes for spec when the targets
// named never accepted a connection: the summary alone, naming them.
func unreachableOutput(t *testing.T, spec previewtester.Spec, unreached ...string) ([]byte, string) {
	t.Helper()
	var out bytes.Buffer
	sum, err := newTesterWriter(t, &out, testerWaits(spec, unreached...)).Close()
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), sum.TerminationMessage()
}

// testerOutput is what the tester writes for spec: each criterion observed
// against a component answering `status` with body `body`.
func testerOutput(t *testing.T, spec previewtester.Spec, status int32, body string) ([]byte, string) {
	t.Helper()
	var out bytes.Buffer
	w := newTesterWriter(t, &out, testerWaits(spec))
	for _, c := range spec.Criteria {
		obs := acceptance.Observe(c, spec.RunTokens(), func(acceptance.Call) acceptance.Response {
			return acceptance.Response{Status: status, Body: []byte(body)}
		})
		if err := w.Write(obs); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), sum.TerminationMessage()
}

// testerSpec is the spec the tester Job carries.
func testerSpec(t *testing.T, job *batchv1.Job) previewtester.Spec {
	t.Helper()
	env := job.Spec.Template.Spec.Containers[0].Env
	if len(env) != 1 || env[0].Name != previewtester.EnvSpec {
		t.Fatalf("tester env = %+v", env)
	}
	spec, err := previewtester.ParseSpec([]byte(env[0].Value))
	if err != nil {
		t.Fatalf("tester spec refused: %v", err)
	}
	return spec
}

func TestVerificationRendersTheTester(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	pv, res := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationRunning || pv.Status.StartedAt == nil || res.RequeueAfter == 0 {
		t.Fatalf("status %+v, requeue %v; want Running, started, polled", pv.Status, res.RequeueAfter)
	}
	job := e.testerJob()
	if job.Name != testerJobName(e.p, pv) || job.Labels[labelVerificationUID] != string(pv.UID) {
		t.Errorf("job %s labels %v", job.Name, job.Labels)
	}
	spec := testerSpec(t, job)
	wantTargets := map[string]string{"web": "10.96.0.10:80", "api": "10.96.0.11:80"}
	if !maps.Equal(spec.Targets, wantTargets) || spec.Run != "a1b2c3" || spec.Round != 1 || spec.Attempt != 1 {
		t.Errorf("spec = %+v", spec)
	}
	if len(spec.Criteria) != 2 || spec.Criteria[0].ID != "items" || spec.Criteria[1].ID != "home" {
		t.Errorf("criteria = %+v, want the automatable ones in plan order", spec.Criteria)
	}
	// The tester's budget ends before the Job's deadline does, so a hang
	// is recorded and summarised rather than cut off.
	deadline := time.Duration(*job.Spec.Template.Spec.ActiveDeadlineSeconds) * time.Second
	if want := previewtester.BudgetFor(deadline); spec.BudgetSeconds != want {
		t.Errorf("budget %ds, want %ds for a %s deadline", spec.BudgetSeconds, want, deadline)
	}
	if c := job.Spec.Template.Spec.Containers[0]; c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Errorf("termination message policy %s", c.TerminationMessagePolicy)
	}
	// Running again renders nothing new.
	e.verify()
	if n := len(e.jobs()); n != 1 {
		t.Errorf("%d Jobs after a second pass", n)
	}
}

func TestVerificationObserved(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	log, msg := testerOutput(t, testerSpec(t, job), 200, `{"items":[]}`)
	log = append([]byte("starting\n"), log...) // a line without the prefix means nothing
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationObserved || pv.Status.Reason != v1alpha1.VerificationReasonOK {
		t.Fatalf("phase %s %s, want Observed ok", pv.Status.Phase, pv.Status.Reason)
	}
	if len(pv.Status.Observations) != 2 || pv.Status.Observations[0].ID != "items" ||
		pv.Status.Observations[0].Tries[0].Steps[0].Status != 200 {
		t.Errorf("observations = %+v", pv.Status.Observations)
	}
	var sum acceptance.Summary
	_ = json.Unmarshal([]byte(msg), &sum)
	if want, _ := crdconv.WaitsToCRD(sum.Waits); len(want) != 2 || !slices.Equal(pv.Status.Readiness, want) {
		t.Errorf("readiness = %+v, want the summary's %+v", pv.Status.Readiness, sum.Waits)
	}
	if pv.Status.SummarySHA256 != sum.SHA256 || pv.Status.CompletedAt == nil ||
		!strings.Contains(pv.Status.TesterImageID, "@sha256:") {
		t.Errorf("status = %+v", pv.Status)
	}
	parts := map[string]v1alpha1.PartFact{}
	for _, f := range pv.Status.Environment {
		parts[f.Name] = f
	}
	if !parts["web"].Ready || !parts["api"].Ready || parts["tester"].Role != v1alpha1.PreviewRoleTester {
		t.Errorf("environment = %+v", pv.Status.Environment)
	}
	if len(e.jobs()) != 0 {
		t.Error("the settled verification's tester Job is still there")
	}
	// Settled is settled: another pass changes nothing.
	again, _ := e.verify()
	if again.Status.Phase != v1alpha1.VerificationObserved || again.ResourceVersion != pv.ResourceVersion {
		t.Errorf("a settled verification changed: %+v", again.Status)
	}
}

// Every pre-check that fails leaves the verification Stale and renders no
// tester.
func TestVerificationPreChecks(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(e *verifyEnv)
		reason v1alpha1.PreviewVerificationReason
	}{
		{"another preview generation", func(e *verifyEnv) {
			e.p.Generation = 2
			e.mustUpdate(e.p)
		}, v1alpha1.VerificationReasonStalePreview},
		{"another head", func(e *verifyEnv) {
			e.pv.Spec.Heads[0].Revision = strings.Repeat("c", 40)
			e.mustUpdate(e.pv)
		}, v1alpha1.VerificationReasonStalePreview},
		{"a head of another repository", func(e *verifyEnv) {
			e.pv.Spec.Heads[1].Repository = "https://github.com/evil/api"
			e.mustUpdate(e.pv)
		}, v1alpha1.VerificationReasonStalePreview},
		{"another preview", func(e *verifyEnv) {
			e.pv.Spec.PreviewRef.UID = "other"
			e.mustUpdate(e.pv)
		}, v1alpha1.VerificationReasonStalePreview},
		{"preview not ready", func(e *verifyEnv) {
			e.p.Status.Phase = v1alpha1.PreviewDeploying
			e.mustStatus(e.p)
		}, v1alpha1.VerificationReasonStalePreview},
		{"preview environment widened", func(e *verifyEnv) {
			e.p.Spec.Environment.Edges = append(e.p.Spec.Environment.Edges,
				v1alpha1.PreviewEdge{From: "web", To: "api", Port: 8080})
			e.mustUpdate(e.p)
		}, v1alpha1.VerificationReasonStalePreview},
		{"another intent", func(e *verifyEnv) {
			e.pv.Spec.IntentRef.UID = "other"
			e.mustUpdate(e.pv)
		}, v1alpha1.VerificationReasonStaleCriteria},
		{"not owned by the intent", func(e *verifyEnv) {
			e.pv.OwnerReferences = nil
			e.mustUpdate(e.pv)
		}, v1alpha1.VerificationReasonStaleCriteria},
		{"criteria digest of another list", func(e *verifyEnv) {
			e.pv.Spec.CriteriaDigest = "sha256:" + strings.Repeat("1", 64)
			e.mustUpdate(e.pv)
		}, v1alpha1.VerificationReasonStaleCriteria},
		{"criteria not the approved ones", func(e *verifyEnv) {
			e.pv.Spec.Criteria[0].Steps[0].Request.Path = "/api/other"
			e.mustUpdate(e.pv)
		}, v1alpha1.VerificationReasonStaleCriteria},
		{"criteria out of plan order", func(e *verifyEnv) {
			e.pv.Spec.Criteria[0], e.pv.Spec.Criteria[1] = e.pv.Spec.Criteria[1], e.pv.Spec.Criteria[0]
			e.mustUpdate(e.pv)
		}, v1alpha1.VerificationReasonStaleCriteria},
		{"a criterion dropped", func(e *verifyEnv) {
			e.pv.Spec.Criteria = e.pv.Spec.Criteria[:1]
			e.mustUpdate(e.pv)
		}, v1alpha1.VerificationReasonStaleCriteria},
		{"approved criteria changed under their digest", func(e *verifyEnv) {
			e.in.Status.Acceptance.Criteria[0].Statement = "something else"
			e.mustStatus(e.in)
		}, v1alpha1.VerificationReasonStaleCriteria},
		{"intent has no approved criteria", func(e *verifyEnv) {
			e.in.Status.Acceptance = nil
			e.mustStatus(e.in)
		}, v1alpha1.VerificationReasonStaleCriteria},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, interceptor.Funcs{})
			if err := e.c.Get(ctx, client.ObjectKeyFromObject(e.in), e.in); err != nil {
				t.Fatal(err)
			}
			tc.mutate(e)
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != tc.reason {
				t.Errorf("phase %s %s, want Stale %s", pv.Status.Phase, pv.Status.Reason, tc.reason)
			}
			if n := len(e.jobs()); n != 0 {
				t.Errorf("%d tester Jobs rendered", n)
			}
		})
	}
}

func (e *verifyEnv) mustUpdate(obj client.Object) {
	e.t.Helper()
	if err := e.c.Update(context.Background(), obj); err != nil {
		e.t.Fatal(err)
	}
}

func (e *verifyEnv) mustStatus(obj client.Object) {
	e.t.Helper()
	if err := e.c.Status().Update(context.Background(), obj); err != nil {
		e.t.Fatal(err)
	}
}

// A component Pod of an earlier template still running holds the tester
// back, and past the grace the attempt is maul's trouble.
func TestVerificationWaitsForTheRollout(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	var dep appsv1.Deployment
	if err := e.slotObject(componentName(e.p, 1), &dep); err != nil {
		t.Fatal(err)
	}
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: dep.Name + "-old", Namespace: dep.Namespace,
		Labels: maps.Clone(dep.Spec.Template.Labels)}, Spec: *dep.Spec.Template.Spec.DeepCopy()}
	old.Spec.Containers[0].Image = e.p.Spec.Components[1].ImageRepository + ":sha-" + strings.Repeat("d", 40)
	if err := e.c.Create(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	pv, res := e.verify()
	if pv.Status.Phase != "" && pv.Status.Phase != v1alpha1.VerificationPending || res.RequeueAfter == 0 {
		t.Fatalf("phase %s, requeue %v; want waiting", pv.Status.Phase, res.RequeueAfter)
	}
	if len(e.jobs()) != 0 {
		t.Fatal("tester rendered beside an old component Pod")
	}
	e.now = e.now.Add(3 * time.Minute)
	pv, _ = e.verify()
	if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != v1alpha1.VerificationReasonRolloutUnsettled {
		t.Errorf("phase %s %s, want Infra rollout-unsettled", pv.Status.Phase, pv.Status.Reason)
	}
}

// The tester's output is verified line by line against its summary and its
// termination message, and must observe exactly the spec's criteria, in
// order, under the spec's run tokens.
func TestVerificationOutputIsChecked(t *testing.T) {
	type output struct {
		log  []byte
		msg  string
		exit int32
	}
	good := func(t *testing.T, spec previewtester.Spec) output {
		log, msg := testerOutput(t, spec, 200, "{}")
		return output{log, msg, 0}
	}
	type edit func([]acceptance.CriterionObservation) []acceptance.CriterionObservation
	reencode := func(t *testing.T, spec previewtester.Spec, edit edit) output {
		t.Helper()
		obs, _, err := acceptance.ParseWire(good(t, spec).log)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		w := newTesterWriter(t, &out, testerWaits(spec))
		for _, o := range edit(obs) {
			if err := w.Write(o); err != nil {
				t.Fatal(err)
			}
		}
		sum, _ := w.Close()
		return output{out.Bytes(), sum.TerminationMessage(), 0}
	}
	// rewait is good output whose summary records waits instead.
	rewait := func(t *testing.T, spec previewtester.Spec, waits []acceptance.TargetWait) output {
		t.Helper()
		obs, _, err := acceptance.ParseWire(good(t, spec).log)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		w := newTesterWriter(t, &out, waits)
		for _, o := range obs {
			if err := w.Write(o); err != nil {
				t.Fatal(err)
			}
		}
		sum, _ := w.Close()
		return output{out.Bytes(), sum.TerminationMessage(), 0}
	}
	for _, tc := range []struct {
		name   string
		output func(t *testing.T, spec previewtester.Spec) output
		reason v1alpha1.PreviewVerificationReason
	}{
		{"no summary", func(t *testing.T, spec previewtester.Spec) output {
			o := good(t, spec)
			lines := bytes.SplitAfter(o.log, []byte("\n"))
			return output{bytes.Join(lines[:len(lines)-2], nil), "", 1}
		}, v1alpha1.VerificationReasonDeadlineNoSummary},
		{"a line dropped", func(t *testing.T, spec previewtester.Spec) output {
			o := good(t, spec)
			lines := bytes.SplitAfter(o.log, []byte("\n"))
			return output{bytes.Join(append(lines[1:2:2], lines[2:]...), nil), o.msg, 0}
		}, v1alpha1.VerificationReasonSummaryMismatch},
		{"termination message disagrees", func(t *testing.T, spec previewtester.Spec) output {
			o := good(t, spec)
			return output{o.log, acceptance.Summary{Count: 2, SHA256: strings.Repeat("e", 64),
				Waits: testerWaits(spec)}.TerminationMessage(), 0}
		}, v1alpha1.VerificationReasonSummaryMismatch},
		{"termination message's waits disagree", func(t *testing.T, spec previewtester.Spec) output {
			o := good(t, spec)
			_, sum, err := acceptance.ParseWire(o.log)
			if err != nil {
				t.Fatal(err)
			}
			sum.Waits[0].WaitMs++
			return output{o.log, sum.TerminationMessage(), 0}
		}, v1alpha1.VerificationReasonSummaryMismatch},
		{"waits for other targets", func(t *testing.T, spec previewtester.Spec) output {
			return rewait(t, spec, []acceptance.TargetWait{{Target: "admin", Attempts: 1}, {Target: "api", Attempts: 1}})
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
		{"a target's wait missing", func(t *testing.T, spec previewtester.Spec) output {
			return rewait(t, spec, testerWaits(spec)[:1])
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
		{"no termination message", func(t *testing.T, spec previewtester.Spec) output {
			o := good(t, spec)
			return output{o.log, "", 0}
		}, v1alpha1.VerificationReasonSummaryMismatch},
		{"not the wire format", func(*testing.T, previewtester.Spec) output {
			return output{[]byte(acceptance.WirePrefix + "{\"v\":2}\n"), "", 0}
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
		{"an unknown criterion", func(t *testing.T, spec previewtester.Spec) output {
			return reencode(t, spec, func(o []acceptance.CriterionObservation) []acceptance.CriterionObservation {
				o[1].ID = "forged"
				return o
			})
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
		{"a criterion twice", func(t *testing.T, spec previewtester.Spec) output {
			return reencode(t, spec, func(o []acceptance.CriterionObservation) []acceptance.CriterionObservation {
				return []acceptance.CriterionObservation{o[0], o[0]}
			})
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
		{"a criterion missing", func(t *testing.T, spec previewtester.Spec) output {
			return reencode(t, spec, func(o []acceptance.CriterionObservation) []acceptance.CriterionObservation {
				return o[:1]
			})
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
		{"out of order", func(t *testing.T, spec previewtester.Spec) output {
			return reencode(t, spec, func(o []acceptance.CriterionObservation) []acceptance.CriterionObservation {
				return []acceptance.CriterionObservation{o[1], o[0]}
			})
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
		{"another run token", func(t *testing.T, spec previewtester.Spec) output {
			return reencode(t, spec, func(o []acceptance.CriterionObservation) []acceptance.CriterionObservation {
				o[0].Tries[0].Run = "v1a1t1-ffffff"
				return o
			})
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
		{"no try", func(t *testing.T, spec previewtester.Spec) output {
			return reencode(t, spec, func(o []acceptance.CriterionObservation) []acceptance.CriterionObservation {
				o[0].Tries = nil
				return o
			})
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
		{"over the read limit", func(t *testing.T, spec previewtester.Spec) output {
			o := good(t, spec)
			pad := bytes.Repeat([]byte("x"), maxTesterLog)
			return output{append(append(pad, '\n'), o.log...), o.msg, 0}
		}, v1alpha1.VerificationReasonTesterOutputInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, interceptor.Funcs{})
			e.verify()
			job := e.testerJob()
			o := tc.output(t, testerSpec(t, job))
			pod := e.startTester(job)
			e.finishTester(job, pod, o.log, o.msg, o.exit)
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != tc.reason {
				t.Errorf("phase %s %s, want Infra %s", pv.Status.Phase, pv.Status.Reason, tc.reason)
			}
			if len(pv.Status.Observations) != 0 {
				t.Errorf("kept observations of refused output: %+v", pv.Status.Observations)
			}
			if len(e.jobs()) != 0 {
				t.Error("tester Job left behind")
			}
		})
	}
}

// TestVerificationUnreachableIsTheApps: a component's Service accepted no
// connection from the tester for the whole readiness window although it
// was Ready. The window absorbs the slot policy's programming delay (about
// a second on k3s); past it the application is refusing the tester (it can
// filter SYNs by source with a classic BPF socket filter, no capability
// needed, and still pass the kubelet's probe), so the attempt is charged to
// it: EnvFailed component-unreachable, the component's fact marked, never
// maul's Infra and never criterion verdicts.
func TestVerificationUnreachableIsTheApps(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	spec := testerSpec(t, job)
	log, msg := unreachableOutput(t, spec, "api")
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed ||
		pv.Status.Reason != v1alpha1.VerificationReasonComponentUnreachable {
		t.Fatalf("phase %s %s, want EnvFailed component-unreachable", pv.Status.Phase, pv.Status.Reason)
	}
	if len(pv.Status.Observations) != 0 {
		t.Errorf("observations = %+v, want none", pv.Status.Observations)
	}
	want, err := crdconv.WaitsToCRD(testerWaits(spec, "api"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pv.Status.Readiness, want) {
		t.Errorf("readiness = %+v, want %+v", pv.Status.Readiness, want)
	}
	parts := map[string]v1alpha1.PartFact{}
	for _, f := range pv.Status.Environment {
		parts[f.Name] = f
	}
	if api, web := parts["api"], parts["web"]; !api.Ready || api.UnreachableForMs != 60000 ||
		web.UnreachableForMs != 0 {
		t.Errorf("api fact %+v, web fact %+v; want api Ready and unreachable for the 60s window", api, web)
	}
	env := crdconv.PartFacts(pv.Status.Environment)
	if acceptance.ClassifyEnvironment(env) != acceptance.EnvAppFailure ||
		!slices.Equal(acceptance.AppFailureParts(env), []string{"api"}) {
		t.Errorf("environment classifies as %s, failing %v; want the application's, api",
			acceptance.ClassifyEnvironment(env), acceptance.AppFailureParts(env))
	}
	if len(e.jobs()) != 0 {
		t.Errorf("%d jobs left", len(e.jobs()))
	}
}

// TestVerificationUnreachableCrashedIsEnvFailed: when the unreachable
// component is the one that crashed (its listener died), the crash is the
// reason given.
func TestVerificationUnreachableCrashedIsEnvFailed(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	log, msg := unreachableOutput(t, testerSpec(t, job), "api")
	e.crash(1, "CrashLoopBackOff")
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed ||
		pv.Status.Reason != v1alpha1.VerificationReasonComponentCrashLoop {
		t.Fatalf("phase %s %s, want EnvFailed component-crashloop", pv.Status.Phase, pv.Status.Reason)
	}
	if len(pv.Status.Observations) != 0 || len(pv.Status.Readiness) != 2 {
		t.Errorf("observations %+v, readiness %+v", pv.Status.Observations, pv.Status.Readiness)
	}
}

// restartAt plays the api component's container having exited at finished
// and run again since started, coming back Ready, restarts times in all.
// Zero times are left unset.
func (e *verifyEnv) restartAt(restarts int32, finished, started time.Time) {
	e.t.Helper()
	var pod corev1.Pod
	if err := e.slotObject(componentName(e.p, 1)+"-pod", &pod); err != nil {
		e.t.Fatal(err)
	}
	st := &pod.Status.ContainerStatuses[0]
	st.RestartCount = restarts
	st.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2,
		Reason: "Error", FinishedAt: metav1.NewTime(finished)}}
	if !started.IsZero() {
		st.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}}
	}
	if err := e.c.Status().Update(context.Background(), &pod); err != nil {
		e.t.Fatal(err)
	}
}

// at is a timestamp offset from the attempt's start (the tester Pod's).
func at(start time.Time, d time.Duration) metav1.Time { return metav1.NewTime(start.Add(d)) }

// TestPodCause pins the one table that attributes what happened to a Pod of
// a component's current revision during the attempt, reason by reason.
// Only events placed in [tester start - skew, tester end] count, each by a
// condition's transition or a container's own timestamps, never by
// deletionTimestamp. The application is charged only on unambiguous
// evidence: its container terminating non-zero or OOM-killed (crashed), or
// status.reason Evicted (the kubelet's eviction for the memory or storage it
// used). Everything else is the cluster's (disrupted): a drain, preemption,
// the taint manager, Pod GC, a deletion, node loss or NotReady, a graceful
// node shutdown or kubelet preemption, an unknown disruption, a clean exit.
func TestPodCause(t *testing.T) {
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	w := attemptWindow{start: start, end: start.Add(5 * time.Minute)}
	in, before, after := at(start, time.Minute), at(start, -time.Hour), at(start, 10*time.Minute)
	running := func(mod func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-pod", CreationTimestamp: at(start, -time.Hour)},
			Status: corev1.PodStatus{Phase: corev1.PodRunning,
				Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "api", Ready: true}}}}
		mod(p)
		return p
	}
	exited := func(reason string, code int32, when metav1.Time) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].RestartCount = 1
			p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code, FinishedAt: when}}
		}
	}
	disruption := func(reason string, when metav1.Time) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.DisruptionTarget,
				Status: corev1.ConditionTrue, Reason: reason, LastTransitionTime: when})
		}
	}
	notReady := func(status corev1.ConditionStatus, when metav1.Time) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.Conditions[0] = corev1.PodCondition{Type: corev1.PodReady, Status: status, LastTransitionTime: when}
		}
	}
	reason := func(r string) func(*corev1.Pod) {
		return func(p *corev1.Pod) { p.Status.Phase, p.Status.Reason = corev1.PodFailed, r }
	}
	both := func(fs ...func(*corev1.Pod)) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			for _, f := range fs {
				f(p)
			}
		}
	}
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want podCauseKind
	}{
		{"healthy", running(func(*corev1.Pod) {}), causeNone},
		// The application's: unambiguous evidence only.
		{"exited non-zero during", running(exited("Error", 2, in)), causeCrashed},
		{"oom killed during", running(exited("OOMKilled", 137, in)), causeCrashed},
		{"exited within the skew before", running(exited("Error", 2, at(start, -time.Second))), causeCrashed},
		{"evicted by the kubelet", running(both(reason("Evicted"), notReady(corev1.ConditionFalse, in))),
			causeEvicted},
		{"evicted, the kubelet's disruption condition too", running(both(reason("Evicted"),
			disruption("TerminationByKubelet", in))), causeEvicted},
		// Outside the window: nothing.
		{"exited before", running(exited("Error", 2, before)), causeNone},
		{"exited after the tester ended", running(exited("Error", 2, after)), causeNone},
		{"evicted before", running(both(reason("Evicted"), notReady(corev1.ConditionFalse, before))), causeNone},
		{"a drain before", running(disruption("EvictionByEvictionAPI", before)), causeNone},
		// The cluster's.
		{"a clean exit", running(exited("Completed", 0, in)), causeDisrupted},
		{"node NotReady, no reason", running(notReady(corev1.ConditionFalse, in)), causeDisrupted},
		{"node lost", running(notReady(corev1.ConditionUnknown, in)), causeDisrupted},
		{"node lost, the old reason", running(both(reason("NodeLost"), notReady(corev1.ConditionFalse, in))),
			causeDisrupted},
		{"graceful node shutdown", running(both(reason("Terminated"),
			disruption("TerminationByKubelet", in))), causeDisrupted},
		{"kubelet preemption", running(both(reason("Preempting"), disruption("TerminationByKubelet", in))),
			causeDisrupted},
		{"TerminationByKubelet alone", running(disruption("TerminationByKubelet", in)), causeDisrupted},
		{"EvictionByEvictionAPI", running(disruption("EvictionByEvictionAPI", in)), causeDisrupted},
		{"PreemptionByScheduler", running(disruption("PreemptionByScheduler", in)), causeDisrupted},
		{"DeletionByTaintManager", running(disruption("DeletionByTaintManager", in)), causeDisrupted},
		{"DeletionByPodGC", running(disruption("DeletionByPodGC", in)), causeDisrupted},
		{"an unknown disruption", running(disruption("SomethingNew", in)), causeDisrupted},
		{"drained, the container exiting non-zero", running(both(disruption("EvictionByEvictionAPI", in),
			exited("Error", 143, in))), causeDisrupted},
		{"deleted through the API, exiting non-zero", running(both(func(p *corev1.Pod) {
			p.DeletionTimestamp = &metav1.Time{Time: start.Add(time.Hour)}
		}, notReady(corev1.ConditionFalse, in), exited("Error", 143, in))), causeDisrupted},
		// deletionTimestamp is the request time plus the grace period, never
		// when anything happened: it places nothing in time.
		{"deleting, nothing placed in the window", running(func(p *corev1.Pod) {
			p.DeletionTimestamp = &in
		}), causeNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podCause(tc.pod, "api", w); got != tc.want {
				t.Errorf("podCause = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVerificationRestartDuringTheAttempt: a component that exited during
// the attempt, by the kubelet's timestamps against the tester Pod's start
// and end, and came back Ready crashed: EnvFailed component-crashed, the
// change's. A restart from before the attempt, or after the tester ended,
// is not charged.
func TestVerificationRestartDuringTheAttempt(t *testing.T) {
	ref := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) // the tester Pod's start (e.now)
	for _, tc := range []struct {
		name              string
		restarts          int32
		finished, started time.Time
		reason            v1alpha1.PreviewVerificationReason
	}{
		{"crash and recovery during the attempt", 1, ref.Add(30 * time.Second), ref.Add(31 * time.Second),
			v1alpha1.VerificationReasonComponentCrashed},
		{"exited during, not yet seen running", 1, ref.Add(30 * time.Second), time.Time{},
			v1alpha1.VerificationReasonComponentCrashed},
		{"within the skew tolerance", 1, ref.Add(-time.Second), ref.Add(-time.Second),
			v1alpha1.VerificationReasonComponentCrashed},
		{"restart before the attempt", 3, ref.Add(-10 * time.Minute), ref.Add(-10 * time.Minute),
			v1alpha1.VerificationReasonOK},
		{"restart after the tester ended", 1, ref.Add(time.Hour), ref.Add(time.Hour),
			v1alpha1.VerificationReasonOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, interceptor.Funcs{})
			e.verify()
			job := e.testerJob()
			log, msg := testerOutput(t, testerSpec(t, job), 200, `{"items":[]}`)
			e.restartAt(tc.restarts, tc.finished, tc.started)
			pod := e.startTester(job)
			e.finishTester(job, pod, log, msg, 0)
			pv, _ := e.verify()
			if pv.Status.Reason != tc.reason {
				t.Fatalf("phase %s %s, want %s", pv.Status.Phase, pv.Status.Reason, tc.reason)
			}
			for _, f := range pv.Status.Environment {
				if f.Name == "api" && f.CrashedDuring != (tc.reason != v1alpha1.VerificationReasonOK) {
					t.Errorf("api fact %+v", f)
				}
			}
		})
	}
}

// replace plays component 1's Pod being replaced during the attempt by a
// fresh, Ready one created `created` after the tester started, the old
// Pod left as old (nil: gone).
func (e *verifyEnv) replace(created time.Duration, old func(*corev1.Pod)) {
	e.t.Helper()
	ctx := context.Background()
	var cur corev1.Pod
	if err := e.slotObject(componentName(e.p, 1)+"-pod", &cur); err != nil {
		e.t.Fatal(err)
	}
	fresh := cur.DeepCopy()
	fresh.ObjectMeta = metav1.ObjectMeta{Name: cur.Name + "-2", Namespace: cur.Namespace, Labels: cur.Labels,
		Annotations: cur.Annotations, CreationTimestamp: metav1.NewTime(e.now.Add(created))}
	if old == nil {
		if err := e.c.Delete(ctx, &cur); err != nil {
			e.t.Fatal(err)
		}
	} else {
		old(&cur)
		if err := e.c.Status().Update(ctx, &cur); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.c.Create(ctx, fresh); err != nil {
		e.t.Fatal(err)
	}
	fresh.Status = corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "api", Ready: true}}}
	if err := e.c.Status().Update(ctx, fresh); err != nil {
		e.t.Fatal(err)
	}
}

// TestVerificationReplacedDuringTheAttempt: a component Pod replaced during
// the attempt is attributed by the replaced Pod's evidence. status.reason
// Evicted is the kubelet's eviction, the application's: EnvFailed
// component-evicted. A kubelet disruption without it (graceful node
// shutdown), a drain, or a replacement whose old Pod is gone is the
// cluster's: Infra component-disrupted, retried. A Pod served a moment
// before the tester started is the one it ran against, never a replacement.
func TestVerificationReplacedDuringTheAttempt(t *testing.T) {
	during := metav1.NewTime(time.Date(2026, 10, 1, 12, 0, 35, 0, time.UTC))
	failed := func(reason string, cond *corev1.PodCondition) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.Phase, p.Status.Reason = corev1.PodFailed, reason
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse,
				LastTransitionTime: during}}
			if cond != nil {
				p.Status.Conditions = append(p.Status.Conditions, *cond)
			}
		}
	}
	kubelet := &corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
		Reason: "TerminationByKubelet", LastTransitionTime: during}
	for _, tc := range []struct {
		name    string
		created time.Duration
		old     func(*corev1.Pod)
		phase   v1alpha1.PreviewVerificationPhase
		reason  v1alpha1.PreviewVerificationReason
	}{
		{"evicted by the kubelet", 40 * time.Second, failed("Evicted", nil),
			v1alpha1.VerificationEnvFailed, v1alpha1.VerificationReasonComponentEvicted},
		{"evicted, with the kubelet's condition", 40 * time.Second, failed("Evicted", kubelet),
			v1alpha1.VerificationEnvFailed, v1alpha1.VerificationReasonComponentEvicted},
		{"graceful node shutdown", 40 * time.Second, failed("Terminated", kubelet),
			v1alpha1.VerificationInfra, v1alpha1.VerificationReasonComponentDisrupted},
		{"drained", 40 * time.Second, failed("", &corev1.PodCondition{Type: corev1.DisruptionTarget,
			Status: corev1.ConditionTrue, Reason: "EvictionByEvictionAPI", LastTransitionTime: during}),
			v1alpha1.VerificationInfra, v1alpha1.VerificationReasonComponentDisrupted},
		{"old Pod gone", 40 * time.Second, nil,
			v1alpha1.VerificationInfra, v1alpha1.VerificationReasonComponentDisrupted},
		{"served a second before the tester", -time.Second, nil,
			v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK},
		{"served within the skew after", time.Second, nil,
			v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, interceptor.Funcs{})
			e.verify()
			job := e.testerJob()
			log, msg := testerOutput(t, testerSpec(t, job), 200, `{"items":[]}`)
			e.replace(tc.created, tc.old)
			pod := e.startTester(job)
			e.finishTester(job, pod, log, msg, 0)
			pv, _ := e.verify()
			if pv.Status.Phase != tc.phase || pv.Status.Reason != tc.reason {
				t.Fatalf("phase %s %s, want %s %s", pv.Status.Phase, pv.Status.Reason, tc.phase, tc.reason)
			}
		})
	}
}

// oldRevisionPod plays a Pod of component 1's previous revision (another
// image) still in the slot, mod setting its state.
func (e *verifyEnv) oldRevisionPod(mod func(*corev1.Pod)) {
	e.t.Helper()
	ctx := context.Background()
	var cur corev1.Pod
	if err := e.slotObject(componentName(e.p, 1)+"-pod", &cur); err != nil {
		e.t.Fatal(err)
	}
	old := cur.DeepCopy()
	old.ObjectMeta = metav1.ObjectMeta{Name: cur.Name + "-prev", Namespace: cur.Namespace, Labels: cur.Labels,
		Annotations: cur.Annotations, CreationTimestamp: metav1.NewTime(e.now.Add(-time.Hour))}
	old.Spec.Containers[0].Image += "-previous"
	if err := e.c.Create(ctx, old); err != nil {
		e.t.Fatal(err)
	}
	mod(old)
	if err := e.c.Status().Update(ctx, old); err != nil {
		e.t.Fatal(err)
	}
}

// TestVerificationOnlyTheCurrentRevisionCounts: only Pods of the
// component's current revision (its spec's image) are judged. A previous
// revision's Pod being evicted or terminating during the attempt is
// ignored, so it can neither charge the change nor mask a crash: a crash
// of the current Pod while an old one terminates is the application's.
func TestVerificationOnlyTheCurrentRevisionCounts(t *testing.T) {
	during := metav1.NewTime(time.Date(2026, 10, 1, 12, 0, 35, 0, time.UTC))
	terminating := func(p *corev1.Pod) {
		p.Status.Phase, p.Status.Reason = corev1.PodFailed, "Evicted"
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse,
			LastTransitionTime: during}, {Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
			Reason: "EvictionByEvictionAPI", LastTransitionTime: during}}
	}
	for _, tc := range []struct {
		name   string
		crash  bool
		phase  v1alpha1.PreviewVerificationPhase
		reason v1alpha1.PreviewVerificationReason
	}{
		{"an old revision's events are ignored", false, v1alpha1.VerificationObserved,
			v1alpha1.VerificationReasonOK},
		{"a crash while an old revision terminates", true, v1alpha1.VerificationEnvFailed,
			v1alpha1.VerificationReasonComponentCrashed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, interceptor.Funcs{})
			e.verify()
			job := e.testerJob()
			log, msg := testerOutput(t, testerSpec(t, job), 200, `{"items":[]}`)
			e.oldRevisionPod(terminating)
			if tc.crash {
				e.restartAt(1, e.now.Add(30*time.Second), e.now.Add(31*time.Second))
			}
			pod := e.startTester(job)
			e.finishTester(job, pod, log, msg, 0)
			pv, _ := e.verify()
			if pv.Status.Phase != tc.phase || pv.Status.Reason != tc.reason {
				t.Fatalf("phase %s %s, want %s %s", pv.Status.Phase, pv.Status.Reason, tc.phase, tc.reason)
			}
		})
	}
}

// TestVerificationNotReadyWithoutEvidenceIsInfra: a component that went
// not-Ready during the attempt with no evidence of the application's (a
// node going NotReady sets Ready False with no reason) is the cluster's:
// Infra component-disrupted, retried and bounded by the attempts, never a
// verdict. The application can at most make itself not-Ready without
// crashing, which earns it an honest Infra and then Unavailable, never a
// pass; and it is unreachable then too, but the not-Ready cause wins over
// component-unreachable.
func TestVerificationNotReadyWithoutEvidenceIsInfra(t *testing.T) {
	for _, unreachable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unreachable %t", unreachable), func(t *testing.T) {
			e := newVerifyEnv(t, interceptor.Funcs{})
			e.verify()
			job := e.testerJob()
			spec := testerSpec(t, job)
			log, msg := testerOutput(t, spec, 200, `{"items":[]}`)
			if unreachable {
				log, msg = unreachableOutput(t, spec, "api")
			}
			var pod corev1.Pod
			if err := e.slotObject(componentName(e.p, 1)+"-pod", &pod); err != nil {
				t.Fatal(err)
			}
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(e.now.Add(20 * time.Second))}}
			pod.Status.ContainerStatuses[0].Ready = false
			if err := e.c.Status().Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
			tester := e.startTester(job)
			e.finishTester(job, tester, log, msg, 0)
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationInfra ||
				pv.Status.Reason != v1alpha1.VerificationReasonComponentDisrupted {
				t.Fatalf("phase %s %s, want Infra component-disrupted", pv.Status.Phase, pv.Status.Reason)
			}
		})
	}
}

// TestVerificationEvictedBeforeTheAttemptIsNotCharged: a Failed Pod left
// over from an eviction before the attempt, its replacement serving since
// before the tester started, is history, not this attempt's failure.
func TestVerificationEvictedBeforeTheAttemptIsNotCharged(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	ctx := context.Background()
	var cur corev1.Pod
	if err := e.slotObject(componentName(e.p, 1)+"-pod", &cur); err != nil {
		t.Fatal(err)
	}
	gone := cur.DeepCopy()
	gone.ObjectMeta = metav1.ObjectMeta{Name: cur.Name + "-old", Namespace: cur.Namespace, Labels: cur.Labels,
		Annotations: cur.Annotations, CreationTimestamp: metav1.NewTime(e.now.Add(-2 * time.Hour))}
	if err := e.c.Create(ctx, gone); err != nil {
		t.Fatal(err)
	}
	gone.Status = corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted",
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(e.now.Add(-time.Hour))}}}
	if err := e.c.Status().Update(ctx, gone); err != nil {
		t.Fatal(err)
	}
	e.verify()
	job := e.testerJob()
	log, msg := testerOutput(t, testerSpec(t, job), 200, `{"items":[]}`)
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationObserved {
		t.Fatalf("phase %s %s, want Observed", pv.Status.Phase, pv.Status.Reason)
	}
}

// TestVerificationStartReenteredHasNoEffect: a second start pass once the
// tester Job exists (its create answered AlreadyExists) changes nothing
// about how the attempt settles.
func TestVerificationStartReenteredHasNoEffect(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	log, msg := testerOutput(t, testerSpec(t, job), 200, `{"items":[]}`)
	e.restartAt(2, e.now.Add(-time.Hour), e.now.Add(-time.Hour))
	pv := e.currentPV()
	pv.Status.Phase = v1alpha1.VerificationPending
	if err := e.c.Status().Update(context.Background(), pv); err != nil {
		t.Fatal(err)
	}
	if pv, _ = e.verify(); pv.Status.Phase != v1alpha1.VerificationRunning || len(e.jobs()) != 1 {
		t.Fatalf("phase %s with %d jobs after the second start", pv.Status.Phase, len(e.jobs()))
	}
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	if pv, _ = e.verify(); pv.Status.Phase != v1alpha1.VerificationObserved ||
		pv.Status.Reason != v1alpha1.VerificationReasonOK {
		t.Fatalf("phase %s %s, want Observed ok", pv.Status.Phase, pv.Status.Reason)
	}
}

// TestVerificationRunningAcrossTheUpgrade: an attempt an older
// preview-controller started (0.13.2 and 0.13.3 rendered the tester's
// targets as Service names) is found by this one on its first pass: its
// tester Job exists, the verification is Running with only startedAt in its
// status. A spec of Service-name targets is recognised as the old format
// and the attempt settles stale-preview, to be run again, never
// tester-output-invalid.
func TestVerificationRunningAcrossTheUpgrade(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	ctx := context.Background()
	pv := e.currentPV()
	s, reason, err := e.v.check(ctx, pv)
	if err != nil || reason != "" {
		t.Fatalf("check = %q, %v", reason, err)
	}
	job, reason, err := e.v.renderTester(ctx, pv, s)
	if err != nil || reason != "" {
		t.Fatalf("renderTester = %q, %v", reason, err)
	}
	// The spec as 0.13.3 rendered it: each target the Service's name.
	spec := testerSpec(t, job)
	spec.Targets = map[string]string{}
	for i, c := range e.p.Spec.Components {
		spec.Targets[c.Name] = v1alpha1.PreviewComponentServiceName(e.p, i) + ":80"
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	job.Spec.Template.Spec.Containers[0].Env[0].Value = string(raw)
	if err := e.c.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	started := metav1.NewTime(e.now)
	pv.Status = v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationRunning, StartedAt: &started}
	if err := e.c.Status().Update(ctx, pv); err != nil {
		t.Fatal(err)
	}
	e.restartAt(5, e.now.Add(-time.Hour), e.now.Add(-time.Hour))
	job = e.testerJob()
	sha := strings.Repeat("0", 64)
	log := []byte(acceptance.WirePrefix + `{"v":1,"summary":{"count":0,"sha256":"` + sha + `"}}` + "\n")
	pod := e.startTester(job)
	e.finishTester(job, pod, log, `{"count":0,"sha256":"`+sha+`"}`, 0)
	pv, _ = e.verify() // the reconciler's first pass
	if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != v1alpha1.VerificationReasonStalePreview {
		t.Fatalf("phase %s %s, want Stale stale-preview", pv.Status.Phase, pv.Status.Reason)
	}
	if _, err := previewtester.ParseSpec(raw); !errors.Is(err, previewtester.ErrNameTarget) {
		t.Errorf("ParseSpec(0.13.3 spec) = %v, want ErrNameTarget", err)
	}
}

// TestVerificationTargetsAreClusterIPs: the tester is rendered each
// component Service's ClusterIP, read live, never a name it would have to
// resolve; a Service with none is maul's render error, settled Infra
// before any tester starts.
func TestVerificationTargetsAreClusterIPs(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	spec := testerSpec(t, e.testerJob())
	if want := map[string]string{"web": "10.96.0.10:80", "api": "10.96.0.11:80"}; !maps.Equal(spec.Targets, want) {
		t.Errorf("targets = %v, want %v", spec.Targets, want)
	}
	for _, ip := range []string{"", corev1.ClusterIPNone} {
		t.Run("clusterIP "+ip, func(t *testing.T) {
			e := newVerifyEnv(t, interceptor.Funcs{})
			e.setClusterIP(1, ip)
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationInfra ||
				pv.Status.Reason != v1alpha1.VerificationReasonServiceNoClusterIP {
				t.Fatalf("phase %s %s, want Infra service-no-cluster-ip", pv.Status.Phase, pv.Status.Reason)
			}
			if len(e.jobs()) != 0 {
				t.Error("a tester was rendered with no address for a component")
			}
		})
	}
}

// TestVerificationUnreachableWithDNSFailures: the tester resolves nothing,
// so a recorded DNS failure is no excuse: a target never reached, however
// its attempts failed, is the application's component-unreachable.
func TestVerificationUnreachableWithDNSFailures(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	var out bytes.Buffer
	sum, err := newTesterWriter(t, &out, []acceptance.TargetWait{
		{Target: "api", WaitMs: 60000, Attempts: 66, DNS: 66},
		{Target: "web", WaitMs: 3, Attempts: 1}}).Close()
	if err != nil {
		t.Fatal(err)
	}
	pod := e.startTester(job)
	e.finishTester(job, pod, out.Bytes(), sum.TerminationMessage(), 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed ||
		pv.Status.Reason != v1alpha1.VerificationReasonComponentUnreachable {
		t.Fatalf("phase %s %s, want EnvFailed component-unreachable", pv.Status.Phase, pv.Status.Reason)
	}
}

// TestVerificationTesterVersionMismatch: a tester image older than the
// controller (mirrored, or overridden) writes another wire version. That is
// maul's configuration, never the change's: Infra tester-version-mismatch,
// the tester's version recorded beside the controller's, not output that is
// merely invalid.
func TestVerificationTesterVersionMismatch(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	sha := strings.Repeat("0", 64)
	log := []byte(acceptance.WirePrefix + `{"v":1,"observation":{"id":"items","tries":[{"run":"v1a1t1-a1b2c3"}]}}` +
		"\n" + acceptance.WirePrefix + `{"v":1,"summary":{"count":1,"sha256":"` + sha + `"}}` + "\n")
	pod := e.startTester(job)
	e.finishTester(job, pod, log, `{"count":1,"sha256":"`+sha+`"}`, 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationInfra ||
		pv.Status.Reason != v1alpha1.VerificationReasonTesterVersionMismatch {
		t.Fatalf("phase %s %s, want Infra tester-version-mismatch", pv.Status.Phase, pv.Status.Reason)
	}
	if pv.Status.TesterWireVersion != 1 || len(pv.Status.Observations) != 0 {
		t.Errorf("tester wire version %d, observations %+v", pv.Status.TesterWireVersion, pv.Status.Observations)
	}
}

// TestVerificationTargetsFromTheRenderedSpec: the targets a tester's waits
// are held to are those its Job was rendered with, read back from the Job,
// never re-derived from the Preview as it is now. A rendered spec that no
// longer matches the Preview means the Preview changed under the attempt:
// stale-preview, not tester-output-invalid.
func TestVerificationTargetsFromTheRenderedSpec(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	spec := testerSpec(t, job)
	log, msg := testerOutput(t, spec, 200, `{"items":[]}`)
	// The Job as rendered names another Service for web than the Preview
	// would now.
	spec.Targets["web"] = "10.96.0.99:80"
	raw, err := spec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	job.Spec.Template.Spec.Containers[0].Env[0].Value = string(raw)
	if err := e.c.Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != v1alpha1.VerificationReasonStalePreview {
		t.Fatalf("phase %s %s, want Stale stale-preview", pv.Status.Phase, pv.Status.Reason)
	}
}

// TestVerificationServiceReaddressedIsStale: a component Service whose
// ClusterIP changed after the tester was rendered (recreated) means the
// tester spoke to another address than the Service has: stale-preview.
func TestVerificationServiceReaddressedIsStale(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	log, msg := testerOutput(t, testerSpec(t, job), 200, `{"items":[]}`)
	e.setClusterIP(0, "10.96.0.77")
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != v1alpha1.VerificationReasonStalePreview {
		t.Fatalf("phase %s %s, want Stale stale-preview", pv.Status.Phase, pv.Status.Reason)
	}
}

// TestVerificationPreviewChangedIsStale: a Preview whose components changed
// while its tester ran settles the attempt stale-preview.
func TestVerificationPreviewChangedIsStale(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	log, msg := testerOutput(t, testerSpec(t, job), 200, `{"items":[]}`)
	var p v1alpha1.Preview
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(e.p), &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components = p.Spec.Components[:1]
	if err := e.c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != v1alpha1.VerificationReasonStalePreview {
		t.Fatalf("phase %s %s, want Stale stale-preview", pv.Status.Phase, pv.Status.Reason)
	}
}

// The status stays under its size cap: body prefixes go first, pointer
// values never.
func TestVerificationStatusIsCapped(t *testing.T) {
	pv := oversizedVerification(120)
	fitStatus(pv)
	if b, _ := json.Marshal(pv); len(b) > maxVerificationObject {
		t.Fatalf("status is %d bytes, over %d", len(b), maxVerificationObject)
	}
	if pv.Status.Phase != v1alpha1.VerificationObserved {
		t.Errorf("phase %s: the observations fit once body prefixes went", pv.Status.Phase)
	}
	kept, pointers := 0, 0
	eachStep(pv, func(s *v1alpha1.StepObservation) {
		pointers += len(s.Pointers)
		if s.BodyPrefix != "" {
			kept++
		}
	})
	if pointers != 16*2*8*4 {
		t.Errorf("%d pointer values kept, want every one", pointers)
	}
	if pv.Status.Observations[0].Tries[0].Steps[0].BodyPrefix == "" {
		t.Error("dropped the first body prefix before the last ones")
	}
	if kept == 0 || kept == 16*2*8 {
		t.Errorf("%d body prefixes kept, want some dropped", kept)
	}
	// Past what dropping the display values can save, the largest
	// criteria's observations become markers one at a time, never the
	// whole result: the values are the application's, so is the failure.
	pv = oversizedVerification(250)
	fitStatus(pv)
	if pv.Status.Phase != v1alpha1.VerificationObserved || len(pv.Status.Observations) != 16 {
		t.Fatalf("oversized status %s %s with %d observations, want Observed with all 16",
			pv.Status.Phase, pv.Status.Reason, len(pv.Status.Observations))
	}
	if b, _ := json.Marshal(pv); len(b) > maxVerificationObject {
		t.Fatalf("status is %d bytes, over %d", len(b), maxVerificationObject)
	}
	markers := 0
	for _, o := range pv.Status.Observations {
		if len(o.Tries) == 0 {
			markers++
		}
	}
	if markers == 0 || markers == 16 {
		t.Errorf("%d observations replaced by markers, want some and not all", markers)
	}
}

// An application that answers its criteria with large values ($present
// pointers it fills with HTML-escaped text) and then fails each one cannot
// push its failure out of the status as maul's Infra: values whose
// judgement cannot change go to the TooLarge sentinel, every criterion's
// verdict stays exactly what it was, and the result is still Observed.
func TestVerificationStatusKeepsAppInflatedFailure(t *testing.T) {
	pv := appInflatedVerification(t)
	rt := acceptance.RunTokens{Round: pv.Spec.Round, Attempt: pv.Spec.Attempt, Run: pv.Spec.Run}
	if b, _ := json.Marshal(pv); len(b) <= maxVerificationObject {
		t.Fatalf("fixture is %d bytes, not over %d", len(b), maxVerificationObject)
	}
	before := judgeAll(t, pv, rt)
	fitStatus(pv)
	if pv.Status.Phase != v1alpha1.VerificationObserved || pv.Status.Reason != v1alpha1.VerificationReasonOK {
		t.Fatalf("phase %s %s, want Observed ok: the app's failure is the intent's", pv.Status.Phase,
			pv.Status.Reason)
	}
	if b, _ := json.Marshal(pv); len(b) > maxVerificationObject {
		t.Fatalf("status is %d bytes, over %d", len(b), maxVerificationObject)
	}
	after := judgeAll(t, pv, rt)
	for i := range before {
		if before[i].Outcome != acceptance.OutcomeFail {
			t.Fatalf("fixture criterion %s judged %s, want fail", before[i].ID, before[i].Outcome)
		}
		bb, _ := json.Marshal(before[i])
		ab, _ := json.Marshal(after[i])
		if !bytes.Equal(bb, ab) {
			t.Errorf("criterion %s judged %s after fitting, %s before", before[i].ID, ab, bb)
		}
	}
}

// judgeAll judges every criterion of pv against its observations.
func judgeAll(t *testing.T, pv *v1alpha1.PreviewVerification, rt acceptance.RunTokens) []acceptance.Verdict {
	t.Helper()
	cs, err := crdconv.CriteriaFromCRD(pv.Spec.Criteria)
	if err != nil || len(cs) != len(pv.Status.Observations) {
		t.Fatalf("criteria: %v (%d for %d observations)", err, len(cs), len(pv.Status.Observations))
	}
	out := make([]acceptance.Verdict, len(cs))
	for i, c := range cs {
		o, err := crdconv.ObservationFromCRD(pv.Status.Observations[i])
		if err != nil {
			t.Fatal(err)
		}
		out[i] = acceptance.Judge(c, o, rt)
	}
	return out
}

// appInflatedVerification is an Observed verification of 16 criteria of
// three steps, each step checking two $present pointers, whose application
// filled every pointer with 250 '<' characters and answered every last step
// 500, so each criterion records two failed tries.
func appInflatedVerification(t *testing.T) *v1alpha1.PreviewVerification {
	t.Helper()
	present := apiextensionsv1.JSON{Raw: []byte(`{"$present":true}`)}
	ok := apiextensionsv1.JSON{Raw: []byte("200")}
	big := `"` + strings.Repeat("<", 250) + `"`
	criteria := make([]v1alpha1.AcceptanceCriterion, 16)
	obs := make([]v1alpha1.CriterionObservation, 16)
	for i := range criteria {
		id := fmt.Sprintf("c%d", i)
		criteria[i] = v1alpha1.AcceptanceCriterion{ID: id, Kind: v1alpha1.AcceptanceHTTP,
			Statement: "the page answers", Target: "api"}
		for range 3 {
			criteria[i].Steps = append(criteria[i].Steps, v1alpha1.AcceptanceStep{
				Request: v1alpha1.AcceptanceRequest{Method: "GET", Path: "/x"},
				Expect: v1alpha1.AcceptanceExpect{Status: &ok,
					JSON: map[string]apiextensionsv1.JSON{"/a": present, "/b": present}},
			})
		}
		obs[i].ID = id
		for try := range 2 {
			steps := make([]v1alpha1.StepObservation, 3)
			for s := range steps {
				status := int32(200)
				if s == 2 {
					status = 500
				}
				steps[s] = v1alpha1.StepObservation{Method: "GET", Path: "/x", Status: status,
					TransportError: v1alpha1.TransportNone, JSONValid: true,
					Pointers: map[string]string{"/a": big, "/b": big}}
			}
			obs[i].Tries = append(obs[i].Tries,
				v1alpha1.TryObservation{Run: fmt.Sprintf("v1a1t%d-a1b2c3", try+1), Steps: steps})
		}
	}
	return &v1alpha1.PreviewVerification{
		Spec: v1alpha1.PreviewVerificationSpec{Round: 1, Attempt: 1, Run: "a1b2c3", Criteria: criteria},
		Status: v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationObserved,
			Reason: v1alpha1.VerificationReasonOK, Observations: obs}}
}

// oversizedVerification is an Observed verification at every bound: 16
// criteria, two tries of eight steps each, every step with four pointer
// values of `value` characters and a full body prefix, and a full log tail.
func oversizedVerification(value int) *v1alpha1.PreviewVerification {
	obs := make([]v1alpha1.CriterionObservation, 16)
	for i := range obs {
		obs[i].ID = fmt.Sprintf("c%d", i)
		for try := range 2 {
			steps := make([]v1alpha1.StepObservation, 8)
			for s := range steps {
				pointers := map[string]string{}
				for k := range 4 {
					pointers[fmt.Sprintf("/k%d", k)] = `"` + strings.Repeat("p", value) + `"`
				}
				steps[s] = v1alpha1.StepObservation{Method: "GET", Path: "/x", Status: 200,
					BodyPrefix: strings.Repeat("QUFB", 171), Pointers: pointers}
			}
			obs[i].Tries = append(obs[i].Tries,
				v1alpha1.TryObservation{Run: fmt.Sprintf("v1a1t%d-a1b2c3", try+1), Steps: steps})
		}
	}
	tail := []v1alpha1.PartFact{{Name: "web", Role: v1alpha1.PreviewRoleComponent, LogTail: strings.Repeat("l", 4096)}}
	return &v1alpha1.PreviewVerification{Status: v1alpha1.PreviewVerificationStatus{
		Phase: v1alpha1.VerificationObserved, Reason: v1alpha1.VerificationReasonOK, Observations: obs,
		Environment: tail}}
}

// eachStep calls f with every step observation of pv.
func eachStep(pv *v1alpha1.PreviewVerification, f func(*v1alpha1.StepObservation)) {
	for i := range pv.Status.Observations {
		for j := range pv.Status.Observations[i].Tries {
			for k := range pv.Status.Observations[i].Tries[j].Steps {
				f(&pv.Status.Observations[i].Tries[j].Steps[k])
			}
		}
	}
}

// A component that crashed during the run fails the environment: the
// verification is EnvFailed, charged to the intent, with the evidence and
// the tester's observations.
func TestVerificationComponentCrashIsEnvFailed(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	job := e.testerJob()
	log, msg := testerOutput(t, testerSpec(t, job), 503, "")
	e.crash(1, "CrashLoopBackOff")
	e.logs.logs["maul-preview-0/"+componentName(e.p, 1)+"-pod/api"] = []byte("panic: nil map\n")
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed ||
		pv.Status.Reason != v1alpha1.VerificationReasonComponentCrashLoop {
		t.Fatalf("phase %s %s, want EnvFailed component-crashloop", pv.Status.Phase, pv.Status.Reason)
	}
	var api v1alpha1.PartFact
	for _, f := range pv.Status.Environment {
		if f.Name == "api" {
			api = f
		}
	}
	if api.Ready || api.Waiting != "CrashLoopBackOff" || api.Restarts != 4 || api.LogTail != "panic: nil map\n" {
		t.Errorf("api fact = %+v", api)
	}
	if len(pv.Status.Observations) != 2 {
		t.Errorf("observations = %+v", pv.Status.Observations)
	}
	for _, r := range e.logs.reads {
		if r.LimitBytes == componentLogLimit && (r.TailLines == nil || *r.TailLines != componentLogLines ||
			r.SinceTime == nil) {
			t.Errorf("component log read %+v, want a bounded tail since the start", r)
		}
	}
}

// crash plays component i's current Pod crashing.
func (e *verifyEnv) crash(i int, reason string) {
	e.t.Helper()
	var pod corev1.Pod
	if err := e.slotObject(componentName(e.p, i)+"-pod", &pod); err != nil {
		e.t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	last := &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: e.p.Spec.Components[i].Name, RestartCount: 4,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		LastTerminationState: corev1.ContainerState{Terminated: last}}}
	if err := e.c.Status().Update(context.Background(), &pod); err != nil {
		e.t.Fatal(err)
	}
}

// A Preview released while its verification runs (a retry, a failure)
// records what the slot showed first, through the preview reconciler's
// BeforeRelease hook, while the Pods can still be seen.
func TestVerificationEnvFailedBeforeRelease(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	e.startTester(e.testerJob())
	e.crash(0, "ImagePullBackOff")
	p := e.step(e.p.Name)
	p.Status.Phase = v1alpha1.PreviewFailed
	e.mustStatus(p)
	e.step(p.Name)
	pv := e.currentPV()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed ||
		pv.Status.Reason != v1alpha1.VerificationReasonComponentImagePull {
		t.Fatalf("phase %s %s, want EnvFailed component-image-pull", pv.Status.Phase, pv.Status.Reason)
	}
	if len(pv.Status.Environment) == 0 || pv.Status.CompletedAt == nil {
		t.Errorf("status = %+v", pv.Status)
	}
	// Idempotent: the hook runs again at each poll cleanup waits.
	if err := e.v.BeforeRelease(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if again := e.currentPV(); again.ResourceVersion != pv.ResourceVersion {
		t.Error("the hook rewrote a settled verification")
	}
}

// A Running verification whose Preview has failed, the hook having missed
// it, is captured by the verification's own pass; with no part failing it
// is preview-failed.
func TestVerificationOfAFailedPreview(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	e.p.Status.Phase = v1alpha1.PreviewFailed
	e.mustStatus(e.p)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed || pv.Status.Reason != v1alpha1.VerificationReasonPreviewFailed {
		t.Errorf("phase %s %s, want EnvFailed preview-failed", pv.Status.Phase, pv.Status.Reason)
	}
	if len(e.jobs()) != 0 {
		t.Error("tester Job left behind")
	}
}

// What keeps the tester from running is maul's, bounded by a grace.
func TestVerificationTesterTrouble(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pod    func(e *verifyEnv, job *batchv1.Job)
		reason v1alpha1.PreviewVerificationReason
	}{
		{"no pod", func(*verifyEnv, *batchv1.Job) {}, v1alpha1.VerificationReasonQuotaRefused},
		{"unschedulable", func(e *verifyEnv, job *batchv1.Job) {
			pod := e.startTester(job)
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable}}
			e.mustStatus(pod)
		}, v1alpha1.VerificationReasonTesterUnscheduled},
		{"image pull", func(e *verifyEnv, job *batchv1.Job) {
			pod := e.startTester(job)
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: testerContainer,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
			e.mustStatus(pod)
		}, v1alpha1.VerificationReasonTesterImagePull},
		{"never finishing", func(e *verifyEnv, job *batchv1.Job) {
			pod := e.startTester(job)
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: testerContainer,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			e.mustStatus(pod)
			e.now = e.now.Add(5 * time.Minute)
		}, v1alpha1.VerificationReasonDeadlineNoSummary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, interceptor.Funcs{})
			e.verify()
			tc.pod(e, e.testerJob())
			if pv, _ := e.verify(); pv.Status.Phase != v1alpha1.VerificationRunning {
				t.Fatalf("phase %s %s within the grace, want Running", pv.Status.Phase, pv.Status.Reason)
			}
			e.now = e.now.Add(3 * time.Minute)
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != tc.reason {
				t.Errorf("phase %s %s, want Infra %s", pv.Status.Phase, pv.Status.Reason, tc.reason)
			}
			if len(e.jobs()) != 0 {
				t.Error("tester Job left behind")
			}
		})
	}
}

// The slot quota refusing the tester Job is maul's trouble, not the
// application's.
func TestVerificationQuotaRefused(t *testing.T) {
	funcs := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
		opts ...client.CreateOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			return kerrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, obj.GetName(),
				fmt.Errorf("exceeded quota: preview-quota, requested: count/jobs.batch=1"))
		}
		return c.Create(ctx, obj, opts...)
	}}
	e := newVerifyEnv(t, funcs)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != v1alpha1.VerificationReasonQuotaRefused {
		t.Errorf("phase %s %s, want Infra quota-refused", pv.Status.Phase, pv.Status.Reason)
	}
}

// The tester's deadline never exceeds what the slot's Job admission policy
// admits.
func TestVerificationDeadlineIsClamped(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.v.DeadlineMax = 3 * time.Minute
	e.verify()
	job := e.testerJob()
	if d := job.Spec.ActiveDeadlineSeconds; d == nil || *d != 180 ||
		*job.Spec.Template.Spec.ActiveDeadlineSeconds != 180 {
		t.Errorf("deadline %v, want 180", d)
	}
}

// A verification deleted, or settled, leaves no tester Job.
func TestSettledVerificationDeletesItsJob(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	if len(e.jobs()) != 1 {
		t.Fatal("no tester Job")
	}
	pv := e.currentPV()
	pv.Status.Phase = v1alpha1.VerificationStale
	pv.Status.Reason = v1alpha1.VerificationReasonStalePreview
	e.mustStatus(pv)
	e.verify()
	if len(e.jobs()) != 0 {
		t.Error("the settled verification's Job is still there")
	}
}

// A verification of a Preview that moved on while its tester ran is Stale,
// and its tester stops.
func TestRunningVerificationGoesStale(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.verify()
	e.p.Generation = 2
	e.mustUpdate(e.p)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != v1alpha1.VerificationReasonStalePreview {
		t.Errorf("phase %s %s, want Stale stale-preview", pv.Status.Phase, pv.Status.Reason)
	}
	if len(e.jobs()) != 0 {
		t.Error("the stale verification's tester still runs")
	}
}

// An API error reading a component's Pods is retried, never recorded as a
// part without a Pod: that would charge the intent for the API server's
// trouble.
func TestVerificationPodListErrorIsRetried(t *testing.T) {
	failing := false
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := list.(*corev1.PodList); ok && failing {
			o := &client.ListOptions{}
			o.ApplyOptions(opts)
			if o.LabelSelector != nil && strings.Contains(o.LabelSelector.String(), labelComponent) {
				return kerrors.NewServiceUnavailable("etcd is sad")
			}
		}
		return c.List(ctx, list, opts...)
	}}
	e := newVerifyEnv(t, funcs)
	e.verify()
	job := e.testerJob()
	log, msg := testerOutput(t, testerSpec(t, job), 200, "{}")
	pod := e.startTester(job)
	e.finishTester(job, pod, log, msg, 0)
	failing = true
	if _, err := e.v.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(e.pv)}); err == nil {
		t.Error("a failed Pod list settled the verification")
	}
	if pv := e.currentPV(); pv.Status.Phase != v1alpha1.VerificationRunning {
		t.Errorf("phase %s %s, want still Running", pv.Status.Phase, pv.Status.Reason)
	}
	failing = false
	if pv, _ := e.verify(); pv.Status.Phase != v1alpha1.VerificationObserved {
		t.Errorf("phase %s %s once the API answers, want Observed", pv.Status.Phase, pv.Status.Reason)
	}
}
