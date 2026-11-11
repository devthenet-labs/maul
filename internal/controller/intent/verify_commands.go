// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/command"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The acceptance commands, /maul verify [fix|stop|--reset|explore], /maul accept
// <id>|all and /maul preview, settle here for both surfaces that offer them:
// the intent issue (from
// decide, on the poll's answer path) and each of the intent's pull requests
// (settlePRVerifyCommands). An intent has one combined change and one
// Preview, so the surface a command is made on changes only where its
// authority is checked (the repository it was made in) and where it is
// answered, never what it does. Every effect is a status write of the intent
// reconciler; nothing here starts a round or touches a Preview: the
// verification loop reads status.verification.requested, the reset
// generation and the auto-fix latch, status.acceptance.accepted, and the
// hand-off (status.verification.handoff), which /maul preview releases.

// verifyResetInterval spaces resets: each restarts every part of the
// Preview and drops its data, so one approver's burst of --reset comments
// costs one restart.
const verifyResetInterval = 5 * time.Minute

// verifyCommand is one acceptance command, from either surface.
type verifyCommand struct {
	surface command.Surface
	// key names the reply (humanAction.key's form: comment ids are unique
	// across issues and pull requests).
	key   string
	id    int64
	at    time.Time
	actor ghclient.Actor
	verb  string
	body  string
	// pr is the pull request it was made on; nil on the intent issue.
	pr *v1alpha1.IntentPullRequest
}

// requestSource is where c was made, as a recorded request keeps it: the
// pull request, nil for the intent issue.
func (c verifyCommand) requestSource() *v1alpha1.VerifyRequestPullRequest {
	if c.pr == nil {
		return nil
	}
	return &v1alpha1.VerifyRequestPullRequest{Repository: c.pr.Repository, Number: c.pr.Number}
}

// isVerifyVerb reports an acceptance command's verb.
func isVerifyVerb(verb string) bool {
	return verb == action.VerbVerify || verb == action.VerbAccept || verb == action.VerbPreview
}

// verifyPhase reports a phase the acceptance commands mean something in:
// review, and a round of it, which is when the Preview exists.
func verifyPhase(phase v1alpha1.IntentPhase) bool {
	return phase == v1alpha1.IntentInReview || phase == v1alpha1.IntentRevising
}

// verifyRecorded reports an acceptance command whose effect the Intent
// already records, by the comment carrying it: only its reply may be owed.
// status.commands.applied holds it durably, written with the effect, so it
// counts after the request it made was consumed or the acceptance it gave
// superseded, whatever became of maul's reply.
func (p *pass) verifyRecorded(id int64) bool {
	if c := p.in.Status.Commands; c != nil && slices.Contains(c.Applied, id) {
		return true
	}
	if v := p.in.Status.Verification; v != nil && v.Requested != nil && v.Requested.CommentID == id {
		return true
	}
	if acc := p.in.Status.Acceptance; acc != nil {
		return slices.ContainsFunc(acc.Accepted, func(r v1alpha1.AcceptanceRecord) bool { return r.CommentID == id })
	}
	return false
}

// settleVerifyIssue settles an acceptance command on the intent issue: an
// approver's with write access to the intent repository, while the intent is
// in review; anything else is refused or not available, as every other
// intent command is.
func (p *pass) settleVerifyIssue(ctx context.Context, a humanAction) error {
	ok, bot, err := p.authorize(ctx, a.actor)
	if err != nil {
		return err
	}
	if !ok || a.actor.ID < 1 {
		// An account with no numeric id cannot be recorded as the actor.
		return p.notAllowed(ctx, a, bot, false, false)
	}
	if !p.set.Verification || !verifyPhase(p.in.Status.Phase) {
		// With verification (the evaluator) off nothing of acceptance is written.
		return p.notAvailable(ctx, a, p.in.Status.Phase, false)
	}
	body := ""
	if a.comment != nil {
		body = a.comment.Body
	}
	n, err := p.applyVerifyCommand(ctx, verifyCommand{surface: command.IntentIssue, key: a.key(), id: a.id,
		at: a.at, actor: a.actor, verb: a.verb, body: body})
	if err != nil {
		return err
	}
	return p.verifyNotice(ctx, a.key(), a.at, n)
}

// replyVerifyRecorded answers an acceptance command the Intent already
// records, from what it records.
func (p *pass) replyVerifyRecorded(ctx context.Context, a humanAction) error {
	body := ""
	if a.comment != nil {
		body = a.comment.Body
	}
	n := p.recordedVerifyNotice(verifyCommand{surface: command.IntentIssue, key: a.key(), id: a.id, actor: a.actor,
		verb: a.verb, body: body})
	return p.verifyNotice(ctx, a.key(), a.at, n)
}

// recordedVerifyNotice is the reply to an acceptance command recorded as
// comment c.id: what it recorded while that still stands, else that it was
// applied.
func (p *pass) recordedVerifyNotice(c verifyCommand) templates.VerifyCommandNotice {
	n := p.verifyNoticeFor(c.surface, c.key, c.actor, c.verb)
	id := c.id
	n.Outcome = templates.VerifyApplied
	if v := p.in.Status.Verification; c.verb == action.VerbPreview && v != nil && v.Handoff != nil &&
		v.Handoff.Released && v.Handoff.Reason == v1alpha1.HandoffReasonCommand {
		n.Outcome = templates.VerifyPreviewReleased
		return n
	}
	if v := p.in.Status.Verification; v != nil && v.Requested != nil && v.Requested.CommentID == id {
		n.Outcome, n.Fix, n.Reset = templates.VerifyRequested, v.Requested.Fix, v.Requested.Reset
		n.Explore = v.Requested.Explore
		n.AfterRound = p.in.Status.Phase == v1alpha1.IntentRevising
		return n
	}
	if v := p.in.Status.Verification; v != nil && v.AutoFix.State == v1alpha1.AutoFixHalted &&
		v.AutoFix.Stop != nil && v.AutoFix.Stop.CommentID == id {
		n.Outcome = templates.VerifyStopped
		return n
	}
	return p.recordedAcceptance(c, n)
}

// recordedAcceptance is recordedVerifyNotice for /maul accept: the ids
// comment c.id is recorded as accepting, while those records stand.
func (p *pass) recordedAcceptance(c verifyCommand, n templates.VerifyCommandNotice) templates.VerifyCommandNotice {
	acc := p.in.Status.Acceptance
	if acc == nil {
		return n
	}
	n.Outcomes = len(acc.Outcomes) > 0
	for _, r := range acc.Accepted {
		if r.CommentID == c.id {
			n.Outcome, n.Criterion, n.Heads = templates.VerifyAccepted, r.ID, r.Heads
			n.NoCriteria = r.ID == acceptAll && !n.Outcomes
			n.Accepted = append(n.Accepted, r.ID)
		}
	}
	if named, ok := command.ParseAccept(c.body); n.Outcome == templates.VerifyAccepted && ok && named == acceptAll {
		n.Criterion = acceptAll // /maul accept all, however many it closed
	}
	return n
}

// verifyNoticeFor is a reply's identity: who, where, which verb.
func (p *pass) verifyNoticeFor(surface command.Surface, key string, actor ghclient.Actor,
	verb string) templates.VerifyCommandNotice {
	return templates.VerifyCommandNotice{Namespace: p.in.Namespace, Intent: p.in.Name, Key: key, Surface: surface,
		Actor: actor.Login, Verb: verb, Workspace: p.set.linkless(p.proj)}
}

// verifyNotice posts a verify reply on the intent issue, once.
func (p *pass) verifyNotice(ctx context.Context, key string, at time.Time, n templates.VerifyCommandNotice) error {
	body, err := templates.RenderVerifyCommandNotice(n)
	return p.notice(ctx, key, at, body, err)
}

// applyVerifyCommand applies an authorised approver's acceptance command,
// made while the intent is in review, and returns the reply. Arguments that
// are not the command's grammar change nothing and are answered with its
// help.
func (p *pass) applyVerifyCommand(ctx context.Context, c verifyCommand) (templates.VerifyCommandNotice, error) {
	n := p.verifyNoticeFor(c.surface, c.key, c.actor, c.verb)
	switch c.verb {
	case action.VerbVerify:
		args, ok := command.ParseVerify(c.body)
		if !ok {
			n.Outcome = templates.VerifyUsage
			return n, nil
		}
		if args.Stop {
			return p.stopAutoFix(ctx, c, n)
		}
		if why, off := p.offWhy(); off {
			// Nothing would ever run it, nor a fix round a confirmation
			// starts: nothing is recorded, and the reply says why (S2-B8),
			// never that it is.
			n.Outcome, n.NotRunWhy, n.Fix, n.Reset = templates.VerifyNotRun, why, args.Fix, args.Reset
			n.Explore = args.Explore
			return n, nil
		}
		if args.Fix && !args.Reset && suspectedConfirmable(p.in.Status.Verification, c.at) {
			return p.confirmSuspected(ctx, c, n)
		}
		return p.requestVerification(ctx, c, args, n)
	case action.VerbAccept:
		id, ok := command.ParseAccept(c.body)
		if !ok {
			n.Outcome = templates.VerifyUsage
			return n, nil
		}
		n.Criterion = id
		return p.acceptCriterion(ctx, c, id, n)
	case action.VerbPreview:
		if !command.ParsePreview(c.body) {
			n.Outcome = templates.VerifyUsage
			return n, nil
		}
		return p.releasePreview(ctx, c, n)
	}
	return n, fmt.Errorf("verify command: unexpected verb %q", c.verb)
}

// releasePreview records /maul preview: the held preview link is released
// at once, reason command, and nothing else changes (the evaluation goes on,
// and its results follow). Only a plan that declares outcomes under a gated
// hand-off holds the link; anything else, or a link released already, is
// answered and records nothing.
func (p *pass) releasePreview(ctx context.Context, c verifyCommand,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	if p.set.linkless(p.proj) {
		// No preview, so no link to show: the hand-off is the result's.
		n.Outcome = templates.VerifyPreviewNone
		return n, nil
	}
	if v := p.in.Status.Verification; v != nil && v.Handoff != nil && v.Handoff.Released {
		n.Outcome = templates.VerifyPreviewAlready
		return n, nil
	}
	mode, held := p.previewHeld()
	if !held {
		n.Outcome = templates.VerifyPreviewNotHeld
		return n, nil
	}
	if err := p.update(ctx, func(in *v1alpha1.Intent) error {
		v := in.Status.Verification
		if v == nil {
			v = &v1alpha1.IntentVerification{}
			in.Status.Verification = v
		}
		at := metav1.NewTime(p.now)
		v.Handoff = &v1alpha1.HandoffState{Mode: mode, Released: true, ReleasedAt: &at,
			Reason: v1alpha1.HandoffReasonCommand}
		rememberApplied(in, c.id)
		return nil
	}); err != nil {
		return n, err
	}
	n.Outcome = templates.VerifyPreviewReleased
	return n, nil
}

// previewHeld reports the hand-off mode in force and whether it holds the
// preview link: only for an approved plan that declares outcomes (the
// evaluator's), under a gated hand-off. The mode is the one recorded on the
// hand-off, else the Project's, gated by default (DESIGN 7.2).
func (p *pass) previewHeld() (v1alpha1.HandoffMode, bool) {
	if p.mode() == modeNone || len(p.in.Status.Acceptance.Outcomes) == 0 {
		return "", false
	}
	mode := p.proj.Spec.HandoffMode(v1alpha1.HandoffGated)
	if v := p.in.Status.Verification; v != nil && v.Handoff != nil {
		mode = v.Handoff.Mode
	}
	return mode, mode == v1alpha1.HandoffGated
}

// requestVerification records /maul verify as status.verification.requested,
// for the verification loop to run at its next opportunity, bypassing the
// automatic interval. A request made while one waits joins it: the newest
// comment stands for both, and neither's fix, reset or explore is lost.
// --reset also bumps the reset generation, which the Preview copies, unless
// the last reset was under verifyResetInterval before it; fix also re-arms
// automatic fixes from the current round (a human round after it, or a
// human push, halts them again); explore makes the round full, never a
// replay (rungFor). Once the approved plan revision has used the Project's
// maxVerifications nothing is recorded and the reply says so. One made while
// verification is off is answered before this (applyVerifyCommand).
func (p *pass) requestVerification(ctx context.Context, c verifyCommand, args command.VerifyArgs,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	var cur v1alpha1.IntentVerification
	if v := p.in.Status.Verification; v != nil {
		cur = *v
	}
	if why, err := p.agentsUnrunnable(ctx); err != nil {
		return n, err
	} else if why != "" {
		// The agents round is known to be unable to run: say so once,
		// here, rather than acknowledge now and answer later (S2-B10).
		n.Outcome, n.NotRunWhy, n.Fix, n.Reset = templates.VerifyNotRun, why, args.Fix, args.Reset
		n.Explore = args.Explore
		n.Stands = p.standingJudgement(&cur)
		return n, nil
	}
	if !args.Reset && !p.fixesStanding(args, &cur) && p.previewExpired(ctx, cur.Reset.Generation) {
		// Nothing deploys the preview again at these commits: say so now
		// rather than acknowledge a wait for nothing (gate D-B5). A reset
		// changes the Preview's spec, which deploys it again, and so does
		// the push of a fix round a fix request starts from a failing
		// judgement (fixesStanding).
		n.Outcome, n.NotRunWhy, n.Fix = templates.VerifyNotRun, templates.NotRunPreviewExpired, args.Fix
		n.Explore = args.Explore
		n.Stands = p.standingJudgement(&cur)
		return n, nil
	}
	if most := p.proj.Spec.Limits.MaxVerificationsLimit(); cur.Automatic >= most {
		// A request bypasses the interval, never the cap: the gate would
		// drop it unrun, so it is not recorded and the reply says why.
		n.Outcome, n.MaxVerifications = templates.VerifyCapReached, most
		return n, nil
	}
	if args.Reset && cur.Reset.At != nil && c.at.Sub(cur.Reset.At.Time) < verifyResetInterval {
		n.Outcome, n.ResetWait = templates.VerifyResetTooSoon, "5 minutes"
		return n, nil
	}
	waiting := cur.Requested
	at := metav1.NewTime(c.at)
	req := joinedRequest(c, args, waiting)
	if err := p.update(ctx, func(in *v1alpha1.Intent) error {
		v := in.Status.Verification
		if v == nil {
			v = &v1alpha1.IntentVerification{}
			in.Status.Verification = v
		}
		v.Requested = &req
		rememberApplied(in, c.id)
		if args.Reset {
			v.Reset = v1alpha1.ResetState{Generation: v.Reset.Generation + 1, By: c.actor.Login, ByID: c.actor.ID,
				At: &at}
		}
		if args.Fix {
			since := metav1.NewTime(p.now)
			v.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed, Since: &since,
				Rearm: &v1alpha1.Rearm{By: c.actor.Login, ByID: c.actor.ID, At: at, Round: in.Status.Rounds}}
		}
		return nil
	}); err != nil {
		return n, err
	}
	n.Outcome, n.Fix, n.Reset, n.Explore = templates.VerifyRequested, args.Fix, args.Reset, args.Explore
	if args.Fix && !p.fixesStanding(args, &cur) {
		// Nothing fails, so nothing is fixed: an anomaly held at these heads
		// stands through the new round, which is told of it (G-B-LIB-1).
		n.OpenAnomalies = len(openAnomaliesNow(&cur))
	}
	n.Joined = waiting != nil
	n.AfterRound = p.in.Status.Phase == v1alpha1.IntentRevising
	return n, nil
}

// joinedRequest is the request c records with args, joining waiting (nil
// for none): the newest comment stands for both, and neither's fix, reset
// or explore is lost.
func joinedRequest(c verifyCommand, args command.VerifyArgs, waiting *v1alpha1.VerifyRequest) v1alpha1.VerifyRequest {
	req := v1alpha1.VerifyRequest{By: c.actor.Login, ByID: c.actor.ID, At: metav1.NewTime(c.at), CommentID: c.id,
		Fix: args.Fix, Reset: args.Reset, Explore: args.Explore, PullRequest: c.requestSource()}
	if waiting != nil {
		req.Fix, req.Reset, req.Explore = req.Fix || waiting.Fix, req.Reset || waiting.Reset,
			req.Explore || waiting.Explore
	}
	return req
}

// fixesStanding reports whether a /maul verify fix re-arms automatic fixes
// over a failing judgement on record, from which startVerifyFix starts a
// fix round without a new evaluation: its push deploys an expired preview
// again, so the request is recorded as before. Should no round start, the
// gate answers the request then (waitPreview).
func (p *pass) fixesStanding(args command.VerifyArgs, v *v1alpha1.IntentVerification) bool {
	return args.Fix && v.State == v1alpha1.VerificationFailing && p.standingJudgement(v) != 0
}

// confirmSuspected records an approver's /maul verify fix as the
// confirmation of the suspected defect at the current heads (fourth review),
// made after maul recorded it (fifth review, 1), in any judged state (5):
// the verification is Failing on it and automatic fixes are armed, so the
// verify-fix round starts exactly as an automatic one would, its failures
// the suspected one and any the round found on saved green checks
// (evaluationFailure). No new evaluation is run.
func (p *pass) confirmSuspected(ctx context.Context, c verifyCommand,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	at := metav1.NewTime(c.at)
	if err := p.update(ctx, func(in *v1alpha1.Intent) error {
		v := in.Status.Verification
		if !suspectedConfirmable(v, c.at) {
			return nil
		}
		rearm := v1alpha1.Rearm{By: c.actor.Login, ByID: c.actor.ID, At: at, Round: in.Status.Rounds}
		v.Suspected.Confirmed = &rearm
		since := metav1.NewTime(p.now)
		v.State = v1alpha1.VerificationFailing
		v.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed, Since: &since, Rearm: &rearm}
		rememberApplied(in, c.id)
		return nil
	}); err != nil {
		return n, err
	}
	n.Outcome, n.Fix = templates.VerifyDefectConfirmed, true
	return n, nil
}

// stopAutoFix records /maul verify stop: automatic fixes halted, reason
// HumanStop, with the approver and the comment, and nothing started. An
// acceptance fix leased but not launched is cancelled, as a human's round
// cancels one (D3: verification.cancelRun, which the run scheduler settles
// as OutcomeStoppedByHuman); one already running finishes and pushes. It
// replaces any earlier halt, so the latch names the approver who stopped
// it, and only /maul verify fix, or a new approved plan, re-arms it.
func (p *pass) stopAutoFix(ctx context.Context, c verifyCommand,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	pending, running := p.inFlightVerifyFix()
	at := metav1.NewTime(c.at)
	if err := p.update(ctx, func(in *v1alpha1.Intent) error {
		v := in.Status.Verification
		if v == nil {
			v = &v1alpha1.IntentVerification{}
			in.Status.Verification = v
		}
		since := metav1.NewTime(p.now)
		v.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted, Reason: v1alpha1.AutoFixHumanStop, Since: &since,
			Stop: &v1alpha1.AutoFixStop{By: c.actor.Login, ByID: c.actor.ID, At: at, CommentID: c.id}}
		if pending != "" {
			v.CancelRun = pending
		}
		rememberApplied(in, c.id)
		return nil
	}); err != nil {
		return n, err
	}
	n.Outcome, n.Cancelled, n.Running = templates.VerifyStopped, pending != "", running
	return n, nil
}

// inFlightVerifyFix is the intent's acceptance-fix round in flight: the
// name of its run when it is leased but not launched (pending), or whether
// its agent already runs (running). Neither outside a round of trigger
// verify.
func (p *pass) inFlightVerifyFix() (pending string, running bool) {
	if p.in.Status.Phase != v1alpha1.IntentRevising {
		return "", false
	}
	run := p.round(v1alpha1.IntentStageRevise, p.in.Status.Rounds, anyRepository).latest()
	if run == nil || roundKindOf(run.Spec.Trigger) != roundVerify {
		return "", false
	}
	switch run.Status.Phase {
	case v1alpha1.RunComplete, v1alpha1.RunFailed:
		return "", false
	case v1alpha1.RunPending, v1alpha1.RunRunning, "":
	}
	if run.Status.JobRef == nil {
		return run.Name, false
	}
	return "", true
}

// acceptAll is the id /maul accept all records for a plan that declares no
// acceptance criteria: the approver's word that the change needs none. For a
// plan that declares outcomes it closes every outcome an approver could
// close one by one, each recorded as itself. A criterion or outcome of that
// id is accepted as itself.
const acceptAll = command.AcceptAll

// acceptCriterion records /maul accept <id> in status.acceptance.accepted,
// bound to the combined change being verified (status.verification's head
// set). A plan that declares outcomes is acceptOutcome's; for a plan that
// declares none, only all is accepted, the approver's word that the change
// needs none.
func (p *pass) acceptCriterion(ctx context.Context, c verifyCommand, id string,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	acc := p.in.Status.Acceptance
	n.Acceptable = p.acceptable()
	mode := p.mode()
	if mode == modeNone {
		// A record of another plan revision, or a criteria-era one, is
		// never accepted for the approved one.
		n.Acceptable, n.Outcome = nil, templates.VerifyAcceptNotYet
		return n, nil
	}
	if len(acc.Outcomes) > 0 {
		return p.acceptOutcome(ctx, c, id, n)
	}
	if mode == modeMismatch {
		// No outcomes, and a record nothing verifies (one the evaluator
		// does not verify, or a criteria-era one): nothing is accepted,
		// all least of all.
		n.Acceptable, n.Outcome = nil, templates.VerifyAcceptNotYet
		return n, nil
	}
	if id != acceptAll {
		n.Outcome = templates.VerifyAcceptUnknown
		return n, nil
	}
	n, err := p.recordAcceptance(ctx, c, []string{id}, n)
	n.NoCriteria = n.Outcome == templates.VerifyAccepted
	return n, err
}

// acceptOutcome records /maul accept <id>|all on a plan that declares
// outcomes (the evaluator's), as acceptCriterion records a criterion: an
// approver closes a manual outcome, or one the evaluator could not confirm,
// found failing or passed only on retry; all closes every such outcome at
// once, each recorded as itself under the one comment. An outcome that
// passed, or that nothing has decided yet, is never closed by hand.
func (p *pass) acceptOutcome(ctx context.Context, c verifyCommand, id string,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	n.Outcomes = true
	declared := slices.ContainsFunc(p.in.Status.Acceptance.Outcomes,
		func(o v1alpha1.AcceptanceOutcome) bool { return o.ID == id })
	ids, anomalies := []string{id}, 0
	switch {
	case !declared && id == acceptAll:
		anomalies = len(openAnomaliesNow(p.in.Status.Verification))
		if len(n.Acceptable) == 0 && anomalies == 0 {
			n.Outcome = templates.VerifyAcceptAutomatic
			return n, nil
		}
		ids = slices.Clone(n.Acceptable)
	case !declared:
		n.Outcome = templates.VerifyAcceptUnknown
		return n, nil
	case !slices.Contains(n.Acceptable, id):
		n.Outcome = templates.VerifyAcceptAutomatic
		return n, nil
	}
	n, err := p.recordAcceptanceOf(ctx, c, ids, anomalies > 0, n)
	if n.Outcome == templates.VerifyAccepted {
		n.Accepted = ids
	}
	return n, err
}

// openAnomaliesNow are the look-around anomalies v holds at its current
// heads that no approver accepted: those /maul accept all accepts.
func openAnomaliesNow(v *v1alpha1.IntentVerification) []v1alpha1.AnomalyRecord {
	if v == nil {
		return nil
	}
	return openAnomalies(heldAnomalies(v, v.HeadSetDigest, -1))
}

// acceptAnomalies records c's acceptance of every anomaly v holds at its
// current heads that no approver accepted, and counts them: each then no
// longer holds those heads (G-B-LIB-1), and a round there carries the
// acceptance with the record.
func acceptAnomalies(v *v1alpha1.IntentVerification, c verifyCommand) int {
	if v == nil || v.HeadSetDigest == "" || v.OutcomesHeadSetDigest != v.HeadSetDigest {
		return 0
	}
	n := 0
	for i := range v.Anomalies {
		if v.Anomalies[i].Accepted != nil {
			continue
		}
		v.Anomalies[i].Accepted = &v1alpha1.AnomalyAcceptance{By: c.actor.Login, ByID: c.actor.ID,
			At: metav1.NewTime(c.at), CommentID: c.id}
		n++
	}
	return n
}

// recordAcceptance records an approver's acceptance of ids in
// status.acceptance.accepted, each replacing any earlier acceptance of the
// same id, bound to the head set under verification, and only when the
// heads were checked when the approver commented (acceptedNow).
func (p *pass) recordAcceptance(ctx context.Context, c verifyCommand, ids []string,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	return p.recordAcceptanceOf(ctx, c, ids, false, n)
}

// recordAcceptanceOf is recordAcceptance that, with anomalies, also accepts
// the look-around anomalies held at the heads (acceptAnomalies, /maul accept
// all), counted in n.Anomalies.
func (p *pass) recordAcceptanceOf(ctx context.Context, c verifyCommand, ids []string, anomalies bool,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	v := p.in.Status.Verification
	if v == nil || v.HeadSetDigest == "" {
		n.Outcome = templates.VerifyAcceptNotYet
		return n, nil
	}
	heads := shortHeads(v.Heads)
	if saw, err := p.acceptedNow(ctx, v, c.at); err != nil || !saw {
		n.Outcome, n.Heads = templates.VerifyAcceptMoved, heads
		if p.set.linkless(p.proj) {
			n.Outcome = templates.VerifyAcceptNotChecked
		}
		return n, err
	}
	note := acceptNote(c.body)
	recs := make([]v1alpha1.AcceptanceRecord, 0, len(ids))
	for _, id := range ids {
		recs = append(recs, v1alpha1.AcceptanceRecord{ID: id, By: c.actor.Login, ByID: c.actor.ID,
			At: metav1.NewTime(c.at), CommentID: c.id, HeadSetDigest: v.HeadSetDigest, Heads: heads, Note: note})
	}
	accepted := 0
	if err := p.update(ctx, func(in *v1alpha1.Intent) error {
		a := in.Status.Acceptance
		if a == nil {
			return errors.New("accept: the acceptance criteria vanished")
		}
		a.Accepted = append(slices.DeleteFunc(slices.Clone(a.Accepted),
			func(r v1alpha1.AcceptanceRecord) bool { return slices.Contains(ids, r.ID) }), recs...)
		rememberApplied(in, c.id)
		accepted = 0
		if anomalies {
			accepted = acceptAnomalies(in.Status.Verification, c)
		}
		// The counts maul/acceptance reads follow the acceptance at once.
		if v := in.Status.Verification; v != nil {
			countManual(v, a)
		}
		return nil
	}); err != nil {
		return n, err
	}
	n.Outcome, n.Heads, n.Anomalies = templates.VerifyAccepted, heads, accepted
	return n, nil
}

// acceptNote is the note of a /maul accept comment: its text below the
// command line, as command.Note makes every note, without the id the
// command line names; "" when there is none. It is the approver's text,
// untrusted, at most v1alpha1.MaxAcceptanceNoteBytes, and shown only
// through untrusted.Visible.
func acceptNote(body string) string {
	cmd, ok := command.Parse(body)
	if !ok {
		return ""
	}
	note := cmd.Note
	if id, ok := command.ParseAccept(body); ok {
		note = strings.TrimPrefix(note, id)
	}
	return untrusted.Cap(strings.TrimSpace(note), v1alpha1.MaxAcceptanceNoteBytes)
}

// rememberApplied records in in's status that the acceptance command in
// comment id was applied, in the write that applies it, keeping the latest
// MaxIntentAppliedCommands.
func rememberApplied(in *v1alpha1.Intent, id int64) {
	c := in.Status.Commands
	if c == nil {
		c = &v1alpha1.IntentCommands{}
		in.Status.Commands = c
	}
	if slices.Contains(c.Applied, id) {
		return
	}
	out := append(slices.Clone(c.Applied), id)
	if over := len(out) - v1alpha1.MaxIntentAppliedCommands; over > 0 {
		out = slices.Clone(out[over:])
	}
	c.Applied = out
}

// acceptable are what an approver can accept by hand: for a plan that
// declares outcomes, the outcomes acceptableOutcomes names; for a plan
// that declares none, all; nothing for a criteria-era record, or one the
// evaluator does not verify.
func (p *pass) acceptable() []string {
	acc := p.in.Status.Acceptance
	if acc == nil {
		return nil
	}
	if len(acc.Outcomes) > 0 {
		return p.acceptableOutcomes()
	}
	if p.mode() == modeMismatch || recordOf(acc) != recordOutcomes {
		return nil // acceptCriterion's: nothing an approver could check
	}
	return []string{acceptAll}
}

// acceptableOutcomes are the outcomes an approver can close by hand, in plan
// order: the manual ones, and those the latest decision left failed, unsure
// or quarantined (passed only on retry), or accepted earlier, which a new
// acceptance rebinds to the current commits. One that passed, or that
// nothing has decided yet, is not among them.
func (p *pass) acceptableOutcomes() []string {
	result := map[string]v1alpha1.OutcomeResultValue{}
	if v := p.in.Status.Verification; v != nil {
		for _, r := range v.JudgedOutcomes() {
			result[r.ID] = r.Result
		}
	}
	var out []string
	for _, o := range p.in.Status.Acceptance.Outcomes {
		switch result[o.ID] {
		case v1alpha1.OutcomeFailed, v1alpha1.OutcomeUnsure, v1alpha1.OutcomeQuarantined, v1alpha1.OutcomeAccepted:
			out = append(out, o.ID)
		case v1alpha1.OutcomePassed, "":
			if o.Manual {
				out = append(out, o.ID)
			}
		}
	}
	return out
}

// shortHeads are the commits of a head set as short SHAs, for display.
func shortHeads(heads []v1alpha1.VerificationHead) []string {
	out := make([]string, 0, len(heads))
	for _, h := range heads {
		if len(h.Revision) >= 7 {
			out = append(out, h.Revision[:7])
		}
	}
	return out
}

// settlePRVerifyCommands settles the acceptance commands on the intent's
// pull request pr, oldest first: those made since its build by one of the
// Project's approvers and not yet answered there. Each is answered on the
// pull request once, a bot-written reply carrying its marker standing for
// the answer. As with every pull request command, an account that is not an
// approver, or is a bot, is never answered at all, so a public pull
// request's commenters cannot make maul write; an approver's is authorised
// against the application repository (write access, by GitHub's own
// answer), refused when its comment was ever edited, and not available
// outside review. revise and retry are left to their own round.
func (p *pass) settlePRVerifyCommands(ctx context.Context, pr *v1alpha1.IntentPullRequest) error {
	if p.leftProject(pr.Repository) {
		return nil // nothing is written to a repository that left the Project
	}
	since := p.prBuiltAt(pr.Repository)
	comments, err := p.prComments(ctx, pr, since.Add(-time.Second))
	if err != nil {
		return fmt.Errorf("list the comments of pull request %d: %w", pr.Number, err)
	}
	bot, err := p.r.GitHub.BotLogin(ctx, pr.Repository)
	if err != nil {
		return fmt.Errorf("read the App's bot login: %w", err)
	}
	own := func(c *ghclient.Comment) bool {
		if bot == "" {
			return markerOf(c.Body) != ""
		}
		return strings.EqualFold(c.UserLogin, bot)
	}
	answered := map[string]bool{}
	for _, c := range comments {
		if m := markerOf(c.Body); m != "" && own(c) {
			answered[m] = true
		}
	}
	var todo []*ghclient.Comment
	for _, c := range comments {
		if c.ID < 1 || c.NodeID == "" || c.CreatedAt.Before(since) || own(c) ||
			refusedLocally(p.proj, c.Author()) {
			continue
		}
		cmd, ok := prCommandParser.Parse(c.Body)
		key := "comment-" + strconv.FormatInt(c.ID, 10)
		if !ok || !isVerifyVerb(cmd.Verb) || answered[templates.NoticeMarker(p.in.Namespace, p.in.Name, key)] {
			continue
		}
		todo = append(todo, c)
	}
	slices.SortFunc(todo, func(a, b *ghclient.Comment) int { return int(a.ID - b.ID) })
	if len(todo) > maxFeedbackCandidates {
		todo = todo[len(todo)-maxFeedbackCandidates:]
	}
	for _, c := range todo {
		if err := p.settlePRVerifyCommand(ctx, pr, c); err != nil {
			return err
		}
	}
	return nil
}

// prBuiltAt is when the build of the approved plan in repoURL finished: the
// pull request's commands before it are not this plan's.
func (p *pass) prBuiltAt(repoURL string) time.Time {
	if ap := p.in.Status.Approval; ap != nil {
		if build := p.round(v1alpha1.IntentStageBuild, ap.PlanRevision, repoURL).latest(); build != nil &&
			build.Status.FinishedAt != nil {
			return build.Status.FinishedAt.Time
		}
	}
	return time.Time{}
}

// settlePRVerifyCommand settles one approver's acceptance command on pr.
func (p *pass) settlePRVerifyCommand(ctx context.Context, pr *v1alpha1.IntentPullRequest, c *ghclient.Comment) error {
	cmd, _ := prCommandParser.Parse(c.Body)
	a := humanAction{source: v1alpha1.IntentActionCommand, id: c.ID, at: c.CreatedAt, actor: c.Author(),
		verb: cmd.Verb, comment: c}
	key := a.key()
	if err := p.r.GitHub.ReactPullRequestComment(ctx, pr.Repository, c.ID); err != nil && !ghclient.IsNotFound(err) {
		return fmt.Errorf("react to pull request comment %d: %w", c.ID, err)
	}
	if p.verifyRecorded(c.ID) {
		body, err := templates.RenderVerifyCommandNotice(
			p.recordedVerifyNotice(verifyCommand{surface: command.IntentPR, key: key, id: c.ID, actor: a.actor,
				verb: a.verb, body: c.Body}))
		return p.prReply(ctx, pr, body, err)
	}
	edited, err := p.r.GitHub.PullRequestCommentEdited(ctx, pr.Repository, c.NodeID)
	switch {
	case errors.Is(err, ghclient.ErrNodeNotFound):
		edited = true // gone since it was listed: nothing to act on
	case err != nil:
		return fmt.Errorf("read whether pull request comment %d was edited: %w", c.ID, err)
	}
	if edited {
		body, err := templates.RenderEditedCommandNotice(templates.EditedCommandNotice{
			Namespace: p.in.Namespace, Intent: p.in.Name, Key: key, Verb: a.verb})
		return p.prReply(ctx, pr, body, err)
	}
	ok, bot, err := p.authorizeIn(ctx, pr.Repository, a.actor)
	if err != nil {
		return err
	}
	if !ok || a.actor.ID < 1 {
		body, err := templates.RenderNotAllowedNotice(templates.NotAllowedNotice{Namespace: p.in.Namespace,
			Intent: p.in.Name, Key: key, Actor: a.actor.Login, Bot: bot, Verb: a.verb})
		return p.prReply(ctx, pr, body, err)
	}
	if !verifyPhase(p.in.Status.Phase) {
		body, err := templates.RenderNotAvailableNotice(templates.NotAvailableNotice{Namespace: p.in.Namespace,
			Intent: p.in.Name, Key: key, Surface: command.IntentPR, Verb: a.verb, Phase: string(p.in.Status.Phase),
			Available: verbs(p.in.Status.Phase, p.set.Verification, !p.set.agentsProject(p.proj))})
		return p.prReply(ctx, pr, body, err)
	}
	n, err := p.applyVerifyCommand(ctx, verifyCommand{surface: command.IntentPR, key: key, id: c.ID, at: c.CreatedAt,
		actor: a.actor, verb: a.verb, body: c.Body, pr: pr})
	if err != nil {
		return err
	}
	body, err := templates.RenderVerifyCommandNotice(n)
	return p.prReply(ctx, pr, body, err)
}

// prReply posts a reply on pull request pr.
func (p *pass) prReply(ctx context.Context, pr *v1alpha1.IntentPullRequest, body string, renderErr error) error {
	if renderErr != nil {
		return renderErr
	}
	c, err := p.r.GitHub.CreatePullRequestComment(ctx, pr.Repository, pr.Number, body)
	if err != nil {
		return fmt.Errorf("reply on pull request %d: %w", pr.Number, err)
	}
	p.notePRComment(pr, c)
	return nil
}
