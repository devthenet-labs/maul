// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import "testing"

// TestProjectApproversHas: a login is on the approver list however it is
// cased, as GitHub compares logins, and nothing else is: not a prefix, not a
// login with a bot suffix, not the empty login, and nobody on an empty list.
func TestProjectApproversHas(t *testing.T) {
	list := ProjectApprovers{Logins: []string{"octocat", "Mona-Lisa", "j.doe_2"}}
	tests := []struct {
		name  string
		list  ProjectApprovers
		login string
		want  bool
	}{
		{"exact spelling", list, "octocat", true},
		{"upper case of a lower-case entry", list, "OCTOCAT", true},
		{"lower case of a mixed-case entry", list, "mona-lisa", true},
		{"mixed case of a mixed-case entry", list, "MONA-lisa", true},
		{"last entry", list, "J.Doe_2", true},
		{"absent", list, "mallory", false},
		{"a prefix of an entry", list, "octo", false},
		{"an entry plus a suffix", list, "octocat2", false},
		{"an entry with a bot suffix", list, "octocat[bot]", false},
		{"surrounding space is not trimmed", list, " octocat", false},
		{"empty login", list, "", false},
		{"empty list", ProjectApprovers{}, "octocat", false},
		{"nil list and empty login", ProjectApprovers{}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.list.Has(tt.login); got != tt.want {
				t.Errorf("ProjectApprovers{%q}.Has(%q) = %t, want %t", tt.list.Logins, tt.login, got, tt.want)
			}
		})
	}
}

// TestProjectApproversHasLeavesTheListAlone: Has only reads. The status page
// asks it about the live object it is about to patch, so a lookup that
// reordered or re-cased the list would change what is written.
func TestProjectApproversHasLeavesTheListAlone(t *testing.T) {
	list := ProjectApprovers{Logins: []string{"Octocat", "mona"}}
	list.Has("OCTOCAT")
	list.Has("nobody")
	if got := list.Logins; len(got) != 2 || got[0] != "Octocat" || got[1] != "mona" {
		t.Errorf("Logins after Has = %q, want [Octocat mona]", got)
	}
}
