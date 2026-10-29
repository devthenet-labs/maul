// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The evaluator's view of the slot (DESIGN 2.9): per component, a redacted
// log tail since the verification started, and the slot's events and the
// components' container states. pc reads them (the evaluator and the tool
// pod hold no Kubernetes access) and writes them into the PV's slot tool
// ConfigMap on each poll, where `maul-tool logs <component>` and
// `maul-tool events` read them.
const (
	// captureLogPrefix keys a component's tail in the tool ConfigMap:
	// logs.<component>.
	captureLogPrefix = "logs."
	// captureEventsKey keys the events and container states, a JSON array
	// of capturedEvent, oldest first.
	captureEventsKey = "events.json"
	// captureObserved marks what the kubelet or the API server wrote (an
	// event, a container state), as against the application's own output.
	captureObserved = "observed"

	// captureTailBytes is the most of a component's log kept per round,
	// the newest (ComponentCapture.Bytes' maximum).
	captureTailBytes = 64 << 10
	// captureReadLimit and captureReadLines bound one log read; a read that
	// fills its byte limit is retried for fewer lines (captureReadRetries
	// times, each an eighth), since the API server applies the line tail
	// first and then keeps the FIRST bytes of it.
	captureReadLimit   = 1 << 20
	captureReadLines   = 4000
	captureReadRetries = 2
	// maxCaptureEvents and maxCaptureEventBytes bound the events kept (the
	// newest); maxCaptureMessage bounds each one's message.
	maxCaptureEvents     = 200
	maxCaptureEventBytes = 64 << 10
	maxCaptureMessage    = 1 << 10

	// redactedMark replaces every redacted value.
	redactedMark = "[redacted]"
	// minRedactedKey is the shortest key redacted: a shorter one would only
	// mangle ordinary words (every key pc mints is far longer).
	minRedactedKey = 16
)

// capturedEvent is one slot event (kind "event") or container state (kind
// "state"), as tools serves it. Everything in it is observed; Message is
// the kubelet's or the API server's text, redacted and bounded, which can
// still quote the application (a probe's output), so tools frames it as
// data.
type capturedEvent struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	Component string    `json:"component,omitempty"`
	Object    string    `json:"object"`
	Container string    `json:"container,omitempty"`
	Type      string    `json:"type,omitempty"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message,omitempty"`
	Count     int32     `json:"count,omitempty"`
	ExitCode  *int32    `json:"exitCode,omitempty"`
	Restarts  int32     `json:"restarts,omitempty"`
}

// componentLog is one component's captured tail; read is false when this
// poll could not read it (the last poll's tail then stands).
type componentLog struct {
	name      string
	text      string
	truncated bool
	read      bool
}

// slotCapture is one poll's capture.
type slotCapture struct {
	logs   []componentLog
	events []capturedEvent
}

func captureLogKey(component string) string { return captureLogPrefix + component }

// refreshCapture captures the slot for pv and writes it into cm, the PV's
// slot tool ConfigMap (updated only when it changed), and its summary into
// pv.Status.Capture (the caller writes the status). keys are the PV's own
// secrets (the CDP and tool keys), redacted wherever they appear.
func (v *VerificationReconciler) refreshCapture(ctx context.Context, pv *v1alpha1.PreviewVerification,
	p *v1alpha1.Preview, slot int32, cm *corev1.ConfigMap, keys []string) error {
	c, err := v.captureSlot(ctx, pv, p, slot, keys)
	if err != nil {
		return err
	}
	data, err := c.data(cm.Data)
	if err != nil {
		return err
	}
	if !maps.Equal(data, cm.Data) {
		cm.Data = data
		if err := v.Update(ctx, cm); err != nil {
			return fmt.Errorf("write the capture into ConfigMap %s/%s: %w", cm.Namespace, cm.Name, err)
		}
	}
	pv.Status.Capture = c.summary(pv.Status.Capture)
	return nil
}

// captureSlot reads every component's log tail since the PV started and the
// slot's events and component container states since then.
func (v *VerificationReconciler) captureSlot(ctx context.Context, pv *v1alpha1.PreviewVerification,
	p *v1alpha1.Preview, slot int32, keys []string) (slotCapture, error) {
	since, now := startedAt(pv), v.now()
	var c slotCapture
	podComponents := map[string]string{}
	for i, comp := range p.Spec.Components {
		all, err := v.allComponentPods(ctx, p, i, slot)
		if err != nil {
			return slotCapture{}, err
		}
		for _, pod := range all {
			podComponents[pod.Name] = comp.Name
		}
		c.events = append(c.events, containerStates(all, comp.Name, since, now, keys)...)
		l := componentLog{name: comp.Name}
		live := slices.DeleteFunc(slices.Clone(all), func(pod corev1.Pod) bool { return !livePod(&pod) })
		pod := currentPod(live, p, comp)
		if pod == nil {
			pod = currentPod(slices.Clone(all), p, comp)
		}
		if pod != nil {
			l.text, l.truncated, l.read = v.readTail(ctx, pod, comp.Name, since, keys)
		}
		c.logs = append(c.logs, l)
	}
	var events corev1.EventList
	if err := v.List(ctx, &events, client.InNamespace(v.Settings.slotName(slot))); err != nil {
		return slotCapture{}, fmt.Errorf("list the events of slot %d: %w", slot, err)
	}
	c.events = boundEvents(append(c.events, slotEvents(events.Items, podComponents, since, keys)...))
	return c, nil
}

// readTail reads container's log in pod since since, redacts it and keeps
// its newest captureTailBytes. ok is false when it could not be read.
func (v *VerificationReconciler) readTail(ctx context.Context, pod *corev1.Pod, container string,
	since time.Time, keys []string) (tail string, truncated, ok bool) {
	if v.Logs == nil {
		return "", false, false
	}
	lines := int64(captureReadLines)
	var b []byte
	for try := 0; ; try++ {
		opts := LogOptions{LimitBytes: captureReadLimit, TailLines: &lines, SinceTime: &since}
		var err error
		if b, err = v.Logs.ReadLog(ctx, pod.Namespace, pod.Name, container, opts); err != nil {
			return "", false, false
		}
		if int64(len(b)) < captureReadLimit || try == captureReadRetries {
			break
		}
		truncated = true
		lines = max(lines/8, 1)
	}
	if int64(len(b)) >= captureReadLimit || int64(strings.Count(string(b), "\n")) >= lines {
		truncated = true
	}
	text := redact(strings.ToValidUTF8(string(b), "�"), keys)
	text, cut := newestTail(text, captureTailBytes)
	return text, truncated || cut, true
}

// newestTail is the last at most n bytes of s, cut on a rune boundary and,
// where the kept part holds one, just after a line break; cut reports a
// drop.
func newestTail(s string, n int) (tail string, cut bool) {
	if len(s) <= n {
		return s, false
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	tail = s[start:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	return tail, true
}

// The redaction patterns. A JWT is three (JWS) to five (JWE) base64url
// segments, the first a JSON object's ("eyJ"); a line break may fall
// anywhere in it (a wrapping logger), so runs of base64url characters may be
// joined by one. That can take the first word of the next line with it,
// which errs on the side of hiding. A Bearer value may follow on the next
// line; an Authorization value runs to the end of its line or quote.
var (
	b64Run     = `[A-Za-z0-9_-]+(?:\r?\n[A-Za-z0-9_-]+)*`
	jwtPattern = regexp.MustCompile(`eyJ(?:[A-Za-z0-9_-]|\r?\n[A-Za-z0-9_-])*` +
		`(?:(?:\r?\n)?\.(?:\r?\n)?(?:` + b64Run + `)?){1,4}`)
	authorizationPattern = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization)(["']?[ \t]*[:=][ \t]*["']?)` +
		`[^\r\n"']+`)
	bearerPattern = regexp.MustCompile(`(?i)\b(bearer)(\s+)[A-Za-z0-9._~+/-]+=*`)
)

// redact removes from s every key (at least minRedactedKey long, a line
// break allowed between any two of its characters), every JWT-shaped string
// and every Bearer and Authorization value.
func redact(s string, keys []string) string {
	for _, k := range keys {
		if len(k) < minRedactedKey || !strings.Contains(stripBreaks(s), k) {
			continue
		}
		s = keyPattern(k).ReplaceAllLiteralString(s, redactedMark)
	}
	s = jwtPattern.ReplaceAllLiteralString(s, redactedMark)
	s = authorizationPattern.ReplaceAllString(s, "${1}${2}"+redactedMark)
	return bearerPattern.ReplaceAllString(s, "${1}${2}"+redactedMark)
}

// stripBreaks is s without its line breaks, a cheap pre-check before a
// key's pattern is compiled.
func stripBreaks(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// keyPattern matches k literally, a line break allowed between any two of
// its characters.
func keyPattern(k string) *regexp.Regexp {
	parts := make([]string, 0, len(k))
	for _, r := range k {
		parts = append(parts, regexp.QuoteMeta(string(r)))
	}
	return regexp.MustCompile(strings.Join(parts, `(?:\r?\n)?`))
}

// capturedReasons are the Normal events kept beside every Warning: a
// container killed (a failed liveness probe restarts it) or a Pod evicted
// or preempted.
var capturedReasons = []string{"Killing", "Evicted", "Preempted", "Preempting"}

// slotEvents are the events from since on that tell of trouble (Warning, or
// a Normal kill or eviction), oldest first, each Pod's mapped to its
// component through pods (Pod name to component), messages redacted (keys
// as redact's) and bounded.
func slotEvents(events []corev1.Event, pods map[string]string, since time.Time, keys []string) []capturedEvent {
	var out []capturedEvent
	for _, e := range events {
		at := eventTime(&e)
		if at.Before(since) || (e.Type != corev1.EventTypeWarning && !slices.Contains(capturedReasons, e.Reason)) {
			continue
		}
		ce := capturedEvent{Time: at, Kind: "event", Source: captureObserved,
			Object: kubeletWord(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name, Type: kubeletWord(e.Type),
			Reason: kubeletWord(e.Reason), Message: boundMessage(redact(e.Message, keys)), Count: max(e.Count, 0)}
		if e.InvolvedObject.Kind == "Pod" {
			ce.Component = pods[e.InvolvedObject.Name]
		}
		if e.Series != nil {
			ce.Count = max(e.Series.Count, ce.Count)
		}
		out = append(out, ce)
	}
	sortEvents(out)
	return out
}

// eventTime is when an event last happened.
func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case !e.FirstTimestamp.IsZero():
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

// startingUp are the waiting reasons of a container on its way up, which
// are no trouble.
var startingUp = []string{"", "ContainerCreating", "PodInitializing"}

// containerStates is what the kubelet reports of component's Pods from since
// on: each container's terminations (OOMKilled, Error, with exit code and
// restarts), a live Pod's waiting reason other than starting up
// (CrashLoopBackOff, an image pull back-off) as of now, and an eviction. A
// container's own termination message is the application's text and is
// never kept; the kubelet's messages are redacted (keys as redact's).
func containerStates(pods []corev1.Pod, component string, since, now time.Time, keys []string) []capturedEvent {
	var out []capturedEvent
	for i := range pods {
		pod := &pods[i]
		state := func(at time.Time, container, reason, msg string) capturedEvent {
			return capturedEvent{Time: at, Kind: "state", Source: captureObserved, Component: component,
				Object: "Pod/" + pod.Name, Container: container, Reason: kubeletWord(reason),
				Message: boundMessage(redact(msg, keys))}
		}
		for _, s := range pod.Status.ContainerStatuses {
			for _, term := range []*corev1.ContainerStateTerminated{s.LastTerminationState.Terminated,
				s.State.Terminated} {
				if term == nil || term.FinishedAt.Time.Before(since) {
					continue
				}
				e := state(term.FinishedAt.Time, s.Name, term.Reason, "")
				e.ExitCode, e.Restarts = ptr(term.ExitCode), max(s.RestartCount, 0)
				out = append(out, e)
			}
			if w := s.State.Waiting; w != nil && livePod(pod) && !slices.Contains(startingUp, w.Reason) {
				e := state(now, s.Name, w.Reason, w.Message)
				e.Restarts = max(s.RestartCount, 0)
				out = append(out, e)
			}
		}
		if pod.Status.Reason == "Evicted" {
			at := now
			if c := podConditionOf(pod, corev1.DisruptionTarget); c != nil && !c.LastTransitionTime.IsZero() {
				at = c.LastTransitionTime.Time
			}
			if !at.Before(since) {
				out = append(out, state(at, "", pod.Status.Reason, pod.Status.Message))
			}
		}
	}
	return out
}

// boundMessage keeps a message's first maxCaptureMessage bytes, valid UTF-8.
func boundMessage(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= maxCaptureMessage {
		return s
	}
	cut := maxCaptureMessage
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func sortEvents(events []capturedEvent) {
	slices.SortStableFunc(events, func(a, b capturedEvent) int {
		return cmp.Or(a.Time.Compare(b.Time), strings.Compare(a.Object, b.Object))
	})
}

// boundEvents sorts events oldest first and keeps the newest that fit
// maxCaptureEvents and maxCaptureEventBytes of JSON.
func boundEvents(events []capturedEvent) []capturedEvent {
	sortEvents(events)
	if len(events) > maxCaptureEvents {
		events = events[len(events)-maxCaptureEvents:]
	}
	size := 2 // the array's brackets
	for i := len(events) - 1; i >= 0; i-- {
		size += jsonSize(events[i]) + 1
		if size > maxCaptureEventBytes {
			return events[i+1:]
		}
	}
	return events
}

// data is the tool ConfigMap's data with this capture in it: every other
// key kept, each component's tail written (or kept from old when this poll
// could not read it), tails of components no longer in the Preview dropped,
// and the events replaced.
func (c slotCapture) data(old map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(old)+len(c.logs)+1)
	for k, val := range old {
		if !strings.HasPrefix(k, captureLogPrefix) {
			out[k] = val
		}
	}
	for _, l := range c.logs {
		key := captureLogKey(l.name)
		if l.read {
			out[key] = l.text
		} else if val, ok := old[key]; ok {
			out[key] = val
		}
	}
	events := c.events
	if events == nil {
		events = []capturedEvent{}
	}
	b, err := json.Marshal(events)
	if err != nil {
		return nil, fmt.Errorf("encode the captured events: %w", err)
	}
	out[captureEventsKey] = string(b)
	return out, nil
}

// summary is the capture's status summary; a component this poll could not
// read keeps its entry in prev.
func (c slotCapture) summary(prev *v1alpha1.CaptureSummary) *v1alpha1.CaptureSummary {
	s := &v1alpha1.CaptureSummary{Events: int32(len(c.events))}
	for _, l := range c.logs {
		entry := v1alpha1.ComponentCapture{Name: l.name, Bytes: int32(len(l.text)), Truncated: l.truncated}
		if !l.read {
			if prev == nil {
				continue
			}
			i := slices.IndexFunc(prev.Components, func(p v1alpha1.ComponentCapture) bool { return p.Name == l.name })
			if i < 0 {
				continue
			}
			entry = prev.Components[i]
		}
		s.Components = append(s.Components, entry)
	}
	return s
}
