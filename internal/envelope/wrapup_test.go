// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package envelope

import (
	"reflect"
	"strings"
	"testing"
)

// TestWrapUpRoundTrip: a cut-off intent stage's wrap-up rides the stage at
// Version 4, under its own key, and decodes back whole; a stage with none
// writes no key at all, so every other event is byte-for-byte what it was.
func TestWrapUpRoundTrip(t *testing.T) {
	want := testPlanEvent()
	want.Plan.WrapUp = &WrapUp{Reason: WrapUpMaxTurns, Resumed: true, Kept: true}
	line, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, `"wrap_up":{"reason":"max_turns","resumed":true,"kept":true}`) {
		t.Errorf("line %s lacks the wrap-up's wire shape", line)
	}
	got, ok := Decode([]byte(line))
	if !ok {
		t.Fatal("Decode() ok = false")
	}
	want.V = Version
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip = %+v, want %+v", got.Plan.WrapUp, want.Plan.WrapUp)
	}

	settleOnly := testPlanEvent()
	settleOnly.Plan.WrapUp = &WrapUp{Reason: WrapUpDeadline}
	if line, err = settleOnly.Encode(); err != nil || !strings.Contains(line, `"wrap_up":{"reason":"deadline"}`) {
		t.Errorf("settle-only wrap-up = %s (%v), want the reason alone", line, err)
	}

	for _, e := range []Event{testEvent(), testPlanEvent()} {
		line, err := e.Encode()
		if err != nil || strings.Contains(line, "wrap_up") {
			t.Errorf("an event with no wrap-up writes %s (%v)", line, err)
		}
	}
}
