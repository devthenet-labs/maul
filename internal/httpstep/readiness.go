// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import "errors"

// The readiness wait's bounds. The PreviewVerification CRD repeats them.
const (
	// MaxWaitTargets is the most targets a summary records a wait for: the
	// tester spec's bound on its targets.
	MaxWaitTargets = 8
	// MaxWaitAttempts bounds one target's connection attempts.
	MaxWaitAttempts = 256
	// MaxWaitMs bounds one target's recorded wait, in milliseconds: the
	// tester's window is at most a minute, plus one connection attempt cut
	// short by it.
	MaxWaitMs = 120_000
)

// TargetWait is how the tester waited, before its first criterion, for one
// target (a repository key) to accept a TCP connection on its Service
// address: how long, in how many attempts, and how each failed attempt
// failed. Every attempt but a last successful one is a failure, so a target
// was reached exactly when Attempts is one more than its failures, and
// unreachable when every attempt failed. No HTTP request is sent and no
// byte the application writes is read: the record is made only of
// connection outcomes, before the application is asked anything.
type TargetWait struct {
	Target   string `json:"target"`
	WaitMs   int32  `json:"waitMs"`
	Attempts int32  `json:"attempts"`
	Refused  int32  `json:"refused,omitempty"`
	Timeout  int32  `json:"timeout,omitempty"`
	Reset    int32  `json:"reset,omitempty"`
	DNS      int32  `json:"dns,omitempty"`
}

// Failures is how many of the wait's attempts failed.
func (w TargetWait) Failures() int32 { return w.Refused + w.Timeout + w.Reset + w.DNS }

// Reached reports whether the wait's last attempt connected.
func (w TargetWait) Reached() bool { return w.Attempts == w.Failures()+1 }

// Record counts one failed attempt under its transport class: refused,
// timeout and dns as themselves, anything else as reset.
func (w *TargetWait) Record(t Transport) {
	switch t {
	case TransportRefused:
		w.Refused++
	case TransportTimeout:
		w.Timeout++
	case TransportDNS:
		w.DNS++
	default:
		w.Reset++
	}
}

// check holds one wait to its bounds: a repository key, at least one
// attempt, every attempt failed or all but the last.
func (w TargetWait) check() error {
	switch {
	case len(w.Target) > MaxKeyLength || !KeyPattern.MatchString(w.Target):
		return errors.New("a wait's target is not a repository key")
	case w.Attempts < 1 || w.Attempts > MaxWaitAttempts || w.WaitMs < 0 || w.WaitMs > MaxWaitMs:
		return errors.New("a wait's numbers are out of range")
	case !countBounded(w.Refused) || !countBounded(w.Timeout) || !countBounded(w.Reset) || !countBounded(w.DNS):
		// Each count is bounded before they are summed, so the sum cannot
		// overflow and wrap to the attempts.
		return errors.New("a wait's failure counts are out of range")
	case w.Failures() != w.Attempts && !w.Reached():
		return errors.New("a wait's failures do not account for its attempts")
	}
	return nil
}

// countBounded reports whether one failure count is within 0 to
// MaxWaitAttempts.
func countBounded(n int32) bool { return n >= 0 && n <= MaxWaitAttempts }

// CheckWaits holds a summary's waits to their bounds: one to
// MaxWaitTargets, in strictly ascending target order (so each target once).
func CheckWaits(waits []TargetWait) error {
	if len(waits) == 0 || len(waits) > MaxWaitTargets {
		return errors.New("a summary records one to MaxWaitTargets waits")
	}
	for i, w := range waits {
		if err := w.check(); err != nil {
			return err
		}
		if i > 0 && waits[i-1].Target >= w.Target {
			return errors.New("a summary's waits are not in ascending target order")
		}
	}
	return nil
}

// Unreached is the targets of waits that were never reached, in order;
// nil when every target was. It is the one definition the tester's run,
// the wire writer and the parser share.
func Unreached(waits []TargetWait) []string {
	var out []string
	for _, w := range waits {
		if !w.Reached() {
			out = append(out, w.Target)
		}
	}
	return out
}
