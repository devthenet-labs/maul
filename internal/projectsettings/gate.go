// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"strings"
	"time"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// The gate's refusal reasons: fixed keys the page words and the logs carry.
const (
	// ReasonNoGitHubIdentity: the session carries no GitHub account id.
	ReasonNoGitHubIdentity = "no-github-identity"
	// ReasonNotApprover: the account is none of the Project's approvers.
	ReasonNotApprover = "not-approver"
	// ReasonNoWriteAccess: an approver whose account GitHub gives no write
	// access to the intent repository.
	ReasonNoWriteAccess = "no-write-access"
	// ReasonApproversUnresolved: intent-controller has not resolved the
	// approver list (or a login added to it) against GitHub yet.
	ReasonApproversUnresolved = "approvers-unresolved"
	// ReasonApproversStale: the last resolution is too old to act on.
	ReasonApproversStale = "approvers-stale"
)

// ApproversMaxAge is how old status.approvers.resolvedAt may be for the
// gate to act on it. intent-controller resolves every ten minutes, so past
// this it is down or cannot read GitHub, and a revoked write access would
// go unnoticed.
const ApproversMaxAge = 30 * time.Minute

// GateResult is the gate's answer: OK with the approver login the account
// resolved to, or the Reason why not.
type GateResult struct {
	OK     bool
	Reason string
	// Login is the approver's login as intent-controller resolved it: who
	// the audit record names and who may not remove themselves.
	Login string
}

// Gate decides whether the GitHub account githubID may change p's settings
// at now. It is the rule intent-controller holds every /maul command to,
// read from what the controller published: the account is one the approver
// list resolved to, GitHub gave it write access to the intent repository,
// and its login is still on the live list (an identity in status is no
// authority alone; ProjectApprovers.Has is the one list rule). All of it
// must have been resolved within ApproversMaxAge.
//
// Run it on the object about to be patched, read uncached, with the id
// from the session's verified token. It trusts no login claim.
func Gate(p *v1alpha1.Project, githubID int64, now time.Time) GateResult {
	if githubID <= 0 {
		return GateResult{Reason: ReasonNoGitHubIdentity}
	}
	st := p.Status.Approvers
	if st == nil || st.ResolvedAt == nil {
		return GateResult{Reason: ReasonApproversUnresolved}
	}
	// Two logins of the list may resolve to one account (a rename), so
	// every identity of the id is asked, not the first.
	listed, writer := false, ""
	for _, who := range st.Identities {
		if who.ID != githubID || !p.Spec.Approvers.Has(who.Login) {
			continue
		}
		listed = true
		if who.Write && writer == "" {
			writer = who.Login
		}
	}
	switch {
	case !listed && unresolvedLogin(p):
		return GateResult{Reason: ReasonApproversUnresolved}
	case !listed:
		return GateResult{Reason: ReasonNotApprover}
	case now.Sub(st.ResolvedAt.Time) > ApproversMaxAge:
		return GateResult{Reason: ReasonApproversStale}
	case writer == "":
		return GateResult{Reason: ReasonNoWriteAccess}
	}
	return GateResult{OK: true, Login: writer}
}

// unresolvedLogin reports a login of the live list that status holds no
// identity for: one added since the last resolution, who may be the very
// person asking.
func unresolvedLogin(p *v1alpha1.Project) bool {
	for _, login := range p.Spec.Approvers.Logins {
		if _, ok := identityOf(p.Status.Approvers, login); !ok {
			return true
		}
	}
	return false
}

// identityOf is the identity status holds for a login of the list, matched
// as GitHub matches logins.
func identityOf(st *v1alpha1.ProjectApproversStatus, login string) (v1alpha1.ProjectApproverIdentity, bool) {
	if st == nil {
		return v1alpha1.ProjectApproverIdentity{}, false
	}
	for _, who := range st.Identities {
		if strings.EqualFold(who.Login, login) {
			return who, true
		}
	}
	return v1alpha1.ProjectApproverIdentity{}, false
}
