// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The check store (DESIGN 2.2 step 10, 4.3 rules 1-4; SPEC S1-19): the
// Intent's saved checks, in an immutable ConfigMap <intent>-checks-<n> the
// Intent owns, labelled for intent-controller's informer, in the release
// namespace. status.acceptance.checks names the store of record with its
// digest and count (storedChecks reads it). A store is never updated: one
// that changes is replaced by the next number and the ref moved, the earlier
// one left to the Intent's cascade (intent-controller deletes no ConfigMap).
//
// A check is stored only when it is one of maul's forms and holds to the
// saved-check grammar (rules 1-2: checks.Validate, at its canonical bytes,
// its digest its own), passed its confirmation run in this round's tools
// ledger with a run token tools generated (rule 3), and was reviewed proves
// for its outcome's exact text (rule 4). A check proposed to show a defect
// is stored only red by assertion in its confirmation and reviewed proves
// on the red question, marked Expect fail. Rules 5 and 6 are judged each
// round (acceptance.Decide). A stored check leaves the store only retired,
// and retired only when disputed with a stored replacement for the same
// outcome reviewed proves (retirable).
//
// Nothing of the store reaches a build or revise Job: those handoffs are
// built from the plan and the reviewers' feedback alone (DESIGN 4.4).

// checkStorePrefix joins an Intent's name to its store's number.
const checkStorePrefix = "-checks-"

// reviewOutcomeMaxBytes bounds an outcome's text in the check review
// handoff (agent-runner's bound; the schema holds an outcome to 500).
const reviewOutcomeMaxBytes = 4 << 10

// reviewOutcomeText is an outcome's text as the check reviewer is handed
// it: bounded and made visible, the approver's words as data.
func reviewOutcomeText(o v1alpha1.AcceptanceOutcome) string {
	return untrusted.Visible(o.Outcome, reviewOutcomeMaxBytes)
}

// outcomeDigest is the digest of the one outcome text a check review is
// asked about (reviewOutcomeText), which the review is cached under: a
// review counts only for the exact text it answered.
func outcomeDigest(o v1alpha1.AcceptanceOutcome) string { return digest([]byte(reviewOutcomeText(o))) }

// storeCandidate is a confirmed check a round may store once reviewed, and
// the outcome it is bound to. A check saved to show a defect (its Expect
// fail) carries the step (1-based) its confirmation failed at, which the
// red question names.
type storeCandidate struct {
	check   v1alpha1.SavedCheck
	outcome v1alpha1.AcceptanceOutcome
	// green: a stored check saved red, now replayed green, to be asked the
	// green question (regreenCandidates); never stored again.
	green bool
}

// red reports a candidate to be asked the red question: proposed to show a
// defect, and not a stored one asked the green question.
func (c storeCandidate) red() bool { return c.check.Expect == v1alpha1.CheckExpectFail && !c.green }

// regreenCandidates are the stored checks saved red whose replays in pv all
// passed with a run token, and that have no cached review of the green
// question for their outcome's text (third review, F5): a check saved red
// passes its outcome only once that question is answered proves too.
func regreenCandidates(pv *v1alpha1.PreviewVerification, outcomes []v1alpha1.AcceptanceOutcome,
	stored []v1alpha1.SavedCheck, reviews []v1alpha1.CheckReviewRecord) []storeCandidate {
	answers := reviewsByKey(reviews)
	var out []storeCandidate
	for _, d := range clearedRed(stored, pv.Status.Replay) {
		i := slices.IndexFunc(stored, func(c v1alpha1.SavedCheck) bool { return c.Digest == d })
		o := slices.IndexFunc(outcomes, func(o v1alpha1.AcceptanceOutcome) bool { return o.ID == stored[i].Outcome })
		if o < 0 || outcomes[o].Manual {
			continue
		}
		c := storeCandidate{check: stored[i], outcome: outcomes[o], green: true}
		if _, ok := answers[c.reviewKey()]; !ok {
			out = append(out, c)
		}
	}
	return out
}

// reviewKey is a candidate's review cache key.
func (c storeCandidate) reviewKey() reviewKey {
	k := reviewKey{check: c.check.Digest, outcome: outcomeDigest(c.outcome)}
	if c.red() {
		k.red, k.step, k.assertion = true, c.check.FailedStep, c.check.FailedAssertion
	}
	return k
}

// reviewKey keys a cached review: the check's digest and its outcome's,
// and, for the red question, the failing assertion it was asked about. A
// check saved red is stored only on an answer to that question about the
// assertion it was saved failing (second review, 2A): another failing
// assertion needs a new review.
type reviewKey struct {
	check, outcome string
	red            bool
	step           int32
	assertion      string
}

// storeCandidates are pv's confirmed checks a round may store, in the order
// preview-controller recorded them, judged from the verification alone
// (rules 1-3): bound to an approved outcome a human does not close, at most
// MaxSavedCheckBytes, its body the canonical form of a check of its id,
// outcome and form whose digest it is, valid under the saved-check grammar
// against the preview, its confirmation a clean pass with a run token (for
// a check proposed to show a defect: red by assertion, confirmedRed), and
// not already stored. Each refusal is reported with its reason.
func storeCandidates(pv *v1alpha1.PreviewVerification, outcomes []v1alpha1.AcceptanceOutcome,
	stored []v1alpha1.SavedCheck, preview checks.Preview,
	refused func(reason string, c v1alpha1.SavedCheck, detail string)) []storeCandidate {
	var out []storeCandidate
	seen := map[string]bool{}
	for _, c := range stored {
		seen[c.Digest] = true
	}
	for _, c := range pv.Status.Confirmed {
		if seen[c.Digest] {
			continue
		}
		i := slices.IndexFunc(outcomes, func(o v1alpha1.AcceptanceOutcome) bool { return o.ID == c.Outcome })
		if i < 0 || outcomes[i].Manual {
			refused("not bound to an outcome maul evaluates", c, c.Outcome)
			continue
		}
		if reason, detail := savedCheckRefusal(c, preview); reason != "" {
			refused(reason, c, detail)
			continue
		}
		cand := storeCandidate{check: c, outcome: outcomes[i]}
		switch {
		case cand.red() && !mayShowDefect(c):
			refused("a check to show a defect must write its run's data and expect only success statuses", c, "")
			continue
		case cand.red() && !confirmedRed(pv.Status.Confirmations, c):
			refused("no confirmation run red by the assertion it was confirmed failing", c, "")
			continue
		case !cand.red() && !confirmedClean(pv.Status.Confirmations, c.Digest):
			refused("no clean confirmation run", c, "")
			continue
		}
		seen[c.Digest] = true
		out = append(out, cand)
	}
	return out
}

// mayShowDefect reports whether c's body may show a defect (checks.ValidateRed:
// it writes its run's data and lists only success statuses); a body that does
// not parse may not.
func mayShowDefect(c v1alpha1.SavedCheck) bool {
	parsed, err := checks.Parse([]byte(c.Body))
	return err == nil && checks.ValidateRed(parsed) == nil
}

// confirmedRed reports that every confirmation of c, expected to fail,
// failed red by assertion with a run token tools generated, by the one
// assertion c was confirmed failing (its FailedStep and FailedAssertion),
// and at least one did.
func confirmedRed(runs []v1alpha1.CheckRun, c v1alpha1.SavedCheck) bool {
	if c.FailedStep == 0 || c.FailedAssertion == "" {
		return false
	}
	seen := false
	for _, r := range runs {
		if r.CheckDigest != c.Digest {
			continue
		}
		if !r.RedByAssertion() || r.Expect != v1alpha1.CheckExpectFail || r.Token == "" ||
			r.AssertionStep != c.FailedStep || r.AssertionID != c.FailedAssertion {
			return false
		}
		seen = true
	}
	return seen
}

// savedCheckRefusal is why c cannot be stored by its own bytes (rules 1-2),
// with a detail, or "" when it can.
func savedCheckRefusal(c v1alpha1.SavedCheck, preview checks.Preview) (string, string) {
	if len(c.Body) > v1alpha1.MaxSavedCheckBytes {
		return "over the saved-check cap", fmt.Sprintf("%d bytes, over %d", len(c.Body),
			v1alpha1.MaxSavedCheckBytes)
	}
	parsed, err := checks.Parse([]byte(c.Body))
	switch {
	case err != nil:
		return "not a check of maul's grammar", err.Error()
	case string(checks.Canonical(parsed)) != c.Body:
		return "not in canonical form", ""
	case checks.Digest(parsed) != c.Digest:
		return "its body is not its digest", ""
	case parsed.ID != c.ID || parsed.Outcome != c.Outcome || string(parsed.Form) != string(c.Form):
		return "its body names another check", ""
	}
	if err := checks.Validate(parsed, preview); err != nil {
		return "refused by the saved-check rules", err.Error()
	}
	return "", ""
}

// confirmedClean reports a confirmation of check digest that passed with a
// run token tools generated, and none that did not pass.
func confirmedClean(runs []v1alpha1.CheckRun, d string) bool {
	pass := false
	for _, r := range runs {
		if r.CheckDigest != d {
			continue
		}
		if r.Result != v1alpha1.CheckRunPass || r.Token == "" {
			return false
		}
		pass = true
	}
	return pass
}

// checkPreview is what pv's checks may reach: its components' keys and
// path prefixes, as the slot tool pod validates them.
func (p *pass) checkPreview(pv *v1alpha1.PreviewVerification) checks.Preview {
	var out checks.Preview
	previews := v1alpha1.EffectivePreviews(p.proj)
	for _, head := range pv.Spec.Heads {
		if i := slices.IndexFunc(previews, func(rp v1alpha1.RepositoryPreview) bool {
			return rp.Name == head.Component
		}); i >= 0 {
			out.Components = append(out.Components, head.Component)
			out.Paths = append(out.Paths, previews[i].Path)
		}
	}
	return out
}

// reviewsByKey indexes cached reviews by key, the latest winning.
func reviewsByKey(rs []v1alpha1.CheckReviewRecord) map[reviewKey]v1alpha1.ReviewAnswer {
	out := make(map[reviewKey]v1alpha1.ReviewAnswer, len(rs))
	for _, r := range rs {
		k := reviewKey{check: r.CheckDigest, outcome: r.OutcomeDigest}
		if r.Expect == v1alpha1.CheckExpectFail {
			k.red, k.step, k.assertion = true, r.FailedStep, r.FailedAssertion
		}
		out[k] = r.Answer
	}
	return out
}

// proven are the candidates reviewed proves for their outcome's text.
func proven(cands []storeCandidate, reviews []v1alpha1.CheckReviewRecord) []v1alpha1.SavedCheck {
	answers := reviewsByKey(reviews)
	var out []v1alpha1.SavedCheck
	for _, c := range cands {
		if answers[c.reviewKey()] == v1alpha1.ReviewProves {
			out = append(out, c.check)
		}
	}
	return out
}

// retirable are the digests of retire a store may retire: each stored and
// disputed with a replacement that is another stored check of the same
// outcome, reviewed proves for that outcome's text. Anything else keeps
// its check (DESIGN 4.3: a check that failed is retired only through a
// dispute with a confirmed replacement). In retire's order.
func retirable(stored []v1alpha1.SavedCheck, outcomes []v1alpha1.AcceptanceOutcome,
	reviews []v1alpha1.CheckReviewRecord, retire []string, disputes []report.Dispute) []string {
	answers := reviewsByKey(reviews)
	find := func(d string) (v1alpha1.SavedCheck, bool) {
		i := slices.IndexFunc(stored, func(c v1alpha1.SavedCheck) bool { return c.Digest == d })
		if i < 0 {
			return v1alpha1.SavedCheck{}, false
		}
		return stored[i], true
	}
	var out []string
	for _, d := range retire {
		old, ok := find(d)
		if !ok || slices.Contains(out, d) {
			continue
		}
		i := slices.IndexFunc(disputes, func(dp report.Dispute) bool { return dp.CheckDigest == d })
		if i < 0 || disputes[i].Replacement == "" || disputes[i].Replacement == d {
			continue
		}
		repl, ok := find(disputes[i].Replacement)
		if !ok || repl.Outcome != old.Outcome {
			continue
		}
		o := slices.IndexFunc(outcomes, func(o v1alpha1.AcceptanceOutcome) bool { return o.ID == old.Outcome })
		if o < 0 || answers[reviewKey{check: repl.Digest, outcome: outcomeDigest(outcomes[o])}] !=
			v1alpha1.ReviewProves {
			continue
		}
		out = append(out, d)
	}
	return out
}

// retireChecks retires the disputed checks of retire that retirable admits
// (S1-20 passes acceptance.Decide's Retire and the round's disputes): the
// rest of the store is written as the next store. It reports whether it
// wrote; a retirement refused writes nothing.
func (p *pass) retireChecks(ctx context.Context, retire []string, disputes []report.Dispute) (bool, error) {
	if p.in.Status.Acceptance == nil || len(retire) == 0 {
		return false, nil
	}
	stored, _, err := p.storedChecks(ctx)
	if err != nil {
		return false, err
	}
	ok := retirable(stored, p.in.Status.Acceptance.Outcomes, p.in.Status.Acceptance.Reviews, retire, disputes)
	if refused := len(retire) - len(ok); refused > 0 {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "a retirement without a stored, proven replacement is refused",
			slog.String("intent", p.in.Name), slog.Int("refused", refused))
	}
	if len(ok) == 0 {
		return false, nil
	}
	kept := slices.DeleteFunc(slices.Clone(stored), func(c v1alpha1.SavedCheck) bool {
		return slices.Contains(ok, c.Digest)
	})
	return true, p.writeCheckStore(ctx, kept)
}

// checkStoreNumber is the number of the store ref names, 0 for none.
func checkStoreNumber(intent string, ref *v1alpha1.CheckStoreRef) int {
	if ref == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(ref.ConfigMap, intent+checkStorePrefix))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// maxStoreProbes bounds the store numbers writeCheckStore tries past one
// an earlier pass left with other checks.
const maxStoreProbes = 8

// writeCheckStore stores checks as the Intent's next check store and moves
// status.acceptance.checks to it. The store is created immutable; a store
// under the next number that a failed pass left holding exactly these
// checks is adopted, and one holding others is skipped, never updated.
func (p *pass) writeCheckStore(ctx context.Context, saved []v1alpha1.SavedCheck) error {
	if len(saved) > v1alpha1.MaxSavedChecks {
		return fmt.Errorf("a check store holds at most %d checks, not %d", v1alpha1.MaxSavedChecks, len(saved))
	}
	data, sum, err := encodeCheckStore(saved)
	if err != nil {
		return err
	}
	n := checkStoreNumber(p.in.Name, p.in.Status.Acceptance.Checks)
	for range maxStoreProbes {
		n++
		name := p.in.Name + checkStorePrefix + strconv.Itoa(n)
		cm, err := p.ensureConfigMap(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: p.in.Namespace,
				Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name},
				OwnerReferences: []metav1.OwnerReference{intentOwner(p.in)},
			},
			Immutable: new(true),
			Data:      map[string]string{keyCheckStore: data},
		}, p.in.UID)
		if err != nil {
			return err
		}
		if digest([]byte(cm.Data[keyCheckStore])) != sum {
			continue // a failed pass's store of other checks: never updated
		}
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "the intent's check store is replaced",
			slog.String("intent", p.in.Name), slog.String("store", name), slog.Int("checks", len(saved)))
		return p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.Acceptance.Checks = &v1alpha1.CheckStoreRef{ConfigMap: name, Digest: sum,
				Count: int32(len(saved))}
			return nil
		})
	}
	return fmt.Errorf("no free check store name after %s", p.in.Name+checkStorePrefix+strconv.Itoa(n))
}
