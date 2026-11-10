// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/command"
	"github.com/devthenet-labs/maul/internal/templates"
)

// currencyCase is one seeded shape of a judged verification's record
// against the pull requests now: the head moved on the pull request alone
// (the head set not yet synced), the outcomes written by another round or
// at another head set than the record's, or the round settled unjudged (a
// recorded reason it could not run, agentsWhy).
type currencyCase struct {
	moved, otherRound, otherHeads, unjudged bool
	// shift moves the record's round and its outcomes' round together.
	shift int32
}

func (c currencyCase) key() string {
	return fmt.Sprintf("moved=%v otherRound=%v otherHeads=%v unjudged=%v",
		c.moved, c.otherRound, c.otherHeads, c.unjudged)
}

// reporterViews is whether each reporter presents the record's judgement
// as the current one, and whether each says the heads moved (the hand-off
// has no words of its own for that: it says maul is still checking).
type reporterViews struct {
	statusPass, handoffReady, commentPass, notRunStands, accepted bool
	statusMoved, commentMoved, handoffChecking                    bool
}

// TestReportersAgreeOnCurrencySeeded (S4 PR 9) is a seeded property over
// (recorded heads, current heads, round, judged round and head set, an
// unjudged settle) on a ready judgement, for the slot, the agents host and
// the agents host switched off: every reporter (maul/acceptance, the
// hand-off, the comments' progress, the not-run answer's standing
// judgement and /maul accept) agrees on whether the record is current. The
// oracle is computed from the case alone: a judgement is current only at
// the pull requests' heads now, written by the record's round at the
// record's head set, by a round that ran; an acceptance binds only at the
// heads now, where something checks them. No reporter presents a judgement
// that is not current (a false pass) or refuses one that is (a false
// fail).
func TestReportersAgreeOnCurrencySeeded(t *testing.T) {
	placements := []struct {
		name  string
		setup func(t *testing.T) (*env, string)
		// statuses, accepts: whether maul/acceptance reports the judgement
		// at all, and whether an acceptance can bind (the host off: nothing
		// verifies or checks, so neither).
		statuses, accepts, hostOff bool
	}{
		{"slot", readySlotJudgement, true, true, false},
		{"agents", readyAgentsJudgement, true, true, false},
		{"agents-host-off", func(t *testing.T) (*env, string) {
			e, name := readyAgentsJudgement(t)
			e.agentsOn(false)
			e.review(name, 4)
			return e, name
		}, false, false, true},
	}
	for _, pl := range placements {
		t.Run(pl.name, func(t *testing.T) {
			t.Parallel()
			e, name := pl.setup(t)
			base := e.get(name).Status.DeepCopy()
			if got := currencyViews(t, e, name); !got.handoffReady || !got.notRunStands ||
				got.statusPass != pl.statuses || got.accepted != pl.accepts {
				t.Fatalf("views of the ready judgement = %+v, want it presented as current", got)
			}
			seen := map[string]bool{}
			for seed := range int64(256) {
				rng := rand.New(rand.NewSource(seed))
				c := currencyCase{moved: rng.Intn(2) == 0, otherRound: rng.Intn(3) == 0,
					otherHeads: rng.Intn(3) == 0, unjudged: rng.Intn(4) == 0, shift: int32(rng.Intn(3))}
				seen[c.key()] = true
				e.setStatus(name, func(in *v1alpha1.Intent) {
					in.Status = *base.DeepCopy()
					c.apply(in, rng)
				})
				got := currencyViews(t, e, name)
				current := !c.moved
				judged := current && !c.otherRound && !c.otherHeads && !c.unjudged
				want := reporterViews{
					statusPass: judged && pl.statuses, handoffReady: judged, commentPass: judged,
					notRunStands: judged, accepted: current && pl.accepts,
					statusMoved: c.moved && pl.statuses, commentMoved: c.moved,
					// Still checking at moved heads; with the host off
					// maul couldn't confirm them (nothing checks them).
					// Elsewhere the hand-off's other words are its own.
					handoffChecking: got.handoffChecking,
				}
				if c.moved {
					want.handoffChecking = !pl.hostOff
				}
				if got != want {
					t.Errorf("seed %d (%s): views = %+v, want %+v", seed, c.key(), got, want)
				}
			}
			if len(seen) != 16 {
				t.Errorf("the seeds reached %d of the 16 shapes", len(seen))
			}
		})
	}
}

// apply writes c onto in's ready judgement.
func (c currencyCase) apply(in *v1alpha1.Intent, rng *rand.Rand) {
	v := in.Status.Verification
	v.Round += c.shift
	v.OutcomesRound += c.shift
	if c.otherRound {
		v.OutcomesRound += 1 + int32(rng.Intn(2))*-2
	}
	if c.otherHeads {
		v.OutcomesHeadSetDigest = "sha256:" + strings.Repeat("c", 64)
	}
	if c.unjudged {
		v.State, v.UnavailableReason = v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationUnavailableTree
	}
	if c.moved {
		in.Status.PullRequests[0].HeadSHA = strings.Repeat(string(rune('d'+rng.Intn(3))), 40)
	}
}

// currencyViews reads every reporter over name's Intent as it stands. The
// acceptance is given an hour from now (after anything was deployed or
// checked) and written, so the caller resets the status before the next.
func currencyViews(t *testing.T, e *env, name string) reporterViews {
	t.Helper()
	var r reporterViews
	p := e.prVerifyPass(name)
	if st, _, ok := p.acceptanceStatus(); ok {
		r.statusPass = st.state == statusSuccess
		r.statusMoved = st == headsMoved
	}
	if h := p.handoffView(); h != nil {
		r.handoffReady = h.State == templates.HandoffReady
		r.handoffChecking = h.State == templates.HandoffChecking
	}
	if prog := p.verifyProgress(); prog != nil {
		r.commentPass = prog.State == templates.VerifyPassed
		r.commentMoved = prog.State == templates.VerifyHeadsMoved
	}
	r.notRunStands = p.standingJudgement(p.in.Status.Verification) != 0
	c := verifyCommand{surface: command.IntentIssue, key: "c-1", id: 1, at: e.clock.Now().Add(time.Hour),
		actor: actorOf(approver), verb: "accept", body: "/maul accept " + oManual}
	n, err := p.recordAcceptance(context.Background(), c, []string{oManual}, templates.VerifyCommandNotice{})
	if err != nil {
		t.Fatal(err)
	}
	r.accepted = n.Outcome == templates.VerifyAccepted
	return r
}

// readySlotJudgement is an intent on a previewed Project judged ready at
// its heads: every automatable outcome confirmed, the manual one accepted,
// the hand-off released.
func readySlotJudgement(t *testing.T) (*env, string) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	readyJudgement(e, name)
	return e, name
}

// readyAgentsJudgement is readySlotJudgement on a cli Project the agents
// tool pod evaluates.
func readyAgentsJudgement(t *testing.T) (*env, string) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	readyJudgement(e, name)
	return e, name
}

// readyJudgement writes a ready judgement of name's current round at its
// heads, as the round's judge and an approver would.
func readyJudgement(e *env, name string) {
	e.t.Helper()
	p := e.prVerifyPass(name)
	if v := p.in.Status.Verification; v == nil || v.Ref == nil || v.HeadSetDigest == "" {
		e.t.Fatalf("verification = %+v, want a round in flight", v)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		routeRecords(in, judgeRoute{acceptManual: true}, e.clock.Now())
		applyJudgement(in, roundJudgement{decision: confirmedTwo}, e.clock.Now())
		p.releaseIfDue(in)
	})
}
