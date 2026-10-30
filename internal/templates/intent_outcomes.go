// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"

	"github.com/devthenet-labs/maul/internal/action"
)

// CriteriaReplanNotice tells the approvers of an open intent, once, that
// its approved plan states slice-1 acceptance criteria, which the agent
// evaluator does not run (DESIGN 7.6): nothing is verified for it. The
// approved plan cannot change while its pull requests are open (replan from
// review is deferred), so the notice says what works: review it as it is,
// or cancel it and open a new issue, which maul plans with outcomes. Every
// word is maul's.
type CriteriaReplanNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker);
	// Key is CriteriaReplanKey of the plan revision.
	Namespace string
	Intent    string
	Key       string
	// PlanRevision is the approved plan revision that states criteria.
	PlanRevision int32
	// TriggerLabel is the Project's trigger label, which a new issue
	// carries to be planned.
	TriggerLabel string
}

// CriteriaReplanKey keys the criteria replan notice of plan revision.
func CriteriaReplanKey(revision int32) string { return fmt.Sprintf("verify-replan-r%d", revision) }

// RenderCriteriaReplanNotice renders a CriteriaReplanNotice.
func RenderCriteriaReplanNotice(n CriteriaReplanNotice) (string, error) {
	issue := "a new issue"
	if label := oneLine(n.TriggerLabel); label != "" {
		issue = fmt.Sprintf("a new issue with the %s label", code(label))
	}
	text := fmt.Sprintf("maul now verifies this Project's changes with its agent evaluator, which checks "+
		"acceptance outcomes stated in plain words and writes its own checks. The approved plan r%d states "+
		"acceptance criteria instead, which the evaluator does not run, so nothing is verified for this intent. "+
		"The approved plan cannot change while its pull requests are open, so its pull requests are reviewed as "+
		"they are. To have the change verified instead, an approver comments %s (the pull requests are left to "+
		"you) and opens %s, which maul plans afresh with outcomes.", n.PlanRevision, slashCommand(action.VerbCancel),
		issue)
	return render("intent_notice_verify.md.tmpl", struct {
		Marker string
		Text   string
		Help   string
	}{Marker: NoticeMarker(n.Namespace, n.Intent, n.Key), Text: text})
}
