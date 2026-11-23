// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/command"
	"github.com/devthenet-labs/maul/internal/templates"
)

// currencyCase is one seeded shape of a judged verification's record
// against the pull requests now: the head moved on the pull request alone
// (the head set not yet synced), the outcomes written by another round or
// at another head set than the record's, or the round settled unjudged (a
// recorded reason it could not run, settledWhy).
type currencyCase struct {
	moved, otherRound, otherHeads, unjudged bool
	// shift moves the record's round and its outcomes' round together.
	shift int32
	// verdict is the judgement's state (verdictReady keeps the ready one;
	// the others record one outcome failing or unsure), and accept how an
	// approver accepted that outcome after the judgement (code review
	// 2026-11-18, rounds 11).
	verdict verdictCase
	accept  acceptCase
}

// verdictCase is the state a judgement settled in.
type verdictCase int

const (
	verdictReady verdictCase = iota
	verdictFailing
	verdictExhausted
	verdictUnconfirmed
	verdictCases
)

// acceptCase is how an approver accepted the outcome maul did not
// confirm, after the judgement: at the record's head set, not at all (a
// partial acceptance: the manual outcome only), or at another head set.
type acceptCase int

const (
	acceptAtHeads acceptCase = iota
	acceptNone
	acceptOtherHeads
	acceptCases
)

func (c currencyCase) key() string {
	return fmt.Sprintf("moved=%v otherRound=%v otherHeads=%v unjudged=%v",
		c.moved, c.otherRound, c.otherHeads, c.unjudged)
}

// verdictKey is c's judgement shape: its verdict and, for any but the
// ready one, how it was accepted.
func (c currencyCase) verdictKey() string {
	if c.verdict == verdictReady {
		return "ready"
	}
	return fmt.Sprintf("verdict=%d accept=%d", c.verdict, c.accept)
}

// ready is the oracle's readiness of c's judgement, its currency apart:
// every outcome maul did not confirm accepted by an approver at the
// record's head set, whatever state the judgement settled in.
func (c currencyCase) ready() bool {
	return c.verdict == verdictReady || c.accept == acceptAtHeads
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
// unjudged settle, and the state the judgement settled in with an
// approver's acceptance after it: Passed, or Failing, Exhausted or
// Unconfirmed with the outcome maul did not confirm accepted at the heads,
// not at all, or at other heads) on a judgement, for the slot, the agents
// host and
// the agents host switched off: every reporter (maul/acceptance, the
// hand-off, the comments' progress, the not-run answer's standing
// judgement and /maul accept) agrees on whether the record is current. The
// oracle is computed from the case alone: a judgement is current only at
// the pull requests' heads now, written by the record's round at the
// record's head set, by a round that ran; it is ready only when every
// outcome maul did not confirm was accepted at the record's head set,
// whatever state it settled in; an acceptance binds only at the heads
// now, where something checks them. No reporter presents a judgement
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
			seen, verdicts := map[string]bool{}, map[string]bool{}
			for seed := range int64(512) {
				rng := rand.New(rand.NewSource(seed))
				c := currencyCase{moved: rng.Intn(2) == 0, otherRound: rng.Intn(3) == 0,
					otherHeads: rng.Intn(3) == 0, unjudged: rng.Intn(4) == 0, shift: int32(rng.Intn(3)),
					verdict: verdictCase(rng.Intn(int(verdictCases))), accept: acceptCase(rng.Intn(int(acceptCases)))}
				seen[c.key()], verdicts[c.verdictKey()] = true, true
				e.setStatus(name, func(in *v1alpha1.Intent) {
					in.Status = *base.DeepCopy()
					c.apply(in, rng)
				})
				got := currencyViews(t, e, name)
				current := !c.moved
				judged := current && !c.otherRound && !c.otherHeads && !c.unjudged
				ready := judged && c.ready()
				want := reporterViews{
					statusPass: ready && pl.statuses, handoffReady: ready, commentPass: ready,
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
					t.Errorf("seed %d (%s %s): views = %+v, want %+v", seed, c.key(), c.verdictKey(), got, want)
				}
			}
			if len(seen) != 16 {
				t.Errorf("the seeds reached %d of the 16 shapes", len(seen))
			}
			if want := 1 + int(verdictCases-1)*int(acceptCases); len(verdicts) != want {
				t.Errorf("the seeds reached %d of the %d judgement shapes", len(verdicts), want)
			}
		})
	}
}

// apply writes c onto in's ready judgement.
func (c currencyCase) apply(in *v1alpha1.Intent, rng *rand.Rand) {
	c.applyVerdict(in)
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
		v.State, v.Cause = v1alpha1.VerificationUnconfirmed, v1alpha1.SettleTree
	}
	if c.moved {
		in.Status.PullRequests[0].HeadSHA = strings.Repeat(string(rune('d'+rng.Intn(3))), 40)
	}
}

// applyVerdict writes c's verdict onto in's ready judgement as the
// round's judge would have (the version outcome failing, or unsure), then
// the approver's acceptance of that outcome after it, as recordAcceptance
// writes one: the state stays as judged.
func (c currencyCase) applyVerdict(in *v1alpha1.Intent) {
	v := in.Status.Verification
	if c.verdict == verdictReady {
		return
	}
	i := slices.IndexFunc(v.Outcomes, func(r v1alpha1.OutcomeResult) bool { return r.ID == oVersion })
	v.Outcomes[i].Result, v.Outcomes[i].Basis, v.Passed = v1alpha1.OutcomeFailed, "", v.Passed-1
	switch c.verdict {
	case verdictFailing:
		v.State, v.Failed = v1alpha1.VerificationFailing, v.Failed+1
	case verdictExhausted:
		v.State, v.Failed = v1alpha1.VerificationExhausted, v.Failed+1
	default:
		v.State, v.Outcomes[i].Result = v1alpha1.VerificationUnconfirmed, v1alpha1.OutcomeUnsure
	}
	headSet := v.HeadSetDigest
	switch c.accept {
	case acceptNone:
		return
	case acceptOtherHeads:
		headSet = "sha256:" + strings.Repeat("e", 64)
	}
	in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{
		ID: oVersion, By: approver, ByID: 7, At: metav1.Now(), CommentID: 98, HeadSetDigest: headSet,
		Heads: []string{"abcdef1"}})
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
		applyJudgement(in, passedJudgement(in))
		p.releaseIfDue(in)
	})
}
