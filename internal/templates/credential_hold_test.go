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
				"`/maul approve`"}, nil},
		{"rejected", CredentialHoldComment{Login: "alice", Reason: "CredentialRejected"},
			[]string{"Anthropic rejected the credential @alice stored", "on the status page's Account page"},
			[]string{"]("}},
		{"unverified", CredentialHoldComment{Login: "alice", Reason: "CredentialUnverified", AccountURL: url},
			[]string{"has not verified yet"}, nil},
		{"resolved", CredentialHoldComment{Login: "alice", Resolved: true},
			[]string{"no longer waits"}, []string{"@alice", "Account page"}},
		{"a login outside the grammar names nobody", CredentialHoldComment{Login: "x](javascript:y)",
			Reason: "CredentialMissing"}, []string{"attributed to the approver"}, []string{"javascript"}},
		{"openai missing", CredentialHoldComment{Login: "alice", Reason: "CredentialMissing", AccountURL: url,
			Provider: "OpenAI", Kinds: "an OpenAI API key"},
			[]string{"who has no OpenAI credential", "(an OpenAI API key)"}, []string{"Anthropic", "setup-token"}},
		{"openai rejected", CredentialHoldComment{Login: "alice", Reason: "CredentialRejected", Provider: "OpenAI",
			Kinds: "an OpenAI API key"}, []string{"OpenAI rejected the credential @alice stored"}, []string{"Anthropic"}},
		{"openai unverified", CredentialHoldComment{Login: "alice", Reason: "CredentialUnverified",
			Provider: "OpenAI"}, []string{"stored OpenAI credential maul has not verified yet"}, nil},
		{"markup in a provider is not rendered", CredentialHoldComment{Login: "alice", Reason: "CredentialMissing",
			Provider: "[x](javascript:y)", Kinds: "<b>"}, []string{"no Anthropic credential",
			"(an API key or a setup-token)"}, []string{"javascript", "<b>"}},
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
		Namespace: "maul", Intent: "demo-7", Key: "c:9", Actor: "bob", Verb: "approve",
		AccountURL: "https://status.example.com/#/account",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{NoticeMarker("maul", "demo-7", "c:9"), "`/maul approve` was not applied",
		"waits for a valid Anthropic credential", "@bob has none", "Add an API key or a setup-token",
		"[Account page](https://status.example.com/#/account)"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	got, err = RenderCredentialApprovalNotice(CredentialApprovalNotice{
		Namespace: "maul", Intent: "demo-7", Key: "l:9", Actor: "bob", Label: "maul:approve",
		LabelRemoved: true, AccountURL: "https://e.com/)(x",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"label was not applied", "maul removed the label", "status page's Account page"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "e.com") {
		t.Errorf("a URL that reshapes the link was linked:\n%s", got)
	}
	got, err = RenderCredentialApprovalNotice(CredentialApprovalNotice{
		Namespace: "maul", Intent: "demo-7", Key: "c:10", Actor: "bob", Verb: "approve",
		Provider: "OpenAI", Kinds: "an OpenAI API key",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"waits for a valid OpenAI credential", "Add an OpenAI API key on"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "setup-token") || strings.Contains(got, "Anthropic") {
		t.Errorf("an OpenAI notice offers Anthropic credentials:\n%s", got)
	}
}
