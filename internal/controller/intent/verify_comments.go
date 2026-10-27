// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
)

// How the acceptance verification is reported on GitHub (acceptance
// verification, slice 1), from the state the verification loop
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
// approved criterion's statement, a failure's evidence and a component's
// log tail, each through the templates' inert rendering. All of it is best
// effort: a write that fails is logged and tried on a later pass, and never
// holds up the human's rounds.

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
// loop has recorded the approved plan revision's criteria and a state, or
// stopped because they cannot be read (criteriaUnreadable).
func (p *pass) verifyReported() bool {
	return p.set.Verification && verifyPhase(p.in.Status.Phase) && p.in.Status.Verification != nil &&
		(p.acceptanceCurrent() || p.criteriaUnreadable())
}

// verifyJudged reports a state holding a round's judged (or given up)
// result: the states the issue's verification comment is written in.
func verifyJudged(s v1alpha1.IntentVerificationState) bool {
	switch s {
	case v1alpha1.VerificationPassed, v1alpha1.VerificationFailing, v1alpha1.VerificationFixing,
		v1alpha1.VerificationExhausted, v1alpha1.VerificationUnavailable:
		return true
	}
	return false
}

// verifyFixRound is the verify-fix round the current verification belongs
// to, counted as the Project's maxVerifyFixes is (the first verification
// is round 1, each fix adds one), and the most there can be.
func (p *pass) verifyFixRound() (round, most int32) {
	most = p.proj.Spec.Limits.MaxVerifyFixesLimit() + 1
	return min(p.verifyFixRounds()+1, most), most
}

// openQuarantined counts the criteria that passed only on retry and no
// approver has accepted at the current head set (D7: never counted as
// passed until a human accepts them or a clean run passes them).
func openQuarantined(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) int32 {
	var n int32
	for _, id := range v.QuarantinedIDs {
		if acceptedAt(acc, id, v.HeadSetDigest) == nil {
			n++
		}
	}
	return n
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

// verifyProgress is where verification stands as the status comment's
// preview line says it; nil when it says nothing (not reported, or Off).
func (p *pass) verifyProgress() *templates.VerifyProgress {
	if !p.verifyReported() {
		return nil
	}
	if p.criteriaUnreadable() {
		return &templates.VerifyProgress{State: templates.VerifyUnreadable}
	}
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	round, most := p.verifyFixRound()
	out := &templates.VerifyProgress{Round: round, MaxRounds: most, ManualOpen: v.ManualOpen}
	total := int32(len(automatable(acc.Criteria)))
	switch v.State {
	case v1alpha1.VerificationNotDeclared:
		out = &templates.VerifyProgress{State: templates.VerifyNotDeclared}
		if len(acc.Criteria) > 0 {
			out.State = templates.VerifyNotChecked
			for _, c := range acc.Criteria {
				if c.Kind == v1alpha1.AcceptanceManual {
					out.ManualOpen++
				}
			}
		}
	case v1alpha1.VerificationWaitingForCI:
		out.State = templates.VerifyWaitingForCI
	case v1alpha1.VerificationWaitingForPreview:
		out.State = templates.VerifyWaiting
	case v1alpha1.VerificationVerifying:
		out.State = templates.VerifyRunning
	case v1alpha1.VerificationPassed:
		out.State, out.Passing, out.Total = templates.VerifyPassed, v.Passed, total
		out.Quarantined = openQuarantined(acc, v)
	case v1alpha1.VerificationFailing:
		out.State, out.Passing, out.Total = templates.VerifyFailing, v.Passed, total
		if v.AutoFix.State == v1alpha1.AutoFixHalted {
			out.AutoFix, out.AutoFixReason = templates.FixHalted, string(v.AutoFix.Reason)
		}
	case v1alpha1.VerificationFixing:
		out.State, out.Passing, out.Total = templates.VerifyFailing, v.Passed, total
		out.AutoFix = templates.FixRunning
	case v1alpha1.VerificationExhausted:
		out.State, out.Passing, out.Total = templates.VerifyExhausted, v.Passed, total
	case v1alpha1.VerificationNeedsReplan:
		out.State = templates.VerifyNeedsReplan
	case v1alpha1.VerificationUnavailable:
		out.State = templates.VerifyUnavailable
	default:
		// Off, or no state yet: nothing to say.
		return nil
	}
	return out
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
	s := &templates.VerifySection{Progress: *prog}
	if p.criteriaUnreadable() {
		// The criteria on record, if any, are not the approved ones.
		return s
	}
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	pv := p.judgedVerification(ctx)
	judged := verifyJudged(v.State) && v.Passed+v.Failed+v.Quarantined > 0
	for _, c := range acc.Criteria {
		s.Criteria = append(s.Criteria, criterionView(c, acc, v, pv, judged))
	}
	if pv != nil {
		s.Logs = p.verifyLogs(pv, pr)
	}
	s.Impostor = p.impostor(pr)
	return s
}

// criterionView is one criterion as a comment shows it.
func criterionView(c v1alpha1.AcceptanceCriterion, acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification,
	pv *v1alpha1.PreviewVerification, judged bool) templates.VerifyCriterion {
	out := templates.VerifyCriterion{ID: c.ID, Kind: string(c.Kind), Statement: c.Statement,
		Outcome: templates.VerifyOutcomeNotRun}
	switch c.Kind {
	case v1alpha1.AcceptanceManual:
		out.Outcome = templates.VerifyOutcomeManual
		if acceptedAt(acc, c.ID, v.HeadSetDigest) != nil {
			out.Outcome = templates.VerifyOutcomeAccepted
		} else if r := latestAcceptance(acc, c.ID); r != nil && len(r.Heads) > 0 {
			// Accepted at other commits: shown as such, and still open.
			out.Outcome, out.AcceptedAt = templates.VerifyOutcomeAccepted, r.Heads[0]
		}
	case v1alpha1.AcceptanceHTTP:
		switch {
		case slices.ContainsFunc(v.Failing, func(f v1alpha1.CriterionState) bool { return f.ID == c.ID }):
			out.Outcome = templates.VerifyOutcomeFail
			if pv != nil {
				out.Expected, out.Observed, out.Body = failureEvidence(c, pv.Status.Observations)
			}
		case slices.Contains(v.QuarantinedIDs, c.ID):
			out.Outcome = templates.VerifyOutcomeQuarantined
			if acceptedAt(acc, c.ID, v.HeadSetDigest) != nil {
				out.Outcome = templates.VerifyOutcomeAccepted
			}
		case judged:
			out.Outcome = templates.VerifyOutcomePass
		}
	case v1alpha1.AcceptanceCommand:
		// Not run until command checks ship.
	}
	return out
}

// latestAcceptance is the latest acceptance of criterion id, at any head
// set, nil when there is none.
func latestAcceptance(acc *v1alpha1.IntentAcceptance, id string) *v1alpha1.AcceptanceRecord {
	var latest *v1alpha1.AcceptanceRecord
	for i := range acc.Accepted {
		if r := &acc.Accepted[i]; r.ID == id && (latest == nil || !r.At.Before(&latest.At)) {
			latest = r
		}
	}
	return latest
}

// failureEvidence renders a failed http criterion's evidence in maul's
// words from its one observation: the request and the statuses the step
// that failed expected, what came back (a status or how the request
// failed), and the response body's prefix. Nothing of the observation is
// shown but a status number, a transport class from the fixed vocabulary
// and the body prefix, which the templates show only as inert code.
func failureEvidence(c v1alpha1.AcceptanceCriterion, obs []v1alpha1.CriterionObservation) (expected, observed,
	body string) {
	i := slices.IndexFunc(obs, func(o v1alpha1.CriterionObservation) bool { return o.ID == c.ID })
	if i < 0 || len(obs[i].Tries) == 0 {
		return "", "not observed", ""
	}
	try := obs[i].Tries[len(obs[i].Tries)-1]
	if len(try.Steps) == 0 {
		return "", "not observed", ""
	}
	n := len(try.Steps) - 1
	step := try.Steps[n]
	if cs, err := criteriaFromCRD([]v1alpha1.AcceptanceCriterion{c}); err == nil && len(cs) == 1 &&
		n < len(cs[0].Steps) {
		expected = expectedOf(cs[0].Steps[n])
	}
	switch t := acceptance.Transport(string(step.TransportError)); t {
	case acceptance.TransportNone, "":
		observed = "status " + strconv.Itoa(int(step.Status))
	case acceptance.TransportRefused, acceptance.TransportTimeout, acceptance.TransportReset,
		acceptance.TransportDNS, acceptance.TransportTLS, acceptance.TransportTooLarge,
		acceptance.TransportCaptureMissing:
		observed = "no response: " + string(t)
	default:
		observed = "no response"
	}
	if raw, err := base64.StdEncoding.DecodeString(step.BodyPrefix); err == nil {
		body = string(raw)
	}
	return expected, observed, body
}

// expectedOf is what a step expects, as a failure names it: its request
// and its acceptable statuses (200 when it lists none).
func expectedOf(st acceptance.Step) string {
	method := st.Request.Method
	if method == "" {
		method = "GET"
	}
	statuses := make([]string, 0, len(st.Expect.Status))
	for _, s := range st.Expect.Status {
		statuses = append(statuses, strconv.Itoa(int(s)))
	}
	if len(statuses) == 0 {
		statuses = []string{"200"}
	}
	return fmt.Sprintf("%s %s: status %s", method, st.Request.Path, strings.Join(statuses, " or "))
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
func (p *pass) syncVerifyComment(ctx context.Context) {
	v := p.in.Status.Verification
	if v.Round == 0 || !verifyJudged(v.State) || p.in.Status.Approval == nil {
		return
	}
	sec := p.verifySection(ctx, nil)
	if sec == nil {
		return
	}
	body, err := templates.RenderVerifyComment(templates.VerifyComment{Namespace: p.in.Namespace,
		Intent: p.in.Name, Round: v.Round, Attempt: v.Attempt, Section: *sec})
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelError, "render the verification comment",
			slog.String("intent", p.in.Name), slog.Any("error", err))
		return
	}
	d := digest([]byte(body))
	var note verifyNote
	p.notes(func(n *previewNotes) { note = n.verify })
	if note.round == v.Round && note.digest == d {
		return
	}
	if above, err := p.rateOK(ctx, p.repo()); err != nil || !above {
		return
	}
	id := int64(0)
	if note.round == v.Round {
		id = note.id
	}
	id, err = p.writeVerifyComment(ctx, id, body)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "write the verification comment; retried at a later pass",
			slog.String("intent", p.in.Name), slog.Int("round", int(v.Round)), slog.Any("error", err))
		return
	}
	p.notes(func(n *previewNotes) { n.verify = verifyNote{round: v.Round, id: id, digest: d} })
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

// syncVerifyReports writes the verification's issue comment and its
// maul/acceptance statuses, while it is reported. changed reports a status
// write; only errConflict is returned.
func (p *pass) syncVerifyReports(ctx context.Context) (bool, error) {
	if !p.verifyReported() {
		return false, nil
	}
	p.syncVerifyComment(ctx)
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
