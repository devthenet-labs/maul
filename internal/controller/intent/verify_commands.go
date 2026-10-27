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

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/command"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
)

// The acceptance commands, /maul verify [fix|stop|--reset] and /maul accept <id>,
// settle here for both surfaces that offer them: the intent issue (from
// decide, on the poll's answer path) and each of the intent's pull requests
// (settlePRVerifyCommands). An intent has one combined change and one
// Preview, so the surface a command is made on changes only where its
// authority is checked (the repository it was made in) and where it is
// answered, never what it does. Every effect is a status write of the intent
// reconciler; nothing here starts a round or touches a Preview: the
// verification loop reads status.verification.requested, the reset
// generation and the auto-fix latch, and status.acceptance.accepted.

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
}

// isVerifyVerb reports an acceptance command's verb.
func isVerifyVerb(verb string) bool {
	return verb == action.VerbVerify || verb == action.VerbAccept
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
		// With --intent-verification off nothing of acceptance is written.
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
	n := p.recordedVerifyNotice(command.IntentIssue, a.key(), a.id, a.actor, a.verb)
	return p.verifyNotice(ctx, a.key(), a.at, n)
}

// recordedVerifyNotice is the reply to an acceptance command recorded as
// comment id: what it recorded while that still stands, else that it was
// applied.
func (p *pass) recordedVerifyNotice(surface command.Surface, key string, id int64, actor ghclient.Actor,
	verb string) templates.VerifyCommandNotice {
	n := p.verifyNoticeFor(surface, key, actor, verb)
	n.Outcome = templates.VerifyApplied
	if v := p.in.Status.Verification; v != nil && v.Requested != nil && v.Requested.CommentID == id {
		n.Outcome, n.Fix, n.Reset = templates.VerifyRequested, v.Requested.Fix, v.Requested.Reset
		n.AfterRound = p.in.Status.Phase == v1alpha1.IntentRevising
		return n
	}
	if v := p.in.Status.Verification; v != nil && v.AutoFix.State == v1alpha1.AutoFixHalted &&
		v.AutoFix.Stop != nil && v.AutoFix.Stop.CommentID == id {
		n.Outcome = templates.VerifyStopped
		return n
	}
	if acc := p.in.Status.Acceptance; acc != nil {
		for _, r := range acc.Accepted {
			if r.CommentID == id {
				n.Outcome, n.Criterion, n.Heads = templates.VerifyAccepted, r.ID, r.Heads
				n.NoCriteria = r.ID == acceptAll && len(acc.Criteria) == 0
			}
		}
	}
	return n
}

// verifyNoticeFor is a reply's identity: who, where, which verb.
func (p *pass) verifyNoticeFor(surface command.Surface, key string, actor ghclient.Actor,
	verb string) templates.VerifyCommandNotice {
	return templates.VerifyCommandNotice{Namespace: p.in.Namespace, Intent: p.in.Name, Key: key, Surface: surface,
		Actor: actor.Login, Verb: verb}
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
		return p.requestVerification(ctx, c, args, n)
	case action.VerbAccept:
		id, ok := command.ParseAccept(c.body)
		if !ok {
			n.Outcome = templates.VerifyUsage
			return n, nil
		}
		n.Criterion = id
		return p.acceptCriterion(ctx, c, id, n)
	}
	return n, fmt.Errorf("verify command: unexpected verb %q", c.verb)
}

// requestVerification records /maul verify as status.verification.requested,
// for the verification loop to run at its next opportunity, bypassing the
// automatic interval. A request made while one waits joins it: the newest
// comment stands for both, and neither's fix or reset is lost. --reset also
// bumps the reset generation, which the Preview copies, unless the last
// reset was under verifyResetInterval before it; fix also re-arms automatic
// fixes from the current round (a human round after it, or a human push,
// halts them again). Once the approved plan revision has used the Project's
// maxVerifications nothing is recorded and the reply says so.
func (p *pass) requestVerification(ctx context.Context, c verifyCommand, args command.VerifyArgs,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	var cur v1alpha1.IntentVerification
	if v := p.in.Status.Verification; v != nil {
		cur = *v
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
	req := v1alpha1.VerifyRequest{By: c.actor.Login, ByID: c.actor.ID, At: at, CommentID: c.id,
		Fix: args.Fix || (waiting != nil && waiting.Fix), Reset: args.Reset || (waiting != nil && waiting.Reset)}
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
	n.Outcome, n.Fix, n.Reset = templates.VerifyRequested, args.Fix, args.Reset
	n.Joined = waiting != nil
	n.AfterRound = p.in.Status.Phase == v1alpha1.IntentRevising
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
// acceptance criteria: the approver's word that the change needs none. It
// means nothing for a plan that declares some (a criterion of that id is
// accepted as itself).
const acceptAll = "all"

// acceptCriterion records /maul accept <id> in status.acceptance.accepted,
// bound to the combined change being verified (status.verification's head
// set), replacing any earlier acceptance of the criterion. Only a manual
// criterion, one maul does not run yet (command), or one quarantined (it
// passed only on retry) is accepted by hand, and for a plan with no
// criteria, all: one maul checks and judged outright is never overridden by
// a human saying so.
func (p *pass) acceptCriterion(ctx context.Context, c verifyCommand, id string,
	n templates.VerifyCommandNotice) (templates.VerifyCommandNotice, error) {
	acc := p.in.Status.Acceptance
	v := p.in.Status.Verification
	n.Acceptable = p.acceptable()
	var crit *v1alpha1.AcceptanceCriterion
	if !p.acceptanceCurrent() {
		// Criteria on record from another plan revision are never
		// accepted for the approved one.
		n.Acceptable, n.Outcome = nil, templates.VerifyAcceptNotYet
		return n, nil
	}
	if acc != nil {
		if i := slices.IndexFunc(acc.Criteria, func(c v1alpha1.AcceptanceCriterion) bool { return c.ID == id }); i >= 0 {
			crit = &acc.Criteria[i]
		}
	}
	switch {
	case crit == nil && (id != acceptAll || len(acc.Criteria) != 0):
		n.Outcome = templates.VerifyAcceptUnknown
		return n, nil
	case !slices.Contains(n.Acceptable, id):
		n.Outcome = templates.VerifyAcceptAutomatic
		return n, nil
	case v == nil || v.HeadSetDigest == "":
		n.Outcome = templates.VerifyAcceptNotYet
		return n, nil
	}
	heads := shortHeads(v.Heads)
	if saw, err := p.liveWhenGiven(ctx, v, c.at); err != nil || !saw {
		n.Outcome, n.Heads = templates.VerifyAcceptMoved, heads
		return n, err
	}
	rec := v1alpha1.AcceptanceRecord{ID: id, By: c.actor.Login, ByID: c.actor.ID, At: metav1.NewTime(c.at),
		CommentID: c.id, HeadSetDigest: v.HeadSetDigest, Heads: heads}
	if err := p.update(ctx, func(in *v1alpha1.Intent) error {
		a := in.Status.Acceptance
		if a == nil {
			return errors.New("accept: the acceptance criteria vanished")
		}
		a.Accepted = append(slices.DeleteFunc(slices.Clone(a.Accepted),
			func(r v1alpha1.AcceptanceRecord) bool { return r.ID == id }), rec)
		rememberApplied(in, c.id)
		// The counts maul/acceptance reads follow the acceptance at once.
		if v := in.Status.Verification; v != nil {
			countManual(v, a)
		}
		return nil
	}); err != nil {
		return n, err
	}
	n.Outcome, n.Heads, n.NoCriteria = templates.VerifyAccepted, heads, crit == nil
	return n, nil
}

// liveWhenGiven reports that the Preview ran the recorded head set v when an
// acceptance was given at at, and still does: it is live at exactly the
// heads the Intent's records derive, those are v's, and it went live at them
// (status.lastDeployedAt, cleared by every new deployment) before at. An
// acceptance is bound to the commits the approver could have checked, never
// to ones recorded or deployed after they commented. A Preview not found is
// not live; any other failed read is returned, deciding nothing.
func (p *pass) liveWhenGiven(ctx context.Context, v *v1alpha1.IntentVerification, at time.Time) (bool, error) {
	if hs, ok := previewHeadSet(p.proj, p.in); !ok || hs.digest != v.HeadSetDigest {
		return false, nil
	}
	var pv v1alpha1.Preview
	err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: p.in.Name}, &pv)
	switch {
	case kerrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the intent's preview: %w", err)
	}
	deployed := pv.Status.LastDeployedAt
	return viewPreview(p.proj, p.in, &pv).state == templates.PreviewLive && deployed != nil &&
		at.After(deployed.Time), nil
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

// acceptable are the criteria an approver can accept by hand, in plan
// order: the manual ones, those maul does not run yet and the quarantined;
// for a plan with no criteria, all.
func (p *pass) acceptable() []string {
	acc := p.in.Status.Acceptance
	if acc == nil {
		return nil
	}
	if len(acc.Criteria) == 0 {
		return []string{acceptAll}
	}
	var quarantined []string
	if v := p.in.Status.Verification; v != nil {
		quarantined = v.QuarantinedIDs
	}
	var out []string
	for _, c := range acc.Criteria {
		if byHand(c) || slices.Contains(quarantined, c.ID) {
			out = append(out, c.ID)
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
			p.recordedVerifyNotice(command.IntentPR, key, c.ID, a.actor, a.verb))
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
			Available: verbs(p.in.Status.Phase, p.set.Verification)})
		return p.prReply(ctx, pr, body, err)
	}
	n, err := p.applyVerifyCommand(ctx, verifyCommand{surface: command.IntentPR, key: key, id: c.ID, at: c.CreatedAt,
		actor: a.actor, verb: a.verb, body: c.Body})
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
