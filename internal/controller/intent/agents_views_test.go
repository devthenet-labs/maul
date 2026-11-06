// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestAgentsNoPreviewCommand (S2-20, from S2-15): a Project with no
// preview offers no /maul preview, and one sent anyway releases nothing
// and says there is no preview to show.
func TestAgentsNoPreviewCommand(t *testing.T) {
	if got := verbs(v1alpha1.IntentInReview, true, false); slices.Contains(got, "preview") {
		t.Errorf("verbs without a preview = %v", got)
	}
	if got := verbs(v1alpha1.IntentInReview, true, true); !slices.Contains(got, "preview") {
		t.Errorf("verbs with a preview = %v", got)
	}
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	if v := e.verification(name); v.Handoff == nil || v.Handoff.Released {
		t.Fatalf("hand-off = %+v, want held", v.Handoff)
	}
	id := e.gh.comment(approver, "/maul preview")
	e.settleActions(name)
	if body := e.onlyReply(id); !strings.Contains(body, "has no preview") {
		t.Errorf("reply = %s", body)
	}
	if v := e.verification(name); v.Handoff == nil || v.Handoff.Released {
		t.Errorf("/maul preview released an agents hand-off: %+v", v.Handoff)
	}
}

// TestAgentsViewsAreLinkless (S2-20, from S2-15): an agents round's status
// comment and pull request comment are the link-less evaluation views
// (Workspace): they say where the hand-off stands, never offer
// /maul preview, and never mention a preview link; the pull request's
// comment is posted under the preview marker without the preview
// projection.
func TestAgentsViewsAreLinkless(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	e.reviewUntil(name, func() bool { return strings.Contains(e.statusBody(), "**Evaluation:**") })
	body := e.statusBody()
	if !strings.Contains(body, "**Evaluation:**") || !strings.Contains(body, "holds its hand-off") ||
		strings.Contains(body, "/maul preview") {
		t.Errorf("status comment:\n%s", body)
	}
	pr := e.get(name).Status.PullRequests[0]
	var comment string
	for _, c := range e.gh.prComments[pr.Number] {
		if strings.Contains(c.Body, templates.NoticeMarker(testNS, name, templates.PreviewKey)) {
			comment = c.Body
		}
	}
	if comment == "" || !strings.Contains(comment, "maul holds its hand-off to the reviewers") ||
		strings.Contains(comment, "/maul preview") || strings.Contains(comment, "is live at") {
		t.Errorf("pull request comment = %q", comment)
	}
}
