// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"encoding/json"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
)

// subject is what a request was re-checked against.
type subject struct {
	intent     *v1alpha1.Intent
	repository v1alpha1.ProjectRepository
	head, base *v1alpha1.Repository
}

// check re-checks pv against its Intent, Project and trees, read uncached,
// and returns why it is stale: stale-approval for an Intent, Project,
// surface, tree or approved outcomes that no longer hold, stale-preview for
// a pull request no longer open at the workspace head. Such a verification
// is dropped unjudged.
func (r *Reconciler) check(ctx context.Context, pv *v1alpha1.PreviewVerification) (*subject,
	v1alpha1.PreviewVerificationReason, error) {
	const staleApproval = v1alpha1.VerificationReasonStaleApproval
	ws := pv.Spec.Workspace
	if ws == nil {
		return nil, staleApproval, nil
	}
	s := &subject{intent: &v1alpha1.Intent{}}
	if gone, err := r.get(ctx, pv.Namespace, pv.Spec.IntentRef.Name, s.intent); err != nil || gone {
		return nil, staleApproval, err
	}
	if s.intent.UID != pv.Spec.IntentRef.UID || !controlledBy(pv.OwnerReferences, s.intent.UID) {
		return nil, staleApproval, nil
	}
	var proj v1alpha1.Project
	if gone, err := r.get(ctx, pv.Namespace, s.intent.Spec.Project, &proj); err != nil || gone {
		return nil, staleApproval, err
	}
	repo, ok := surfaceRepository(&proj, ws.Repository)
	if !ok {
		return nil, staleApproval, nil
	}
	s.repository = repo
	if !openAt(s.intent, repo.URL, ws.HeadSHA) {
		return nil, v1alpha1.VerificationReasonStalePreview, nil
	}
	if !approvedOutcomes(pv, s.intent) {
		return nil, staleApproval, nil
	}
	var err error
	if s.head, err = r.tree(ctx, pv, s.intent, ws.HeadRepositoryRef, repo.URL, ws.HeadSHA); err != nil ||
		s.head == nil {
		return nil, staleApproval, err
	}
	if s.base, err = r.tree(ctx, pv, s.intent, ws.BaseRepositoryRef, repo.URL, ws.BaseSHA); err != nil ||
		s.base == nil {
		return nil, staleApproval, err
	}
	return s, "", nil
}

// get reads a release-namespace object uncached, reporting one that is
// gone.
func (r *Reconciler) get(ctx context.Context, namespace, name string, obj client.Object) (bool, error) {
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj)
	if kerrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

// tree is the commit-pinned Repository ref names, nil when it no longer
// holds: the very object (its UID), the Intent's, of repoURL pinned to sha,
// Ready at exactly sha with its artifact.
func (r *Reconciler) tree(ctx context.Context, pv *v1alpha1.PreviewVerification, in *v1alpha1.Intent,
	ref v1alpha1.ObjectReference, repoURL, sha string) (*v1alpha1.Repository, error) {
	var repo v1alpha1.Repository
	if gone, err := r.get(ctx, pv.Namespace, ref.Name, &repo); err != nil || gone {
		return nil, err
	}
	a := repo.Status.Artifact
	if repo.UID != ref.UID || !controlledBy(repo.OwnerReferences, in.UID) || repo.Spec.Ref.Commit != sha ||
		!sameRepo(repo.Spec.URL, repoURL) || repo.Status.ResolvedSHA != sha || a == nil || a.Digest == "" ||
		a.URL == "" || !meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) ||
		!repo.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	return &repo, nil
}

// surfaceRepository is Project p's repository key names, when p still
// declares a cli or library surface that builds it (the one its surface
// names, or its only one when it names none).
func surfaceRepository(p *v1alpha1.Project, key string) (v1alpha1.ProjectRepository, bool) {
	sf := p.Spec.Surfaces
	if !sf.Agents() {
		return v1alpha1.ProjectRepository{}, false
	}
	want := ""
	if sf.CLI != nil {
		want = sf.CLI.Repository
	} else {
		want = sf.Library.Repository
	}
	for _, r := range p.Spec.Repositories {
		if r.Name == key && (want == r.Name || want == "" && len(p.Spec.Repositories) == 1) {
			return r, true
		}
	}
	return v1alpha1.ProjectRepository{}, false
}

// openAt reports in's pull request in repoURL open at head.
func openAt(in *v1alpha1.Intent, repoURL, head string) bool {
	for _, pr := range in.Status.PullRequests {
		if pr.State == "open" && sameRepo(pr.Repository, repoURL) && pr.HeadSHA == head {
			return true
		}
	}
	return false
}

// controlledBy reports whether refs name uid as the controller.
func controlledBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.UID == uid {
			return true
		}
	}
	return false
}

// sameRepo compares two repository URLs as intent-controller does: case,
// a trailing slash and a .git suffix aside.
func sameRepo(a, b string) bool { return normalizeRepoURL(a) == normalizeRepoURL(b) }

func normalizeRepoURL(u string) string {
	u = strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/"))
	return strings.TrimSuffix(u, ".git")
}

// approvedOutcomes holds pv to its Intent as preview-controller holds a
// slot outcome verification: exactly the approved outcomes under their
// digest; its saved checks under the check store's digest (none before a
// first check is saved), each parsing, its id, outcome, form and digest its
// body's, within the bound, bound to an approved outcome; and its tools.
func approvedOutcomes(pv *v1alpha1.PreviewVerification, in *v1alpha1.Intent) bool {
	a := in.Status.Acceptance
	if a == nil || len(a.Outcomes) == 0 || len(pv.Spec.Outcomes) == 0 || pv.Spec.Tools == nil {
		return false
	}
	approved, ok := planOutcomes(a.Outcomes)
	requested, ok2 := planOutcomes(pv.Spec.Outcomes)
	digest := acceptance.OutcomesDigest(approved)
	if !ok || !ok2 || digest != a.OutcomesDigest || digest != pv.Spec.OutcomesDigest ||
		acceptance.OutcomesDigest(requested) != digest {
		return false
	}
	if a.Checks == nil {
		return len(pv.Spec.Checks) == 0 && pv.Spec.ChecksDigest == ""
	}
	return pv.Spec.ChecksDigest == a.Checks.Digest && savedChecksValid(pv.Spec.Checks, a.Outcomes)
}

// savedChecksValid reports whether every saved check parses, its id,
// outcome, form and digest its body's, within the bound, bound to one of
// outcomes.
func savedChecksValid(saved []v1alpha1.SavedCheck, outcomes []v1alpha1.AcceptanceOutcome) bool {
	ids := map[string]bool{}
	for _, o := range outcomes {
		ids[o.ID] = true
	}
	for _, sc := range saved {
		c, err := checks.Parse([]byte(sc.Body))
		if err != nil || c.ID != sc.ID || c.Outcome != sc.Outcome || string(c.Form) != string(sc.Form) ||
			checks.Digest(c) != sc.Digest || len(sc.Body) > v1alpha1.MaxSavedCheckBytes || !ids[sc.Outcome] {
			return false
		}
	}
	return true
}

// planOutcomes converts stored outcomes to the plan grammar's.
func planOutcomes(os []v1alpha1.AcceptanceOutcome) ([]acceptance.PlanOutcome, bool) {
	b, err := json.Marshal(os)
	if err != nil {
		return nil, false
	}
	var out []acceptance.PlanOutcome
	return out, json.Unmarshal(b, &out) == nil
}
