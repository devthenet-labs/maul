// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestEarlierReleasesDisputeIsNeverReady ("evaluator observed"): a record an
// earlier release left, not yet withdrawn (no judge marker: its Project
// cannot run a round now, so the supersede waits), whose judgement every
// outcome passed but which disputed a saved check, waited on a replay that
// this release never runs. Its readers are gone, but it is still never
// ready: neither the hand-off nor maul/acceptance says so. The same record
// without the dispute is ready, as it was before.
func TestEarlierReleasesDisputeIsNeverReady(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disputed []string
		ready    bool
	}{
		{"disputed", []string{"sha256:" + strings.Repeat("d", 64)}, false},
		{"no dispute", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.review(name, 3)
			e.setStatus(name, func(in *v1alpha1.Intent) {
				routeRecords(in, judgeRoute{acceptManual: true}, e.clock.Now())
				applyJudgement(in, passedJudgement(in))
				v := in.Status.Verification
				v.Judge, v.Disputed = "", tc.disputed
				v.State = v1alpha1.VerificationUnconfirmed
			})
			p := e.prVerifyPass(name)
			acc, v := p.in.Status.Acceptance, p.in.Status.Verification
			if got := outcomesReady(acc, v); got != tc.ready {
				t.Fatalf("outcomesReady = %v, want %v", got, tc.ready)
			}
			st, _, ok := p.acceptanceStatus()
			if success := ok && st.state == statusSuccess; success != tc.ready {
				t.Errorf("maul/acceptance = %+v (posted %v), want success %v", st, ok, tc.ready)
			}
			if due := p.handoffDue(acc, v); (due == v1alpha1.HandoffReasonReady) != tc.ready {
				t.Errorf("hand-off due = %q, want released ready %v", due, tc.ready)
			}
		})
	}
}
