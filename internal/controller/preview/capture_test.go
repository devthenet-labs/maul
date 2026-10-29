// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// A JWT as a real issuer prints one: base64url header, payload and
// signature, '-' and '_' included.
const testJWT = "eyJhbGciOiJSUzI1NiIsImtpZCI6Ik1fZC0xIn0.eyJzdWIiOiJ1LTEiLCJhdWQiOlsibWF1bCJdfQ." +
	"Qm9vX3NpZy1uYXR1cmVfLWJ5dGVz"

const testKey = "k3y-0f-the-pv-9f8e7d6c5b4a"

// TestRedact pins what pc removes from a log tail before tools sees it:
// JWT-shaped strings (also split across a line break), Bearer and
// Authorization values, and the PV's own keys (also split); everything else
// is kept byte for byte.
func TestRedact(t *testing.T) {
	half := len(testJWT) / 2
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain text kept", "GET /api/items 200 12ms\n", "GET /api/items 200 12ms\n"},
		{"a jwt", "token=" + testJWT + " ok", "token=[redacted] ok"},
		{"a jwt split across a line break", "t " + testJWT[:half] + "\n" + testJWT[half:] + " ok",
			"t [redacted] ok"},
		{"a jwt split at its dot", "t " + strings.Replace(testJWT, ".", "\n.", 1) + " ok", "t [redacted] ok"},
		{"an unsigned jwt", "t eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1In0. ok", "t [redacted] ok"},
		{"a five-part jwe", "t eyJhbGciOiJBMjU2In0.a-b.c_d.e-f.g_h ok", "t [redacted] ok"},
		{"a bearer value", "sent Bearer abc.DEF-123_~+/== to api", "sent Bearer [redacted] to api"},
		{"a bearer value on the next line", "sent bearer\n  abc123 ok", "sent bearer\n  [redacted] ok"},
		{"an authorization header", "Authorization: Basic dXNlcjpwYXNz\nnext", "Authorization: [redacted]\nnext"},
		{"an authorization in json", `{"authorization": "Token s3cr3t", "x": 1}`,
			`{"authorization": "[redacted]", "x": 1}`},
		{"a proxy-authorization header", "Proxy-Authorization=abc def\n", "Proxy-Authorization=[redacted]\n"},
		{"the word authorization in prose", "authorization failed for user", "authorization failed for user"},
		{"the pv's key", "cdp key " + testKey + " used", "cdp key [redacted] used"},
		{"the pv's key inside a word", "x" + testKey + "y", "x[redacted]y"},
		{"the pv's key split across a line break", "k " + testKey[:7] + "\r\n" + testKey[7:] + " end",
			"k [redacted] end"},
		{"a short key is ignored", "the cat sat", "the cat sat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redact(tc.in, []string{testKey, "cat", ""}); got != tc.want {
				t.Errorf("redact(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Redaction never leaves a secret's bytes in the output, wherever the line
// breaks fall.
func TestRedactEveryBreak(t *testing.T) {
	for i := 1; i < len(testJWT); i++ {
		in := "a " + testJWT[:i] + "\n" + testJWT[i:] + " b"
		if got := redact(in, nil); strings.Contains(got, "Qm9vX3NpZy1uYXR1cmVf") ||
			strings.Contains(got, "eyJzdWIi") {
			t.Fatalf("break at %d leaks: %q", i, got)
		}
	}
	for i := 1; i < len(testKey); i++ {
		in := "a " + testKey[:i] + "\n" + testKey[i:] + " b"
		if got := redact(in, []string{testKey}); got != "a [redacted] b" {
			t.Fatalf("key break at %d: %q", i, got)
		}
	}
}

// newestTail keeps the newest n bytes, on a rune and, where it can, a line
// boundary, and reports whether anything was dropped.
func TestNewestTail(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        string
		n         int
		want      string
		truncated bool
	}{
		{"fits", "a\nb\n", 8, "a\nb\n", false},
		{"cut on a line", "one\ntwo\nthree\n", 9, "three\n", true},
		{"one long line cut on a rune", "ééééé", 5, "éé", true},
		{"no newline in the kept part", "x\nabcdefgh", 5, "defgh", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, tr := newestTail(tc.in, tc.n)
			if got != tc.want || tr != tc.truncated {
				t.Errorf("newestTail(%q, %d) = %q, %v; want %q, %v", tc.in, tc.n, got, tr, tc.want, tc.truncated)
			}
			if len(got) > tc.n || !utf8.ValidString(got) {
				t.Errorf("tail %q over %d bytes or invalid", got, tc.n)
			}
		})
	}
}

// event is a slot event on object kind/name at time at.
func event(name, kind, obj, typ, reason, msg string, at time.Time) corev1.Event {
	return corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "maul-preview-0"},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: obj, Namespace: "maul-preview-0"},
		Type:           typ, Reason: reason, Message: msg, Count: 1, LastTimestamp: metav1.NewTime(at)}
}

// slotEvents keeps the slot's Warning events, and the Normal ones that
// report a kill or an eviction, from since on, newest last; maps a Pod to its
// component; and redacts and bounds the message.
func TestSlotEvents(t *testing.T) {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	pods := map[string]string{"web-pod": "web"}
	in := []corev1.Event{
		event("e1", "Pod", "web-pod", corev1.EventTypeWarning, "Unhealthy",
			"Readiness probe failed: Authorization: Bearer abc", since.Add(2*time.Minute)),
		event("e2", "Pod", "web-pod", corev1.EventTypeWarning, "BackOff", "back-off", since.Add(-time.Minute)),
		event("e3", "Pod", "web-pod", corev1.EventTypeNormal, "Pulled", "pulled", since.Add(time.Minute)),
		event("e4", "Pod", "web-pod", corev1.EventTypeNormal, "Killing", "liveness", since.Add(time.Minute)),
		event("e5", "ReplicaSet", "web-rs", corev1.EventTypeWarning, "FailedCreate", "quota", since.Add(3*time.Minute)),
		event("e6", "Pod", "other", corev1.EventTypeWarning, "OOMKilling", strings.Repeat("x", 4000),
			since.Add(4*time.Minute)),
	}
	got := slotEvents(in, pods, since, nil)
	reasons := make([]string, 0, len(got))
	for _, e := range got {
		reasons = append(reasons, e.Reason)
		if e.Source != captureObserved || e.Kind != "event" {
			t.Errorf("%s: source %q kind %q", e.Reason, e.Source, e.Kind)
		}
	}
	if fmt.Sprint(reasons) != "[Killing Unhealthy FailedCreate OOMKilling]" {
		t.Fatalf("reasons %v", reasons)
	}
	if got[1].Component != "web" || got[1].Object != "Pod/web-pod" || got[2].Component != "" ||
		got[2].Object != "ReplicaSet/web-rs" {
		t.Errorf("objects %+v", got)
	}
	if strings.Contains(got[1].Message, "abc") {
		t.Errorf("message not redacted: %q", got[1].Message)
	}
	if len(got[3].Message) > maxCaptureMessage {
		t.Errorf("message of %d bytes", len(got[3].Message))
	}
}

// The newest events are kept within the count and byte bounds.
func TestBoundEvents(t *testing.T) {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	in := make([]corev1.Event, 0, maxCaptureEvents+50)
	for i := range maxCaptureEvents + 50 {
		in = append(in, event(fmt.Sprint("e", i), "Pod", "p", corev1.EventTypeWarning, "BackOff",
			strings.Repeat("m", maxCaptureMessage), since.Add(time.Duration(i)*time.Second)))
	}
	got := boundEvents(slotEvents(in, nil, since, nil))
	if len(got) == 0 || len(got) > maxCaptureEvents {
		t.Fatalf("%d events kept", len(got))
	}
	b, _ := json.Marshal(got)
	if len(b) > maxCaptureEventBytes {
		t.Errorf("events of %d bytes", len(b))
	}
	if last := got[len(got)-1]; !last.Time.Equal(since.Add(time.Duration(maxCaptureEvents+49) * time.Second)) {
		t.Errorf("newest dropped: last at %v", last.Time)
	}
}

// containerStates reports what the kubelet says about a component's
// containers from since on: a termination (OOMKilled with its exit code and
// restarts), a waiting reason other than starting up (CrashLoopBackOff), an
// eviction; older terminations and the container's own termination message
// (application text) are left out.
func TestContainerStates(t *testing.T) {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := since.Add(5 * time.Minute)
	term := func(reason string, at time.Time) *corev1.ContainerStateTerminated {
		return &corev1.ContainerStateTerminated{Reason: reason, ExitCode: 137, FinishedAt: metav1.NewTime(at),
			Message: "app says Bearer zzz"}
	}
	pod := func(name string, s corev1.ContainerStatus) corev1.Pod {
		s.Name = "web"
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{s}}}
	}
	oom := pod("p1", corev1.ContainerStatus{RestartCount: 3,
		LastTerminationState: corev1.ContainerState{Terminated: term("OOMKilled", since.Add(time.Minute))},
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff",
			Message: "back-off 40s restarting failed container"}}})
	old := pod("p2", corev1.ContainerStatus{RestartCount: 1,
		LastTerminationState: corev1.ContainerState{Terminated: term("Error", since.Add(-time.Minute))},
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}})
	evicted := pod("p3", corev1.ContainerStatus{})
	evicted.Status.Phase, evicted.Status.Reason = corev1.PodFailed, "Evicted"
	evicted.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(since.Add(2 * time.Minute))}}

	got := containerStates([]corev1.Pod{oom, old, evicted}, "web", since, now, nil)
	seen := make([]string, 0, len(got))
	for _, s := range got {
		seen = append(seen, s.Object+":"+s.Reason)
		if s.Source != captureObserved || s.Kind != "state" || s.Component != "web" {
			t.Errorf("%+v", s)
		}
		if strings.Contains(s.Message, "zzz") {
			t.Errorf("termination message kept: %q", s.Message)
		}
	}
	if fmt.Sprint(seen) != "[Pod/p1:OOMKilled Pod/p1:CrashLoopBackOff Pod/p3:Evicted]" {
		t.Fatalf("states %v", seen)
	}
	if got[0].ExitCode == nil || *got[0].ExitCode != 137 || got[0].Restarts != 3 {
		t.Errorf("oom %+v", got[0])
	}
}

// boundedLogs serves one log, applying TailLines and LimitBytes as the API
// server does (tail first, then the first LimitBytes of it), and records
// each read.
type boundedLogs struct {
	log   string
	err   error
	reads []LogOptions
}

func (b *boundedLogs) ReadLog(_ context.Context, _, _, _ string, opts LogOptions) ([]byte, error) {
	b.reads = append(b.reads, opts)
	if b.err != nil {
		return nil, b.err
	}
	lines := strings.SplitAfter(b.log, "\n")
	if opts.TailLines != nil && int64(len(lines)) > *opts.TailLines {
		lines = lines[int64(len(lines))-*opts.TailLines:]
	}
	s := strings.Join(lines, "")
	if int64(len(s)) > opts.LimitBytes {
		s = s[:opts.LimitBytes]
	}
	return []byte(s), nil
}

// readTail reads since the PV started, and when a read fills its byte
// limit asks again for fewer lines, so the newest output is what is kept.
func TestReadTailKeepsTheNewest(t *testing.T) {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var b strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&b, "%06d %s\n", i, strings.Repeat("y", 300))
	}
	logs := &boundedLogs{log: b.String()}
	v := &VerificationReconciler{Logs: logs}
	got, truncated, ok := v.readTail(context.Background(), &corev1.Pod{}, "web", since, nil)
	if !ok || !truncated {
		t.Fatalf("ok %v truncated %v", ok, truncated)
	}
	if len(got) > captureTailBytes || !strings.HasSuffix(got, "019999 "+strings.Repeat("y", 300)+"\n") {
		t.Errorf("tail of %d bytes ends %q", len(got), got[max(0, len(got)-20):])
	}
	if len(logs.reads) < 2 || len(logs.reads) > 4 {
		t.Errorf("%d reads", len(logs.reads))
	}
	for _, r := range logs.reads {
		if r.SinceTime == nil || !r.SinceTime.Equal(since) || r.LimitBytes != captureReadLimit {
			t.Errorf("read %+v", r)
		}
	}
	short := &boundedLogs{log: "hello " + testJWT + "\n"}
	v.Logs = short
	got, truncated, ok = v.readTail(context.Background(), &corev1.Pod{}, "web", since, nil)
	if !ok || truncated || got != "hello [redacted]\n" || len(short.reads) != 1 {
		t.Errorf("short log %q %v %v, %d reads", got, truncated, ok, len(short.reads))
	}
}

// toolConfigMap is the PV's slot tool ConfigMap as S1-13 renders it, with
// data of its own the capture leaves alone.
func (e *verifyEnv) toolConfigMap() *corev1.ConfigMap {
	e.t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "tools-" + string(e.pv.UID),
		Namespace: "maul-preview-0"}, Data: map[string]string{"spec.json": "{}", "logs.gone": "old"}}
	if err := e.c.Create(context.Background(), cm); err != nil {
		e.t.Fatal(err)
	}
	return cm
}

// refreshCapture writes each component's redacted tail and the slot's
// events into the tool ConfigMap, leaves the ConfigMap's other data alone,
// drops tails of components no longer there, summarises on the PV status,
// and keeps a tail it could not read this poll as it was.
func TestRefreshCapture(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	ctx := context.Background()
	pv := e.currentPV()
	since := startedAt(pv)
	web, api := componentName(e.p, 0)+"-pod", componentName(e.p, 1)+"-pod"
	e.logs.logs["maul-preview-0/"+web+"/web"] = []byte("GET / 200 Authorization: Bearer x\n")
	e.logs.logs["maul-preview-0/"+api+"/api"] = []byte(strings.Repeat("z", captureTailBytes+10))
	for _, ev := range []corev1.Event{
		event("w1", "Pod", api, corev1.EventTypeWarning, "Unhealthy", "probe "+testKey, since.Add(time.Second)),
		event("w0", "Pod", api, corev1.EventTypeWarning, "BackOff", "old", since.Add(-time.Hour)),
	} {
		if err := e.c.Create(ctx, &ev); err != nil {
			t.Fatal(err)
		}
	}
	cm := e.toolConfigMap()
	if err := e.v.refreshCapture(ctx, pv, e.p, 0, cm, []string{testKey}); err != nil {
		t.Fatal(err)
	}
	var stored corev1.ConfigMap
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(cm), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Data["spec.json"] != "{}" {
		t.Error("the ConfigMap's own data changed")
	}
	if _, ok := stored.Data["logs.gone"]; ok {
		t.Error("a departed component's tail kept")
	}
	if got := stored.Data[captureLogKey("web")]; got != "GET / 200 Authorization: [redacted]\n" {
		t.Errorf("web tail %q", got)
	}
	if got := stored.Data[captureLogKey("api")]; len(got) != captureTailBytes {
		t.Errorf("api tail of %d bytes", len(got))
	}
	var evs []capturedEvent
	if err := json.Unmarshal([]byte(stored.Data[captureEventsKey]), &evs); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Reason != "Unhealthy" || evs[0].Component != "api" ||
		strings.Contains(evs[0].Message, testKey) {
		t.Errorf("events %+v", evs)
	}
	want := v1alpha1.CaptureSummary{Components: []v1alpha1.ComponentCapture{
		{Name: "web", Bytes: int32(len("GET / 200 Authorization: [redacted]\n"))},
		{Name: "api", Bytes: captureTailBytes, Truncated: true}}, Events: 1}
	if pv.Status.Capture == nil || fmt.Sprint(*pv.Status.Capture) != fmt.Sprint(want) {
		t.Errorf("summary %+v, want %+v", pv.Status.Capture, want)
	}

	// A poll whose reads fail keeps what the last one captured.
	e.v.Logs = &boundedLogs{err: fmt.Errorf("apiserver unavailable")}
	if err := e.v.refreshCapture(ctx, pv, e.p, 0, &stored, []string{testKey}); err != nil {
		t.Fatal(err)
	}
	var again corev1.ConfigMap
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(cm), &again); err != nil {
		t.Fatal(err)
	}
	if again.Data[captureLogKey("web")] != stored.Data[captureLogKey("web")] ||
		fmt.Sprint(*pv.Status.Capture) != fmt.Sprint(want) {
		t.Errorf("an unread tail was not kept: %q, %+v", again.Data[captureLogKey("web")], pv.Status.Capture)
	}
}
