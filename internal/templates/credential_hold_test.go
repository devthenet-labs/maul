// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

func TestRenderCredentialHoldComment(t *testing.T) {
	const url = "https://status.example.com/#/account"
	for _, tc := range []struct {
		name string
		in   CredentialHoldComment
		want []string
		not  []string
	}{
		{"missing", CredentialHoldComment{Login: "alice", Reason: "CredentialMissing", AccountURL: url},
			[]string{"attributed to @alice, who has no Anthropic credential", "[Account page](" + url + ")",
				"`/patchy approve`"}, nil},
		{"rejected", CredentialHoldComment{Login: "alice", Reason: "CredentialRejected"},
			[]string{"Anthropic rejected the credential @alice stored", "on the status page's Account page"},
			[]string{"]("}},
		{"unverified", CredentialHoldComment{Login: "alice", Reason: "CredentialUnverified", AccountURL: url},
			[]string{"has not verified yet"}, nil},
		{"resolved", CredentialHoldComment{Login: "alice", Resolved: true},
			[]string{"no longer waits"}, []string{"@alice", "Account page"}},
		{"a login outside the grammar names nobody", CredentialHoldComment{Login: "x](javascript:y)",
			Reason: "CredentialMissing"}, []string{"attributed to the approver"}, []string{"javascript"}},
		{"a url that would reshape the link is not linked", CredentialHoldComment{Login: "alice",
			Reason: "CredentialMissing", AccountURL: "https://e.com/)[x](y"},
			[]string{"on the status page's Account page"}, []string{"e.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderCredentialHoldComment(tc.in)
			if !strings.HasPrefix(got, CredentialHoldMarker+"\n") {
				t.Errorf("not headed by the marker: %q", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
		})
	}
}
