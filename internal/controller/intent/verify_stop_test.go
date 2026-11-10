// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// checkRearmedAfterStop: re-armed by an approver, the latch is never halted
// again by the old stop, which is answered once.
func (e *env) checkRearmedAfterStop(name string, id int64) {
	e.t.Helper()
	t := e.t
	e.gh.comment(approver, "/maul verify fix")
	e.settleActions(name)
	if a := e.verification(name).AutoFix; a.State != v1alpha1.AutoFixArmed || a.Stop != nil {
		t.Fatalf("auto-fix after fix = %+v, want armed with the stop cleared", a)
	}
	e.clock.Advance(time.Minute)
	e.settleActions(name)
	if a := e.verification(name).AutoFix; a.State != v1alpha1.AutoFixArmed {
		t.Fatalf("the stop applied again: %+v", a)
	}
	if n := len(e.gh.withMarker("comment-" + itoa(id))); n != 1 {
		t.Errorf("the stop was answered %d times", n)
	}
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
