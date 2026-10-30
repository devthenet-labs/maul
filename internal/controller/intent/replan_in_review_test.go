// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestNoReplanOfferedInReview (S1-29): a replan is not available once an
// intent's pull requests are open (replan from review is deferred), so
// nothing maul says about an intent in review offers one. An intent whose
// approved plan states slice-1 criteria when the evaluator is switched on
// is told what does work (cancel, then a new issue), on the issue, in its
// status comment, its condition and its maul/acceptance status; and a
// /maul replan there is indeed answered as not available.
func TestNoReplanOfferedInReview(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.review(name, 2)
	e.evaluating(true)
	e.readyPreview(name)
	e.review(name, 6)
	in := e.get(name)
	key := templates.CriteriaReplanKey(in.Status.Approval.PlanRevision)
	notices := e.gh.withMarker(key)
	if len(notices) != 1 {
		t.Fatalf("criteria notices = %d, want 1", len(notices))
	}
	for _, want := range []string{"`/maul cancel`", "new issue", "cannot change while its pull requests are open"} {
		if !strings.Contains(strings.Join(strings.Fields(notices[0].Body), " "), want) {
			t.Errorf("notice does not say %q:\n%s", want, notices[0].Body)
		}
	}

	e.gh.comment(approver, "/maul replan")
	e.settleActions(name)
	in = e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview || len(e.specsOf("plan")) != 1 {
		t.Fatalf("phase %s, %d plan Jobs: a replan was taken in review", in.Status.Phase, len(e.specsOf("plan")))
	}
	var said []string
	for _, c := range e.gh.ownComments() {
		if !strings.Contains(markerOf(c.Body), "maul:plan") {
			// The plan comment was written for its approval, before review.
			said = append(said, c.Body)
		}
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c != nil {
		said = append(said, c.Message)
	}
	for _, pr := range in.Status.PullRequests {
		for _, s := range e.acceptanceStatuses(pr.HeadSHA) {
			said = append(said, s.Description)
		}
	}
	replied := false
	for _, s := range said {
		if strings.Contains(s, "`/maul replan` does nothing while this intent is `InReview`") {
			// maul's answer to the /maul replan: not available here.
			replied = true
			continue
		}
		if strings.Contains(s, "/maul replan") || strings.Contains(s, "replan needed") {
			t.Errorf("maul offers a replan in review:\n%s", s)
		}
	}
	status := e.gh.withMarker("maul:intent")
	const why = "cannot change while its pull requests are open"
	if len(status) == 0 || !strings.Contains(status[len(status)-1].Body, why) {
		t.Error("the status comment does not say the plan cannot change in review")
	}
	if !replied {
		t.Error("the /maul replan in review was not answered as not available")
	}
}
