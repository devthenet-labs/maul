// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"

	"github.com/devthenet-labs/maul/internal/action"
)

// CriteriaReplanNotice tells the approvers of an open intent, once, that
// its approved plan states slice-1 acceptance criteria, which the agent
// evaluator does not run (DESIGN 7.6): nothing is verified until a plan
// stating outcomes is approved. Every word is maul's.
type CriteriaReplanNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker);
	// Key is CriteriaReplanKey of the plan revision.
	Namespace string
	Intent    string
	Key       string
	// PlanRevision is the approved plan revision that states criteria.
	PlanRevision int32
	// TriggerLabel is the Project's trigger label, the other way to ask
	// for a new plan.
	TriggerLabel string
}

// CriteriaReplanKey keys the criteria replan notice of plan revision.
func CriteriaReplanKey(revision int32) string { return fmt.Sprintf("verify-replan-r%d", revision) }

// RenderCriteriaReplanNotice renders a CriteriaReplanNotice.
func RenderCriteriaReplanNotice(n CriteriaReplanNotice) (string, error) {
	ask := slashCommand(action.VerbReplan)
	if label := oneLine(n.TriggerLabel); label != "" {
		ask = fmt.Sprintf("comment %s or re-apply the %s label", ask, code(label))
	} else {
		ask = "comment " + ask
	}
	text := fmt.Sprintf("maul now verifies this Project's changes with its agent evaluator, which checks "+
		"acceptance outcomes stated in plain words and writes its own checks. The approved plan r%d states "+
		"acceptance criteria instead, which the evaluator does not run, so nothing is verified for this intent "+
		"until a new plan is approved. To ask for one, %s, then approve the plan that follows.", n.PlanRevision, ask)
	return render("intent_notice_verify.md.tmpl", struct {
		Marker string
		Text   string
		Help   string
	}{Marker: NoticeMarker(n.Namespace, n.Intent, n.Key), Text: text})
}
