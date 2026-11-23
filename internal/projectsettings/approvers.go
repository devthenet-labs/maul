// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// MaxLoginLength is the longest login an approver list may hold (the
// schema's).
const MaxLoginLength = 64

// loginPattern is GitHub's login grammar as the Project schema writes it: a
// person's, so no "[bot]" suffix. A Bot never approves.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Why a list of logins cannot be a Project's approver list.
const (
	// ApproversEmpty: a Project has at least one approver.
	ApproversEmpty = "empty"
	// ApproversTooMany: more than v1alpha1.MaxProjectApprovers.
	ApproversTooMany = "too-many"
	// ApproverNotALogin: an entry that is no person's GitHub login.
	ApproverNotALogin = "not-a-login"
	// ApproverDuplicate: a login listed twice, compared as GitHub compares
	// logins (the schema's set compares exactly, and would admit "alice"
	// beside "Alice").
	ApproverDuplicate = "duplicate"
)

// ApproverListError says why a list cannot be an approver list: the Reason,
// with the Login it is about or the Count that is too many.
type ApproverListError struct {
	Reason string
	Login  string
	Count  int
}

func (e *ApproverListError) Error() string {
	switch e.Reason {
	case ApproversEmpty:
		return "a Project has at least one approver"
	case ApproversTooMany:
		return fmt.Sprintf("%d approvers: a Project has at most %d", e.Count, v1alpha1.MaxProjectApprovers)
	case ApproverDuplicate:
		return fmt.Sprintf("approver %q is listed twice", e.Login)
	}
	return fmt.Sprintf("approver %q is not a GitHub login (a person's, at most %d characters; a Bot never approves)",
		e.Login, MaxLoginLength)
}

// CheckApprovers holds a list to the approver-list rule: one to twenty
// logins, each a person's GitHub login, none twice whatever its case. It is
// the one statement of the rule: `maul setup project` holds --approver to
// it before it writes a manifest, and the settings page a changed list
// before it writes the Project. nil means the list is one.
func CheckApprovers(logins []string) *ApproverListError {
	switch {
	case len(logins) == 0:
		return &ApproverListError{Reason: ApproversEmpty}
	case len(logins) > v1alpha1.MaxProjectApprovers:
		return &ApproverListError{Reason: ApproversTooMany, Count: len(logins)}
	}
	seen := map[string]bool{}
	for _, l := range logins {
		if len(l) > MaxLoginLength || !loginPattern.MatchString(l) {
			return &ApproverListError{Reason: ApproverNotALogin, Login: l}
		}
		if seen[strings.ToLower(l)] {
			return &ApproverListError{Reason: ApproverDuplicate, Login: l}
		}
		seen[strings.ToLower(l)] = true
	}
	return nil
}
