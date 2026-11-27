// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"testing"
)

// branchPattern is status.branch's schema pattern.
var branchPattern = regexp.MustCompile(`^maul-intent/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func TestIntentBranch(t *testing.T) {
	cases := []struct {
		name               string
		plan, branchesFrom int32
		want               string
	}{
		{"never replanned in review", 1, 0, "maul-intent/app-7"},
		{"never replanned, a later plan revision", 4, 0, "maul-intent/app-7"},
		{"the superseded plan keeps its branch", 3, 4, "maul-intent/app-7"},
		{"the first plan after the replan", 4, 4, "maul-intent/app-7-p4"},
		{"a later replan's plan", 9, 4, "maul-intent/app-7-p9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IntentBranch("app-7", tc.plan, tc.branchesFrom); got != tc.want {
				t.Errorf("IntentBranch(app-7, %d, %d) = %q, want %q", tc.plan, tc.branchesFrom, got, tc.want)
			}
		})
	}
}

// TestIntentBranchSeededProperty: an intent never replanned in review keeps
// today's branch at every plan revision, byte for byte; every branch is one
// status.branch admits; and no branch of one intent is another intent's,
// whatever the two names and revisions.
func TestIntentBranchSeededProperty(t *testing.T) {
	seed := rand.Uint64()
	r := rand.New(rand.NewPCG(seed, seed))
	name := func() string {
		project := []string{"app", "a", "web-ui", "x9", "p-12-p"}[r.IntN(5)]
		return fmt.Sprintf("%s-%d", project, 1+r.IntN(99999))
	}
	for range 2000 {
		a, b := name(), name()
		ra, rb := int32(1+r.IntN(MaxIntentRound)), int32(1+r.IntN(MaxIntentRound))
		fa, fb := int32(r.IntN(MaxIntentRound+1)), int32(r.IntN(MaxIntentRound+1))
		if got := IntentBranch(a, ra, 0); got != IntentBranchPrefix+a {
			t.Fatalf("seed %d: IntentBranch(%s, %d, 0) = %q, want today's branch", seed, a, ra, got)
		}
		ba, bb := IntentBranch(a, ra, fa), IntentBranch(b, rb, fb)
		if !branchPattern.MatchString(ba) || len(ba) > 255 {
			t.Fatalf("seed %d: %q is not a status.branch", seed, ba)
		}
		if a != b && ba == bb {
			t.Fatalf("seed %d: intents %s and %s share branch %q", seed, a, b, ba)
		}
	}
}
