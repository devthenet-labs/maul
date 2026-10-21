// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package attribution

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// Mode is the --attribution-mode switch every run controller reads.
type Mode string

// Modes.
const (
	// ModeOff stamps nothing: run specs and Jobs are as they were before
	// attribution existed. The default.
	ModeOff Mode = "off"
	// ModeAttribute stamps spec.attributedTo on every new run and the
	// attributed-to annotation on every agent Job. The org credential still
	// pays for every run; attribution only records whom it was for.
	ModeAttribute Mode = "attribute"
)

// Flag is the name of the switch, shared by every controller that launches
// agent Jobs.
const Flag = "attribution-mode"

// ParseMode reads --attribution-mode; empty is ModeOff.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.TrimSpace(s)) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeAttribute:
		return ModeAttribute, nil
	}
	return "", fmt.Errorf("unknown attribution mode %q: want %s or %s", s, ModeOff, ModeAttribute)
}

// On reports whether the mode stamps anything.
func (m Mode) On() bool { return m == ModeAttribute }

// Stamp is the spec.attributedTo a controller writes on a new run: a under
// ModeAttribute, nil under ModeOff, so the off spec is exactly the spec
// before the feature.
func (m Mode) Stamp(a *v1alpha1.Attribution) *v1alpha1.Attribution {
	if !m.On() {
		return nil
	}
	return a
}

// Annotation is the attributed-to annotation value a controller puts on a
// run's Job and pod template, "" for none: under ModeAttribute always one,
// "org" for a run with no attribution (one created before the mode was
// turned on, or the org's); under ModeOff never.
func (m Mode) Annotation(a *v1alpha1.Attribution) string {
	if !m.On() {
		return ""
	}
	return Value(a)
}

// ValueOrg is the annotation value of a run no person is behind.
const ValueOrg = "org"

// idPrefix introduces a person's numeric GitHub id in the annotation. It is
// not "github:", the status page's username prefix for a login, because a
// GitHub login may itself be all digits.
const idPrefix = "github-id:"

// ValuePattern is the annotation grammar, which the chart's attribution
// admission policy holds every Job and pod to: "org", or "github-id:" and a
// positive decimal int64 without leading zeros.
const ValuePattern = `^(org|github-id:[1-9][0-9]{0,18})$`

var valueRE = regexp.MustCompile(ValuePattern)

// Value is the annotation value for a: the person's id, or "org" when a is
// nil or names no person.
func Value(a *v1alpha1.Attribution) string {
	if a == nil || a.User == nil || a.User.ID < 1 {
		return ValueOrg
	}
	return idPrefix + strconv.FormatInt(a.User.ID, 10)
}

// ErrValue is ParseValue's refusal.
var ErrValue = errors.New("not an attributed-to value")

// ParseValue reads an annotation value: the person's numeric id, or 0 for
// "org". Anything outside ValuePattern is ErrValue.
func ParseValue(v string) (int64, error) {
	if !valueRE.MatchString(v) {
		return 0, fmt.Errorf("%w: %q", ErrValue, v)
	}
	if v == ValueOrg {
		return 0, nil
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(v, idPrefix), 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%w: %q", ErrValue, v)
	}
	return id, nil
}

// Org is the attribution of a run no human is behind.
func Org() *v1alpha1.Attribution {
	return &v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}
}

// loginRE is v1alpha1.UserRef's login grammar: a GitHub login, never a
// Bot's "<name>[bot]".
var loginRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// maxLogin is v1alpha1.UserRef's login length bound.
const maxLogin = 64

// Registered reports whether (login, id) is a maul identity a run can be
// attributed to: a known GitHub numeric id beside a login UserRef admits.
// An approval made on the status page ("github:<login>") or in the CLI (a
// kube username) carries no id, so it is not one.
func Registered(login string, id int64) bool {
	return id >= 1 && len(login) <= maxLogin && loginRE.MatchString(login)
}

// To attributes a run to the person (login, id) for reason, a person
// reason (v1alpha1.PersonReason). When that person has no maul identity
// (Registered), the run is re-attributed to the org with the matching
// "-unregistered" reason, so the record says whom the table named and why
// no one is billed. A reason that names no person gives the org.
func To(login string, id int64, reason v1alpha1.AttributionReason) *v1alpha1.Attribution {
	if !v1alpha1.PersonReason(reason) {
		return Org()
	}
	if !Registered(login, id) {
		if reason == v1alpha1.AttributionSubmitter {
			return &v1alpha1.Attribution{Reason: v1alpha1.AttributionSubmitterUnregistered}
		}
		return &v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}
	}
	return &v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: login, ID: id}, Reason: reason}
}

// Display is how a status comment names whom a run is attributed to:
// "@login" for a person, "" for the org or no attribution.
func Display(a *v1alpha1.Attribution) string {
	if a == nil || a.User == nil {
		return ""
	}
	return "@" + a.User.Login
}
