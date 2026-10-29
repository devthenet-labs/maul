// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The maul/acceptance commit status (acceptance verification, slice 1):
// what the verification of the combined change says, posted on every open
// pull request's head of an intent in review whose Project verifies. It is
// maul's word on behavioural evidence, never read back as state.
//
//   - success only when every approved criterion passed at the current head
//     set or an approver accepted it there by hand (/maul accept): a manual
//     criterion, one maul does not run yet (command), one that passed only
//     on retry; a plan with no criteria at all only once an approver's
//     /maul accept all. The description names who accepted. Never a green
//     tick for work nothing and no one checked.
//   - pending while maul waits or verifies, and while a result waits for a
//     human; failure when a check failed; error when maul could not run
//     them, an approved criterion cannot run as approved, or the approved
//     criteria cannot be read.
//   - When verification is switched off for an intent (the Project, its
//     Preview or the controller stops verifying), a final error on every pull
//     request maul posted a status on, so none is left pending or green.
//   - Nothing is posted for a Project that never verified the intent, or an
//     ended intent.
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
// maul-authored description (counts and approvers' logins only, within
// GitHub's 140 bytes).
type acceptanceState struct {
	state, description string
}

// switchedOff is the final status of an intent whose verification was
// switched off.
var switchedOff = acceptanceState{statusError, "verification switched off"}

// acceptanceStatus is the maul/acceptance status the recorded verification
// calls for; ok is false when none is posted. postedOnly: it goes only on
// the pull requests maul posted a status on before (switchedOff).
func (p *pass) acceptanceStatus() (st acceptanceState, postedOnly, ok bool) {
	if !verifyPhase(p.in.Status.Phase) {
		return acceptanceState{}, false, false
	}
	if !p.set.Verification || !p.verifies() {
		return switchedOff, true, true
	}
	if !p.verifyReported() {
		return acceptanceState{}, false, false
	}
	switch p.verifyStopped() {
	case reasonCriteriaUnreadable:
		return acceptanceState{statusError, "the approved acceptance criteria cannot be read: replan needed"}, false,
			true
	case reasonCriteriaReplan:
		return acceptanceState{statusError, "the approved plan states criteria, not outcomes: replan needed"}, false,
			true
	}
	st, ok = p.verifyingStatus()
	return st, false, ok
}

// verifyingStatus is the status of a verifying intent's recorded
// verification; ok is false in a state it is about to leave (none yet, or
// Off on a Project that verifies again).
func (p *pass) verifyingStatus() (acceptanceState, bool) {
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	total := int32(len(automatable(acc.Criteria)))
	used, most := p.verifyFixesUsed()
	passed := fmt.Sprintf("%d/%d automated passed", v.Passed, total)
	switch v.State {
	case v1alpha1.VerificationNotDeclared:
		return notCheckedStatus(acc, v), true
	case v1alpha1.VerificationWaitingForCI, v1alpha1.VerificationWaitingForPreview:
		if v.Round > 0 {
			return acceptanceState{statusPending, "combined change moved: re-verifying"}, true
		}
		if v.State == v1alpha1.VerificationWaitingForCI {
			return acceptanceState{statusPending, "waiting for the pull requests' checks"}, true
		}
		return acceptanceState{statusPending, "waiting for the preview"}, true
	case v1alpha1.VerificationVerifying:
		return acceptanceState{statusPending, fmt.Sprintf("verifying: verification %d, %d %s, %d of %d automatic %s used",
			v.Round, total, plural(total, "check", "checks"), used, most, plural(most, "fix", "fixes"))}, true
	case v1alpha1.VerificationPassed:
		return passedStatus(passed, acc, v), true
	case v1alpha1.VerificationFailing:
		if v.AutoFix.State == v1alpha1.AutoFixHalted {
			return acceptanceState{statusFailure, passed + "; automatic fixes halted"}, true
		}
		return acceptanceState{statusFailure, fmt.Sprintf("%s, %d failing", passed, v.Failed)}, true
	case v1alpha1.VerificationFixing:
		// The running fix is already one of verifyFixRounds.
		return acceptanceState{statusFailure, fmt.Sprintf("%s; automatic fix %d of %d running", passed,
			used, most)}, true
	case v1alpha1.VerificationExhausted:
		return acceptanceState{statusFailure, passed + "; no automatic fix left"}, true
	case v1alpha1.VerificationNeedsReplan:
		return acceptanceState{statusError, "a check cannot run as approved: replan needed"}, true
	case v1alpha1.VerificationUnavailable:
		if previewNotLive(v) {
			return acceptanceState{statusError, fmt.Sprintf("the preview was not live within %d minutes, so the "+
				"checks could not run", int(previewWaitMax.Minutes()))}, true
		}
		return acceptanceState{statusError, "maul could not run the checks"}, true
	case v1alpha1.VerificationOff:
	}
	return acceptanceState{}, false
}

// passedStatus is the status of a verification that found no failure: a
// pass only on retry holds it pending until a human accepts it or a clean
// run passes it (D7); so does any criterion only a human closes (manual, or
// not run in this slice) until an approver accepts it. Success names who
// accepted what was closed by hand.
func passedStatus(passed string, acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) acceptanceState {
	if q := openQuarantined(acc, v); q > 0 {
		return acceptanceState{statusPending, fmt.Sprintf("%d passed only on retry", q)}
	}
	h := handClosed(acc, v)
	switch {
	case h.open > 0:
		return acceptanceState{statusPending, fmt.Sprintf("%s, %d to accept by hand", passed, h.open)}
	case h.total > 0:
		return acceptanceState{statusSuccess, fmt.Sprintf("%s; %d accepted by hand %s", passed, h.total,
			acceptedBy(h.accepters))}
	}
	return acceptanceState{statusSuccess, passed}
}

// notCheckedStatus is the status of a plan with nothing maul checks: with
// no criteria at all, pending until an approver's /maul accept all; with
// only criteria a human closes, pending until an approver accepted each at
// the current head set. Success names who accepted.
func notCheckedStatus(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) acceptanceState {
	const none = "no acceptance criteria declared"
	if len(acc.Criteria) == 0 {
		if r := acceptedAt(acc, acceptAll, v.HeadSetDigest); r != nil {
			return acceptanceState{statusSuccess, none + "; accepted " + acceptedBy([]string{r.By})}
		}
		return acceptanceState{statusPending, none + "; an approver may /maul accept all"}
	}
	h := handClosed(acc, v)
	if h.open > 0 {
		return acceptanceState{statusPending, fmt.Sprintf("%d/%d accepted by hand", h.total-h.open, h.total)}
	}
	return acceptanceState{statusSuccess, fmt.Sprintf("%d/%d accepted by hand %s", h.total, h.total,
		acceptedBy(h.accepters))}
}

// handTally is what only a human closes at a verification's head set.
type handTally struct {
	// total and open count the criteria only a human closes, and those no
	// approver has accepted at the head set.
	total, open int32
	// accepters are the approvers who accepted the others, latest first.
	accepters []string
}

// handClosed tallies the criteria only a human closes at v's head set: the
// manual ones and those maul does not run in this slice (command). A
// quarantined criterion is openQuarantined's.
func handClosed(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) handTally {
	var h handTally
	var accepted []*v1alpha1.AcceptanceRecord
	for _, c := range acc.Criteria {
		if !byHand(c) {
			continue
		}
		h.total++
		if r := acceptedAt(acc, c.ID, v.HeadSetDigest); r != nil {
			accepted = append(accepted, r)
		} else {
			h.open++
		}
	}
	for _, id := range v.QuarantinedIDs {
		if r := acceptedAt(acc, id, v.HeadSetDigest); r != nil {
			h.total++
			accepted = append(accepted, r)
		}
	}
	slices.SortStableFunc(accepted, func(a, b *v1alpha1.AcceptanceRecord) int { return b.At.Compare(a.At.Time) })
	for _, r := range accepted {
		if !slices.Contains(h.accepters, r.By) {
			h.accepters = append(h.accepters, r.By)
		}
	}
	return h
}

// byHand reports a criterion only a human closes: manual, or of a kind maul
// does not run in this slice.
func byHand(c v1alpha1.AcceptanceCriterion) bool {
	return c.Kind == v1alpha1.AcceptanceManual || c.Kind == v1alpha1.AcceptanceCommand
}

// acceptedBy names the approvers who accepted, latest first, as a status
// description says it: logins only (GitHub's grammar, the CRD's pattern),
// the first and how many more.
func acceptedBy(logins []string) string {
	switch len(logins) {
	case 0:
		return ""
	case 1:
		return "by " + logins[0]
	}
	return fmt.Sprintf("by %s and %d %s", logins[0], len(logins)-1, plural(int32(len(logins)-1), "other", "others"))
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
	st, postedOnly, ok := p.acceptanceStatus()
	if !ok {
		return false, nil
	}
	headSet := ""
	if v := p.in.Status.Verification; v != nil {
		headSet = v.HeadSetDigest
	}
	target := p.verifyTargetURL()
	var records []acceptanceRecord
	for _, pr := range p.in.Status.PullRequests {
		if !p.acceptanceTarget(pr, postedOnly) {
			continue
		}
		d := acceptanceDigest(pr.HeadSHA, headSet, st)
		if d == pr.AcceptanceDigest && (postedOnly || !p.impostorLatest(ctx, pr)) {
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

// acceptanceTarget reports a pull request maul/acceptance goes on: open,
// with a known head, in a repository the Project still lists, and, when the
// status goes only where one was posted (postedOnly), carrying maul's.
func (p *pass) acceptanceTarget(pr v1alpha1.IntentPullRequest, postedOnly bool) bool {
	return pr.State == prOpen && pr.HeadSHA != "" && !p.leftProject(pr.Repository) &&
		(!postedOnly || pr.AcceptanceStatusID != 0)
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
