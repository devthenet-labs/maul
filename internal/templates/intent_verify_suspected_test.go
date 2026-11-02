// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import "testing"

// TestSuspectedNoteGolden: the note on a suspected defect, waiting for a
// person (what maul believes, that a person should confirm it, the command,
// the known limits and the reproduction, defanged and fenced) and once an
// approver confirmed it.
func TestSuspectedNoteGolden(t *testing.T) {
	repro := "POST /notes {\"name\":\"tok-v\"} answered 201\nGET /version?q=tok answered {\"note\":\"old\"}\n" +
		"See #12 and @octocat."
	for _, tt := range []struct {
		name string
		s    VerifySuspected
	}{
		{"intent_verify_suspected.md", VerifySuspected{Outcomes: []string{"version-shown"}, Reproduction: repro}},
		{"intent_verify_suspected_two.md", VerifySuspected{Outcomes: []string{"version-shown", "still-healthy"}}},
		{"intent_verify_suspected_confirmed.md", VerifySuspected{Outcomes: []string{"version-shown"},
			Reproduction: repro, ConfirmedBy: "peter"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			golden(t, tt.name, suspectedNote(&tt.s))
		})
	}
}
