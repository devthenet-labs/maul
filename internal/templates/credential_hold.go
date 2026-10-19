// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"regexp"
	"strings"
)

// CredentialHoldMarker heads the Finding tracking issue's sticky comment
// saying its remediation waits for a person's model credential; the
// projection edits it in place by it.
const CredentialHoldMarker = "<!-- patchy:credential-hold -->"

// CredentialHoldComment is the data behind that comment, every field
// patchy's own: the approver's login from the Finding's approval (held to
// the GitHub login grammar here too), the hold's condition reason, and the
// operator's Account page URL.
type CredentialHoldComment struct {
	// Login is the person whose credential is missing; empty renders a
	// comment that names nobody.
	Login string
	// Reason is the credgate reason: CredentialMissing, CredentialRejected
	// or CredentialUnverified.
	Reason string
	// AccountURL is the Account page; empty names the page without a link.
	AccountURL string
	// Provider is the provider whose credential the attempt needs, as a
	// person reads it ("Anthropic", "OpenAI"); empty is Anthropic. Kinds
	// is what may be stored for it ("an API key or a setup-token", "an
	// OpenAI API key"); empty is the former. Both patchy's own
	// (credgate.ProviderName, credgate.CredentialKinds).
	Provider, Kinds string
	// Resolved says the hold lifted: the comment is edited to say so.
	Resolved bool
}

// holdLoginRE is the GitHub login grammar, so the login can only ever be a
// mention of an account and nothing else.
var holdLoginRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// holdURLRE admits an absolute http(s) URL of characters that cannot end or
// reshape a Markdown link (runnercfg holds --account-url to the same).
var holdURLRE = regexp.MustCompile(`^https?://[A-Za-z0-9.:/#?&=%~+,;@$-]+$`)

// holdWordRE admits the plain words a provider name or kinds phrase is
// made of, so neither can carry markup into the comment.
var holdWordRE = regexp.MustCompile(`^[A-Za-z][A-Za-z -]{0,60}$`)

// holdWord is s when it is plain words, else def.
func holdWord(s, def string) string {
	if holdWordRE.MatchString(s) {
		return s
	}
	return def
}

// RenderCredentialHoldComment renders the sticky comment, headed by
// CredentialHoldMarker.
func RenderCredentialHoldComment(c CredentialHoldComment) string {
	var b strings.Builder
	b.WriteString(CredentialHoldMarker + "\n")
	who := "the approver"
	if holdLoginRE.MatchString(c.Login) {
		who = "@" + c.Login
	}
	if c.Resolved {
		b.WriteString("### ✅ Model credential\n\n")
		b.WriteString("The remediation no longer waits for a model credential; patchy has resumed it.\n")
		return b.String()
	}
	provider, kinds := holdWord(c.Provider, "Anthropic"), holdWord(c.Kinds, "an API key or a setup-token")
	b.WriteString("### ⏸️ Waiting for a model credential\n\n")
	switch c.Reason {
	case "CredentialRejected":
		b.WriteString("This remediation is attributed to " + who + ", and " + provider + " rejected the " +
			"credential " + who + " stored in patchy. ")
	case "CredentialUnverified":
		b.WriteString("This remediation is attributed to " + who + ", whose stored " + provider + " credential " +
			"patchy has not verified yet. ")
	default:
		b.WriteString("This remediation is attributed to " + who + ", who has no " + provider + " credential " +
			"stored in patchy. ")
	}
	b.WriteString("Runs attributed to a person use their own credential (" + kinds + "), so patchy holds it " +
		"rather than spend an attempt on a refusal.\n\n")
	where := "on the status page's Account page"
	if holdURLRE.MatchString(c.AccountURL) {
		where = "on the [Account page](" + c.AccountURL + ")"
	}
	b.WriteString("- " + who + " can add or replace one " + where + "; the remediation resumes on its own " +
		"once it is valid, nothing re-investigated.\n")
	b.WriteString("- Or an approver who has a valid credential can approve this finding again " +
		"(`/patchy approve`), which makes the remediation theirs.\n")
	return b.String()
}
