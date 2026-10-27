// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestVerifyCommentNumbering (beatbattle-44, rough edge 8): the second
// verification, after one fix, is "verification 2" in its comment's header
// and in its summary line alike, and the fixes are counted apart from it,
// against maxVerifyFixes: "1 of 2 automatic fixes used". The word "round"
// numbers neither, so the two can no longer disagree.
func TestVerifyCommentNumbering(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, _ := e.failingVerification()
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.readyPreview(name)
	e.review(name, 4)
	again := e.verification(name)
	if again.Ref == nil || again.Round != 2 {
		t.Fatalf("no second verification: %+v", again)
	}
	e.observe(again.Ref.Name, v1alpha1.VerificationObserved, answering(500))
	e.review(name, 3)
	comments := e.gh.withMarker("verify-r2")
	if len(comments) != 1 {
		t.Fatalf("verification 2 comments = %d, want one", len(comments))
	}
	body := comments[0].Body
	for _, want := range []string{"### maul: verification 2, attempt 1", "Verification 2 failing",
		"1 of 2 automatic fixes used"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment omits %q:\n%s", want, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "round") {
		t.Errorf("comment numbers something as a round:\n%s", body)
	}
	first := e.gh.withMarker("verify-r1")
	if len(first) != 1 || !strings.Contains(first[0].Body, "### maul: verification 1, attempt 1") ||
		!strings.Contains(first[0].Body, "Verification 1 failing") ||
		!strings.Contains(first[0].Body, "1 of 2 automatic fixes used") {
		t.Errorf("verification 1 comment = %+v", first)
	}
}
