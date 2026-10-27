// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The maul/acceptance commit status (acceptance verification, slice 1):
// what the verification of the combined change says, posted on every open
// pull request's head. It is maul's word on behavioural evidence, never
// read back as state.
//
//   - pending while maul waits, verifies, or holds a result for a human (a
//     pass only on retry, or with the Project's manualGatesStatus an open
//     manual criterion); success only when every automated check passed at
//     the current head set; failure when one failed; error when maul could
//     not run them or an approved criterion cannot run as approved.
//   - Nothing is posted for a plan with nothing maul checks, a Project or
//     Preview that does not verify, or an ended intent.
//   - Each pull request's record keeps the digest of the head, the head-set
//     digest, the state and the description last posted, so an unchanged
//     status is never posted again, and a sibling's move (a new head-set
//     digest) re-posts it on a head that did not move: never a stale green.
//   - Nothing is posted while the Project's App may not post statuses
//     (AcceptanceStatusUnavailable): the comments say so instead.
//   - Impostors (critique fix 5): at most once per poll interval per head,
//     the statuses at the head are listed; a later maul/acceptance someone
//     other than the App posted is answered by maul's own posted again, and
//     the account is named in the pull request's verification section.

// acceptanceContext is the commit status context maul posts under.
const acceptanceContext = "maul/acceptance"

// Commit status states.
const (
	statusPending = "pending"
	statusSuccess = "success"
	statusFailure = "failure"
	statusError   = "error"
)

// acceptanceState is one maul/acceptance status: its state and its
// maul-authored description (counts only, within GitHub's 140 bytes).
type acceptanceState struct {
	state, description string
}

// acceptanceStatus is the maul/acceptance status the recorded verification
// calls for; ok is false when none is posted.
func (p *pass) acceptanceStatus() (acceptanceState, bool) {
	if !p.verifyReported() || !p.verifies() {
		return acceptanceState{}, false
	}
	if p.criteriaUnreadable() {
		return acceptanceState{statusError, "the approved acceptance criteria cannot be read: replan needed"}, true
	}
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	total := int32(len(automatable(acc.Criteria)))
	if total == 0 || v.HeadSetDigest == "" {
		return acceptanceState{}, false
	}
	round, most := p.verifyFixRound()
	passed := fmt.Sprintf("%d/%d automated passed", v.Passed, total)
	switch v.State {
	case v1alpha1.VerificationWaitingForCI, v1alpha1.VerificationWaitingForPreview:
		if v.Round > 0 {
			return acceptanceState{statusPending, "combined change moved: re-verifying"}, true
		}
		if v.State == v1alpha1.VerificationWaitingForCI {
			return acceptanceState{statusPending, "waiting for the pull requests' checks"}, true
		}
		return acceptanceState{statusPending, "waiting for the preview"}, true
	case v1alpha1.VerificationVerifying:
		return acceptanceState{statusPending, fmt.Sprintf("verifying: round %d of %d, %d %s", round, most, total,
			plural(total, "check", "checks"))}, true
	case v1alpha1.VerificationPassed:
		return p.passedStatus(passed, v), true
	case v1alpha1.VerificationFailing:
		if v.AutoFix.State == v1alpha1.AutoFixHalted {
			return acceptanceState{statusFailure, passed + "; automatic fixes halted"}, true
		}
		return acceptanceState{statusFailure, fmt.Sprintf("%s, %d failing", passed, v.Failed)}, true
	case v1alpha1.VerificationFixing:
		// The running fix is already one of verifyFixRounds, so it is the
		// round before the verification it will produce.
		return acceptanceState{statusFailure, fmt.Sprintf("%s; automatic fix %d of %d running", passed,
			round-1, most-1)}, true
	case v1alpha1.VerificationExhausted:
		return acceptanceState{statusFailure, passed + "; no automatic fix left"}, true
	case v1alpha1.VerificationNeedsReplan:
		return acceptanceState{statusError, "a check cannot run as approved: replan needed"}, true
	case v1alpha1.VerificationUnavailable:
		return acceptanceState{statusError, "maul could not run the checks"}, true
	}
	return acceptanceState{}, false
}

// passedStatus is the status of a verification that found no failure: a
// pass only on retry holds it pending until a human accepts it or a clean
// run passes it (D7); an open manual criterion does under the Project's
// manualGatesStatus, and is named otherwise; command criteria are not run
// in this slice and are named.
func (p *pass) passedStatus(passed string, v *v1alpha1.IntentVerification) acceptanceState {
	if q := openQuarantined(p.in.Status.Acceptance, v); q > 0 {
		return acceptanceState{statusPending, fmt.Sprintf("%d passed only on retry", q)}
	}
	gates := p.proj.Spec.Verification != nil && p.proj.Spec.Verification.ManualGatesStatus
	var open []string
	if v.ManualOpen > 0 {
		open = append(open, fmt.Sprintf("%d manual open", v.ManualOpen))
	}
	if v.NotRun > 0 {
		open = append(open, fmt.Sprintf("%d not run", v.NotRun))
	}
	switch {
	case gates && v.ManualOpen > 0:
		return acceptanceState{statusPending, strings.Join(append([]string{passed}, open...), ", ")}
	case len(open) > 0:
		return acceptanceState{statusSuccess, strings.Join(append([]string{"automated part verified"}, open...),
			", ")}
	}
	return acceptanceState{statusSuccess, passed}
}

func plural(n int32, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// acceptanceDigest is what a maul/acceptance status is recorded under: the
// head it is on, the combined change it is about, and what it says.
func acceptanceDigest(head, headSet string, s acceptanceState) string {
	return digest([]byte(strings.Join([]string{head, headSet, s.state, s.description}, "\n")))
}

// acceptanceRecord is one pull request's maul/acceptance as a pass posted
// it.
type acceptanceRecord struct {
	repository string
	number     int64
	id         int64
	head       string
	digest     string
}

// syncAcceptanceStatuses posts the maul/acceptance status the verification
// calls for on every open pull request's head whose record says otherwise,
// or where an impostor's status is the latest, under its repository's rate
// floor, and records what it posted. A failed post is logged and tried
// again on a later pass. changed reports a status write; only errConflict
// is returned.
func (p *pass) syncAcceptanceStatuses(ctx context.Context) (bool, error) {
	if p.statusesUnavailable() {
		return false, nil
	}
	st, ok := p.acceptanceStatus()
	if !ok {
		return false, nil
	}
	headSet := p.in.Status.Verification.HeadSetDigest
	target := p.verifyTargetURL()
	var records []acceptanceRecord
	for _, pr := range p.in.Status.PullRequests {
		if pr.State != prOpen || pr.HeadSHA == "" || p.leftProject(pr.Repository) {
			continue
		}
		d := acceptanceDigest(pr.HeadSHA, headSet, st)
		if d == pr.AcceptanceDigest && !p.impostorLatest(ctx, pr) {
			continue
		}
		if above, err := p.rateOK(ctx, pr.Repository); err != nil || !above {
			continue
		}
		id, err := p.r.GitHub.CreateCommitStatus(ctx, pr.Repository, pr.HeadSHA, st.state, acceptanceContext,
			st.description, target)
		if err != nil {
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "post maul/acceptance; retried at a later pass",
				slog.String("intent", p.in.Name), slog.String("repository", pr.Repository),
				slog.Int64("number", pr.Number), slog.Any("error", err))
			continue
		}
		records = append(records, acceptanceRecord{repository: pr.Repository, number: pr.Number, id: id,
			head: pr.HeadSHA, digest: d})
	}
	if len(records) == 0 {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		for _, rec := range records {
			for i := range cur.Status.PullRequests {
				if pr := &cur.Status.PullRequests[i]; sameRepo(pr.Repository, rec.repository) &&
					pr.Number == rec.number {
					pr.AcceptanceStatusID, pr.AcceptanceHead, pr.AcceptanceDigest = rec.id, rec.head, rec.digest
				}
			}
		}
		return nil
	})
}

// statusesUnavailable reports a Project whose App may not post commit
// statuses (its AcceptanceStatusUnavailable condition, True): no
// maul/acceptance is posted or listed, and the comments say why. The
// verification itself goes on.
func (p *pass) statusesUnavailable() bool {
	return meta.IsStatusConditionTrue(p.proj.Status.Conditions, ConditionAcceptanceStatusUnavailable)
}

// statusCheck is when the statuses at a pull request's head were last
// listed for an impostor.
type statusCheck struct {
	head string
	at   time.Time
}

// impostorLatest reports that the latest maul/acceptance at pr's head is
// someone else's: a later status (by id) than maul's recorded one, posted
// by an account other than the App's bot. The statuses are listed at most
// once per poll interval per head, under the repository's rate floor; a
// failed listing reports none. The account is remembered for the
// verification section.
func (p *pass) impostorLatest(ctx context.Context, pr v1alpha1.IntentPullRequest) bool {
	if pr.AcceptanceStatusID == 0 || pr.AcceptanceHead != pr.HeadSHA {
		return false
	}
	key, due := prKey(pr), false
	p.notes(func(n *previewNotes) {
		c := n.checked[key]
		due = c.head != pr.HeadSHA || p.now.Sub(c.at) >= p.set.PRPollInterval
	})
	if !due {
		return false
	}
	if above, err := p.rateOK(ctx, pr.Repository); err != nil || !above {
		return false
	}
	p.notes(func(n *previewNotes) { n.checked[key] = statusCheck{head: pr.HeadSHA, at: p.now} })
	statuses, err := p.r.GitHub.ListCommitStatuses(ctx, pr.Repository, pr.HeadSHA)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "list the statuses at a pull request head",
			slog.String("intent", p.in.Name), slog.String("repository", pr.Repository),
			slog.Int64("number", pr.Number), slog.Any("error", err))
		return false
	}
	var latestID int64
	var creator string
	for _, s := range statuses {
		if s.Context == acceptanceContext && s.ID > latestID {
			latestID, creator = s.ID, s.Creator
		}
	}
	if latestID <= pr.AcceptanceStatusID {
		return false
	}
	bot, err := p.r.GitHub.BotLogin(ctx, pr.Repository)
	switch {
	case err != nil || strings.EqualFold(creator, bot):
		// The App's own (a write lost after posting), or unknown: never
		// named as someone else's.
		return false
	case bot == "":
		// A personal access token posts as its user, whose own statuses
		// cannot be told from another's: maul's state is posted again,
		// naming no one.
		return true
	}
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "another account posted maul/acceptance; maul posts its own again",
		slog.String("intent", p.in.Name), slog.String("repository", pr.Repository),
		slog.Int64("number", pr.Number), slog.String("creator", creator))
	p.notes(func(n *previewNotes) { n.impostors[key] = impostorNote{head: pr.HeadSHA, login: creator} })
	return true
}
