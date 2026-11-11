// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolwire"
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

// testerJob is the one Job in the slot: a tool pod's, named as slice 1's
// tester Jobs were (testerJobName).
func (e *verifyEnv) testerJob() *batchv1.Job {
	e.t.Helper()
	jobs := e.jobs()
	if len(jobs) != 1 {
		e.t.Fatalf("%d Jobs in the slot, want 1", len(jobs))
	}
	return &jobs[0]
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

// runTools plays the round's tool pod from start to finish: the Job
// controller creating it, the kubelet running it Ready, and the relay
// writing log and msg and exiting four minutes in (finishToolPod), where the
// clock then stands: a controller sees a pod finish only after it has.
func (e *verifyEnv) runTools(job *batchv1.Job, log []byte, msg string) {
	e.t.Helper()
	pod := e.startToolPod(job, true)
	e.finishToolPod(job, pod, log, msg)
	e.now = e.now.Add(4 * time.Minute)
}

// readinessLedger is what the relay writes when its readiness wait ends
// with waits and some target never reached: the wait, its own finish, and
// the summary.
func readinessLedger(t *testing.T, waits []httpstep.TargetWait) ([]byte, string) {
	t.Helper()
	var out bytes.Buffer
	w := toolwire.NewWriter(&out)
	at := time.Date(2026, 10, 1, 12, 1, 0, 0, time.UTC)
	readiness, err := json.Marshal(waits)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []toolwire.LedgerEntry{
		{Kind: toolwire.KindRelay, Verb: toolwire.VerbHTTP, TS: at,
			Args: json.RawMessage(`{"readiness":{"targets":["api","web"]}}`), Output: string(readiness)},
		{Kind: toolwire.KindRelay, Verb: toolwire.VerbFinish, TS: at,
			Args: json.RawMessage(`{"reason":"components-unreachable"}`)},
	} {
		if _, err := w.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), sum.TerminationMessage()
}

// A slice-1 verification (spec.criteria, no outcomes) is no longer run:
// with or without a tester image it is answered Stale stale-criteria at
// once, before anything is read, and nothing renders in the slot. A tester
// Job an older preview-controller left running for it is deleted, and so is
// one left for a criteria verification already settled.
func TestVerificationCriteriaNoLongerRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings func() Settings
		running  bool
		settled  bool
	}{
		{"pending", verifySettings, false, false},
		{"running, its tester Job left over", verifySettings, true, false},
		{"settled, its tester Job left over", verifySettings, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, interceptor.Funcs{})
			e.v.Settings, e.r.Settings = tc.settings(), tc.settings()
			if tc.running {
				e.legacyTesterRun()
			}
			if tc.settled {
				pv := e.currentPV()
				pv.Status.Phase, pv.Status.Reason = v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK
				e.mustStatus(pv)
			}
			pv, res := e.verify()
			want := v1alpha1.VerificationStale
			if tc.settled {
				want = v1alpha1.VerificationObserved
			}
			if pv.Status.Phase != want || res.RequeueAfter != 0 ||
				(!tc.settled && pv.Status.Reason != v1alpha1.VerificationReasonStaleCriteria) {
				t.Fatalf("status %+v, requeue %v; want %s", pv.Status, res.RequeueAfter, want)
			}
			if n := len(e.jobs()); n != 0 {
				t.Errorf("%d Jobs in the slot after the pass, want none", n)
			}
			again, _ := e.verify()
			if again.ResourceVersion != pv.ResourceVersion || len(e.jobs()) != 0 {
				t.Error("a settled verification changed, or a Job was rendered")
			}
		})
	}
}

// The Preview of a slice-1 verification need not be there for it to be
// answered stale-criteria: nothing of it is read.
func TestVerificationCriteriaWithoutItsPreview(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	if err := e.c.Delete(context.Background(), e.p); err != nil {
		t.Fatal(err)
	}
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != v1alpha1.VerificationReasonStaleCriteria {
		t.Fatalf("phase %s %s, want Stale stale-criteria", pv.Status.Phase, pv.Status.Reason)
	}
}

// BeforeRelease leaves a running slice-1 verification to its own pass,
// which answers it stale-criteria: nothing of the slot is recorded for it.
func TestVerificationCriteriaBeforeRelease(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	e.v.Settings, e.r.Settings = verifySettings(), verifySettings()
	e.legacyTesterRun()
	if err := e.v.BeforeRelease(context.Background(), e.p); err != nil {
		t.Fatal(err)
	}
	if pv := e.currentPV(); pv.Status.Phase != v1alpha1.VerificationRunning || len(pv.Status.Environment) != 0 {
		t.Fatalf("BeforeRelease recorded a slice-1 verification: %+v", pv.Status)
	}
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != v1alpha1.VerificationReasonStaleCriteria ||
		len(e.jobs()) != 0 {
		t.Fatalf("phase %s %s with %d Jobs, want Stale stale-criteria and none", pv.Status.Phase,
			pv.Status.Reason, len(e.jobs()))
	}
}

// legacyTesterRun plays an older preview-controller having started the
// criteria verification: its tester Job and Pod in the slot, the
// verification Running.
func (e *verifyEnv) legacyTesterRun() {
	e.t.Helper()
	job := legacyTesterJob(e.p, e.pv)
	if err := e.c.Create(context.Background(), job); err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.Create(context.Background(), testerPod(job, job.Name+"-x7k2p")); err != nil {
		e.t.Fatal(err)
	}
	pv := e.currentPV()
	started := metav1.NewTime(e.now)
	pv.Status = v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationRunning, StartedAt: &started}
	e.mustStatus(pv)
}

// Every pre-check that fails leaves the verification Stale and renders no
// tool pod.
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
		{"intent has no approved acceptance", func(e *verifyEnv) {
			e.in.Status.Acceptance = nil
			e.mustStatus(e.in)
		}, v1alpha1.VerificationReasonStaleCriteria},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			if err := e.c.Get(ctx, client.ObjectKeyFromObject(e.in), e.in); err != nil {
				t.Fatal(err)
			}
			tc.mutate(e)
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != tc.reason {
				t.Errorf("phase %s %s, want Stale %s", pv.Status.Phase, pv.Status.Reason, tc.reason)
			}
			if n := len(e.jobs()); n != 0 {
				t.Errorf("%d Jobs rendered", n)
			}
		})
	}
}

// A component Pod of an earlier template still running holds the tool pod
// back, and past the grace the attempt is maul's trouble.
func TestVerificationWaitsForTheRollout(t *testing.T) {
	e := newToolEnv(t)
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
		t.Fatal("tool pod rendered beside an old component Pod")
	}
	e.now = e.now.Add(3 * time.Minute)
	pv, _ = e.verify()
	if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != v1alpha1.VerificationReasonRolloutUnsettled {
		t.Errorf("phase %s %s, want Infra rollout-unsettled", pv.Status.Phase, pv.Status.Reason)
	}
}

// A component the relay never reached for its whole readiness window
// while it was Ready refused it: the window absorbs the slot policy's
// programming delay (about a second on k3s); past it the application is
// refusing the relay (it can filter SYNs by source with a classic BPF
// socket filter, no capability needed, and still pass the kubelet's probe),
// so the attempt is charged to it: EnvFailed component-unreachable, the
// component's fact marked, never maul's Infra.
func TestVerificationUnreachableIsTheApps(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	log, msg := toolLedger(t, nil, "api")
	e.runTools(e.testerJob(), log, msg)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed ||
		pv.Status.Reason != v1alpha1.VerificationReasonComponentUnreachable {
		t.Fatalf("phase %s %s, want EnvFailed component-unreachable", pv.Status.Phase, pv.Status.Reason)
	}
	want, err := crdconv.WaitsToCRD([]httpstep.TargetWait{
		{Target: "api", WaitMs: 60000, Attempts: 30, Refused: 30}, {Target: "web", WaitMs: 40, Attempts: 1}})
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
	if httpstep.ClassifyEnvironment(env) != httpstep.EnvAppFailure ||
		!slices.Equal(httpstep.AppFailureParts(env), []string{"api"}) {
		t.Errorf("environment classifies as %s, failing %v; want the application's, api",
			httpstep.ClassifyEnvironment(env), httpstep.AppFailureParts(env))
	}
	if len(e.jobs()) != 0 {
		t.Errorf("%d jobs left", len(e.jobs()))
	}
}

// When the unreachable component is the one that crashed (its listener
// died), the crash is the reason given.
func TestVerificationUnreachableCrashedIsEnvFailed(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	log, msg := toolLedger(t, nil, "api")
	e.crash(1, "CrashLoopBackOff")
	e.runTools(e.testerJob(), log, msg)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed ||
		pv.Status.Reason != v1alpha1.VerificationReasonComponentCrashLoop {
		t.Fatalf("phase %s %s, want EnvFailed component-crashloop", pv.Status.Phase, pv.Status.Reason)
	}
	if len(pv.Status.Readiness) != 2 {
		t.Errorf("readiness %+v", pv.Status.Readiness)
	}
}

// The relay resolves nothing (its targets are ClusterIPs), so a recorded
// DNS failure is no excuse: a target never reached, however its attempts
// failed, is the application's component-unreachable.
func TestVerificationUnreachableWithDNSFailures(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	log, msg := readinessLedger(t, []httpstep.TargetWait{
		{Target: "api", WaitMs: 60000, Attempts: 66, DNS: 66}, {Target: "web", WaitMs: 3, Attempts: 1}})
	e.runTools(e.testerJob(), log, msg)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed ||
		pv.Status.Reason != v1alpha1.VerificationReasonComponentUnreachable {
		t.Fatalf("phase %s %s, want EnvFailed component-unreachable", pv.Status.Phase, pv.Status.Reason)
	}
}

// A component that exited during the attempt, by the kubelet's timestamps
// against the attempt's start and end, and came back Ready crashed:
// EnvFailed component-crashed, the change's. A restart from before the
// attempt, or after it ended, is not charged. The attempt ends when the
// tool pod's tools container terminated, not at the settle: a crash
// between the two came after every check ran, so it is not the change's.
func TestVerificationRestartDuringTheAttempt(t *testing.T) {
	ref := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) // the attempt's start (e.now)
	for _, tc := range []struct {
		name              string
		restarts          int32
		finished, started time.Time
		settle            time.Duration
		reason            v1alpha1.PreviewVerificationReason
	}{
		{"crash and recovery during the attempt", 1, ref.Add(30 * time.Second), ref.Add(31 * time.Second), 0,
			v1alpha1.VerificationReasonComponentCrashed},
		{"exited during, not yet seen running", 1, ref.Add(30 * time.Second), time.Time{}, 0,
			v1alpha1.VerificationReasonComponentCrashed},
		{"within the skew tolerance", 1, ref.Add(-time.Second), ref.Add(-time.Second), 0,
			v1alpha1.VerificationReasonComponentCrashed},
		{"restart before the attempt", 3, ref.Add(-10 * time.Minute), ref.Add(-10 * time.Minute), 0,
			v1alpha1.VerificationReasonOK},
		{"restart after the attempt ended", 1, ref.Add(time.Hour), ref.Add(time.Hour), 0,
			v1alpha1.VerificationReasonOK},
		{"just before the tools exited", 1, ref.Add(239 * time.Second), ref.Add(239 * time.Second), time.Minute,
			v1alpha1.VerificationReasonComponentCrashed},
		{"after the tools exited, before the settle", 1, ref.Add(270 * time.Second),
			ref.Add(271 * time.Second), time.Minute, v1alpha1.VerificationReasonOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			e.verify()
			log, msg := toolLedger(t, nil)
			e.restartAt(tc.restarts, tc.finished, tc.started)
			e.runTools(e.testerJob(), log, msg) // the tools exit, and are seen, at ref+4m
			e.now = e.now.Add(tc.settle)
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

// A component Pod replaced during the attempt is attributed by the
// replaced Pod's evidence. status.reason Evicted is the kubelet's
// eviction, the application's: EnvFailed component-evicted. A kubelet
// disruption without it (graceful node shutdown), a drain, or a replacement
// whose old Pod is gone is the cluster's: Infra component-disrupted,
// retried. A Pod served a moment before the attempt started is the one it
// ran against, never a replacement.
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
		{"served a second before the attempt", -time.Second, nil,
			v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK},
		{"served within the skew after", time.Second, nil,
			v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			e.verify()
			log, msg := toolLedger(t, nil)
			e.replace(tc.created, tc.old)
			e.runTools(e.testerJob(), log, msg)
			pv, _ := e.verify()
			if pv.Status.Phase != tc.phase || pv.Status.Reason != tc.reason {
				t.Fatalf("phase %s %s, want %s %s", pv.Status.Phase, pv.Status.Reason, tc.phase, tc.reason)
			}
		})
	}
}

// Only Pods of the component's current revision (its spec's image) are
// judged. A previous revision's Pod being evicted or terminating during the
// attempt is ignored, so it can neither charge the change nor mask a
// crash: a crash of the current Pod while an old one terminates is the
// application's.
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
			e := newToolEnv(t)
			e.verify()
			log, msg := toolLedger(t, nil)
			e.oldRevisionPod(terminating)
			if tc.crash {
				e.restartAt(1, e.now.Add(30*time.Second), e.now.Add(31*time.Second))
			}
			e.runTools(e.testerJob(), log, msg)
			pv, _ := e.verify()
			if pv.Status.Phase != tc.phase || pv.Status.Reason != tc.reason {
				t.Fatalf("phase %s %s, want %s %s", pv.Status.Phase, pv.Status.Reason, tc.phase, tc.reason)
			}
		})
	}
}

// A component that went not-Ready during the attempt with no evidence of
// the application's (a node going NotReady sets Ready False with no reason)
// is the cluster's: Infra component-disrupted, retried and bounded by the
// attempts, never a verdict. The application can at most make itself
// not-Ready without crashing, which earns it an honest Infra and then
// Unavailable, never a pass; and it is unreachable then too, but the
// not-Ready cause wins over component-unreachable.
func TestVerificationNotReadyWithoutEvidenceIsInfra(t *testing.T) {
	for _, unreachable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unreachable %t", unreachable), func(t *testing.T) {
			e := newToolEnv(t)
			e.verify()
			log, msg := toolLedger(t, nil)
			if unreachable {
				log, msg = toolLedger(t, nil, "api")
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
			e.runTools(e.testerJob(), log, msg)
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationInfra ||
				pv.Status.Reason != v1alpha1.VerificationReasonComponentDisrupted {
				t.Fatalf("phase %s %s, want Infra component-disrupted", pv.Status.Phase, pv.Status.Reason)
			}
		})
	}
}

// A Failed Pod left over from an eviction before the attempt, its
// replacement serving since before the attempt started, is history, not
// this attempt's failure.
func TestVerificationEvictedBeforeTheAttemptIsNotCharged(t *testing.T) {
	e := newToolEnv(t)
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
	log, msg := toolLedger(t, nil)
	e.runTools(e.testerJob(), log, msg)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationObserved {
		t.Fatalf("phase %s %s, want Observed", pv.Status.Phase, pv.Status.Reason)
	}
}

// A second start pass once the tool Job exists (its create answered
// AlreadyExists) changes nothing about how the attempt settles.
func TestVerificationStartReenteredHasNoEffect(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	job := e.testerJob()
	log, msg := toolLedger(t, nil)
	e.restartAt(2, e.now.Add(-time.Hour), e.now.Add(-time.Hour))
	pv := e.currentPV()
	pv.Status.Phase = v1alpha1.VerificationPending
	if err := e.c.Status().Update(context.Background(), pv); err != nil {
		t.Fatal(err)
	}
	if pv, _ = e.verify(); pv.Status.Phase != v1alpha1.VerificationRunning || len(e.jobs()) != 1 {
		t.Fatalf("phase %s with %d jobs after the second start", pv.Status.Phase, len(e.jobs()))
	}
	e.runTools(job, log, msg)
	if pv, _ = e.verify(); pv.Status.Phase != v1alpha1.VerificationObserved ||
		pv.Status.Reason != v1alpha1.VerificationReasonOK {
		t.Fatalf("phase %s %s, want Observed ok", pv.Status.Phase, pv.Status.Reason)
	}
}

// The relay is configured with each component Service's ClusterIP, read
// live, never a name it would have to resolve; a Service with none is
// maul's render error, settled Infra before any tool pod starts.
func TestVerificationTargetsAreClusterIPs(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	var cm corev1.ConfigMap
	if err := e.slotObject(toolName(e.p, e.pv, toolConfigInfix), &cm); err != nil {
		t.Fatal(err)
	}
	cfg, err := previewtools.ParseConfig([]byte(cm.Data[previewtools.ConfigFile]))
	if err != nil {
		t.Fatal(err)
	}
	addrs := map[string]string{}
	for _, c := range cfg.Components {
		addrs[c.Key] = c.Addr
	}
	if want := map[string]string{"web": "10.96.0.10:80", "api": "10.96.0.11:80"}; !maps.Equal(addrs, want) {
		t.Errorf("targets = %v, want %v", addrs, want)
	}
	for _, ip := range []string{"", corev1.ClusterIPNone} {
		t.Run("clusterIP "+ip, func(t *testing.T) {
			e := newToolEnv(t)
			e.setClusterIP(1, ip)
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationInfra ||
				pv.Status.Reason != v1alpha1.VerificationReasonServiceNoClusterIP {
				t.Fatalf("phase %s %s, want Infra service-no-cluster-ip", pv.Status.Phase, pv.Status.Reason)
			}
			if len(e.jobs()) != 0 || !e.slotGone(evaluatorPolicyName(e.p, pv), &networkingv1.NetworkPolicy{}) {
				t.Error("a tool pod or its policy was rendered with no address for a component")
			}
		})
	}
}

// A Preview whose components changed while its tool pod ran settles the
// attempt stale-preview.
func TestVerificationPreviewChangedIsStale(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	log, msg := toolLedger(t, nil)
	var p v1alpha1.Preview
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(e.p), &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components = p.Spec.Components[:1]
	if err := e.c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	e.runTools(e.testerJob(), log, msg)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != v1alpha1.VerificationReasonStalePreview {
		t.Fatalf("phase %s %s, want Stale stale-preview", pv.Status.Phase, pv.Status.Reason)
	}
}

// A component that crashed during the run fails the environment: the
// verification is EnvFailed, charged to the intent, with the evidence and
// the round's ledger.
func TestVerificationComponentCrashIsEnvFailed(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	log, msg := toolLedger(t, nil)
	e.crash(1, "CrashLoopBackOff")
	e.logs.logs["maul-preview-0/"+componentName(e.p, 1)+"-pod/api"] = []byte("panic: nil map\n")
	e.runTools(e.testerJob(), log, msg)
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
	if pv.Status.Ledger == nil {
		t.Error("the round's ledger was not kept beside the environment")
	}
	for _, r := range e.logs.reads {
		if r.LimitBytes == componentLogLimit && (r.TailLines == nil || *r.TailLines != componentLogLines ||
			r.SinceTime == nil) {
			t.Errorf("component log read %+v, want a bounded tail since the start", r)
		}
	}
}

// A Preview released while its verification runs (a retry, a failure)
// records what the slot showed first, through the preview reconciler's
// BeforeRelease hook, while the Pods can still be seen.
func TestVerificationEnvFailedBeforeRelease(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	e.startToolPod(e.testerJob(), true)
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
	e := newToolEnv(t)
	e.verify()
	e.p.Status.Phase = v1alpha1.PreviewFailed
	e.mustStatus(e.p)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed || pv.Status.Reason != v1alpha1.VerificationReasonPreviewFailed {
		t.Errorf("phase %s %s, want EnvFailed preview-failed", pv.Status.Phase, pv.Status.Reason)
	}
	if len(e.jobs()) != 0 {
		t.Error("tool Job left behind")
	}
}

// The slot quota refusing the tool Job is maul's trouble (quota-refused),
// and so is an admission policy refusing it (tools-unscheduled): never the
// application's.
func TestVerificationJobRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason v1alpha1.PreviewVerificationReason
	}{
		{"quota", kerrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "tools",
			fmt.Errorf("exceeded quota: preview-quota, requested: count/jobs.batch=1")),
			v1alpha1.VerificationReasonQuotaRefused},
		{"admission", kerrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "tools",
			fmt.Errorf("ValidatingAdmissionPolicy 'maul-preview-jobs' denied request")),
			v1alpha1.VerificationReasonToolsUnscheduled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnvWith(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch,
				obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*batchv1.Job); ok {
					return tc.err
				}
				return c.Create(ctx, obj, opts...)
			}})
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != tc.reason {
				t.Errorf("phase %s %s, want Infra %s", pv.Status.Phase, pv.Status.Reason, tc.reason)
			}
		})
	}
}

// The tool Job's deadline never exceeds what the slot's Job admission
// policy admits.
func TestVerificationDeadlineIsClamped(t *testing.T) {
	e := newToolEnv(t)
	e.v.DeadlineMax = 3 * time.Minute
	e.verify()
	job := e.testerJob()
	if d := job.Spec.ActiveDeadlineSeconds; d == nil || *d != 180 ||
		*job.Spec.Template.Spec.ActiveDeadlineSeconds > 180 {
		t.Errorf("deadline %v, pod deadline %v; want 180 and at most 180", d,
			job.Spec.Template.Spec.ActiveDeadlineSeconds)
	}
}

// A verification settled leaves no Job.
func TestSettledVerificationDeletesItsJob(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	if len(e.jobs()) != 1 {
		t.Fatal("no tool Job")
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

// A verification of a Preview that moved on while its tool pod ran is
// Stale, and its tool pod stops.
func TestRunningVerificationGoesStale(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	e.p.Generation = 2
	e.mustUpdate(e.p)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationStale || pv.Status.Reason != v1alpha1.VerificationReasonStalePreview {
		t.Errorf("phase %s %s, want Stale stale-preview", pv.Status.Phase, pv.Status.Reason)
	}
	if len(e.jobs()) != 0 {
		t.Error("the stale verification's tool pod still runs")
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
	e := newToolEnvWith(t, funcs)
	e.verify()
	log, msg := toolLedger(t, nil)
	e.runTools(e.testerJob(), log, msg)
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

// An outcome verification's status is held to its cap: the parts' log
// tails go first, from the last part back, and the round stands; one still
// over is maul's trouble, its ledger fields dropped, never a verdict.
func TestFitToolStatus(t *testing.T) {
	tail := strings.Repeat("x", 400<<10)
	over := func(pad int) *v1alpha1.PreviewVerification {
		pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: "shop-1-v1-a1",
			Annotations: map[string]string{"pad": strings.Repeat("p", pad)}}}
		pv.Status = v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationObserved,
			Reason: v1alpha1.VerificationReasonOK, Ledger: &v1alpha1.LedgerSummary{Count: 1,
				SHA256: strings.Repeat("a", 64)}, Environment: []v1alpha1.PartFact{
				{Name: "web", LogTail: tail}, {Name: "api", LogTail: tail}, {Name: "tools", LogTail: tail},
				{Name: "extra", LogTail: tail}}}
		return pv
	}
	pv := over(0)
	fitToolStatus(pv)
	if pv.Status.Phase != v1alpha1.VerificationObserved || pv.Status.Ledger == nil {
		t.Fatalf("phase %s %s, ledger %v: the round fits once tails go", pv.Status.Phase, pv.Status.Reason,
			pv.Status.Ledger)
	}
	if n := jsonSize(pv); n > maxToolVerificationObject {
		t.Errorf("status is %d bytes, over %d", n, maxToolVerificationObject)
	}
	if pv.Status.Environment[0].LogTail == "" || pv.Status.Environment[3].LogTail != "" {
		t.Error("tails not dropped from the last part back")
	}
	pv = over(maxToolVerificationObject)
	fitToolStatus(pv)
	if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != v1alpha1.VerificationReasonLedgerMismatch ||
		pv.Status.Ledger != nil {
		t.Errorf("phase %s %s, ledger %v; want Infra ledger-mismatch and no ledger", pv.Status.Phase,
			pv.Status.Reason, pv.Status.Ledger)
	}
}
