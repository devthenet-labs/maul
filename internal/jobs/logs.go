// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/transcript"
)

// maxEventLine bounds one log line while scanning for events: a remediation
// event carries the changeset's base64 file contents (5 MiB cap → ~7 MiB
// encoded), so allow generous headroom.
const maxEventLine = 32 << 20

// ErrLogsGone marks a finished Job whose agent log can never be read: the
// pod was deleted, the kubelet removed the terminated container with its log
// (as it does with an evicted pod's), the agent never ran, a line of the log
// is too long to ever scan, or every read of it has failed for LogReadGrace.
// A collector settles such a run with its no-result outcome rather than
// retrying, since a retry cannot succeed and would only hold the run until
// the Job's TTL deletes it. Every other read error is worth retrying.
var ErrLogsGone = errors.New("the agent log is gone")

// LogReadGrace bounds the retries of a finished Job's log that keeps failing
// to read for a reason the pod does not explain. It is counted from the first
// failed read this process saw, not from when the Job finished, so a
// collector that falls behind still retries a log it has only just failed
// to read.
const LogReadGrace = 5 * time.Minute

// staleFailure is how long a first-failure record is kept before it is
// pruned: far past LogReadGrace, so a record still being retried is never
// dropped, while one whose run settled some other way does not linger.
const staleFailure = 12 * LogReadGrace

// readFailures remembers, per agent pod, when this process first failed to
// read its log, so LogReadGrace bounds the retries from that moment.
type readFailures struct {
	mu    sync.Mutex
	first map[string]time.Time
}

// since records now as pod's first failure unless one is recorded, prunes
// stale records, and returns how long pod's log has been failing.
func (f *readFailures) since(pod string, now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.first == nil {
		f.first = map[string]time.Time{}
	}
	for name, at := range f.first {
		if now.Sub(at) > staleFailure {
			delete(f.first, name)
		}
	}
	at, ok := f.first[pod]
	if !ok {
		f.first[pod] = now
		return 0
	}
	return now.Sub(at)
}

// forget drops pod's record: its log read, or it settled as gone.
func (f *readFailures) forget(pod string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.first, pod)
}

// logReader opens a pod's container log stream; the indirection exists so
// tests can feed canned or piped logs (the fake clientset cannot serve
// custom log bodies).
type logReader interface {
	Stream(ctx context.Context, pod, container string, follow bool) (io.ReadCloser, error)
}

// podLogs is the real logReader.
type podLogs struct {
	cs        kubernetes.Interface
	namespace string
}

func (p podLogs) Stream(ctx context.Context, pod, container string, follow bool) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{Container: container, Follow: follow}
	return p.cs.CoreV1().Pods(p.namespace).GetLogs(pod, opts).Stream(ctx)
}

// RunOutput is everything one agent Job's log carried: the stage results the
// controller routes on, and the conversation that produced them. Both come
// from a single read — the changeset alone can be several MiB, so reading the
// log twice to separate them would be wasteful.
type RunOutput struct {
	Events []envelope.Event
	Turns  []transcript.Turn
}

// Result waits for the agent container to finish, then reads its full logs
// and returns everything found — the idempotent fallback/reconciliation path.
// Waiting for termination is what makes the read complete: a non-follow read
// of a still-running container would miss every event emitted after it.
//
// A log that can never be read is ErrLogsGone (see there).
func (c *Client) Result(ctx context.Context, jobName string) (RunOutput, error) {
	stream, pod, err := c.openFinished(ctx, jobName)
	if err != nil {
		return RunOutput{}, err
	}
	defer func() { _ = stream.Close() }()

	var out RunOutput
	err = scanLog(stream, func(e envelope.Event) error {
		out.Events = append(out.Events, e)
		return nil
	}, func(t transcript.Turn) error {
		out.Turns = append(out.Turns, t)
		return nil
	})
	if err != nil {
		// Neither handler fails, so this is the read itself failing.
		return RunOutput{}, c.readFailed(ctx, jobName, pod, err)
	}
	c.failures.forget(pod)
	return out, nil
}

// ResultLines waits for the agent container to finish, then reads its full
// log and offers every raw line to fn. It is the envelope-agnostic sibling of
// Result: the evaluation collector decodes its own event vocabulary from the
// lines, so this package never learns it. A slice passed to fn is only valid
// for the duration of the call. A log that can never be read is ErrLogsGone,
// as for Result.
func (c *Client) ResultLines(ctx context.Context, jobName string, fn func(line []byte) error) error {
	stream, pod, err := c.openFinished(ctx, jobName)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 64<<10), maxEventLine)
	for sc.Scan() {
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return c.readFailed(ctx, jobName, pod, err)
	}
	c.failures.forget(pod)
	return nil
}

// openFinished waits for jobName's agent container to finish and opens its
// whole log, returning the stream and the pod it reads.
func (t *Tailer) openFinished(ctx context.Context, jobName string) (io.ReadCloser, string, error) {
	pod, err := t.waitForAgent(ctx, jobName, true)
	if err != nil {
		return nil, "", err
	}
	stream, err := t.logs.Stream(ctx, pod, agentContainerName, false)
	if err != nil {
		return nil, "", t.readFailed(ctx, jobName, pod, err)
	}
	return stream, pod, nil
}

// readFailed classifies a failure to open or to scan pod's log: ErrLogsGone
// when no retry can read it (see logGone), the wrapped error otherwise. A
// line too long to scan is gone at once: the container has finished, so its
// log is what it is and every read would stop at the same line.
func (t *Tailer) readFailed(ctx context.Context, jobName, pod string, err error) error {
	err = fmt.Errorf("jobs: read logs of %s: %w", jobName, err)
	why := ""
	if errors.Is(err, bufio.ErrTooLong) {
		why = fmt.Sprintf("a line of it exceeds %d MiB", maxEventLine>>20)
	} else {
		why = t.logGone(ctx, jobName, pod)
	}
	if why == "" {
		return err
	}
	t.failures.forget(pod)
	return fmt.Errorf("%w (%s): %w", ErrLogsGone, why, err)
}

// logGone says why the agent log of jobName's pod can never be read, or ""
// when it may still be (including when the pod or the Job cannot be looked
// at just now). It is asked only after a read failed.
//
// An evicted pod is gone at once only when the kubelet has also removed its
// agent container: until then the log may still be served, and a failed read
// of it is retried like any other, within LogReadGrace.
func (t *Tailer) logGone(ctx context.Context, jobName, podName string) string {
	pod, err := t.findPod(ctx, jobName)
	if err != nil {
		return ""
	}
	if pod == nil {
		return "the pod is gone"
	}
	if pod.Name != podName {
		// A newer pod replaced it: the next read is of that one.
		return ""
	}
	evicted := ""
	if pod.Status.Reason == "Evicted" {
		evicted = "the pod was evicted"
		if pod.Status.Message != "" {
			evicted += ": " + pod.Status.Message
		}
	}
	if containerRemoved(pod) {
		if evicted != "" {
			return evicted
		}
		return "the terminated agent container was removed with its log"
	}
	job, err := t.cs.BatchV1().Jobs(t.namespace).Get(ctx, jobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "the job is gone"
	}
	if err != nil || finishedAt(job).IsZero() {
		return ""
	}
	if t.failures.since(podName, t.now()) <= LogReadGrace {
		return ""
	}
	why := fmt.Sprintf("unreadable for over %s", LogReadGrace)
	if evicted != "" {
		why = evicted + "; " + why
	}
	return why
}

// containerRemoved mirrors the kubelet's own test for a log it cannot serve:
// the agent container has terminated with no container ID, and no earlier
// instance of it has one either.
func containerRemoved(pod *corev1.Pod) bool {
	agent := agentStatus(pod)
	if agent == nil || agent.State.Terminated == nil || agent.State.Terminated.ContainerID != "" {
		return false
	}
	last := agent.LastTerminationState.Terminated
	return last == nil || last.ContainerID == ""
}

// finishedAt is when job reached its terminal condition, zero before.
func finishedAt(job *batchv1.Job) time.Time {
	for _, cond := range job.Status.Conditions {
		terminal := cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed
		if terminal && cond.Status == corev1.ConditionTrue {
			return cond.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// scanLog splits the agent's log into its two persisted prefixed streams,
// delivering each line to the matching handler and ignoring everything else;
// a handler error stops the scan. Either handler may be nil.
//
// Turns are tested first, and a line that decodes as one is never offered to
// envelope.Decode: that decoder searches for its prefix anywhere in the line
// to survive log wrapping, so a turn whose text quotes the envelope prefix
// would otherwise be mis-scanned as a stage result.
//
// The live command output (MAUL-OUTPUT chunks) is the log's third stream,
// and nothing here keeps it: a chunk is skipped before envelope.Decode, so a
// run's many chunks never pass through the result's decoder.
//
// A line is skipped as a turn or a chunk only when it decodes as one, not
// merely because it holds the prefix: a stage result whose report quotes
// either prefix must still be found. Neither stream's JSON-encoded text can
// make its line decode as another stream's: a quoted line's own quotes are
// escaped there, so what follows a prefix inside it is never a JSON value.
func scanLog(r io.Reader, onEvent func(envelope.Event) error, onTurn func(transcript.Turn) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxEventLine)
	for sc.Scan() {
		if transcript.HasPrefix(sc.Bytes()) {
			if t, ok := transcript.Decode(sc.Bytes()); ok {
				if onTurn != nil {
					if err := onTurn(t); err != nil {
						return err
					}
				}
				continue
			}
		}
		if transcript.HasOutputPrefix(sc.Bytes()) {
			if _, ok := transcript.DecodeOutput(sc.Bytes()); ok {
				continue
			}
		}
		e, ok := envelope.Decode(sc.Bytes())
		if !ok || onEvent == nil {
			continue
		}
		if err := onEvent(e); err != nil {
			return err
		}
	}
	return sc.Err()
}
