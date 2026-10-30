// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// heldText is what a held link says in its place.
const heldText = "maul holds its link until it has checked the approved outcomes"

// TestHandoffMaxReleasesOutOfTime: a gated hand-off holds the link while
// the evaluation runs, its clock started when the first evaluation was
// created (CI green at the first heads); once handoffMax passes from then,
// with the evaluation still running, the link is released as "maul
// couldn't confirm this: it ran out of time".
func TestHandoffMaxReleasesOutOfTime(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationEvaluating || v.FirstGreenAt == nil || v.Handoff == nil ||
		v.Handoff.Mode != v1alpha1.HandoffGated || v.Handoff.Released {
		t.Fatalf("verification = %+v, want a held gated hand-off with its clock started", v)
	}
	started := v.FirstGreenAt.Time
	e.reviewUntil(name, func() bool { return strings.Contains(e.statusBody(), heldText) })
	if strings.Contains(e.statusBody(), previewHostOf(name)) {
		t.Errorf("the held link is shown:\n%s", e.statusBody())
	}

	e.clock.Advance(started.Add(time.Hour - 2*time.Minute).Sub(e.clock.Now()))
	e.review(name, 1)
	if v := e.verification(name); v.Handoff.Released {
		t.Fatalf("hand-off = %+v released before handoffMax", v.Handoff)
	}
	e.review(name, 2)
	v = e.verification(name)
	if !v.Handoff.Released || v.Handoff.Reason != v1alpha1.HandoffReasonOutOfTime ||
		v.State != v1alpha1.VerificationEvaluating {
		t.Fatalf("hand-off = %+v (state %s), want released out of time while it still evaluates", v.Handoff, v.State)
	}
	h := e.prVerifyPass(name).handoffView()
	if h == nil || h.State != templates.HandoffUnconfirmed || h.Why != templates.HandoffWhyTime {
		t.Errorf("hand-off view = %+v, want couldn't confirm: out of time", h)
	}
	e.reviewUntil(name, func() bool { return strings.Contains(e.statusBody(), "it ran out of time") })
	if !strings.Contains(e.statusBody(), previewHostOf(name)) {
		t.Errorf("the released link is not shown:\n%s", e.statusBody())
	}
}

// TestHumanFeedbackReleasesAndHalts: an approver's review in the middle of
// an evaluation round, still inside its quiet window, releases the held
// link at once and halts automatic fixes (the human wins); the round goes
// on, and the human's round follows once the window ends.
func TestHumanFeedbackReleasesAndHalts(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating || v.Handoff.Released {
		t.Fatalf("verification = %+v, want an evaluation with the link held", v)
	}
	e.reviewOn(e.prNumber(name), 1901, "The version page needs the build time too.")
	e.review(name, 1)
	v := e.verification(name)
	if !v.Handoff.Released || v.Handoff.Reason != v1alpha1.HandoffReasonHuman {
		t.Fatalf("hand-off = %+v, want released for the human", v.Handoff)
	}
	if v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixHumanReview {
		t.Errorf("auto-fix = %+v, want halted for the human's review", v.AutoFix)
	}
	if v.State != v1alpha1.VerificationEvaluating || len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 0 {
		t.Errorf("state %s, revise runs %d: the round goes on and the human's waits for its window", v.State,
			len(e.runsOf(name, v1alpha1.IntentStageRevise)))
	}
	if h := e.prVerifyPass(name).handoffView(); h == nil || h.State != templates.HandoffChecking {
		t.Errorf("hand-off view = %+v, want shown while maul is still checking", h)
	}
	e.reviewUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStageRevise)) > 0 })
}

// TestPushBeforeTheFirstEvaluationReleases: the link is held from the
// first pass, before the first evaluation records its hand-off; a push
// maul did not make in that time releases it at once all the same, and
// the evaluation that follows keeps it released.
func TestPushBeforeTheFirstEvaluationReleases(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.reviewUntil(name, func() bool { return e.verification(name).HeadSetDigest != "" })
	if v := e.verification(name); v.Handoff != nil || !e.prVerifyPass(name).linkHeld() {
		t.Fatalf("verification = %+v, want the link held with no hand-off recorded yet", v)
	}
	e.pushHead(e.prNumber(name), strings.Repeat("e", 40))
	e.review(name, 2)
	v := e.verification(name)
	if v.Handoff == nil || !v.Handoff.Released || v.Handoff.Reason != v1alpha1.HandoffReasonHuman ||
		v.Handoff.Mode != v1alpha1.HandoffGated {
		t.Fatalf("hand-off = %+v, want released for the human's push", v.Handoff)
	}
	if v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixHumanPush {
		t.Errorf("auto-fix = %+v, want halted for the push", v.AutoFix)
	}
	e.readyPreview(name)
	e.reviewUntil(name, func() bool { return e.verification(name).FirstGreenAt != nil })
	if h := e.verification(name).Handoff; !h.Released || h.Reason != v1alpha1.HandoffReasonHuman {
		t.Errorf("hand-off = %+v after the first evaluation, want it kept released", h)
	}
}

// TestPushReArmedAfterReleasesTheLinkOnly: a push an approver re-armed
// automatic fixes after still releases the held link (the human wins it),
// while automatic fixes stay armed.
func TestPushReArmedAfterReleasesTheLinkOnly(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	if v := e.verification(name); v.Handoff == nil || v.Handoff.Released {
		t.Fatalf("verification = %+v, want the link held", v)
	}
	e.pushHead(e.prNumber(name), strings.Repeat("e", 40))
	e.clock.Advance(6 * time.Second)
	e.rearm(name)
	e.review(name, 2)
	v := e.verification(name)
	if !movedTo(v, strings.Repeat("e", 40)) {
		t.Fatalf("verification = %+v, want the pushed head set recorded", v)
	}
	if !v.Handoff.Released || v.Handoff.Reason != v1alpha1.HandoffReasonHuman {
		t.Errorf("hand-off = %+v, want released for the human's push", v.Handoff)
	}
	if v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Errorf("auto-fix = %+v, want the later re-arm to stand", v.AutoFix)
	}
}

// TestEarlyHandoffKeepsSliceOne: under an early hand-off nothing is held:
// it is released at once as the evaluation starts, the views carry no
// hand-off, and the status comment links the preview exactly as slice 1
// does, with no hand-off label.
func TestEarlyHandoffKeepsSliceOne(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4, func(p *v1alpha1.Project) {
		p.Spec.Verification.Handoff = v1alpha1.HandoffEarly
	})
	e.review(name, 3)
	v := e.verification(name)
	if v.Handoff == nil || v.Handoff.Mode != v1alpha1.HandoffEarly || !v.Handoff.Released ||
		v.Handoff.Reason != v1alpha1.HandoffReasonEarly {
		t.Fatalf("hand-off = %+v, want early, released at once", v.Handoff)
	}
	p := e.prVerifyPass(name)
	if h := p.handoffView(); h != nil {
		t.Errorf("hand-off view = %+v, want none under early", h)
	}
	if p.linkHeld() {
		t.Error("an early hand-off holds the link")
	}
	link := "[`" + previewHostOf(name) + "`](https://" + previewHostOf(name) + ")"
	e.reviewUntil(name, func() bool { return strings.Contains(e.statusBody(), "**Preview:** "+link) })
	for _, label := range []string{heldText, "Ready:", "couldn't confirm", "still checking"} {
		if strings.Contains(e.statusBody(), label) {
			t.Errorf("an early hand-off's status comment says %q", label)
		}
	}
}

// versionCheck2 is a second, corrected check of the version outcome.
const versionCheck2 = `{"id":"version-shown.again","outcome":"version-shown","form":"http","http":{"target":"app",
 "steps":[{"request":{"method":"POST","path":"/notes","json":{"name":"{{run}}-w"}},"expect":{"status":201}},
 {"request":{"path":"/version?q={{run}}"},"expect":{"json":{"/note":"{{run}}-w"}}}]}}`

// TestDisputeRetiresAndReplays: the evaluator disputes the stored check
// that failed its replay and proposes a corrected one; once the
// replacement is confirmed and reviewed proves, the disputed check is
// retired from the store (a new store, in a pass of its own) and the round
// is Unconfirmed with the link held for the one replay round. That round
// runs no evaluator, passes the outcome on the replacement, and the link is
// then handed over.
func TestDisputeRetiresAndReplays(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	old, repl := savedCheck(t, versionCheck), savedCheck(t, versionCheck2)
	e.evaluateReporting(`---
verdicts:
  - id: version-shown
    verdict: unsure
  - id: still-healthy
    verdict: pass
    ledger: [3]
proposed:
  - id: version-shown.again
    outcome: version-shown
    digest: "` + repl.Digest + `"
disputes:
  - check_digest: "` + old.Digest + `"
    reason: the note it reads is another run's
    replacement: version-shown.again
    ledger: [2]
---
The stored version check reads a note another run wrote.
`)
	e.reviewChecksWith(&fakeReviewer{})
	e.storeChecks(name, old)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.confirm(pv.Name, repl)
	got := e.pvNamed(pv.Name)
	got.Status.Replay = []v1alpha1.CheckRun{{CheckDigest: old.Digest, Result: v1alpha1.CheckRunFail, Token: "t0",
		Seq: 2}}
	if err := e.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	e.settleReviews(name)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || !slices.Equal(v.Disputed, []string{old.Digest}) ||
		v.Handoff.Released {
		t.Fatalf("verification = %s disputed %v hand-off %+v; want Unconfirmed awaiting its replay, link held",
			v.State, v.Disputed, v.Handoff)
	}
	if _, stored := e.checkStore(name); !slices.Equal(digestsOf(stored), []string{repl.Digest}) {
		t.Fatalf("store = %v, want the replacement alone", digestsOf(stored))
	}

	e.reviewUntil(name, func() bool { return len(e.verifications(name)) == 2 })
	var replay v1alpha1.PreviewVerification
	for _, p := range e.verifications(name) {
		if p.Spec.Round == 2 {
			replay = p
		}
	}
	if replay.Spec.Rung != v1alpha1.RungReplay || len(replay.Spec.Checks) != 1 ||
		replay.Spec.Checks[0].Digest != repl.Digest {
		t.Fatalf("replay round = %+v, want the replacement replayed", replay.Spec)
	}
	e.playTools(replay.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.replayed(replay.Name, v1alpha1.CheckRun{CheckDigest: repl.Digest, Result: v1alpha1.CheckRunPass, Token: "t3",
		Seq: 1})
	e.reviewUntil(name, func() bool { return e.verification(name).State != v1alpha1.VerificationEvaluating })
	v = e.verification(name)
	if r := v.Outcomes[0]; r.Result != v1alpha1.OutcomePassed || r.Basis != repl.Digest {
		t.Errorf("version outcome = %+v, want passed on the replacement", r)
	}
	if len(v.Disputed) != 0 || !v.Handoff.Released {
		t.Errorf("disputed %v, hand-off %+v: want the replay done and the link handed over", v.Disputed, v.Handoff)
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageEvaluate)); n != 1 {
		t.Errorf("evaluate runs = %d, want the full round's alone", n)
	}
}
