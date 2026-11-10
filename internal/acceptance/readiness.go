// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"slices"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// Unreached reports whether the summary records a target the tester never
// reached: the run observed nothing, and the target is the application's
// failure.
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
	case httpstep.CheckWaits(s.Waits) != nil:
		return ErrWireInvalid
	case !slices.Equal(s.Unreachable, httpstep.Unreached(s.Waits)):
		return ErrWireInvalid
	case s.Unreached() && s.Count != 0:
		return ErrWireInvalid
	}
	return nil
}
