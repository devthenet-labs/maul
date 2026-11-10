// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestSuspectedNoteGolden: the note on a suspected defect, waiting for a
// person (what maul believes, that a person should confirm it, the command,
// the known limits and the reproduction, defanged and fenced) and once an
// approver confirmed it; with the hand-off held or released, on a slot
// Project and on one with no preview (gate D-B3).
func TestSuspectedNoteGolden(t *testing.T) {
	repro := "POST /notes {\"name\":\"tok-v\"} answered 201\nGET /version?q=tok answered {\"note\":\"old\"}\n" +
		"See #12 and @octocat."
	for _, tt := range []struct {
		name string
		s    VerifySuspected
	}{
		{"intent_verify_suspected.md", VerifySuspected{Outcomes: []string{"version-shown"}, Reproduction: repro,
			Held: true}},
		{"intent_verify_suspected_two.md", VerifySuspected{Outcomes: []string{"version-shown", "still-healthy"},
			Held: true}},
		{"intent_verify_suspected_released.md", VerifySuspected{Outcomes: []string{"version-shown"}}},
		{"intent_verify_suspected_workspace.md", VerifySuspected{Outcomes: []string{"echo-kept"},
			Reproduction: repro, Held: true, Workspace: true}},
		{"intent_verify_suspected_workspace_released.md", VerifySuspected{Outcomes: []string{"echo-kept"},
			Workspace: true}},
		{"intent_verify_suspected_confirmed.md", VerifySuspected{Outcomes: []string{"version-shown"},
			Reproduction: repro, ConfirmedBy: "peter"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			golden(t, tt.name, suspectedNote(&tt.s))
		})
	}
}

// TestSuspectedNoteClaimsOnlyAHoldInForce (gate D-B3): the note says the
// hand-off is held only while it is, and a Project with no preview is
// never told of a preview link or of a limit of a check it cannot run.
func TestSuspectedNoteClaimsOnlyAHoldInForce(t *testing.T) {
	for _, tt := range []struct {
		name   string
		s      VerifySuspected
		want   string
		absent []string
	}{
		{"slot held", VerifySuspected{Outcomes: []string{"a"}, Held: true}, "the preview link stays held", nil},
		{"slot released", VerifySuspected{Outcomes: []string{"a"}}, "", []string{"held"}},
		{"workspace held", VerifySuspected{Outcomes: []string{"a"}, Held: true, Workspace: true},
			"maul holds its hand-off to the reviewers", []string{"preview", "clickable", "http"}},
		{"workspace released", VerifySuspected{Outcomes: []string{"a"}, Workspace: true}, "",
			[]string{"held", "holds", "preview", "clickable", "http"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := suspectedNote(&tt.s)
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("note does not say %q:\n%s", tt.want, got)
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("note says %q:\n%s", a, got)
				}
			}
		})
	}
}
