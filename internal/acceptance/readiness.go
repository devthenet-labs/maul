// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"errors"
	"slices"
)

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
	case len(w.Target) > maxKeyLength || !keyPattern.MatchString(w.Target):
		return errors.New("a wait's target is not a repository key")
	case w.Attempts < 1 || w.Attempts > MaxWaitAttempts || w.WaitMs < 0 || w.WaitMs > MaxWaitMs:
		return errors.New("a wait's numbers are out of range")
	case w.Refused < 0 || w.Timeout < 0 || w.Reset < 0 || w.DNS < 0:
		return errors.New("a wait's failure counts are negative")
	case w.Failures() != w.Attempts && !w.Reached():
		return errors.New("a wait's failures do not account for its attempts")
	}
	return nil
}

// checkWaits holds a summary's waits to their bounds: one to
// MaxWaitTargets, in strictly ascending target order (so each target once).
func checkWaits(waits []TargetWait) error {
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

// unreachable is the targets of waits that were never reached, in order;
// nil when every target was.
func unreachable(waits []TargetWait) []string {
	var out []string
	for _, w := range waits {
		if !w.Reached() {
			out = append(out, w.Target)
		}
	}
	return out
}

// Unreached reports whether the summary records a target the tester never
// reached: the run observed nothing, and settles as infrastructure.
func (s Summary) Unreached() bool { return len(s.Unreachable) > 0 }

// Equal reports whether two summaries are the same, as the controller
// compares the log's summary with the termination message.
func (s Summary) Equal(o Summary) bool {
	return s.Count == o.Count && s.SHA256 == o.SHA256 && slices.Equal(s.Waits, o.Waits) &&
		slices.Equal(s.Unreachable, o.Unreachable)
}

// check holds a summary to what the tester writes: a count and hash, the
// readiness wait of every target, the unreachable ones named exactly, and
// no observation after a target that was never reached.
func (s Summary) check() error {
	switch {
	case s.Count < 0 || s.Count > MaxCriteria || !sha256Hex(s.SHA256):
		return ErrWireInvalid
	case checkWaits(s.Waits) != nil:
		return ErrWireInvalid
	case !slices.Equal(s.Unreachable, unreachable(s.Waits)):
		return ErrWireInvalid
	case s.Unreached() && s.Count != 0:
		return ErrWireInvalid
	}
	return nil
}
