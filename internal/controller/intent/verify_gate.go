// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The verification gate: what must hold before the intent reconciler asks
// preview-controller to run the approved acceptance criteria against the
// intent's Preview (a PreviewVerification), and the pure pieces both the
// gate and the loop (verify_loop.go) share: the head set and the bounds.

// verifyInterval spaces verifications: an automatic one while automatic
// fixes are halted (a human pushing repeatedly is not re-verified on every
// push), and the runs of approvers' requests (one per interval per intent).
const verifyInterval = 5 * time.Minute

// previewWaitMax bounds how long a due verification waits for the Preview
// to be live at exactly the heads (WaitingForPreview). A Preview that is
// Failed, expired or never served (no preview-controller) would otherwise
// hold maul/acceptance pending for ever. It is long enough for a queued
// Preview to get a slot and for a few of preview-controller's rollout
// attempts (--preview-rollout-timeout, 10 minutes by default); past it the
// verification is Unavailable (PreviewNotLive) until the Preview is live.
const previewWaitMax = time.Hour

// verificationRetention is how many of an Intent's PreviewVerifications are
// kept: the latest few carry the evidence a fix round or a comment may
// still read; the rest are deleted (and all of them with the Intent).
const verificationRetention = 3

// annotationVerificationTrigger records on a PreviewVerification whether an
// approver's request started it (verificationByRequest) or the loop did:
// the spacing of requests is read back from it.
const annotationVerificationTrigger = "maul.run/verification-trigger"

const (
	verificationByRequest   = "request"
	verificationByAutomatic = "automatic"
)

// headSet is the combined change a verification is about: every previewed
// component of the Intent's Preview and the revision it runs, with their
// digest.
type headSet struct {
	digest string
	heads  []v1alpha1.VerificationHead
}

// headSetDigest is "sha256:" and the hex sha256 of the JSON list of the
// heads' [repository, revision] pairs, sorted: it binds the combined change
// and nothing of the order or the component names it was listed under.
func headSetDigest(heads []v1alpha1.VerificationHead) string {
	pairs := make([][2]string, 0, len(heads))
	for _, h := range heads {
		pairs = append(pairs, [2]string{h.Repository, h.Revision})
	}
	slices.SortFunc(pairs, func(a, b [2]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return strings.Compare(a[1], b[1])
	})
	b, err := json.Marshal(pairs)
	if err != nil {
		// Unreachable: a list of string pairs always encodes.
		b = []byte("!unencodable")
	}
	return digest(b)
}

// verifies reports whether this pass's Intent is verified: its Project is
// (Settings.verifiesProject, the predicate the plan-time planAsk shares),
// and its tool host can verify it (toolHost.verifies: for the slot, the
// Intent's Preview is verification-capable).
func (p *pass) verifies() bool {
	return p.set.verifiesProject(p.proj) && p.toolHost().verifies(p)
}

// verifyKind is why a verification is due.
type verifyKind int

const (
	verifyNotDue verifyKind = iota
	// verifyAutomatic: the head set has no verification yet.
	verifyAutomatic
	// verifyRetry: the latest attempt ended without a judgement (maul's own
	// failure, a stale request, a vanished object): its round's next
	// attempt.
	verifyRetry
	// verifyRequest: an approver's /maul verify.
	verifyRequest
	// verifyRequestRefused: an approver's /maul verify with the Project's
	// maxVerifications spent: a request bypasses the interval, never the
	// cap, so it is consumed without a run.
	verifyRequestRefused
)

// verifyDue is why a verification is due at the recorded head set, if it
// is: a retry of an unjudged attempt; else an approver's request, once the
// latest attempt settled and none ran by request within verifyInterval;
// else, for a head set never verified, an automatic one within the bounds
// (verifyBounded). The bounds and the spacing are read from v and from the
// Intent's PreviewVerifications (lastRequestAt).
func (p *pass) verifyDue(ctx context.Context, v *v1alpha1.IntentVerification) (verifyKind, error) {
	switch v.State {
	case v1alpha1.VerificationNeedsReplan, v1alpha1.VerificationOff, v1alpha1.VerificationNotDeclared:
		return verifyNotDue, nil
	}
	if v.Ref != nil && (v.State == v1alpha1.VerificationVerifying || v.State == v1alpha1.VerificationEvaluating) {
		return verifyNotDue, nil // in flight
	}
	if v.Ref == nil && v.Attempt > 0 {
		return verifyRetry, nil
	}
	if v.Requested != nil {
		if v.Automatic >= p.proj.Spec.Limits.MaxVerificationsLimit() {
			return verifyRequestRefused, nil
		}
		last, err := p.lastRequestAt(ctx)
		if err != nil || (!last.IsZero() && p.now.Sub(last) < verifyInterval) {
			return verifyNotDue, err
		}
		return verifyRequest, nil
	}
	if v.Ref != nil || !p.verifyBounded(v) {
		return verifyNotDue, nil
	}
	return verifyAutomatic, nil
}

// verifyBounded reports whether an automatic verification is within the
// bounds (critique fix 13): fewer than the Project's maxVerifications this
// plan revision, and, while automatic fixes are halted, verifyInterval since
// the last.
func (p *pass) verifyBounded(v *v1alpha1.IntentVerification) bool {
	if v.Automatic >= p.proj.Spec.Limits.MaxVerificationsLimit() {
		return false
	}
	if v.AutoFix.State == v1alpha1.AutoFixHalted && v.LastAutomaticAt != nil &&
		p.now.Sub(v.LastAutomaticAt.Time) < verifyInterval {
		return false
	}
	return true
}

// lastRequestAt is when the newest PreviewVerification an approver's
// request started was created, zero when none exists.
func (p *pass) lastRequestAt(ctx context.Context) (time.Time, error) {
	pvs, err := p.ownVerifications(ctx)
	if err != nil {
		return time.Time{}, err
	}
	var last time.Time
	for _, pv := range pvs {
		if pv.Annotations[annotationVerificationTrigger] == verificationByRequest &&
			pv.CreationTimestamp.After(last) {
			last = pv.CreationTimestamp.Time
		}
	}
	return last, nil
}

// checksGreen reports whether every open pull request of the Intent has the
// Project's named checks (checks.fix) completed and passed at its head.
// With none named there is nothing to wait for. A pull request whose
// repository left the Project is not maul's to verify and holds nothing.
func (p *pass) checksGreen(ctx context.Context, prs []v1alpha1.IntentPullRequest) (bool, error) {
	if len(p.proj.Spec.Checks.Fix) == 0 {
		return true, nil
	}
	for _, pr := range prs {
		if pr.State != prOpen || p.leftProject(pr.Repository) {
			continue
		}
		if pr.HeadSHA == "" {
			return false, nil
		}
		failed, settled, err := p.failedNamedChecks(ctx, pr.Repository, pr.HeadSHA)
		if err != nil {
			return false, fmt.Errorf("read the checks of pull request %d: %w", pr.Number, err)
		}
		if !settled || len(failed.checkIDs) > 0 || len(failed.statusIDs) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// newRunToken draws a verification attempt's random run token: six
// lowercase hex characters.
func newRunToken() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("draw a verification run token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ownVerifications are the Intent's PreviewVerifications, read uncached:
// those carrying its label and controlled by it.
func (p *pass) ownVerifications(ctx context.Context) ([]v1alpha1.PreviewVerification, error) {
	var list v1alpha1.PreviewVerificationList
	if err := p.r.APIReader.List(ctx, &list, client.InNamespace(p.in.Namespace),
		client.MatchingLabels{v1alpha1.LabelIntent: p.in.Name}); err != nil {
		return nil, fmt.Errorf("list the intent's verifications: %w", err)
	}
	out := list.Items[:0]
	for _, pv := range list.Items {
		if controlledBy(pv.OwnerReferences, p.in.UID) {
			out = append(out, pv)
		}
	}
	return out, nil
}
