// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"context"
	"encoding/json"
	stdimage "image"
	"image/color"
	"image/png"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// assignUIDs plays the API server giving each created object a UID, which
// an owner reference needs.
var assignUIDs = interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
	opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("uid-" + obj.GetName()))
	}
	return c.Create(ctx, obj, opts...)
}}

// newToolEnv is newVerifyEnv with the evaluator on, its Intent's approved
// plan declaring outcomes with one saved check, and an outcome
// verification of its Preview.
func newToolEnv(t *testing.T) *verifyEnv {
	t.Helper()
	e := newVerifyEnv(t, assignUIDs)
	e.v.Settings, e.r.Settings = evalSettings(), evalSettings()
	e.in.Status.Acceptance = &v1alpha1.IntentAcceptance{PlanRevision: 1,
		Digest: acceptance.Digest(nil), Outcomes: testOutcomes(), OutcomesDigest: outcomesDigest(t, testOutcomes()),
		Checks: &v1alpha1.CheckStoreRef{ConfigMap: "shop-1-checks", Digest: "sha256:" + strings.Repeat("c", 64)}}
	e.mustStatus(e.in)
	if err := e.c.Delete(context.Background(), e.pv); err != nil {
		t.Fatal(err)
	}
	e.pv = e.toolPV(nil)
	return e
}

// toolPV creates an outcome verification of the Preview, mut applied first.
func (e *verifyEnv) toolPV(mut func(*v1alpha1.PreviewVerification)) *v1alpha1.PreviewVerification {
	e.t.Helper()
	heads := make([]v1alpha1.VerificationHead, 0, len(e.p.Spec.Components))
	urls := map[string]string{"web": "https://github.com/acme/acme.web_app", "api": "https://github.com/acme/api"}
	for _, c := range e.p.Spec.Components {
		heads = append(heads, v1alpha1.VerificationHead{Component: c.Name, Repository: urls[c.Name], Revision: c.Revision})
	}
	pv := &v1alpha1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PreviewVerificationName(e.in.Name, 1, 1), Namespace: "maul",
			UID: "pv-tools", CreationTimestamp: metav1.NewTime(e.now),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "maul.run/v1alpha1", Kind: "Intent",
				Name: e.in.Name, UID: e.in.UID, Controller: ptr(true)}}},
		Spec: v1alpha1.PreviewVerificationSpec{
			IntentRef:  v1alpha1.ObjectReference{Name: e.in.Name, UID: e.in.UID},
			PreviewRef: v1alpha1.ObjectReference{Name: e.p.Name, UID: e.p.UID}, PreviewGeneration: e.p.Generation,
			HeadSetDigest: "sha256:" + strings.Repeat("0", 64), Heads: heads, Round: 1, Attempt: 1, Run: "a1b2c3",
			Outcomes: testOutcomes(), OutcomesDigest: e.in.Status.Acceptance.OutcomesDigest,
			Checks:       []v1alpha1.SavedCheck{savedCheck(e.t, testCheckBody)},
			ChecksDigest: e.in.Status.Acceptance.Checks.Digest, Rung: v1alpha1.RungFull,
			Tools:   &v1alpha1.ToolsSpec{Browser: true, Sandbox: true, MaxToolCalls: 120},
			Timeout: metav1.Duration{Duration: 20 * time.Minute},
		},
	}
	if mut != nil {
		mut(pv)
	}
	if err := e.c.Create(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
	return pv
}

// startTools plays the Job controller creating the tool pod and the
// kubelet running it, its tools container Ready or not.
func (e *verifyEnv) startToolPod(job *batchv1.Job, ready bool) *corev1.Pod {
	e.t.Helper()
	pod := testerPod(job, job.Name+"-t0012")
	if err := e.c.Create(context.Background(), pod); err != nil {
		e.t.Fatal(err)
	}
	pod.Status.PodIP = "10.42.1.7"
	pod.Status.Phase = corev1.PodRunning
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: toolsContainer, Ready: ready,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(e.now)}}}}
	if err := e.c.Status().Update(context.Background(), pod); err != nil {
		e.t.Fatal(err)
	}
	return pod
}

// finishToolPod plays tools writing log and msg and exiting, and the Job
// controller completing the Job.
func (e *verifyEnv) finishToolPod(job *batchv1.Job, pod *corev1.Pod, log []byte, msg string) {
	e.t.Helper()
	ctx := context.Background()
	e.logs.logs[pod.Namespace+"/"+pod.Name+"/"+toolsContainer] = log
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: toolsContainer,
		ImageID: "ghcr.io/devthenet-labs/maul-preview-tools@sha256:" + strings.Repeat("11", 32),
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed",
			Message: msg, StartedAt: metav1.NewTime(e.now), FinishedAt: metav1.NewTime(e.now.Add(4 * time.Minute))}}}}
	if err := e.c.Status().Update(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(e.now)}}
	if err := e.c.Status().Update(ctx, job); err != nil {
		e.t.Fatal(err)
	}
}

// testPNG is a w by h PNG.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := stdimage.NewNRGBA(stdimage.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// toolLedger is what the tools container writes for a full round against
// the saved check: its readiness wait (unreached naming the components it
// never reached), the replay, the agent's green dry run of a new check and
// a screenshot, the runner's confirm and the relay's confirmation, the
// finish, the screenshot as evidence, and the summary.
func toolLedger(t *testing.T, shot []byte, unreached ...string) ([]byte, string) {
	t.Helper()
	var out bytes.Buffer
	w := toolwire.NewWriter(&out)
	at := time.Date(2026, 10, 1, 12, 1, 0, 0, time.UTC)
	add := func(e toolwire.LedgerEntry) toolwire.LedgerEntry {
		t.Helper()
		e.TS = at
		got, err := w.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	waits := make([]acceptance.TargetWait, 0, 2)
	for _, key := range []string{"api", "web"} {
		wt := acceptance.TargetWait{Target: key, WaitMs: 40, Attempts: 1}
		if strings.Contains(strings.Join(unreached, ","), key) {
			wt = acceptance.TargetWait{Target: key, WaitMs: 60000, Attempts: 30, Refused: 30}
		}
		waits = append(waits, wt)
	}
	readiness, _ := json.Marshal(waits)
	add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbHTTP,
		Args: json.RawMessage(`{"readiness":{"targets":["api","web"]}}`), Output: string(readiness)})
	if len(unreached) > 0 {
		add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbFinish,
			Args: json.RawMessage(`{"reason":"components-unreachable"}`)})
		sum, err := w.Close()
		if err != nil {
			t.Fatal(err)
		}
		return out.Bytes(), sum.TerminationMessage()
	}
	saved, err := checks.Parse([]byte(testCheckBody))
	if err != nil {
		t.Fatal(err)
	}
	pass := []toolwire.Attempt{{Run: "0123456789abcdef", Result: toolwire.ResultPass}}
	add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbCheckRun,
		Args: json.RawMessage(`{}`), CheckRun: &toolwire.CheckRunRecord{
			Digest: strings.TrimPrefix(checks.Digest(saved), "sha256:"), CheckID: saved.ID, Mode: toolwire.ModeReplay,
			Result: toolwire.RunPass, Attempts: pass}})
	proposed, err := checks.Parse([]byte(strings.Replace(testCheckBody, `"items.body"`, `"items.echo"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(checks.Digest(proposed), "sha256:")
	args, _ := json.Marshal(toolwire.CheckRunRequest{Spec: checks.Canonical(proposed)})
	dry := add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbCheckRun, Args: args,
		CheckRun: &toolwire.CheckRunRecord{Digest: digest, CheckID: proposed.ID, Mode: toolwire.ModeDryRun,
			Result: toolwire.RunPass, Attempts: pass}})
	browse := add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbBrowser,
		Args: json.RawMessage(`{"verb":"screenshot"}`), Output: "shot",
		Artifacts: []toolwire.Artifact{{Name: "page.png", SHA256: sha256Hex(shot), Bytes: int64(len(shot))}}})
	add(toolwire.LedgerEntry{Kind: toolwire.KindRunner, Verb: toolwire.VerbConfirm,
		Args: json.RawMessage(`{"digests":["` + digest + `"]}`)})
	add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbConfirm,
		Args: json.RawMessage(`{"digest":"` + digest + `"}`), CheckRun: &toolwire.CheckRunRecord{Digest: digest,
			CheckID: proposed.ID, Mode: toolwire.ModeConfirm, Result: toolwire.RunPass,
			Attempts: []toolwire.Attempt{{Run: "fedcba9876543210", Result: toolwire.ResultPass}}}})
	add(toolwire.LedgerEntry{Kind: toolwire.KindRunner, Verb: toolwire.VerbFinish, Args: json.RawMessage(`{}`)})
	if dry.Seq != 3 {
		t.Fatalf("dry run at %d", dry.Seq)
	}
	if err := w.Evidence(browse.Seq, shot); err != nil {
		t.Fatal(err)
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), sum.TerminationMessage()
}

// slotGone reports whether the named slot object is gone.
func (e *verifyEnv) slotGone(name string, obj client.Object) bool {
	return kerrors.IsNotFound(e.slotObject(name, obj))
}

func TestToolsRenderedForAnOutcomeVerification(t *testing.T) {
	e := newToolEnv(t)
	pv, res := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationRunning || pv.Status.StartedAt == nil || res.RequeueAfter == 0 {
		t.Fatalf("status %+v, requeue %v; want Running", pv.Status, res.RequeueAfter)
	}
	if pv.Status.Tools != nil {
		t.Errorf("status.tools %+v before the tool pod is Ready", pv.Status.Tools)
	}
	job := e.testerJob()
	if job.Spec.Template.Labels[labelRole] != v1alpha1.PreviewRoleTools ||
		job.Spec.Template.Spec.Containers[0].Name != toolsContainer {
		t.Fatalf("rendered %s, not the tool pod", job.Name)
	}
	var cm corev1.ConfigMap
	if err := e.slotObject(toolName(e.p, pv, toolConfigInfix), &cm); err != nil {
		t.Fatal(err)
	}
	cfg, err := previewtools.ParseConfig([]byte(cm.Data[previewtools.ConfigFile]))
	if err != nil {
		t.Fatalf("the relay refuses its config: %v", err)
	}
	if cfg.Components[1].Addr != "10.96.0.11:80" || len(cfg.Checks) != 1 || cfg.TimeoutSeconds != 20*60 {
		t.Errorf("config %+v", cfg)
	}
	var sec corev1.Secret
	if err := e.slotObject(toolName(e.p, pv, toolKeyInfix), &sec); err != nil || len(sec.Data[cdpKeyFile]) != 64 {
		t.Errorf("key Secret %v: %v", sec.Data, err)
	}
	if sec.OwnerReferences[0].UID != job.UID || cm.OwnerReferences[0].UID != job.UID {
		t.Error("the key Secret and ConfigMap are not the Job's")
	}
	if err := e.slotObject(evaluatorPolicyName(e.p, pv), &networkingv1.NetworkPolicy{}); err != nil {
		t.Errorf("ingress policy: %v", err)
	}
	// A second pass renders nothing new.
	e.verify()
	if n := len(e.jobs()); n != 1 {
		t.Errorf("%d Jobs after a second pass", n)
	}
}

// status.tools is written only once the tools container is Ready; until
// then, and after, each poll refreshes the capture in the tool ConfigMap,
// the CDP key redacted from it.
func TestToolsStatusOnlyWhenReady(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	job := e.testerJob()
	pod := e.startToolPod(job, false)
	web := componentName(e.p, 0) + "-pod"
	key := e.v.toolStateOf(e.pv.UID).key
	e.logs.logs["maul-preview-0/"+web+"/web"] = []byte("GET / 200 leaked " + key + "\n")
	e.logs.logs["maul-preview-0/"+componentName(e.p, 1)+"-pod/api"] = []byte("listening\n")
	pv, _ := e.verify()
	if pv.Status.Tools != nil || pv.Status.Phase != v1alpha1.VerificationRunning {
		t.Fatalf("status %+v while tools is not Ready", pv.Status)
	}
	var cm corev1.ConfigMap
	if err := e.slotObject(toolName(e.p, pv, toolConfigInfix), &cm); err != nil {
		t.Fatal(err)
	}
	if got := cm.Data[toolwire.CaptureLogKey("web")]; got != "GET / 200 leaked [redacted]\n" {
		t.Errorf("captured web tail %q", got)
	}
	if _, ok := cm.Data[toolwire.CaptureEventsKey]; !ok || cm.Data[previewtools.ConfigFile] == "" {
		t.Errorf("tool ConfigMap keys %v", cm.Data)
	}
	if pv.Status.Capture == nil || len(pv.Status.Capture.Components) != 2 {
		t.Errorf("capture summary %+v", pv.Status.Capture)
	}
	e.c.Delete(context.Background(), pod) //nolint:errcheck // replaced below
	e.startToolPod(job, true)
	pv, _ = e.verify()
	want := v1alpha1.ToolsStatus{PodIP: "10.42.1.7", Port: previewtools.Port, Ready: true}
	if pv.Status.Tools == nil || *pv.Status.Tools != want {
		t.Errorf("status.tools %+v, want %+v", pv.Status.Tools, want)
	}
}

// A restarted preview-controller holds no CDP key in memory and never reads
// its Secret back, so it must not capture with a key it does not know: the
// capture is refused (the last one written stands) rather than refreshed
// with the key unredacted, and status.tools is still written. A restart
// while the tool pod runs and one between writing the Secret and marking
// the verification Running are both covered.
func TestToolsCaptureRefusedAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		name          string
		beforeRunning bool
	}{
		{name: "while running"},
		{name: "before the status was written", beforeRunning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			pv, _ := e.verify()
			var sec corev1.Secret
			if err := e.slotObject(toolName(e.p, pv, toolKeyInfix), &sec); err != nil {
				t.Fatal(err)
			}
			key := string(sec.Data[cdpKeyFile])
			if tc.beforeRunning {
				pv.Status.Phase, pv.Status.StartedAt = "", nil
				e.mustStatus(pv)
			}
			// The restart: a new reconciler, no tool state.
			e.v = &VerificationReconciler{Client: e.v.Client, Settings: e.v.Settings, Logs: e.v.Logs,
				DeadlineMax: e.v.DeadlineMax, Now: e.v.Now, Log: e.v.Log}
			if tc.beforeRunning {
				if pv, _ = e.verify(); pv.Status.Phase != v1alpha1.VerificationRunning {
					t.Fatalf("phase %q after the restart", pv.Status.Phase)
				}
			}
			e.startToolPod(e.testerJob(), true)
			e.logs.logs["maul-preview-0/"+componentName(e.p, 0)+"-pod/web"] = []byte("GET / 200 leaked " + key + "\n")
			e.logs.logs["maul-preview-0/"+componentName(e.p, 1)+"-pod/api"] = []byte("listening\n")
			pv, _ = e.verify()
			var cm corev1.ConfigMap
			if err := e.slotObject(toolName(e.p, pv, toolConfigInfix), &cm); err != nil {
				t.Fatal(err)
			}
			for k, v := range cm.Data {
				if strings.Contains(v, key) {
					t.Errorf("tool ConfigMap %s holds the CDP key unredacted: %q", k, v)
				}
			}
			if got, _ := json.Marshal(pv.Status); bytes.Contains(got, []byte(key)) {
				t.Errorf("status holds the CDP key: %s", got)
			}
			if pv.Status.Tools == nil || !pv.Status.Tools.Ready {
				t.Errorf("status.tools %+v, want Ready", pv.Status.Tools)
			}
		})
	}
}

func TestToolsObserved(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	job := e.testerJob()
	pod := e.startToolPod(job, true)
	e.verify()
	shot := testPNG(t, 2560, 40)
	log, msg := toolLedger(t, shot)
	e.finishToolPod(job, pod, append([]byte("relay starting\n"), log...), msg)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationObserved || pv.Status.Reason != v1alpha1.VerificationReasonOK {
		t.Fatalf("phase %s %s, want Observed ok", pv.Status.Phase, pv.Status.Reason)
	}
	var sum toolwire.Summary
	_ = json.Unmarshal([]byte(msg), &sum)
	checkToolsStatus(t, pv, sum, shot)
	checkToolsConfigMaps(t, e, pv, shot)
	// Settled: the Job, its key Secret, its ConfigMap and the ingress
	// policy are gone, and a second pass is a no-op.
	for name, obj := range map[string]client.Object{
		job.Name: &batchv1.Job{}, toolName(e.p, pv, toolKeyInfix): &corev1.Secret{},
		toolName(e.p, pv, toolConfigInfix): &corev1.ConfigMap{},
		evaluatorPolicyName(e.p, pv):       &networkingv1.NetworkPolicy{},
	} {
		if !e.slotGone(name, obj) {
			t.Errorf("%T %s survived the settle", obj, name)
		}
	}
	again, _ := e.verify()
	if again.ResourceVersion != pv.ResourceVersion {
		t.Error("a settled verification changed")
	}
}

// checkToolsStatus holds an Observed tool verification's status to the
// toolLedger it was collected from.
func checkToolsStatus(t *testing.T, pv *v1alpha1.PreviewVerification, sum toolwire.Summary, shot []byte) {
	t.Helper()
	l := pv.Status.Ledger
	if l == nil || l.Count != 7 || l.SHA256 != sum.SHA256 || len(l.Entries) != 7 ||
		l.ConfigMap != pv.Name+ledgerSuffix || l.Evidence != pv.Name+evidenceSuffix {
		t.Fatalf("ledger %+v", l)
	}
	browse := l.Entries[3]
	if browse.Verb != v1alpha1.LedgerVerbBrowser || !slices.Equal(browse.Artifacts, []string{sha256Hex(shot)}) {
		t.Errorf("index entry 4 %+v", browse)
	}
	wantReplay := []v1alpha1.CheckRun{{CheckDigest: pv.Spec.Checks[0].Digest, Result: v1alpha1.CheckRunPass,
		Token: "0123456789abcdef", Seq: 2}}
	if !slices.Equal(pv.Status.Replay, wantReplay) {
		t.Errorf("replay %+v", pv.Status.Replay)
	}
	confirms, confirmed := pv.Status.Confirmations, pv.Status.Confirmed
	if len(confirms) != 1 || confirms[0] != (v1alpha1.CheckRun{CheckDigest: confirms[0].CheckDigest,
		Result: v1alpha1.CheckRunPass, Token: "fedcba9876543210", Seq: 6, DryRunSeq: 3}) {
		t.Errorf("confirmations %+v", confirms)
	}
	if len(confirmed) != 1 || confirmed[0].ID != "items.echo" || confirmed[0].Outcome != "items" ||
		confirmed[0].Digest != confirms[0].CheckDigest {
		t.Errorf("confirmed %+v", confirmed)
	}
	if !strings.Contains(pv.Status.TesterImageID, "maul-preview-tools@sha256:") {
		t.Errorf("image %q", pv.Status.TesterImageID)
	}
}

// checkToolsConfigMaps holds the ledger and evidence ConfigMaps an
// Observed tool verification wrote.
func checkToolsConfigMaps(t *testing.T, e *verifyEnv, pv *v1alpha1.PreviewVerification, shot []byte) {
	t.Helper()
	ctx := context.Background()
	var ledger, evidence corev1.ConfigMap
	if err := e.c.Get(ctx, types.NamespacedName{Namespace: "maul", Name: pv.Name + ledgerSuffix}, &ledger); err != nil {
		t.Fatal(err)
	}
	if strings.Count(ledger.Data[ledgerKey], "\n") != 7 || ledger.Labels[v1alpha1.LabelIntent] != e.in.Name ||
		ledger.OwnerReferences[0].UID != pv.UID || !*ledger.OwnerReferences[0].Controller {
		t.Errorf("ledger ConfigMap %+v", ledger.ObjectMeta)
	}
	key := types.NamespacedName{Namespace: "maul", Name: pv.Name + evidenceSuffix}
	if err := e.c.Get(ctx, key, &evidence); err != nil {
		t.Fatal(err)
	}
	img := evidence.BinaryData[evidenceKey(4, sha256Hex(shot))]
	cfg, err := png.DecodeConfig(bytes.NewReader(img))
	if err != nil || cfg.Width != evidenceWidth || cfg.Height != 20 {
		t.Errorf("stored evidence %dx%d: %v", cfg.Width, cfg.Height, err)
	}
}

// A log that does not hold its summary is maul's trouble, never a
// verdict, and nothing of it is stored.
func TestToolsTamperedLogIsInfra(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	job := e.testerJob()
	pod := e.startToolPod(job, true)
	log, msg := toolLedger(t, testPNG(t, 4, 4))
	tampered := bytes.Replace(log, []byte(`"result":"pass"`), []byte(`"result":"fail"`), 1)
	e.finishToolPod(job, pod, tampered, msg)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != v1alpha1.VerificationReasonLedgerMismatch {
		t.Fatalf("phase %s %s, want Infra ledger-mismatch", pv.Status.Phase, pv.Status.Reason)
	}
	if pv.Status.Ledger != nil || pv.Status.Confirmed != nil {
		t.Errorf("a tampered ledger reached status: %+v", pv.Status.Ledger)
	}
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "maul", Name: pv.Name + ledgerSuffix},
		&corev1.ConfigMap{})
	if !kerrors.IsNotFound(err) {
		t.Errorf("ledger ConfigMap written: %v", err)
	}
}

// An evidence image whose bytes are not its sha256 is dropped and counted;
// the ledger still holds.
func TestToolsBadEvidenceIsDropped(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	job := e.testerJob()
	pod := e.startToolPod(job, true)
	shot := testPNG(t, 8, 8)
	log, msg := toolLedger(t, shot)
	b64 := func(b []byte) []byte { out, _ := json.Marshal(b); return bytes.Trim(out, `"`) }
	bad := bytes.Replace(log, b64(shot), b64(testPNG(t, 9, 9)), 1)
	if bytes.Equal(bad, log) {
		t.Fatal("fixture: the evidence was not replaced")
	}
	e.finishToolPod(job, pod, bad, msg)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationObserved || pv.Status.Ledger == nil ||
		pv.Status.Ledger.Evidence != "" {
		t.Fatalf("status %s %s, ledger %+v", pv.Status.Phase, pv.Status.Reason, pv.Status.Ledger)
	}
}

// A component the relay never reached is the application's.
func TestToolsUnreachableIsEnvFailed(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	job := e.testerJob()
	pod := e.startToolPod(job, false)
	log, msg := toolLedger(t, nil, "api")
	e.finishToolPod(job, pod, log, msg)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed ||
		pv.Status.Reason != v1alpha1.VerificationReasonComponentUnreachable {
		t.Fatalf("phase %s %s, want EnvFailed component-unreachable", pv.Status.Phase, pv.Status.Reason)
	}
	if len(pv.Status.Readiness) != 2 || pv.Status.Readiness[0].Target != "api" {
		t.Errorf("readiness %+v", pv.Status.Readiness)
	}
}

// A tool pod that never comes (a quota or LimitRange refusal leaves the
// Job Running with no pod) is Infra tools-not-ready past its bound.
func TestToolsNeverReadyIsInfra(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  func(*verifyEnv, *batchv1.Job)
		want v1alpha1.PreviewVerificationReason
	}{
		{"no pod", func(*verifyEnv, *batchv1.Job) {}, v1alpha1.VerificationReasonToolsNotReady},
		{"sidecar image", func(e *verifyEnv, job *batchv1.Job) {
			pod := e.startToolPod(job, false)
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: browserContainer,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
			e.mustStatus(pod)
		}, v1alpha1.VerificationReasonToolsImagePull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			e.verify()
			tc.pod(e, e.testerJob())
			if pv, _ := e.verify(); pv.Status.Phase != v1alpha1.VerificationRunning {
				t.Fatalf("settled %s within the grace", pv.Status.Phase)
			}
			e.now = e.now.Add(verifyGrace)
			pv, _ := e.verify()
			if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != tc.want {
				t.Fatalf("phase %s %s, want Infra %s", pv.Status.Phase, pv.Status.Reason, tc.want)
			}
			if len(e.jobs()) != 0 {
				t.Error("the tool Job outlived the settle")
			}
		})
	}
}

// An outcome verification that is not the Intent's approved one renders
// nothing; with the evaluator off it is maul's trouble.
func TestToolsPreChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mut    func(*v1alpha1.PreviewVerification)
		off    bool
		phase  v1alpha1.PreviewVerificationPhase
		reason v1alpha1.PreviewVerificationReason
	}{
		{"another outcome", func(pv *v1alpha1.PreviewVerification) { pv.Spec.Outcomes[0].Outcome = "anything" },
			false, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleCriteria},
		{"another check store", func(pv *v1alpha1.PreviewVerification) {
			pv.Spec.ChecksDigest = "sha256:" + strings.Repeat("d", 64)
		}, false, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleCriteria},
		{"a forged check", func(pv *v1alpha1.PreviewVerification) {
			pv.Spec.Checks[0].Body = strings.Replace(pv.Spec.Checks[0].Body, "/api/echo", "/api/admin", 1)
		}, false, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleCriteria},
		{"a check of no approved outcome", func(pv *v1alpha1.PreviewVerification) {
			c := savedCheck(t, strings.ReplaceAll(testCheckBody, `items`, `other`))
			pv.Spec.Checks = []v1alpha1.SavedCheck{c}
		}, false, v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleCriteria},
		{"evaluator off", nil, true, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			if err := e.c.Delete(context.Background(), e.pv); err != nil {
				t.Fatal(err)
			}
			e.pv = e.toolPV(tc.mut)
			if tc.off {
				e.v.Settings.Evaluator = false
			}
			pv, _ := e.verify()
			if pv.Status.Phase != tc.phase || pv.Status.Reason != tc.reason {
				t.Fatalf("phase %s %s, want %s %s", pv.Status.Phase, pv.Status.Reason, tc.phase, tc.reason)
			}
			if len(e.jobs()) != 0 {
				t.Error("a Job was rendered")
			}
		})
	}
}

// At most MaxEvaluations tool pods run at once: another waits.
func TestToolsWaitForRoom(t *testing.T) {
	e := newToolEnv(t)
	e.v.Settings.MaxEvaluations = 1
	other := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: "other-v1-a1", Namespace: "maul",
		UID: "pv-other"}, Spec: v1alpha1.PreviewVerificationSpec{Outcomes: testOutcomes()}}
	if err := e.c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	other.Status.Phase = v1alpha1.VerificationRunning
	e.mustStatus(other)
	e.now = e.now.Add(verifyGrace) // waiting for room is not the rollout's wait
	pv, res := e.verify()
	if pv.Status.Phase != "" || res.RequeueAfter == 0 || len(e.jobs()) != 0 {
		t.Fatalf("started beside a running evaluation: %+v", pv.Status)
	}
	other.Status.Phase = v1alpha1.VerificationObserved
	e.mustStatus(other)
	if pv, _ := e.verify(); pv.Status.Phase != v1alpha1.VerificationRunning {
		t.Errorf("phase %s once there was room", pv.Status.Phase)
	}
}

// Waiting for room is bounded by the verification's timeout.
func TestToolsRoomWaitIsBounded(t *testing.T) {
	e := newToolEnv(t)
	e.v.Settings.MaxEvaluations = 1
	other := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: "other-v1-a1", Namespace: "maul",
		UID: "pv-other"}, Spec: v1alpha1.PreviewVerificationSpec{Outcomes: testOutcomes()}}
	if err := e.c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	other.Status.Phase = v1alpha1.VerificationRunning
	e.mustStatus(other)
	e.now = e.now.Add(e.pv.Spec.Timeout.Duration + verifyGrace)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationInfra || pv.Status.Reason != v1alpha1.VerificationReasonToolsNotReady {
		t.Errorf("phase %s %s, want Infra tools-not-ready", pv.Status.Phase, pv.Status.Reason)
	}
}

// BeforeRelease records the slot of a running outcome verification with
// the tool pod as its own part, and tears the pod down.
func TestToolsBeforeRelease(t *testing.T) {
	e := newToolEnv(t)
	e.verify()
	job := e.testerJob()
	e.startToolPod(job, true)
	if err := e.v.BeforeRelease(context.Background(), e.p); err != nil {
		t.Fatal(err)
	}
	pv := e.currentPV()
	if pv.Status.Phase != v1alpha1.VerificationEnvFailed && pv.Status.Phase != v1alpha1.VerificationInfra {
		t.Fatalf("phase %s", pv.Status.Phase)
	}
	found := false
	for _, f := range pv.Status.Environment {
		found = found || (f.Name == toolsPart && f.Role == v1alpha1.PreviewRoleToolPod)
	}
	if !found {
		t.Errorf("environment %+v lacks the tool pod", pv.Status.Environment)
	}
	if !e.slotGone(job.Name, &batchv1.Job{}) || !e.slotGone(toolName(e.p, pv, toolKeyInfix), &corev1.Secret{}) {
		t.Error("the tool pod outlived the release")
	}
}

// The settle path's environment capture keeps component log tails in status:
// the CDP key is redacted from them, and with a browser tool pod's key
// unknown (a restart) no tail is kept at all.
func TestEnvironmentRedactsCDPKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
		want    string
	}{
		{"key held", false, "GET / 200 leaked [redacted]\n"},
		{"key unknown", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t)
			e.verify()
			key := e.v.toolStateOf(e.pv.UID).key
			if key == "" {
				t.Fatal("no CDP key held")
			}
			if tc.restart {
				e.v.forgetTools(e.pv.UID)
			}
			web := componentName(e.p, 0) + "-pod"
			e.logs.logs["maul-preview-0/"+web+"/web"] = []byte("GET / 200 leaked " + key + "\n")
			facts, err := e.v.environment(context.Background(), e.p, 0, nil, e.pv)
			if err != nil {
				t.Fatal(err)
			}
			var got *string
			for i := range facts {
				if facts[i].Name == "web" {
					got = &facts[i].LogTail
				}
			}
			if got == nil || *got != tc.want {
				t.Errorf("web log tail %v, want %q", got, tc.want)
			}
		})
	}
}
