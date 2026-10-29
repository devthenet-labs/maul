// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// The evaluator's and the check reviewer's handoffs: what crosses into a
// role Job (DESIGN 4.4). Each is strict JSON of agent-runner's own type
// (agentrun.EvaluateHandoff, agentrun.CheckReviewHandoff), which it decodes
// refusing unknown fields, carried in the run's input as its issue.md. The
// prompts are agent-runner's templates; everything here is data, and every
// string a human or the application wrote reaches the handoff bounded and
// through untrusted.Visible, the templates quoting it as fenced data. Nothing
// of the check store, the evaluator's transcript or its notes ever reaches a
// build or revise handoff: those are built elsewhere, from the plan and the
// reviewers' feedback alone.

// Handoff bounds. A handoff over its bound is cut where it can be (the
// changed paths, the least of it) and otherwise refused, never truncated
// mid-field: agent-runner refuses one past the same bound.
const (
	// evaluateHandoffMaxBytes is agent-runner's bound on the evaluate
	// handoff (256 KiB).
	evaluateHandoffMaxBytes = 256 << 10
	// checkReviewHandoffMaxBytes is agent-runner's bound on the checkreview
	// handoff (512 KiB: up to 40 checks of 8 KiB each beside their outcomes).
	checkReviewHandoffMaxBytes = 512 << 10
	// evaluateRequestMaxBytes bounds the intent's request in the handoff.
	evaluateRequestMaxBytes = 32 << 10
	// changedPathMaxBytes bounds one changed path, and changedPathsMax the
	// paths read per pull request (one page of GitHub's listing).
	changedPathMaxBytes = 512
	changedPathsMax     = 100
	// priorReasonMaxBytes bounds a previous verdict's reason (the Intent's
	// schema bound on it).
	priorReasonMaxBytes = 600
)

// evaluationRunToken is the run token the evaluator names the data it
// creates with while exploring: the round, the attempt and the
// verification's own random token, in the evaluate prompt's grammar.
func evaluationRunToken(pv *v1alpha1.PreviewVerification) string {
	return fmt.Sprintf("ev%da%d-%s", pv.Spec.Round, pv.Spec.Attempt, pv.Spec.Run)
}

// evaluateHandoff renders pv's evaluate handoff from the intent's records:
// the round and its run token, the request (the input snapshot's issue
// text), the approved outcomes, the previewed components at pv's heads,
// the tool pod's tools, the files the open pull requests change with
// their line counts (SPEC Q15), and on a round after a judged one, that
// round's results and the diff since its heads (previousRound).
func (p *pass) evaluateHandoff(ctx context.Context, pv *v1alpha1.PreviewVerification) (string, error) {
	outcomes, err := crdconv.OutcomesFromCRD(pv.Spec.Outcomes)
	if err != nil {
		return "", err
	}
	h := agentrun.EvaluateHandoff{
		Round:    int(pv.Spec.Round),
		RunToken: evaluationRunToken(pv),
		Outcomes: outcomes,
	}
	if t := pv.Spec.Tools; t != nil {
		h.Tools = agentrun.EvaluateTools{Browser: t.Browser, Sandbox: t.Sandbox,
			MaxToolCalls: int(t.MaxToolCalls), LookAroundRoutes: int(t.LookAroundRoutes)}
	}
	previews := v1alpha1.EffectivePreviews(p.proj)
	for _, head := range pv.Spec.Heads {
		if i := slices.IndexFunc(previews, func(rp v1alpha1.RepositoryPreview) bool {
			return rp.Name == head.Component
		}); i >= 0 {
			h.Components = append(h.Components, agentrun.EvaluateComponent{Key: head.Component,
				Path: previews[i].Path})
		}
	}
	if h.Request, err = p.requestText(ctx); err != nil {
		return "", err
	}
	h.ChangedPaths = p.changedPaths(ctx)
	if err := p.previousRound(ctx, pv, &h); err != nil {
		return "", err
	}
	return encodeEvaluateHandoff(h)
}

// previousRound fills h, pv's handoff, with what the controller stores of
// the last judged round before pv (SPEC Q15), on a round after one: maul's
// results of the approved outcomes (status.verification.outcomes, which
// only a judgement writes), how that round's saved checks replayed, and
// the diff since the heads it evaluated (fixDiff). Each string is bounded
// and made visible. A previous round's PreviewVerification already pruned
// leaves the verdicts alone.
func (p *pass) previousRound(ctx context.Context, pv *v1alpha1.PreviewVerification,
	h *agentrun.EvaluateHandoff) error {
	if pv.Spec.Round <= 1 {
		return nil
	}
	prev, err := p.judgedBefore(ctx, pv)
	if err != nil {
		return err
	}
	h.PriorVerdicts = p.priorVerdicts()
	if prev == nil {
		if len(h.PriorVerdicts) > 0 {
			h.PriorRound = int(pv.Spec.Round) - 1
		}
		return nil
	}
	h.PriorRound = int(prev.Spec.Round)
	h.PriorReplay = priorReplay(prev)
	h.FixDiff = p.fixDiff(ctx, prev, pv)
	return nil
}

// judgedBefore is the Intent's latest PreviewVerification before pv's round
// that was judged (Observed, or EnvFailed) on the same approved outcomes:
// nil when there is none.
func (p *pass) judgedBefore(ctx context.Context, pv *v1alpha1.PreviewVerification) (
	*v1alpha1.PreviewVerification, error) {
	pvs, err := p.ownVerifications(ctx)
	if err != nil {
		return nil, err
	}
	var prev *v1alpha1.PreviewVerification
	for i := range pvs {
		c := &pvs[i]
		judged := c.Status.Phase == v1alpha1.VerificationObserved || c.Status.Phase == v1alpha1.VerificationEnvFailed
		if !judged || c.Spec.Round >= pv.Spec.Round || c.Spec.OutcomesDigest != pv.Spec.OutcomesDigest {
			continue
		}
		if prev == nil || c.Spec.Round > prev.Spec.Round ||
			c.Spec.Round == prev.Spec.Round && c.Spec.Attempt > prev.Spec.Attempt {
			prev = c
		}
	}
	return prev, nil
}

// priorVerdicts are maul's recorded results of the approved outcomes, in
// plan order, each reason bounded and made visible.
func (p *pass) priorVerdicts() []agentrun.PriorVerdict {
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	if acc == nil || v == nil {
		return nil
	}
	var out []agentrun.PriorVerdict
	for _, o := range acc.Outcomes {
		i := slices.IndexFunc(v.Outcomes, func(r v1alpha1.OutcomeResult) bool { return r.ID == o.ID })
		if i < 0 {
			continue
		}
		r := v.Outcomes[i]
		out = append(out, agentrun.PriorVerdict{Outcome: r.ID, Result: string(r.Result),
			Reason: untrusted.Visible(r.Reason, priorReasonMaxBytes)})
	}
	return out
}

// priorReplay is how prev's saved checks replayed, in replay order: each by
// its id and outcome, a run of a check prev does not carry left out.
func priorReplay(prev *v1alpha1.PreviewVerification) []agentrun.PriorReplay {
	var out []agentrun.PriorReplay
	for _, r := range prev.Status.Replay {
		i := slices.IndexFunc(prev.Spec.Checks, func(c v1alpha1.SavedCheck) bool { return c.Digest == r.CheckDigest })
		if i < 0 {
			continue
		}
		c := prev.Spec.Checks[i]
		out = append(out, agentrun.PriorReplay{Check: c.ID, Outcome: c.Outcome, Result: string(r.Result)})
	}
	return out
}

// fixDiff is the diff since the heads prev evaluated (SPEC Q15): one
// compare per component whose head moved, under its repository's name,
// bounded to what the prompt quotes and made visible; "" when no head
// moved. A compare that fails is logged and left out: the evaluator judges
// without it rather than not at all.
func (p *pass) fixDiff(ctx context.Context, prev, pv *v1alpha1.PreviewVerification) string {
	var b strings.Builder
	for _, head := range pv.Spec.Heads {
		i := slices.IndexFunc(prev.Spec.Heads, func(o v1alpha1.VerificationHead) bool {
			return o.Component == head.Component
		})
		if i < 0 || prev.Spec.Heads[i].Revision == head.Revision || b.Len() >= templates.FixDiffMaxBytes {
			continue
		}
		patch, err := p.r.GitHub.ComparePatch(ctx, head.Repository, prev.Spec.Heads[i].Revision, head.Revision)
		if err != nil {
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "compare the heads for the evaluator's fix diff; left out",
				slog.String("intent", p.in.Name), slog.String("repository", head.Repository), slog.Any("error", err))
			continue
		}
		fmt.Fprintf(&b, "# %s %s...%s\n%s\n", repoSlug(head.Repository), prev.Spec.Heads[i].Revision,
			head.Revision, strings.TrimRight(patch, "\n"))
	}
	return untrusted.Visible(b.String(), templates.FixDiffMaxBytes)
}

// requestText is the intent's request as its input snapshot recorded it,
// bounded and made visible: "" when the snapshot is gone.
func (p *pass) requestText(ctx context.Context) (string, error) {
	in := p.in.Status.Input
	if in == nil || in.ConfigMap == "" {
		return "", nil
	}
	var cm corev1.ConfigMap
	err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: in.ConfigMap}, &cm)
	switch {
	case kerrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read the input snapshot: %w", err)
	}
	return untrusted.Visible(cm.Data[keyIssue], evaluateRequestMaxBytes), nil
}

// changedPaths are the files the intent's open pull requests change, with
// their line counts, in pull request then GitHub order: each path bounded
// and made visible. A listing that fails is logged and left out: the
// evaluator explores without it rather than not at all.
func (p *pass) changedPaths(ctx context.Context) []agentrun.ChangedPath {
	var out []agentrun.ChangedPath
	for _, pr := range p.in.Status.PullRequests {
		if pr.State != prOpen {
			continue
		}
		files, err := p.r.GitHub.ListPullRequestFiles(ctx, pr.Repository, pr.Number, changedPathsMax)
		if err != nil {
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "list a pull request's files for the evaluator; left out",
				slog.String("intent", p.in.Name), slog.Int64("pull_request", pr.Number), slog.Any("error", err))
			continue
		}
		for _, f := range files {
			path := untrusted.Visible(f.Path, changedPathMaxBytes)
			if len(p.in.Status.PullRequests) > 1 {
				// Several repositories change: each path names its own.
				path = repoSlug(pr.Repository) + ": " + path
			}
			out = append(out, agentrun.ChangedPath{Path: path, Added: f.Additions, Deleted: f.Deletions})
		}
	}
	return out
}

// encodeEvaluateHandoff encodes h as agent-runner decodes it, within its
// bound: changed paths are dropped from the end until it fits, and a
// handoff that does not fit without them is refused.
func encodeEvaluateHandoff(h agentrun.EvaluateHandoff) (string, error) {
	for {
		b, err := json.Marshal(h)
		if err != nil {
			return "", fmt.Errorf("encode the evaluate handoff: %w", err)
		}
		if len(b) <= evaluateHandoffMaxBytes {
			return string(b), nil
		}
		if len(h.ChangedPaths) == 0 {
			return "", fmt.Errorf("the evaluate handoff is %d bytes, over %d", len(b), evaluateHandoffMaxBytes)
		}
		h.ChangedPaths = h.ChangedPaths[:len(h.ChangedPaths)/2]
	}
}

// encodeCheckReviewHandoff encodes the check reviewer's handoff as
// agent-runner decodes it: one review per new check, keyed by the check's
// and its outcome's digests, the check as maul's canonical JSON. One past
// agent-runner's bound is refused, never cut: a cut review would be another
// review. S1-19's check review round renders it.
func encodeCheckReviewHandoff(reviews []agentrun.CheckReviewItem) (string, error) {
	if len(reviews) == 0 {
		return "", fmt.Errorf("a check review has no reviews")
	}
	b, err := json.Marshal(agentrun.CheckReviewHandoff{Reviews: reviews})
	if err != nil {
		return "", fmt.Errorf("encode the check review handoff: %w", err)
	}
	if len(b) > checkReviewHandoffMaxBytes {
		return "", fmt.Errorf("the check review handoff is %d bytes, over %d", len(b), checkReviewHandoffMaxBytes)
	}
	return string(b), nil
}
