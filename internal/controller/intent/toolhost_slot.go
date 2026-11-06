// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"
	"slices"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// slotHost is the slot tool host (S1): an Intent's evaluation runs against
// its Preview, in the Preview's slot; preview-controller renders the tool
// pod and writes the PreviewVerification's status. The functions below it
// are S1's, moved here unchanged (previewHeadSet, livePreview, waitPreview).
type slotHost struct{}

func (slotHost) placement() v1alpha1.VerificationPlacement { return v1alpha1.PlacementSlot }

// verifies: the Intent's Preview is verification-capable
// (v1alpha1.DesiredPreviewEnvironment, the derivation preview-controller
// re-checks).
func (slotHost) verifies(p *pass) bool {
	_, ok := v1alpha1.DesiredPreviewEnvironment(p.proj, p.in)
	return ok
}

// heads: the Preview's head set (previewHeadSet).
func (slotHost) heads(p *pass) (headSet, bool) { return previewHeadSet(p.proj, p.in) }

// gate: the Preview live at exactly the heads (else waitPreview), then
// every named check green (else WaitingForCI). place pins the Preview, by
// UID and the generation the heads were read at; the placement is left
// unset, which reads as slot, so a slot verification stores exactly what it
// stored before placements existed.
func (slotHost) gate(ctx context.Context, p *pass, v *v1alpha1.IntentVerification,
	prs []v1alpha1.IntentPullRequest) (func(*v1alpha1.PreviewVerificationSpec), bool, error) {
	preview := p.livePreview(ctx, v)
	if preview == nil {
		changed, err := p.waitPreview(ctx)
		return nil, changed, err
	}
	green, err := p.checksGreen(ctx, prs)
	if err != nil || !green {
		if err != nil {
			return nil, false, err
		}
		changed, err := p.waitVerification(ctx, v1alpha1.VerificationWaitingForCI)
		return nil, changed, err
	}
	return slotPlace(preview), false, nil
}

// slotPlace pins a slot verification's Preview.
func slotPlace(preview *v1alpha1.Preview) func(*v1alpha1.PreviewVerificationSpec) {
	return func(spec *v1alpha1.PreviewVerificationSpec) {
		spec.PreviewRef = v1alpha1.ObjectReference{Name: preview.Name, UID: preview.UID}
		spec.PreviewGeneration = preview.Generation
	}
}

// previewHeadSet is the head set of the Preview derived from in's records
// (v1alpha1.DesiredPreviewComponents), each component with its repository's
// URL. ok is false when in has no Preview.
func previewHeadSet(proj *v1alpha1.Project, in *v1alpha1.Intent) (headSet, bool) {
	desired, ok := v1alpha1.DesiredPreviewComponents(proj, in)
	if !ok {
		return headSet{}, false
	}
	previews := v1alpha1.EffectivePreviews(proj)
	heads := make([]v1alpha1.VerificationHead, 0, len(desired))
	for _, d := range desired {
		i := slices.IndexFunc(previews, func(rp v1alpha1.RepositoryPreview) bool { return rp.Name == d.Name })
		if i < 0 {
			return headSet{}, false
		}
		heads = append(heads, v1alpha1.VerificationHead{Component: d.Name, Repository: previews[i].URL,
			Revision: d.Revision})
	}
	return headSet{digest: headSetDigest(heads), heads: heads}, true
}

// livePreview reads the Intent's Preview (uncached) and returns it when a
// verification may run against it now: it is live at exactly the heads
// derived from the Intent's records (viewPreview), verification-capable
// (spec.environment), and its parts were restarted for the latest reset the
// Intent records. nil otherwise, a failed read included.
func (p *pass) livePreview(ctx context.Context, v *v1alpha1.IntentVerification) *v1alpha1.Preview {
	var pv v1alpha1.Preview
	err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: p.in.Name}, &pv)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the intent's preview; no verification this pass",
				slog.String("intent", p.in.Name), slog.Any("error", err))
		}
		return nil
	}
	if viewPreview(p.proj, p.in, &pv).state != templates.PreviewLive ||
		pv.Spec.Environment == nil || !pv.Spec.Environment.Verification {
		return nil
	}
	if reset := v.Reset.Generation; pv.Spec.ResetGeneration != reset || pv.Status.ObservedResetGeneration != reset {
		return nil
	}
	return &pv
}

// waitPreview records that a due verification waits for the Preview to be
// live at its heads, for at most previewWaitMax from when the wait began.
// Past it the verification is Unavailable (PreviewNotLive) and stays so,
// reported as an error, until the Preview is live (the gate then verifies)
// or the heads move. A verification in flight or judged stands, as in
// waitVerification, unless an approver's request waits.
func (p *pass) waitPreview(ctx context.Context) (bool, error) {
	v := p.in.Status.Verification
	switch {
	case v.Ref != nil && v.Requested == nil, previewNotLive(v):
		return false, nil
	case v.State != v1alpha1.VerificationWaitingForPreview || v.PreviewWaitSince == nil:
		since := metav1.NewTime(p.now)
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.Verification.State = v1alpha1.VerificationWaitingForPreview
			cur.Status.Verification.PreviewWaitSince = &since
			return nil
		})
	case p.now.Sub(v.PreviewWaitSince.Time) < previewWaitMax:
		return false, nil
	}
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "the preview was not live at the heads in time; verification unavailable",
		slog.String("intent", p.in.Name), slog.Duration("waited", p.now.Sub(v.PreviewWaitSince.Time)))
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.UnavailableReason = v1alpha1.VerificationUnavailable, v1alpha1.VerificationUnavailablePreviewNotLive
		return nil
	})
}
