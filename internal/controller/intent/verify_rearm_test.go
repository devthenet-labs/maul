// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// rearmEnv is an intent in review whose verification ran at maul's own
// heads, with automatic fixes halted for an approver's earlier review: the
// state an approver re-arms from with /maul verify fix.
func rearmEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	if v := e.verification(name); v.HeadSetDigest == "" {
		t.Fatalf("verification = %+v, want its head set recorded", v)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		since := metav1.NewTime(e.clock.Now())
		in.Status.Verification.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted,
			Reason: v1alpha1.AutoFixHumanReview, Since: &since}
	})
	return e, name
}

// rearm records an approver's /maul verify fix posted now, as the command
// settles it.
func (e *env) rearm(name string) {
	e.t.Helper()
	e.setStatus(name, func(in *v1alpha1.Intent) {
		at := metav1.NewTime(e.clock.Now())
		in.Status.Verification.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed, Since: &at,
			Rearm: &v1alpha1.Rearm{By: approver, ByID: actorOf(approver).ID, At: at, Round: in.Status.Rounds}}
	})
}

// movedTo reports v recorded at the head set whose app head is sha.
func movedTo(v *v1alpha1.IntentVerification, sha string) bool {
	return v.HeadSetDigest == headSetDigest([]v1alpha1.VerificationHead{{Component: "app",
		Repository: appRepoURL, Revision: sha}})
}

// TestVerifyFixAfterAPushKeepsAutoFixArmed (S1 demo bug 2): an approver
// pushes to the pull request and seconds later re-arms automatic fixes with
// /maul verify fix, all before maul's next poll sees the push. GitHub
// recorded the push before the comment, so the re-arm is the later word:
// once maul records the new head set, automatic fixes stay armed.
func TestVerifyFixAfterAPushKeepsAutoFixArmed(t *testing.T) {
	e, name := rearmEnv(t)
	moved := strings.Repeat("f", 40)
	e.pushHead(e.prNumber(name), moved)
	e.clock.Advance(6 * time.Second)
	e.rearm(name)
	e.review(name, 3)
	v := e.verification(name)
	if !movedTo(v, moved) {
		t.Fatalf("verification = %+v, want the pushed head set recorded", v)
	}
	if v.AutoFix.State != v1alpha1.AutoFixArmed || v.AutoFix.Rearm == nil {
		t.Fatalf("auto-fix = %+v, want the re-arm made after the push to stand", v.AutoFix)
	}
}

// TestVerifyFixAfterAPushFixesOnTheHumansHead: the re-arm made after the
// push is the approver's leave to fix on top of it (H10): a failed
// verification at the pushed head starts an acceptance-fix round there,
// where a head maul did not push otherwise halts automatic fixes (guard 5).
// Without the re-arm the same failure starts nothing.
func TestVerifyFixAfterAPushFixesOnTheHumansHead(t *testing.T) {
	for _, rearmed := range []bool{true, false} {
		t.Run(map[bool]string{true: "re-armed after the push", false: "not re-armed"}[rearmed], func(t *testing.T) {
			e, name := rearmEnv(t)
			moved := strings.Repeat("f", 40)
			e.pushHead(e.prNumber(name), moved)
			e.clock.Advance(6 * time.Second)
			if rearmed {
				e.rearm(name)
			}
			e.setStatus(name, func(in *v1alpha1.Intent) {
				in.Status.Verification.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: actorOf(approver).ID,
					At: metav1.NewTime(e.clock.Now()), CommentID: 4242}
			})
			e.review(name, 2)
			e.readyPreview(name)
			e.reviewUntil(name, func() bool { return e.verification(name).Ref != nil && movedTo(e.verification(name), moved) })
			e.observe(e.verification(name).Ref.Name, v1alpha1.VerificationObserved, answering(500))
			e.review(name, 2)
			v := e.verification(name)
			if v.State != v1alpha1.VerificationFailing && len(e.verifyRuns(name)) == 0 {
				t.Fatalf("verification = %+v, want a failure at the pushed head", v)
			}
			if rearmed && (len(e.verifyRuns(name)) != 1 || v.AutoFix.Reason == v1alpha1.AutoFixHumanPush) {
				t.Fatalf("fix rounds %d, auto-fix %+v: want one round on the re-armed head", len(e.verifyRuns(name)),
					v.AutoFix)
			}
			if !rearmed && (len(e.verifyRuns(name)) != 0 || v.AutoFix.State != v1alpha1.AutoFixHalted) {
				t.Fatalf("fix rounds %d, auto-fix %+v: want none, halted", len(e.verifyRuns(name)), v.AutoFix)
			}
		})
	}
}

// TestVerifyFixCommandAfterAPushKeepsAutoFixArmed is the same through the
// command itself: the push, then the approver's /maul verify fix on the
// issue six seconds later, both before the pull request poll.
func TestVerifyFixCommandAfterAPushKeepsAutoFixArmed(t *testing.T) {
	e, name := rearmEnv(t)
	moved := strings.Repeat("f", 40)
	e.pushHead(e.prNumber(name), moved)
	e.clock.Advance(6 * time.Second)
	e.gh.comment(approver, "/maul verify fix")
	e.review(name, 3)
	v := e.verification(name)
	if !movedTo(v, moved) || v.AutoFix.Rearm == nil {
		t.Fatalf("verification = %+v, want the pushed head set and the re-arm recorded", v)
	}
	if v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Fatalf("auto-fix = %+v, want armed: the re-arm came after the push", v.AutoFix)
	}
}

// TestPushAfterVerifyFixHalts: the human always wins otherwise. A push
// GitHub recorded after the re-arm, or at the same second, halts automatic
// fixes (HumanPush), as does a push maul cannot date (GitHub shows no push
// to that head, or will not answer).
func TestPushAfterVerifyFixHalts(t *testing.T) {
	for _, tc := range []struct {
		name string
		push func(e *env, name, sha string)
	}{
		{"pushed after the re-arm", func(e *env, name, sha string) {
			e.rearm(name)
			e.clock.Advance(20 * time.Second)
			e.pushHead(e.prNumber(name), sha)
		}},
		{"pushed in the re-arm's second", func(e *env, name, sha string) {
			e.rearm(name)
			e.pushHead(e.prNumber(name), sha)
		}},
		{"no push recorded", func(e *env, name, sha string) {
			e.clock.Advance(-time.Minute)
			e.gh.mu.Lock()
			e.gh.prs[e.prNumber(name)].pr.HeadSHA = sha
			e.gh.mu.Unlock()
			e.clock.Advance(time.Minute)
			e.rearm(name)
		}},
		{"GitHub refuses the activity", func(e *env, name, sha string) {
			e.pushHead(e.prNumber(name), sha)
			e.clock.Advance(6 * time.Second)
			e.rearm(name)
			e.gh.mu.Lock()
			e.gh.repoErrs["PushedAt acme/app"] = ghError(http.StatusForbidden, "Resource not accessible by integration")
			e.gh.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := rearmEnv(t)
			moved := strings.Repeat("e", 40)
			tc.push(e, name, moved)
			e.review(name, 3)
			v := e.verification(name)
			if !movedTo(v, moved) {
				t.Fatalf("verification = %+v, want the pushed head set recorded", v)
			}
			if v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixHumanPush {
				t.Fatalf("auto-fix = %+v, want halted for the human's push", v.AutoFix)
			}
		})
	}
}

// TestPushAfterVerifyFixTransientErrorWaits: a GitHub error that may pass
// fails the pass with nothing recorded (the head set not moved, nothing
// halted); the next pass decides.
func TestPushAfterVerifyFixTransientErrorWaits(t *testing.T) {
	e, name := rearmEnv(t)
	moved := strings.Repeat("f", 40)
	e.pushHead(e.prNumber(name), moved)
	e.clock.Advance(6 * time.Second)
	e.rearm(name)
	e.gh.mu.Lock()
	e.gh.errs = map[string][]error{"PushedAt": {errors.New("boom")}}
	e.gh.mu.Unlock()
	failed := 0
	for range 4 {
		e.clock.Advance(time.Minute)
		if err := e.reconcileIntent(name); err != nil {
			failed++
			if !strings.Contains(err.Error(), "boom") {
				t.Fatalf("pass failed: %v", err)
			}
			if v := e.verification(name); movedTo(v, moved) || v.AutoFix.State != v1alpha1.AutoFixArmed {
				t.Fatalf("a failed pass recorded %+v", v)
			}
		}
	}
	v := e.verification(name)
	if failed != 1 || !movedTo(v, moved) || v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Fatalf("verification = %+v, want the head set recorded and the re-arm standing once GitHub answered", v)
	}
}
