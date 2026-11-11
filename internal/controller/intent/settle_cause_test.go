// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestRetryOwedIsRecorded: an attempt that ends without a judgement
// records the retry it owes (RetryOwed, its reference cleared), and the
// round's next attempt, once created, clears it; once that attempt is in
// flight nothing more is created.
func TestRetryOwedIsRecorded(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	first := e.onlyVerification(name)
	e.playTools(first.Name, v1alpha1.VerificationInfra, "", false)
	e.review(name, 1)
	if v := e.verification(name); !v.RetryOwed || v.Ref != nil || v.Attempt != 1 {
		t.Fatalf("verification = %+v, want the retry owed after attempt 1", v)
	}
	e.review(name, 2)
	v := e.verification(name)
	if v.RetryOwed || v.Ref == nil || v.Attempt != 2 || v.State != v1alpha1.VerificationEvaluating {
		t.Fatalf("verification = %+v, want attempt 2 in flight and no retry owed", v)
	}
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 2 {
		t.Fatalf("verifications = %d, want 2: the retry was made once", n)
	}
}

// TestRetryOwedMigration: a record written before RetryOwed existed owes
// its retry by the rule it was written under (no reference, an attempt on
// record), so it is still retried, once, and the retry then stands as any
// other attempt does.
func TestRetryOwedMigration(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	first := e.onlyVerification(name)
	e.playTools(first.Name, v1alpha1.VerificationInfra, "", false)
	// The old controller's unjudged write: the reference cleared, no
	// RetryOwed.
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.Ref, v.RetryOwed, v.InfraRetries = nil, false, 1
	})
	if v := e.verification(name); v.RetryOwed || v.Ref != nil || v.Attempt != 1 {
		t.Fatalf("verification = %+v, want an old record owing its retry", v)
	}
	e.review(name, 3)
	pvs := e.verifications(name)
	if len(pvs) != 2 || pvs[1].Name != v1alpha1.PreviewVerificationName(name, 1, 2) {
		t.Fatalf("verifications = %d, want attempt 2 as the old record's retry", len(pvs))
	}
	if v := e.verification(name); v.RetryOwed || v.Ref == nil || v.Ref.UID != pvs[1].UID || v.Attempt != 2 {
		t.Fatalf("verification = %+v, want attempt 2 in flight", v)
	}
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 2 {
		t.Fatalf("verifications = %d, want the old record retried once", n)
	}
}

// TestRetryOwedAfterTheLastAttempt: after the last attempt no retry is
// owed, whatever the round settled for.
func TestRetryOwedAfterTheLastAttempt(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	for attempt := int32(1); attempt <= v1alpha1.MaxVerificationAttempts; attempt++ {
		e.playTools(v1alpha1.PreviewVerificationName(name, 1, attempt), v1alpha1.VerificationInfra, "", false)
		e.review(name, 3)
	}
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnavailable || v.RetryOwed || v.Cause != v1alpha1.SettleNone {
		t.Fatalf("verification = %+v, want Unavailable, no retry owed, no cause", v)
	}
}

// TestUnjudgedRoundRecordsItsCause: an agents round whose every attempt
// settled for a reason a cause names records that cause, and every
// surface names it, in the state it settled in before causes had names:
// Unavailable, never a judgement and never a fix. Before, every such round
// read as maul not running its checks (review H3, the one wording table).
func TestUnjudgedRoundRecordsItsCause(t *testing.T) {
	for _, tc := range []struct {
		reason v1alpha1.PreviewVerificationReason
		cause  v1alpha1.SettleCause
		why    string
	}{
		{v1alpha1.VerificationReasonSandboxUnenforced, v1alpha1.SettleSandboxUnenforced, templates.HandoffWhySandbox},
		{v1alpha1.VerificationReasonToolsEvicted, v1alpha1.SettleToolsLost, templates.HandoffWhyUnavailable},
		{v1alpha1.VerificationReasonToolsDeadline, v1alpha1.SettleToolsLost, templates.HandoffWhyUnavailable},
		{v1alpha1.VerificationReasonKeptCopyFailed, v1alpha1.SettleToolsLost, templates.HandoffWhyUnavailable},
		{v1alpha1.VerificationReasonReplayIncomplete, v1alpha1.SettleReplayIncomplete, templates.HandoffWhyReplay},
		{v1alpha1.VerificationReasonLedgerMismatch, v1alpha1.SettleNone, templates.HandoffWhyUnavailable},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			e, name := newAgentsEnv(t, repoImage)
			e.reviewReady(name, 4)
			for attempt := int32(1); attempt <= v1alpha1.MaxVerificationAttempts; attempt++ {
				v := e.verification(name)
				if v.Ref == nil || v.Attempt != attempt {
					t.Fatalf("attempt %d: verification = %+v", attempt, v)
				}
				e.settleTools(v.Ref.Name, v1alpha1.VerificationInfra, tc.reason)
				e.reviewReady(name, 3)
			}
			v := e.verification(name)
			if v.State != v1alpha1.VerificationUnavailable || v.Cause != tc.cause || v.RetryOwed ||
				len(v.Outcomes) != 0 {
				t.Fatalf("verification = %+v, want Unavailable for %q", v, tc.cause)
			}
			if runs := e.runsOf(name, v1alpha1.IntentStageRevise); len(runs) != 0 {
				t.Fatalf("revise runs = %d: maul's own failure started a fix", len(runs))
			}
			if h := e.prVerifyPass(name).handoffView(); h == nil || h.State != templates.HandoffUnconfirmed ||
				h.Why != tc.why {
				t.Fatalf("hand-off = %+v, want unconfirmed for %q", h, tc.why)
			}
			head := e.get(name).Status.PullRequests[0].HeadSHA
			if s := e.latestAcceptance(head); s.State != statusError ||
				s.Description != templates.CauseWords(tc.cause, true).Short {
				t.Fatalf("status = %+v, want an error naming %q", s, tc.cause)
			}
		})
	}
}
