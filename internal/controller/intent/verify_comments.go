// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
)

// How the acceptance verification is reported on GitHub, from the state
// the verification loop
// (verify_loop.go) records and nothing else: maul never reads a status or a
// comment back as state.
//
//   - a clause on the status comment's preview line (verifyProgress);
//   - a section of each previewed pull request's preview comment
//     (verifySection, written by syncPreviewComments);
//   - one comment per verification round on the issue, posted once the
//     round is judged and edited while it stands (syncVerifyComment);
//   - the maul/acceptance commit status on every open pull request's head
//     (acceptance_status.go).
//
// Every word is maul's; the only agent or application text shown is an
// approved outcome's statement, a failing check's app output, the
// evaluator's look-around anomalies and a component's log tail, each
// through the templates' inert rendering. All of it is best effort: a write
// that fails is logged and tried on a later pass, and never holds up the
// human's rounds.

// verifyNote is what syncVerifyComment remembers of the issue's latest
// verification comment: its round, its id and the digest of the body last
// written, so an unchanged comment is neither listed for nor edited.
type verifyNote struct {
	round  int32
	id     int64
	digest string
}

// impostorNote is the account that last posted a maul/acceptance status
// other than maul's at a pull request's head.
type impostorNote struct {
	head, login string
}

// verifyReported reports whether this pass reports the verification: the
// controller verifies, the intent is in review (or a round of it), and the
// loop has recorded the approved plan revision's acceptance and a state,
// or stopped because it cannot be read or states slice-1 criteria
// (verifyStopped).
func (p *pass) verifyReported() bool {
	return p.set.Verification && verifyPhase(p.in.Status.Phase) && p.in.Status.Verification != nil &&
		(p.mode() != modeNone || p.verifyStopped() != "")
}

// verifyJudged reports a state holding a round's judged (or given up)
// result: the states the issue's verification comment is written in.
func verifyJudged(s v1alpha1.IntentVerificationState) bool {
	switch s {
	case v1alpha1.VerificationPassed, v1alpha1.VerificationFailing, v1alpha1.VerificationFixing,
		v1alpha1.VerificationExhausted, v1alpha1.VerificationUnavailable, v1alpha1.VerificationUnconfirmed:
		return true
	}
	return false
}

// verifyFixesUsed are the automatic fix rounds spent against the Project's
// maxVerifyFixes, and that limit: how every report counts fixes, apart
// from the verification's own number (status.verification.round).
func (p *pass) verifyFixesUsed() (used, most int32) {
	most = p.proj.Spec.Limits.MaxVerifyFixesLimit()
	return min(p.verifyFixRounds(), most), most
}

// acceptedAt is an approver's acceptance of criterion id at the head set
// headSet, nil when there is none.
func acceptedAt(acc *v1alpha1.IntentAcceptance, id, headSet string) *v1alpha1.AcceptanceRecord {
	if headSet == "" {
		return nil
	}
	for i := range acc.Accepted {
		if r := &acc.Accepted[i]; r.ID == id && r.HeadSetDigest == headSet {
			return r
		}
	}
	return nil
}

// resultMoved reports that v holds a result (verifyJudged) about other
// heads than the pull requests' now (atHeads, review SM-1): no comment
// shows it until a round is about the new ones.
func (p *pass) resultMoved(v *v1alpha1.IntentVerification) bool {
	return verifyJudged(v.State) && !p.atHeads(v)
}

// verifyProgress is where verification stands as the status comment's
// preview line says it; nil when it says nothing (not reported, or Off).
func (p *pass) verifyProgress() *templates.VerifyProgress {
	if !p.verifyReported() {
		return nil
	}
	switch p.verifyStopped() {
	case reasonCriteriaUnreadable:
		return &templates.VerifyProgress{State: templates.VerifyUnreadable}
	case reasonCriteriaReplan:
		return &templates.VerifyProgress{State: templates.VerifyCriteriaReplan}
	}
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	if p.resultMoved(v) {
		return &templates.VerifyProgress{State: templates.VerifyHeadsMoved, Outcomes: len(acc.Outcomes) > 0}
	}
	if len(acc.Outcomes) > 0 {
		used, most := p.verifyFixesUsed()
		return p.evaluationProgress(&templates.VerifyProgress{Verification: v.Round, FixesUsed: used,
			MaxFixes: most, ManualOpen: v.ManualOpen})
	}
	// A plan that declares no outcome is only ever NotDeclared
	// (syncEvaluation): nothing is evaluated. Off, or no state yet, says
	// nothing.
	if v.State != v1alpha1.VerificationNotDeclared {
		return nil
	}
	return &templates.VerifyProgress{State: templates.VerifyNotDeclared}
}

// evaluationProgress is verifyProgress for the agent evaluator's outcomes
// (out holds the round and fix counts): evaluations, and outcomes confirmed
// or accepted out of every approved one. nil when it says nothing.
func (p *pass) evaluationProgress(out *templates.VerifyProgress) *templates.VerifyProgress {
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	out.Outcomes = true
	judged := func(state string) *templates.VerifyProgress {
		out.State, out.Passing, out.Total = state, confirmedCount(acc, v), int32(len(acc.Outcomes))
		return out
	}
	switch v.State {
	case v1alpha1.VerificationNotDeclared:
		return &templates.VerifyProgress{State: templates.VerifyNotChecked, ManualOpen: v.ManualOpen, Outcomes: true}
	case v1alpha1.VerificationWaitingForCI:
		out.State = templates.VerifyWaitingForCI
	case v1alpha1.VerificationWaitingForPreview:
		out.State = templates.VerifyWaiting
	case v1alpha1.VerificationEvaluating:
		out.State = templates.VerifyEvaluating
	case v1alpha1.VerificationPassed:
		return judged(templates.VerifyPassed)
	case v1alpha1.VerificationUnconfirmed:
		if outcomesReady(acc, v) {
			return judged(templates.VerifyPassed)
		}
		out.Why, out.Workspace = p.gaveUp(acc, v), p.set.linkless(p.proj)
		return judged(templates.VerifyUnconfirmed)
	case v1alpha1.VerificationFailing:
		switch {
		case v.AutoFix.State == v1alpha1.AutoFixHalted:
			out.AutoFix, out.AutoFixReason = templates.FixHalted, string(v.AutoFix.Reason)
		case defectConfirmed(v):
			out.DefectConfirmed, out.AutoFix = true, templates.FixQueued
		}
		return judged(templates.VerifyFailing)
	case v1alpha1.VerificationFixing:
		out.AutoFix, out.DefectConfirmed = templates.FixRunning, defectConfirmed(v)
		return judged(templates.VerifyFailing)
	case v1alpha1.VerificationExhausted:
		return judged(templates.VerifyExhausted)
	case v1alpha1.VerificationNeedsReplan:
		out.State = templates.VerifyNeedsReplan
	case v1alpha1.VerificationUnavailable:
		out.State = templates.VerifyUnavailable
		if previewNotLive(v) {
			out.State = templates.VerifyPreviewNotLive
		}
	default:
		return nil
	}
	return out
}

// outcomeSection fills s with the evaluator's outcomes in plan order, each
// with its result as reported now (an approver's acceptance at the heads
// included), maul's line on the check it rests on and, for a failure, the
// application's output from pv's ledger; and the look-around anomalies.
// Only the judgement of the current round at the current heads is shown
// (JudgedOutcomes): a round that settled unjudged shows none of an earlier
// round's results.
func (p *pass) outcomeSection(ctx context.Context, s *templates.VerifySection, pv *v1alpha1.PreviewVerification) {
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	results := v.JudgedOutcomes()
	if !verifyJudged(v.State) {
		results = nil
	}
	for _, o := range acc.Outcomes {
		row := templates.VerifyOutcomeResult{ID: o.ID, Kind: string(o.EffectiveKind()), Statement: o.Outcome,
			Result: templates.OutcomeResultUnsure}
		i := slices.IndexFunc(results, func(r v1alpha1.OutcomeResult) bool { return r.ID == o.ID })
		if i < 0 {
			if o.Manual {
				row.Reason = "manual"
			}
			if r := acceptedAt(acc, o.ID, v.HeadSetDigest); r != nil {
				row.Result, row.AcceptedAt = templates.OutcomeResultAccepted, firstHead(r)
			}
			s.Outcomes = append(s.Outcomes, row)
			continue
		}
		r := results[i]
		row.Result, row.Reason, row.Check = string(effectiveResult(acc, v, r)), r.Reason, r.Line
		if rec := acceptedAt(acc, o.ID, v.HeadSetDigest); rec != nil && row.Result == templates.OutcomeResultAccepted {
			row.AcceptedAt = firstHead(rec)
		}
		if (row.Result == templates.OutcomeResultFailed || r.Reason == acceptance.ReasonSuspectedDefect) && pv != nil {
			row.AppOutput = p.appOutput(ctx, pv, r.Basis)
		}
		s.Outcomes = append(s.Outcomes, row)
	}
	for _, a := range v.Anomalies {
		if results == nil {
			break
		}
		s.Anomalies = append(s.Anomalies, templates.VerifyAnomaly{Summary: a.Summary, Route: a.Route})
	}
	s.Suspected = p.suspectedView(ctx, pv)
}

// suspectedView is the suspected defect at the current heads (fourth
// review) as the comment shows it, with the reproduction a fix round is
// handed, rendered from pv's own ledger: shown whatever else the round
// found (fifth review, 5), and once a person confirmed it, saying so (6);
// nil when there is none.
func (p *pass) suspectedView(ctx context.Context, pv *v1alpha1.PreviewVerification) *templates.VerifySuspected {
	v, acc := p.in.Status.Verification, p.in.Status.Acceptance
	if !suspectedPending(v) && !defectConfirmed(v) || pv == nil || v.Suspected.Verification != pv.Name {
		return nil
	}
	// The note claims a hold only while the hand-off shows one (gate D-B3).
	h := p.handoffView()
	out := &templates.VerifySuspected{Outcomes: slices.Clone(v.Suspected.Outcomes),
		Held: h != nil && h.State == templates.HandoffHeld, Workspace: p.set.linkless(p.proj)}
	if c := v.Suspected.Confirmed; c != nil {
		out.ConfirmedBy = c.By
	}
	j := p.decideRound(ctx, pv, p.evaluateRun(pv))
	if s := j.decision.Suspected; s != nil {
		if repro, err := p.reproduction(ctx, pv, acc, s, j.checks); err == nil {
			out.Reproduction = repro
		}
	}
	return out
}

// firstHead is the first short head an acceptance was given at, "" for
// none.
func firstHead(r *v1alpha1.AcceptanceRecord) string {
	if len(r.Heads) == 0 {
		return ""
	}
	return r.Heads[0]
}

// judgedVerification is the PreviewVerification the recorded result was
// judged from, read once per pass (uncached) and only while a judged result
// stands: its observations are a failure's evidence and its environment the
// log tails. nil otherwise, or when it is gone or not this Intent's.
func (p *pass) judgedVerification(ctx context.Context) *v1alpha1.PreviewVerification {
	if p.verifyPVRead {
		return p.verifyPV
	}
	p.verifyPVRead = true
	v := p.in.Status.Verification
	if v == nil || v.Ref == nil || !verifyJudged(v.State) {
		return nil
	}
	pv, err := p.readVerification(ctx, *v.Ref)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the judged verification for its evidence; shown without it",
			slog.String("intent", p.in.Name), slog.String("verification", v.Ref.Name), slog.Any("error", err))
		return nil
	}
	if pv != nil && pv.Spec.HeadSetDigest != v.HeadSetDigest {
		return nil
	}
	p.verifyPV = pv
	return pv
}

// verifySection is the verification as a comment shows it: on the issue
// (pr nil) with every component's log tail; on pr's preview comment with
// its own repository's component's log tail only under the Project's
// postLogs, and the account that posted an impostor status at its head.
// nil when the verification is not reported.
func (p *pass) verifySection(ctx context.Context, pr *v1alpha1.IntentPullRequest) *templates.VerifySection {
	prog := p.verifyProgress()
	if prog == nil {
		return nil
	}
	s := &templates.VerifySection{Progress: *prog, StatusNotPosted: p.statusesUnavailable()}
	if p.verifyStopped() != "" {
		// Nothing of the approved revision is on record.
		return s
	}
	if prog.State == templates.VerifyHeadsMoved {
		// A judgement of other heads than the pull requests' now (SM-1):
		// none of it is shown.
		s.Impostor = p.impostor(pr)
		return s
	}
	pv := p.judgedVerification(ctx)
	if len(p.in.Status.Acceptance.Outcomes) > 0 {
		p.outcomeSection(ctx, s, pv)
	}
	if pv != nil {
		s.Logs = p.verifyLogs(pv, pr)
	}
	s.Impostor = p.impostor(pr)
	s.NoDefect = p.noDefectView()
	return s
}

// noDefectView is the latest acceptance-fix round's conclusion that the
// code has no defect, as a comment shows it, from the Intent's own record
// (status.verification.noDefect), whatever halted the latch, while the
// verification is about the heads it judged; nil otherwise. syncHeadSet,
// the same writer, drops the record when the heads move; this check keeps
// a record that outlived them from ever being shown.
func (p *pass) noDefectView() *templates.VerifyNoDefect {
	v := p.in.Status.Verification
	if v == nil || v.NoDefect == nil || v.NoDefect.HeadSetDigest == "" ||
		v.NoDefect.HeadSetDigest != v.HeadSetDigest {
		return nil
	}
	return &templates.VerifyNoDefect{Run: v.NoDefect.Run, Reason: v.NoDefect.Reason}
}

// verifyLogs are the component log tails of pv's environment a comment
// shows: every one on the issue (pr nil); on pr's preview comment its own
// repository's component's alone, and only under the Project's postLogs.
func (p *pass) verifyLogs(pv *v1alpha1.PreviewVerification, pr *v1alpha1.IntentPullRequest) []templates.VerifyLog {
	var own string
	if pr != nil {
		if vf := p.proj.Spec.Verification; vf == nil || !vf.PostLogs {
			return nil
		}
		previews := v1alpha1.EffectivePreviews(p.proj)
		i := slices.IndexFunc(previews, func(rp v1alpha1.RepositoryPreview) bool {
			return sameRepo(rp.URL, pr.Repository)
		})
		if i < 0 {
			return nil
		}
		own = previews[i].Name
	}
	var out []templates.VerifyLog
	for _, f := range pv.Status.Environment {
		if f.LogTail == "" || f.Role != v1alpha1.PreviewRoleComponent || (pr != nil && f.Name != own) {
			continue
		}
		out = append(out, templates.VerifyLog{Part: f.Name, Tail: f.LogTail})
	}
	return out
}

// impostor is the account that last posted a maul/acceptance status other
// than maul's at pr's current head (on the issue, pr nil, at any pull
// request's), "" for none.
func (p *pass) impostor(pr *v1alpha1.IntentPullRequest) string {
	login := ""
	p.notes(func(n *previewNotes) {
		for _, cand := range p.in.Status.PullRequests {
			if pr != nil && (!sameRepo(cand.Repository, pr.Repository) || cand.Number != pr.Number) {
				continue
			}
			if note, ok := n.impostors[prKey(cand)]; ok && note.head == cand.HeadSHA {
				login = note.login
				return
			}
		}
	})
	return login
}

// verifySectionDigest is the digest a preview comment's verification
// section is recorded under (IntentPullRequest.VerifyDigest), "" for none.
func verifySectionDigest(s *templates.VerifySection) string {
	if s == nil {
		return ""
	}
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return digest(b)
}

// syncVerifyComment keeps the issue's verification comment of the current
// round: posted once the round is judged, found first among maul's own
// comments by its marker (a lost write never posts twice), and edited only
// when what it says changes. A later round gets its own comment; an earlier
// round's is left as it last stood. Best effort.
//
// A verification that gave up waiting for its Preview (PreviewNotLive) is
// reported in the comment of the attempt that could not start: the
// round's next attempt when one is owed (a retry), else the next round's
// first, as createVerification would number it.
func (p *pass) syncVerifyComment(ctx context.Context) {
	v := p.in.Status.Verification
	round, attempt := v.Round, v.Attempt
	if previewNotLive(v) {
		round, attempt = v.Round+1, 1
		if v.Attempt > 0 {
			round, attempt = v.Round, v.Attempt+1
		}
	}
	if round == 0 || !verifyJudged(v.State) || p.in.Status.Approval == nil || p.resultMoved(v) {
		// A round's comment is its own heads' record: neither posted nor
		// edited while the pull requests are at others (SM-1).
		return
	}
	sec := p.verifySection(ctx, nil)
	if sec == nil {
		return
	}
	body, err := templates.RenderVerifyComment(templates.VerifyComment{Namespace: p.in.Namespace,
		Intent: p.in.Name, Round: round, Attempt: attempt, Section: *sec})
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelError, "render the verification comment",
			slog.String("intent", p.in.Name), slog.Any("error", err))
		return
	}
	d := digest([]byte(body))
	var note verifyNote
	p.notes(func(n *previewNotes) { note = n.verify })
	if note.round == round && note.digest == d {
		return
	}
	if above, err := p.rateOK(ctx, p.repo()); err != nil || !above {
		return
	}
	id := int64(0)
	if note.round == round {
		id = note.id
	}
	id, err = p.writeVerifyComment(ctx, id, body)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "write the verification comment; retried at a later pass",
			slog.String("intent", p.in.Name), slog.Int("round", int(round)), slog.Any("error", err))
		return
	}
	p.notes(func(n *previewNotes) { n.verify = verifyNote{round: round, id: id, digest: d} })
}

// writeVerifyComment writes body to the current round's verification
// comment: edited in place when id names it, else adopted by its marker
// (edited if it says something else) or posted. It returns the comment's id.
func (p *pass) writeVerifyComment(ctx context.Context, id int64, body string) (int64, error) {
	if id != 0 {
		err := p.r.GitHub.EditIssueComment(ctx, p.repo(), id, body)
		if !ghclient.IsNotFound(err) {
			return id, err
		}
		// Deleted: posted again.
	}
	marker := templates.NoticeMarker(p.in.Namespace, p.in.Name, templates.VerifyKey(p.in.Status.Verification.Round))
	c, err := p.findOwn(ctx, marker, p.in.Status.Approval.At.Time)
	switch {
	case err != nil:
		return 0, err
	case c == nil:
		if c, err = p.r.GitHub.CreateIssueComment(ctx, p.repo(), p.number(), body); err != nil {
			return 0, err
		}
	case c.Body != body:
		if err := p.r.GitHub.EditIssueComment(ctx, p.repo(), c.ID, body); err != nil {
			return 0, err
		}
	}
	return c.ID, nil
}

// verifyTargetURL is where maul/acceptance links: the issue's verification
// comment of the current round once maul knows it, else the issue.
func (p *pass) verifyTargetURL() string {
	url := p.in.Spec.Issue.URL
	if url == "" {
		return ""
	}
	var note verifyNote
	p.notes(func(n *previewNotes) { note = n.verify })
	if v := p.in.Status.Verification; note.id != 0 && v != nil && note.round == v.Round {
		return fmt.Sprintf("%s#issuecomment-%d", url, note.id)
	}
	return url
}

// syncVerifyReports writes the verification's issue comment while it is
// reported, and its maul/acceptance statuses (acceptanceStatus decides).
// changed reports a status write; only errConflict is returned.
func (p *pass) syncVerifyReports(ctx context.Context) (bool, error) {
	if p.verifyReported() {
		p.syncVerifyComment(ctx)
	}
	// Also when verification was switched off: its final status.
	return p.syncAcceptanceStatuses(ctx)
}

// previewVerifySection is the verification section of pr's preview comment
// in state: none for a pull request whose repository is not previewed, or
// once the preview is removed.
func (p *pass) previewVerifySection(ctx context.Context, pr v1alpha1.IntentPullRequest, previewed bool,
	state string) *templates.VerifySection {
	if !previewed || state == templates.PreviewRemoved {
		return nil
	}
	return p.verifySection(ctx, &pr)
}
