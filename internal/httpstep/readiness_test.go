// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"slices"
	"testing"
)

// TestUnreached: the one definition of the targets a run never reached.
func TestUnreached(t *testing.T) {
	waits := []TargetWait{{Target: "api", Attempts: 2, Refused: 2}, {Target: "db", Attempts: 1},
		{Target: "web", Attempts: 3, DNS: 3}}
	if got := Unreached(waits); !slices.Equal(got, []string{"api", "web"}) {
		t.Errorf("Unreached = %v", got)
	}
	if got := Unreached(waits[1:2]); got != nil {
		t.Errorf("Unreached of a reached run = %v, want nil", got)
	}
}
