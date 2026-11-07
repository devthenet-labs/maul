// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestAgentsVerifyAcknowledgementSpeaksOfNoPreview (S2-B10): the
// acknowledgement of a /maul verify on a Project with no preview says the
// checks run in maul's own build, never "the preview".
func TestAgentsVerifyAcknowledgementSpeaksOfNoPreview(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	e.passAgentsRound(name, e.onlyVerification(name))
	e.review(name, 2)
	e.clock.Advance(time.Minute)
	id := e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	reply := e.onlyReply(id)
	if !strings.Contains(reply, "is recorded") || !strings.Contains(reply, "in its own build of the pull request") ||
		strings.Contains(reply, "preview") {
		t.Fatalf("reply = %s", reply)
	}
}

// TestAgentsVerifyKnownUnrunnableIsAnsweredOnce (S2-B10): a /maul verify
// the agents round cannot run, because the repository has no accepted
// image or repository images are off (the live run 4 case), is answered
// once, that it did not run, at the moment it is acknowledged: never
// "recorded" first and "did not run" seconds later.
func TestAgentsVerifyKnownUnrunnableIsAnsweredOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(e *env)
	}{
		{"no accepted image", func(e *env) { dropRunnerImages(t, e) }},
		{"repository images off", func(e *env) { e.intent.Images.Enabled = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newAgentsEnv(t, repoImage)
			e.reviewReady(name, 4)
			e.passAgentsRound(name, e.onlyVerification(name))
			e.review(name, 2)
			tc.remove(e)
			e.clock.Advance(time.Minute)
			id := e.gh.comment(approver, "/maul verify")
			e.settleActions(name)
			e.review(name, 2)
			reply := e.onlyReply(id)
			if strings.Contains(reply, "is recorded") ||
				!strings.Contains(reply, "did not run: the repository has no accepted runner image") ||
				!strings.Contains(reply, "Evaluation 1's result at these commits stands") {
				t.Fatalf("reply = %s", reply)
			}
			if replies := e.notRunReplies(); len(replies) != 0 {
				t.Fatalf("a second did-not-run reply was posted: %q", replies)
			}
			if v := e.verification(name); v.Requested != nil || v.State != v1alpha1.VerificationPassed {
				t.Fatalf("verification = %+v, want no request and round 1's Passed standing", v)
			}
		})
	}
}
