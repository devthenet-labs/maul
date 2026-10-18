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

func TestRenderCredentialApprovalNotice(t *testing.T) {
	got, err := RenderCredentialApprovalNotice(CredentialApprovalNotice{
		Namespace: "patchy", Intent: "demo-7", Key: "c:9", Actor: "bob", Verb: "approve",
		AccountURL: "https://status.example.com/#/account",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{NoticeMarker("patchy", "demo-7", "c:9"), "`/patchy approve` was not applied",
		"@bob has none", "[Account page](https://status.example.com/#/account)"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	got, err = RenderCredentialApprovalNotice(CredentialApprovalNotice{
		Namespace: "patchy", Intent: "demo-7", Key: "l:9", Actor: "bob", Label: "patchy:approve",
		LabelRemoved: true, AccountURL: "https://e.com/)(x",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"label was not applied", "patchy removed the label", "status page's Account page"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "e.com") {
		t.Errorf("a URL that reshapes the link was linked:\n%s", got)
	}
}
