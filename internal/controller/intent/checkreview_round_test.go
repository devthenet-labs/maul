// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/report"
)

// Checks of outcomesPlan's outcomes on its one component (app, at /).
const (
	// versionCheck creates a build note under the run token and reads it
	// back: substantive, and scoped to its own run.
	versionCheck = `{"id":"version-shown","outcome":"version-shown","form":"http","http":{"target":"app","steps":[
	 {"request":{"method":"POST","path":"/notes","json":{"name":"{{run}}-v"}},"expect":{"status":201}},
	 {"request":{"path":"/version?q={{run}}"},"expect":{"json":{"/note":"{{run}}-v"}}}]}}`
	// healthCheck reads the health endpoint's body.
	healthCheck = `{"id":"still-healthy","outcome":"still-healthy","form":"http","http":{"target":"app","steps":[
	 {"request":{"path":"/healthz"},"expect":{"json":{"/status":"ok"}}}]}}`
	// healthCheck2 is a second check of the same outcome.
	healthCheck2 = `{"id":"still-healthy.again","outcome":"still-healthy","form":"http","http":{"target":"app",
	 "steps":[{"request":{"path":"/healthz?again=1"},"expect":{"json":{"/status":"ok"}}}]}}`
	// vacuousCheck asserts only that the call worked (rule 2 refuses it).
	vacuousCheck = `{"id":"version-shown.vacuous","outcome":"version-shown","form":"http","http":{"target":"app",
	 "steps":[{"request":{"path":"/"},"expect":{"status":200}}]}}`
	// manualCheck is bound to the manual outcome, which only a human closes.
	manualCheck = `{"id":"looks-right","outcome":"looks-right","form":"http","http":{"target":"app","steps":[
	 {"request":{"path":"/version"},"expect":{"json":{"/look":"right"}}}]}}`
)

// savedCheck is raw as a confirmation stores it: canonical body, digest.
func savedCheck(t *testing.T, raw string) v1alpha1.SavedCheck {
	t.Helper()
	c, err := checks.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return v1alpha1.SavedCheck{ID: c.ID, Outcome: c.Outcome, Form: v1alpha1.CheckForm(c.Form),
		Body: string(checks.Canonical(c)), Digest: checks.Digest(c)}
}

// fakeReviewer plays the check reviewer's Jobs: each review it is handed is
// answered from answers by check digest (proves when unnamed), and every
// handoff is kept.
type fakeReviewer struct {
	answers  map[string]v1alpha1.ReviewAnswer
	handoffs []agentrun.CheckReviewHandoff
	fail     bool
	// unreviewed are the checks whose review ended with no report
	// agent-runner accepts: it answers them unclear, unreviewed.
	unreviewed map[string]bool
}

// reviewChecksWith makes e's fake Jobs answer checkreview runs as r does.
func (e *env) reviewChecksWith(r *fakeReviewer) {
	prev := e.jobs.output
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase != "checkreview" {
			return prev(spec)
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(spec.IssueMarkdown)))
		dec.DisallowUnknownFields()
		var h agentrun.CheckReviewHandoff
		if err := dec.Decode(&h); err != nil {
			e.t.Errorf("check review handoff %s: %v", spec.IssueMarkdown, err)
		}
		r.handoffs = append(r.handoffs, h)
		stage := envelope.Stage{Outcome: envelope.OutcomeOK, Harness: "claude", Model: spec.Model,
			Usage: envelope.Usage{InputTokens: 5, OutputTokens: 5, CostUSD: 0.05}}
		if r.fail {
			stage.Outcome, stage.Detail = envelope.OutcomeRuntimeError, "crashed"
			return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeCheckReview,
				CheckReview: &envelope.CheckReview{Stage: stage}}}}
		}
		cr := &envelope.CheckReview{Stage: stage}
		for _, it := range h.Reviews {
			a := r.answers[it.CheckDigest]
			if a == "" {
				a = v1alpha1.ReviewProves
			}
			if r.unreviewed[it.CheckDigest] {
				cr.Answers = append(cr.Answers, envelope.ReviewAnswer{CheckDigest: it.CheckDigest,
					OutcomeDigest: it.OutcomeDigest, Answer: string(v1alpha1.ReviewUnclear),
					Reason: "maul: the review's report was refused", Unreviewed: true})
				continue
			}
			cr.Answers = append(cr.Answers, envelope.ReviewAnswer{CheckDigest: it.CheckDigest,
				OutcomeDigest: it.OutcomeDigest, Answer: string(a), Reason: "read the steps"})
		}
		return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeCheckReview,
			CheckReview: cr}}}
	}
}

// confirm plays preview-controller collecting pv with confirmed as the
// checks that passed their confirmation.
func (e *env) confirm(pvName string, confirmed ...v1alpha1.SavedCheck) {
	e.t.Helper()
	e.playTools(pvName, v1alpha1.VerificationObserved, toolPodIP, true)
	pv := e.pvNamed(pvName)
	pv.Status.Confirmed = confirmed
	pv.Status.Confirmations = nil
	for i, c := range confirmed {
		pv.Status.Confirmations = append(pv.Status.Confirmations, v1alpha1.CheckRun{CheckDigest: c.Digest,
			Result: v1alpha1.CheckRunPass, Token: "cf-tok", Seq: int32(10 + i), DryRunSeq: int32(1 + i)})
	}
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// settleReviews runs review passes and the run scheduler until the round
// is judged, at most a dozen times each.
func (e *env) settleReviews(name string) {
	e.t.Helper()
	for range 12 {
		if e.verification(name).State != v1alpha1.VerificationEvaluating {
			return
		}
		e.review(name, 1)
		e.runRuns()
		e.runRuns()
	}
}

// checkStore reads the intent's check store: its ConfigMap and checks.
func (e *env) checkStore(name string) (*corev1.ConfigMap, []v1alpha1.SavedCheck) {
	e.t.Helper()
	ref := e.get(name).Status.Acceptance.Checks
	if ref == nil {
		return nil, nil
	}
	var cm corev1.ConfigMap
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: ref.ConfigMap}, &cm); err != nil {
		e.t.Fatal(err)
	}
	var out []v1alpha1.SavedCheck
	if err := json.Unmarshal([]byte(cm.Data[keyCheckStore]), &out); err != nil {
		e.t.Fatal(err)
	}
	if digest([]byte(cm.Data[keyCheckStore])) != ref.Digest || int(ref.Count) != len(out) {
		e.t.Fatalf("store %s is not what its ref %+v records", cm.Name, ref)
	}
	return &cm, out
}

// digestsOf are the checks' digests, in order.
func digestsOf(cs []v1alpha1.SavedCheck) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Digest
	}
	return out
}

// TestStoreCandidates: what a round may store, judged from the
// verification alone (rules 1-3), before any review: a confirmed, valid,
// canonical check of an evaluable outcome not already stored, once.
func TestStoreCandidates(t *testing.T) {
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	outcomes := []v1alpha1.AcceptanceOutcome{{ID: "version-shown", Outcome: "shows the version"},
		{ID: "still-healthy", Outcome: "still healthy", Kind: v1alpha1.OutcomeKindKeep},
		{ID: "looks-right", Outcome: "looks right", Manual: true}}
	pass := func(cs ...v1alpha1.SavedCheck) []v1alpha1.CheckRun {
		out := make([]v1alpha1.CheckRun, 0, len(cs))
		for i, c := range cs {
			out = append(out, v1alpha1.CheckRun{CheckDigest: c.Digest, Result: v1alpha1.CheckRunPass, Token: "t",
				Seq: int32(i + 1)})
		}
		return out
	}
	edited := version
	edited.Body = strings.Replace(edited.Body, "/version", "/versions", 1)
	unbound := health
	unbound.Outcome = "version-shown"
	spaced := version
	spaced.Body = " " + version.Body
	big := version
	big.Body = strings.Repeat("x", v1alpha1.MaxSavedCheckBytes+1)
	unknown := savedCheck(t, strings.ReplaceAll(healthCheck, "still-healthy", "gone"))
	for _, tt := range []struct {
		name      string
		confirmed []v1alpha1.SavedCheck
		runs      []v1alpha1.CheckRun
		stored    []v1alpha1.SavedCheck
		want      []string
	}{
		{"both", []v1alpha1.SavedCheck{version, health}, pass(version, health), nil,
			[]string{version.Digest, health.Digest}},
		{"already stored", []v1alpha1.SavedCheck{version, health}, pass(version, health),
			[]v1alpha1.SavedCheck{health}, []string{version.Digest}},
		{"twice", []v1alpha1.SavedCheck{version, version}, pass(version), nil, []string{version.Digest}},
		{"no passing confirmation", []v1alpha1.SavedCheck{version}, nil, nil, nil},
		{"a failed confirmation beside the pass", []v1alpha1.SavedCheck{version},
			append(pass(version), v1alpha1.CheckRun{CheckDigest: version.Digest, Result: v1alpha1.CheckRunFail,
				Token: "t", Seq: 9}), nil, nil},
		{"a pass with no token", []v1alpha1.SavedCheck{version},
			[]v1alpha1.CheckRun{{CheckDigest: version.Digest, Result: v1alpha1.CheckRunPass, Seq: 1}}, nil, nil},
		{"body not its digest", []v1alpha1.SavedCheck{edited}, pass(edited), nil, nil},
		{"bound to another outcome than its body", []v1alpha1.SavedCheck{unbound}, pass(unbound), nil, nil},
		{"not canonical", []v1alpha1.SavedCheck{spaced}, pass(spaced), nil, nil},
		{"over the cap", []v1alpha1.SavedCheck{big}, pass(big), nil, nil},
		{"vacuous", []v1alpha1.SavedCheck{savedCheck(t, vacuousCheck)}, pass(savedCheck(t, vacuousCheck)), nil, nil},
		{"manual outcome", []v1alpha1.SavedCheck{savedCheck(t, manualCheck)}, pass(savedCheck(t, manualCheck)), nil,
			nil},
		{"unknown outcome", []v1alpha1.SavedCheck{unknown}, pass(unknown), nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pv := &v1alpha1.PreviewVerification{}
			pv.Status.Confirmed, pv.Status.Confirmations = tt.confirmed, tt.runs
			got := storeCandidates(pv, outcomes, tt.stored, checks.Context{Preview: &checks.Preview{
				Paths: []string{"/"}, Components: []string{"app"}}}, func(string, v1alpha1.SavedCheck, string) {})
			var digests []string
			for _, c := range got {
				digests = append(digests, c.check.Digest)
			}
			if !slices.Equal(digests, tt.want) {
				t.Errorf("candidates = %v, want %v", digests, tt.want)
			}
		})
	}
}

// TestRetireNeedsProvenReplacement: a disputed check is retired only with
// a replacement the store holds for the same outcome, reviewed proves for
// that outcome's text; anything else is refused and the store is kept.
func TestRetireNeedsProvenReplacement(t *testing.T) {
	health, again, version := savedCheck(t, healthCheck), savedCheck(t, healthCheck2), savedCheck(t, versionCheck)
	outcomes := []v1alpha1.AcceptanceOutcome{{ID: "version-shown", Outcome: "shows the version"},
		{ID: "still-healthy", Outcome: "still healthy", Kind: v1alpha1.OutcomeKindKeep}}
	proves := func(c v1alpha1.SavedCheck, text string, a v1alpha1.ReviewAnswer) v1alpha1.CheckReviewRecord {
		return v1alpha1.CheckReviewRecord{CheckDigest: c.Digest, OutcomeDigest: digest([]byte(text)), Answer: a,
			Run: "cr"}
	}
	stored := []v1alpha1.SavedCheck{health, again, version}
	dispute := func(of, by v1alpha1.SavedCheck) report.Dispute {
		return report.Dispute{CheckDigest: of.Digest, Reason: "stale data", Replacement: by.Digest}
	}
	for _, tt := range []struct {
		name     string
		stored   []v1alpha1.SavedCheck
		reviews  []v1alpha1.CheckReviewRecord
		disputes []report.Dispute
		want     []string
	}{
		{"a proven replacement", stored, []v1alpha1.CheckReviewRecord{proves(again, "still healthy",
			v1alpha1.ReviewProves)}, []report.Dispute{dispute(health, again)}, []string{health.Digest}},
		{"no dispute", stored, []v1alpha1.CheckReviewRecord{proves(again, "still healthy", v1alpha1.ReviewProves)},
			nil, nil},
		{"no replacement", stored, nil, []report.Dispute{{CheckDigest: health.Digest, Reason: "stale"}}, nil},
		{"replacement not stored", []v1alpha1.SavedCheck{health, version},
			[]v1alpha1.CheckReviewRecord{proves(again, "still healthy", v1alpha1.ReviewProves)},
			[]report.Dispute{dispute(health, again)}, nil},
		{"replacement does not prove", stored, []v1alpha1.CheckReviewRecord{proves(again, "still healthy",
			v1alpha1.ReviewDoesNotProve)}, []report.Dispute{dispute(health, again)}, nil},
		{"replacement reviewed for another text", stored, []v1alpha1.CheckReviewRecord{proves(again, "other",
			v1alpha1.ReviewProves)}, []report.Dispute{dispute(health, again)}, nil},
		{"replacement of another outcome", stored, []v1alpha1.CheckReviewRecord{proves(version, "shows the version",
			v1alpha1.ReviewProves)}, []report.Dispute{dispute(health, version)}, nil},
		{"itself", stored, []v1alpha1.CheckReviewRecord{proves(health, "still healthy", v1alpha1.ReviewProves)},
			[]report.Dispute{dispute(health, health)}, nil},
		{"not stored", []v1alpha1.SavedCheck{again}, []v1alpha1.CheckReviewRecord{proves(again, "still healthy",
			v1alpha1.ReviewProves)}, []report.Dispute{dispute(health, again)}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := retirable(tt.stored, outcomes, tt.reviews, []string{health.Digest}, tt.disputes)
			if !slices.Equal(got, tt.want) {
				t.Errorf("retirable = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRetireChecksWritesTheNextStore: retiring a disputed check with a
// proven replacement stores the rest in the next store; a refused
// retirement writes nothing.
func TestRetireChecksWritesTheNextStore(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	health, again := savedCheck(t, healthCheck), savedCheck(t, healthCheck2)
	data, sum, err := encodeCheckStore([]v1alpha1.SavedCheck{health, again})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-checks-1", Namespace: testNS,
			Labels:          map[string]string{v1alpha1.LabelIntent: name},
			OwnerReferences: []metav1.OwnerReference{intentOwner(e.get(name))}},
		Immutable: new(true), Data: map[string]string{keyCheckStore: data},
	}); err != nil {
		t.Fatal(err)
	}
	e.review(name, 1) // the approved outcomes recorded
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Checks = &v1alpha1.CheckStoreRef{ConfigMap: name + "-checks-1", Digest: sum, Count: 2}
	})
	newPass := func() *pass {
		p := &pass{r: e.intent, set: e.intent.Settings, in: e.get(name), proj: e.project0(), now: e.clock.Now()}
		p.runs = e.runPtrs(name)
		return p
	}
	disputes := []report.Dispute{{CheckDigest: health.Digest, Reason: "stale", Replacement: again.Digest}}
	if changed, err := newPass().retireChecks(context.Background(), []string{health.Digest}, disputes); changed ||
		err != nil {
		t.Fatalf("retire with an unreviewed replacement = %v %v, want nothing", changed, err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Reviews = []v1alpha1.CheckReviewRecord{{CheckDigest: again.Digest,
			OutcomeDigest: digest([]byte("the app still answers its health check")),
			Answer:        v1alpha1.ReviewProves, Run: "cr"}}
	})
	if changed, err := newPass().retireChecks(context.Background(), []string{health.Digest}, disputes); !changed ||
		err != nil {
		t.Fatalf("retire = %v %v", changed, err)
	}
	if cm, stored := e.checkStore(name); cm.Name != name+"-checks-2" ||
		!slices.Equal(digestsOf(stored), []string{again.Digest}) {
		t.Fatalf("store %s = %v, want the replacement alone", cm.Name, digestsOf(stored))
	}
}
