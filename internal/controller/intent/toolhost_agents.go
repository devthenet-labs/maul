// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// agentsHost is the agents tool host (SPEC-S2): a Project with a cli or
// library surface and no Preview, evaluated through an agents-namespace
// tool pod that builds one repository's pull request head and its merge
// base with the PR's base branch (O3) in the repository's accepted runner
// image (O8), under the build's network profile. The intent reconciler only
// creates the round's PreviewVerification (placement agents, its
// workspace); intent-controller's agents host reconciler renders the pod
// and is the single writer of the verification's status (O1).
//
// The gate, once a round is due: every named check green; the build's
// accepted image (none, or one the schema would refuse, settles the round
// Unconfirmed: never a block); the merge base; and the head and base trees,
// two commit-pinned, Intent-owned Repositories, both Ready (else the round
// waits, at most treeWaitMax from a tree's creation, then is Unavailable;
// a commit GitHub cannot serve is Unavailable at once).
type agentsHost struct{}

// treeWaitMax bounds how long a due round waits for its trees to be Ready.
const treeWaitMax = 15 * time.Minute

func (agentsHost) placement() v1alpha1.VerificationPlacement { return v1alpha1.PlacementAgents }

// verifies: the surface names one of the Project's repositories.
func (agentsHost) verifies(p *pass) bool {
	_, ok := surfaceRepository(p.proj)
	return ok
}

// heads: the surface repository's open pull request at its head, keyed by
// the repository's key (Q30: one repository per tool pod). ok is false
// while it has none at a full commit.
func (agentsHost) heads(p *pass) (headSet, bool) {
	repo, ok := surfaceRepository(p.proj)
	if !ok {
		return headSet{}, false
	}
	pr, ok := p.surfacePullRequest(repo)
	if !ok {
		return headSet{}, false
	}
	heads := []v1alpha1.VerificationHead{{Component: repo.Name, Repository: repo.URL, Revision: pr.HeadSHA}}
	return headSet{digest: headSetDigest(heads), heads: heads}, true
}

// surfacePullRequest is the intent's open pull request in repo at a full
// commit; ok false when there is none.
func (p *pass) surfacePullRequest(repo v1alpha1.ProjectRepository) (v1alpha1.IntentPullRequest, bool) {
	for _, pr := range p.in.Status.PullRequests {
		if pr.State == prOpen && sameRepo(pr.Repository, repo.URL) && v1alpha1.ValidCommitSHA(pr.HeadSHA) {
			return pr, true
		}
	}
	return v1alpha1.IntentPullRequest{}, false
}

// surfaceRepository is the Project repository its cli or library surface
// builds: the one its key names, or the Project's only one when it names
// none. ok is false for a Project without such a surface or whose key names
// none of its repositories.
func surfaceRepository(p *v1alpha1.Project) (v1alpha1.ProjectRepository, bool) {
	sf := p.Spec.Surfaces
	if !sf.Agents() {
		return v1alpha1.ProjectRepository{}, false
	}
	key := ""
	if sf.CLI != nil {
		key = sf.CLI.Repository
	} else {
		key = sf.Library.Repository
	}
	if key == "" {
		if len(p.Spec.Repositories) != 1 {
			return v1alpha1.ProjectRepository{}, false
		}
		return p.Spec.Repositories[0], true
	}
	i := slices.IndexFunc(p.Spec.Repositories, func(r v1alpha1.ProjectRepository) bool { return r.Name == key })
	if i < 0 {
		return v1alpha1.ProjectRepository{}, false
	}
	return p.Spec.Repositories[i], true
}

// gate is the agents gate (agentsHost's doc): checks green, the image, the
// merge base and both trees Ready, then place fills the workspace.
func (agentsHost) gate(ctx context.Context, p *pass, v *v1alpha1.IntentVerification,
	prs []v1alpha1.IntentPullRequest) (func(*v1alpha1.PreviewVerificationSpec), bool, error) {
	green, err := p.checksGreen(ctx, prs)
	if err != nil || !green {
		if err != nil {
			return nil, false, err
		}
		changed, err := p.waitVerification(ctx, v1alpha1.VerificationWaitingForCI)
		return nil, changed, err
	}
	repo, _ := surfaceRepository(p.proj)
	pr, ok := p.surfacePullRequest(repo)
	if !ok {
		return nil, false, nil
	}
	img, why, err := p.sandboxImage(ctx, repo.URL)
	if err != nil {
		return nil, false, err
	}
	if why != "" {
		changed, err := p.agentsUnconfirmed(ctx, v1alpha1.VerificationReasonSandboxImageUnavailable, why)
		return nil, changed, err
	}
	base, why, err := p.mergeBase(ctx, repo.URL, pr.HeadSHA)
	if err != nil {
		return nil, false, err
	}
	if why != "" {
		changed, err := p.treesUnavailable(ctx, why)
		return nil, changed, err
	}
	head, baseTree, why, err := p.workspaceTrees(ctx, repo.URL, pr.HeadSHA, base)
	switch {
	case err != nil:
		return nil, false, err
	case why != "":
		changed, err := p.treesUnavailable(ctx, why)
		return nil, changed, err
	case head == nil || baseTree == nil:
		return nil, false, nil // a tree is not Ready yet
	}
	p.pruneTrees(ctx, head.Name, baseTree.Name)
	ws := &v1alpha1.VerificationWorkspace{
		Repository: repo.Name, HeadSHA: pr.HeadSHA, BaseSHA: base,
		HeadRepositoryRef: v1alpha1.ObjectReference{Name: head.Name, UID: head.UID},
		BaseRepositoryRef: v1alpha1.ObjectReference{Name: baseTree.Name, UID: baseTree.UID},
		SandboxImage:      img.image, SandboxSearchPath: img.searchPath, NetworkProfile: img.profile,
		Surface: p.workspaceSurface(),
	}
	evaluate := msDuration(p.set.grant(p.proj, v1alpha1.IntentStageEvaluate).TimeoutMilliseconds)
	timeout := min(evaluate+p.set.toolBuildTimeout(p.proj)+EvaluationToolsMargin,
		v1alpha1.MaxAgentsVerificationTimeout)
	return func(spec *v1alpha1.PreviewVerificationSpec) {
		spec.Placement, spec.Workspace = v1alpha1.PlacementAgents, ws
		// No browser and no look-around: there is nothing to browse.
		spec.Tools.Browser, spec.Tools.LookAroundRoutes = false, 0
		spec.Timeout = metav1.Duration{Duration: max(timeout, minVerificationTimeout)}
	}, false, nil
}

// workspaceSurface is the Project's surface as one verification resolves
// it: a cli's build and binary with its build timeout applied, or a
// library's language.
func (p *pass) workspaceSurface() v1alpha1.WorkspaceSurface {
	sf := p.proj.Spec.Surfaces
	if sf.CLI != nil {
		return v1alpha1.WorkspaceSurface{CLI: &v1alpha1.WorkspaceCLI{Build: slices.Clone(sf.CLI.Build),
			Binary: sf.CLI.Binary, BuildTimeout: metav1.Duration{Duration: p.set.toolBuildTimeout(p.proj)}}}
	}
	return v1alpha1.WorkspaceSurface{Library: &v1alpha1.WorkspaceLibrary{Language: sf.Library.Language}}
}

// sandboxPin is the image an agents round's sandboxes run and the network
// profile its tool pod gets: the build's.
type sandboxPin struct {
	image, searchPath, profile string
}

// The workspace fields' schema patterns, repeated so a value the API server
// would refuse settles the round instead of failing its create.
var (
	sandboxImagePattern = regexp.MustCompile(`^[a-z0-9][^@\s]*@sha256:[0-9a-f]{64}$`)
	searchPathPattern   = regexp.MustCompile(`^/[^:\x00-\x1f]*(:/[^:\x00-\x1f]*)*$`)
)

// maxSandboxField bounds sandboxImage and sandboxSearchPath (their schema's
// maxLength).
const (
	maxSandboxImage      = 512
	maxSandboxSearchPath = 4096
)

// sandboxImage is the accepted runner image of repoURL's latest build, with
// its search path and the operator network profile a build gets, through
// the very calls a build launch makes (PinFor on the build's own
// Repository, the default branch's tree it was built on, then
// requestProfile on a build Job's phase and kind): the image and the
// network are never another's than the build's (O8, 7.8). why says why
// there is none to run, or why the one there cannot be written: the round
// is then Unconfirmed. An image is never read from the pull request's head.
func (p *pass) sandboxImage(ctx context.Context, repoURL string) (sandboxPin, string, error) {
	repo, err := p.buildRepository(ctx, repoURL)
	if err != nil || repo == nil {
		if err == nil {
			return sandboxPin{}, "no build of " + repoURL + " pinned a runner image", nil
		}
		return sandboxPin{}, "", err
	}
	spec := jobs.Spec{Phase: jobPhase[v1alpha1.IntentStageBuild], Kind: KindIntent}
	if skipped := p.r.Images.PinFor(&spec, repo); skipped != "" {
		return sandboxPin{}, skipped, nil
	}
	switch {
	case len(spec.RunnerImage) > maxSandboxImage || !sandboxImagePattern.MatchString(spec.RunnerImage):
		return sandboxPin{}, fmt.Sprintf("the accepted runner image %q is not a digest reference", spec.RunnerImage),
			nil
	case len(spec.RunnerSearchPath) > maxSandboxSearchPath || !searchPathPattern.MatchString(spec.RunnerSearchPath):
		return sandboxPin{}, "the accepted runner image's search path holds entries a sandbox cannot use", nil
	}
	profile, skipped := requestProfile(p.r.Images, &spec, repoURL)
	if skipped != "" {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "the agents tool pod gets no network profile, as the build does not",
			slog.String("intent", p.in.Name), slog.String("reason", skipped))
	}
	return sandboxPin{image: spec.RunnerImage, searchPath: spec.RunnerSearchPath, profile: profile}, "", nil
}

// buildRepository is the Repository the latest build run of repoURL was
// built on, the intent's runner-image anchor (deletePlanRepository keeps
// it); nil when no build of it has one, or one this Intent's run does not
// own.
func (p *pass) buildRepository(ctx context.Context, repoURL string) (*v1alpha1.Repository, error) {
	for _, run := range slices.Backward(p.runs) {
		if run.Spec.Stage != v1alpha1.IntentStageBuild || !sameRepo(run.Spec.Repository.URL, repoURL) {
			continue
		}
		var repo v1alpha1.Repository
		key := types.NamespacedName{Namespace: p.in.Namespace, Name: run.Spec.Repository.RepositoryRef.Name}
		switch err := p.r.Get(ctx, key, &repo); {
		case kerrors.IsNotFound(err):
			return nil, nil
		case err != nil:
			return nil, fmt.Errorf("read the build's repository %s: %w", key.Name, err)
		}
		if !controlledBy(repo.OwnerReferences, run.UID) {
			return nil, nil
		}
		return &repo, nil
	}
	return nil, nil
}

// mergeBase is the merge base of head and repoURL's default branch, the
// branch an intent's pull request merges into (O3), read when a round is
// created and recorded on it. A refusal or a missing commit is why, for
// the round to be Unavailable; any other failure an error.
func (p *pass) mergeBase(ctx context.Context, repoURL, head string) (string, string, error) {
	branch, err := p.r.GitHub.DefaultBranch(ctx, repoURL)
	if err == nil {
		var base string
		if base, err = p.r.GitHub.MergeBase(ctx, repoURL, branch, head); err == nil {
			return base, "", nil
		}
	}
	if ghclient.IsNotFound(err) || ghclient.IsRefused(err) {
		return "", fmt.Sprintf("the merge base of %s in %s cannot be read: %v", head, repoURL, err), nil
	}
	return "", "", fmt.Errorf("read the merge base of %s: %w", head, err)
}

// treeName is the name of the intent's commit-pinned Repository of sha.
func treeName(intent, sha string) string { return intent + "-tree-" + sha }

// workspaceTrees ensures the head and base trees exist, each a
// commit-pinned Repository of repoURL owned by the Intent, and returns both
// once Ready at exactly their commits (a nil one is not yet). why says why
// a tree never will be: its commit is not found, or it has waited past
// treeWaitMax.
func (p *pass) workspaceTrees(ctx context.Context, repoURL, head, base string) (
	*v1alpha1.Repository, *v1alpha1.Repository, string, error) {
	var out [2]*v1alpha1.Repository
	for i, sha := range []string{head, base} {
		repo, err := p.ensureTree(ctx, repoURL, sha)
		if err != nil {
			return nil, nil, "", err
		}
		ready, why := p.treeReady(repo, sha)
		if why != "" {
			return nil, nil, why, nil
		}
		if ready {
			out[i] = repo
		}
	}
	return out[0], out[1], "", nil
}

// ensureTree reads, or creates, the Intent's commit Repository of sha. One
// under its name that is not the Intent's, or not of repoURL at sha, is an
// error: it is never used.
func (p *pass) ensureTree(ctx context.Context, repoURL, sha string) (*v1alpha1.Repository, error) {
	key := types.NamespacedName{Namespace: p.in.Namespace, Name: treeName(p.in.Name, sha)}
	var repo v1alpha1.Repository
	err := p.r.Get(ctx, key, &repo)
	if kerrors.IsNotFound(err) {
		repo = v1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
				// Never LabelFinding: the Finding flow's mappers ignore it.
				Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name},
				OwnerReferences: []metav1.OwnerReference{intentOwner(p.in)},
			},
			Spec: v1alpha1.RepositorySpec{URL: repoURL, Ref: v1alpha1.RepositoryRef{Commit: sha}},
		}
		if err = p.r.Create(ctx, &repo); kerrors.IsAlreadyExists(err) {
			err = p.r.APIReader.Get(ctx, key, &repo)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("tree %s: %w", key.Name, err)
	}
	if !controlledBy(repo.OwnerReferences, p.in.UID) || repo.Spec.Ref.Commit != sha ||
		!sameRepo(repo.Spec.URL, repoURL) {
		return nil, fmt.Errorf("tree %s: %w", key.Name, errNotOwned)
	}
	return &repo, nil
}

// treeReady reports a tree Ready at exactly sha with its artifact, or why it
// never will be: source-controller found no such commit (CommitNotFound,
// which also covers a credential that cannot see the repository), or it is
// still not Ready treeWaitMax after it was created.
func (p *pass) treeReady(repo *v1alpha1.Repository, sha string) (bool, string) {
	if c := meta.FindStatusCondition(repo.Status.Conditions, v1alpha1.ConditionStalled); c != nil &&
		c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ReasonCommitNotFound {
		return false, fmt.Sprintf("commit %s of %s does not exist or is not visible to maul's credential", sha,
			repo.Spec.URL)
	}
	if meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) &&
		repo.Status.ResolvedSHA == sha && repo.Status.Artifact != nil && repo.Status.Artifact.Digest != "" {
		return true, ""
	}
	if created := repo.CreationTimestamp.Time; !created.IsZero() && p.now.Sub(created) >= treeWaitMax {
		return false, fmt.Sprintf("the tree of %s at %s was not ready within %s", repo.Spec.URL, sha, treeWaitMax)
	}
	return false, ""
}

// pruneTrees deletes the Intent's commit Repositories that neither the
// round about to be created (keep) nor any PreviewVerification kept
// (verificationRetention) names. Best effort: the Intent's deletion takes
// any left.
func (p *pass) pruneTrees(ctx context.Context, keep ...string) {
	pvs, err := p.ownVerifications(ctx)
	if err != nil {
		return
	}
	for _, pv := range pvs {
		if ws := pv.Spec.Workspace; ws != nil {
			keep = append(keep, ws.HeadRepositoryRef.Name, ws.BaseRepositoryRef.Name)
		}
	}
	var list v1alpha1.RepositoryList
	if err := p.r.APIReader.List(ctx, &list, client.InNamespace(p.in.Namespace),
		client.MatchingLabels{v1alpha1.LabelIntent: p.in.Name}); err != nil {
		return
	}
	for i := range list.Items {
		repo := &list.Items[i]
		if repo.Spec.Ref.Commit == "" || !controlledBy(repo.OwnerReferences, p.in.UID) ||
			slices.Contains(keep, repo.Name) || !repo.DeletionTimestamp.IsZero() {
			continue
		}
		uid := repo.UID
		if err := p.r.Delete(ctx, repo, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err !=
			nil && !kerrors.IsNotFound(err) {
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "delete an old workspace tree", slog.String("intent", p.in.Name),
				slog.String("repository", repo.Name), slog.Any("error", err))
		}
	}
}

// agentsUnconfirmed settles a due agents round that cannot run at all
// (reason, a PreviewVerification reason; why, what is missing):
// Unconfirmed, any request consumed, never a block (O8), recording why
// (SandboxImageUnavailable, which the hand-off says). Written once; a later
// pass that finds what was missing runs the round.
func (p *pass) agentsUnconfirmed(ctx context.Context, reason v1alpha1.PreviewVerificationReason,
	why string) (bool, error) {
	v := p.in.Status.Verification
	if v.Ref != nil || v.State == v1alpha1.VerificationUnconfirmed && v.Requested == nil &&
		v.UnavailableReason == v1alpha1.VerificationUnavailableSandboxImage {
		return false, nil
	}
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "the agents round cannot run; the verification is unconfirmed",
		slog.String("intent", p.in.Name), slog.String("reason", string(reason)), slog.String("detail", why))
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.UnavailableReason, nv.Requested = v1alpha1.VerificationUnconfirmed,
			v1alpha1.VerificationUnavailableSandboxImage, nil
		return nil
	})
}

// treesUnavailable settles a due agents round whose trees cannot be had
// (VerificationReasonTreeUnavailable; why, which tree and how):
// Unavailable for that reason (TreeUnavailable), any request consumed,
// until a later pass finds them Ready or the heads move. Written once.
func (p *pass) treesUnavailable(ctx context.Context, why string) (bool, error) {
	v := p.in.Status.Verification
	if v.Ref != nil || v.State == v1alpha1.VerificationUnavailable && v.Requested == nil &&
		v.UnavailableReason == v1alpha1.VerificationUnavailableTree {
		return false, nil
	}
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "the agents round's trees are unavailable; the verification is unavailable",
		slog.String("intent", p.in.Name), slog.String("reason", string(v1alpha1.VerificationReasonTreeUnavailable)),
		slog.String("detail", why))
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.UnavailableReason, nv.Requested = v1alpha1.VerificationUnavailable,
			v1alpha1.VerificationUnavailableTree, nil
		return nil
	})
}
