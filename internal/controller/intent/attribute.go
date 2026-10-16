// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
)

// attributeRun is whom a new plan or build run of the Intent is for (design
// §2), and a check-fix round too, which asks with the build stage:
//
//   - plan: whoever applied the trigger the plan answers (the newest
//     consumed trigger action, or the one that created the Intent) when they
//     are one of the Project's approvers, reason requester; the org
//     otherwise.
//   - build and check-fix: the approver of the plan being built, reason
//     approver.
//
// A revise round by command or review is attributed where it is leased (its
// commenter, its newest reviewer). A person with no GitHub id on record (an
// Intent from before ids were kept) is re-attributed to the org, reason
// approver-unregistered. The stamp is only written under --attribution-mode
// attribute (Settings.Attribution.Stamp).
func (p *pass) attributeRun(stage v1alpha1.IntentStage) *v1alpha1.Attribution {
	switch stage {
	case v1alpha1.IntentStagePlan:
		lt := p.lastTrigger()
		if !isApprover(p.proj, lt.Login) {
			return attribution.Org()
		}
		return attribution.To(lt.Login, lt.UserID, v1alpha1.AttributionRequester)
	default:
		ap := p.in.Status.Approval
		if ap == nil {
			return attribution.Org()
		}
		return attribution.To(ap.By, ap.ByID, v1alpha1.AttributionApprover)
	}
}
