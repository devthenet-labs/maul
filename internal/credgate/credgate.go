// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package credgate

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// Mode is the per-user credentials switch the run controllers read
// (--user-credentials), the same values as the broker's.
type Mode string

// Modes.
const (
	// ModeOff holds nothing: runs are created as before. The default.
	ModeOff Mode = "off"
	// ModeAttribute holds nothing either: the org credential pays for every
	// run, and attribution only records whom it was for.
	ModeAttribute Mode = "attribute"
	// ModeEnforce holds a run attributed to a person until that person's
	// credential is Valid.
	ModeEnforce Mode = "enforce"
)

// Flag is the switch's name, shared by the controllers that hold runs.
const Flag = "user-credentials"

// AccountURLFlag names the status page Account page's URL, which every
// notice of a hold links to.
const AccountURLFlag = "account-url"

// AccountURLUsage is --account-url's help text, shared by every binary
// that takes it.
const AccountURLUsage = "the status page's Account page, linked from every notice that a run waits for a " +
	"person's credential (empty: the notice names the page without a link)"

// maxAccountURL bounds --account-url: it is written into GitHub comments.
const maxAccountURL = 512

// CheckAccountURL holds --account-url to what may be written into a GitHub
// comment as a Markdown link target: an absolute http(s) URL without
// credentials, at most 512 bytes of printable ASCII, and none of the
// characters that could end or reshape the link.
func CheckAccountURL(raw string) error {
	if len(raw) > maxAccountURL {
		return fmt.Errorf("longer than %d bytes", maxAccountURL)
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f || r > 0x7e }) {
		return fmt.Errorf("%q holds whitespace, control or non-ASCII characters", raw)
	}
	if strings.ContainsAny(raw, "()<>[]\"'`\\|*_{}!") {
		return fmt.Errorf("%q holds a character Markdown would read (one of ()<>[]\"'`\\|*_{}!)", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return fmt.Errorf("%q is not an absolute http or https URL without credentials", raw)
	}
	return nil
}

// ParseMode reads --user-credentials; empty is ModeOff.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(s)); m {
	case "", ModeOff:
		return ModeOff, nil
	case ModeAttribute, ModeEnforce:
		return m, nil
	}
	return "", fmt.Errorf("user credentials mode %q is not %s, %s or %s", s, ModeOff, ModeAttribute, ModeEnforce)
}

// Enforced reports whether the mode holds runs at all.
func (m Mode) Enforced() bool { return m == ModeEnforce }

// Hold reasons: the condition reason of ConditionCredentialRequired.
const (
	// ReasonMissing: the person has no credential stored (or the record
	// found is not theirs).
	ReasonMissing = "CredentialMissing"
	// ReasonRejected: the provider refused the stored credential.
	ReasonRejected = "CredentialRejected"
	// ReasonUnverified: a credential is stored but the broker has not yet
	// found it Valid (it probes every new one within moments).
	ReasonUnverified = "CredentialUnverified"
)

// Provider is the one provider a person's credential is for in version 1.
const Provider = v1alpha1.ModelCredentialProviderAnthropic

// Verdict is Check's answer for one run.
type Verdict struct {
	// Hold is true when the run must not be created yet.
	Hold bool
	// User is the person the run would be attributed to (whose credential
	// is missing), set whenever Hold is.
	User *v1alpha1.UserRef
	// Reason is the hold's condition reason, "" when not held.
	Reason string
}

// Person is the user a attributes a run to, or nil for the org (no
// attribution, the org's, or a person with no numeric id on record).
func Person(a *v1alpha1.Attribution) *v1alpha1.UserRef {
	if a == nil || a.User == nil || a.User.ID < 1 {
		return nil
	}
	return a.User
}

// Owner is the ModelCredential owner value of u.
func Owner(u *v1alpha1.UserRef) string { return v1alpha1.ModelCredentialOwner(u.ID) }

// Judge decides a run attributed by a, given cred, the person's
// ModelCredential (nil when they have none). It never holds under off or
// attribute, never holds an org run, and under enforce holds a person's
// run unless cred is that very person's anthropic record and Valid.
func Judge(mode Mode, a *v1alpha1.Attribution, cred *v1alpha1.ModelCredential) Verdict {
	u := Person(a)
	if !mode.Enforced() || u == nil {
		return Verdict{}
	}
	held := Verdict{Hold: true, User: u}
	owner := Owner(u)
	switch {
	case cred == nil || cred.Spec.Owner != owner || cred.Spec.Provider != Provider ||
		cred.Name != v1alpha1.ModelCredentialName(owner, Provider):
		held.Reason = ReasonMissing
	case cred.Status.Health == v1alpha1.ModelCredentialValid:
		return Verdict{}
	case cred.Status.Health == v1alpha1.ModelCredentialRejected:
		held.Reason = ReasonRejected
	default:
		held.Reason = ReasonUnverified
	}
	return held
}

// Lookup reads a person's ModelCredential by its derived name: nil, nil
// when there is none.
type Lookup interface {
	ModelCredential(ctx context.Context, owner string, provider v1alpha1.ModelCredentialProvider) (
		*v1alpha1.ModelCredential, error)
}

// Check is Judge with the person's record read through lookup, only when
// there is a person to read it for under enforce: off, attribute and org
// runs read nothing.
func Check(ctx context.Context, mode Mode, a *v1alpha1.Attribution, lookup Lookup) (Verdict, error) {
	u := Person(a)
	if !mode.Enforced() || u == nil {
		return Verdict{}, nil
	}
	if lookup == nil {
		return Verdict{}, fmt.Errorf("user credentials are enforced but no credential lookup is configured")
	}
	cred, err := lookup.ModelCredential(ctx, Owner(u), Provider)
	if err != nil {
		return Verdict{}, fmt.Errorf("read the model credential of %s: %w", u.Login, err)
	}
	return Judge(mode, a, cred), nil
}

// Holds reports whether the person (login, id) would be held: Check for a
// run attributed to them. A re-approval while held is taken only from an
// approver it reports false for.
func Holds(ctx context.Context, mode Mode, login string, id int64, lookup Lookup) (bool, error) {
	if id < 1 {
		return mode.Enforced(), nil
	}
	v, err := Check(ctx, mode, &v1alpha1.Attribution{
		User: &v1alpha1.UserRef{Login: login, ID: id}, Reason: v1alpha1.AttributionApprover,
	}, lookup)
	return v.Hold, err
}

// Message is what a hold says where a human reads it (the intent's status
// comment, the Finding's tracking issue): who must add a credential, why,
// and where. accountURL may be empty (no Account page configured).
// reapprove says a later approval by someone holding a valid credential
// would lift the hold too (a build or a remediation, not a plan).
func Message(v Verdict, accountURL string, reapprove bool) string {
	if !v.Hold || v.User == nil {
		return ""
	}
	who := "@" + v.User.Login
	var why string
	switch v.Reason {
	case ReasonRejected:
		why = who + "'s Anthropic credential was rejected by Anthropic"
	case ReasonUnverified:
		why = who + "'s Anthropic credential has not been verified yet"
	default:
		why = who + " has no Anthropic credential stored in patchy"
	}
	where := "on the status page's Account page"
	if accountURL != "" {
		where = "on the Account page (" + accountURL + ")"
	}
	resume := "Nothing is replanned: this resumes on its own once the credential is valid."
	if reapprove {
		resume = "Nothing is replanned: this resumes on its own once the credential is valid, " +
			"or once an approver who has a valid credential approves it."
	}
	return why + ", and runs attributed to them use their own credential or none. " +
		who + " can add or replace an API key or a setup-token " + where + ". " + resume
}
