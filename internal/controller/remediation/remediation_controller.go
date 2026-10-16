// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentresult"
	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/changeset"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/report"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
	"github.com/bitwise-media-group/patchy/internal/schedule"
	"github.com/bitwise-media-group/patchy/internal/templates"
	"github.com/bitwise-media-group/patchy/internal/transcriptstore"
	"github.com/bitwise-media-group/patchy/internal/usagemeter"
)

// remSchedulerRequest is the singleton request every watch event maps to.
const remSchedulerRequest = "\x00scheduler"

// CRRunner is the slice of the jobs client the CRD-native remediation
// engine needs (distinct name: the legacy Runner interface lives in
// controller.go until the cutover).
type CRRunner interface {
	Create(ctx context.Context, spec jobs.Spec) (string, v1alpha1.RunnerImageRef, error)
	Result(ctx context.Context, jobName string) (jobs.RunOutput, error)
	Status(ctx context.Context, jobName string) (jobs.Status, error)
	Delete(ctx context.Context, jobName string) error
}

// ForgeWriter performs the two forge writes remediation owns: the branch
// push (Git Data API, scoped write token) and the pull request. It wraps
// internal/forge + internal/ghpush; tests substitute a fake.
type ForgeWriter interface {
	// Push replays the changeset as branch on the repository and returns
	// the SHA of the commit the branch now points at.
	Push(ctx context.Context, namespace, repoURL, branch string, cs *envelope.Changeset) (commit string, err error)
	// EnsurePR opens (or finds, idempotently by head branch) the pull
	// request for branch.
	EnsurePR(ctx context.Context, namespace, repoURL, branch, title, body string) (number int64, url string, err error)
}

// RemediationReconciler grants bounded slots to pending remediations in
// priority order, launches the agent Job, and applies results: push + PR on
// success, retry or exhaustion on failure.
type RemediationReconciler struct {
	client.Client
	// Runner creates and observes agent Jobs.
	Runner CRRunner
	// Forge performs the branch push and PR creation.
	Forge ForgeWriter
	// Namespace the CRs live in.
	Namespace string
	// MaxConcurrent bounds simultaneously running remediations (default 1).
	MaxConcurrent int
	// Paused stops new agent Jobs (the --agents-paused switch): the
	// scheduler grants no slot and a run granted before the pause took
	// effect does not launch, while running Jobs are still collected.
	Paused bool
	// MaxAttempts bounds remediation attempts per finding.
	MaxAttempts int32
	// Aging lifts long-waiting remediations.
	Aging schedule.AgingPolicy
	// Enabled is the set of harness ids this deployment can run. launch
	// re-checks the spawner-resolved harness against it, so an enabled-set
	// change between spawn and launch fails the run cleanly instead of
	// creating an unrunnable Job.
	Enabled []string
	// Images decides whether a launch runs the Repository's pinned
	// repository-declared image (the --repository-images kill switch and
	// the sandbox breaker); the zero value never does.
	Images runnerguard.Guard
	// MaxChangesetEntries caps upserts plus deletes in a changeset held to
	// the repository-image rules, before any forge call is made; <= 0 means
	// changeset.DefaultMaxEntries.
	MaxChangesetEntries int
	// Attribution is --attribution-mode: under attribute every Job carries
	// the attributed-to annotation of its Remediation's spec.attributedTo
	// ("org" for one stamped before the mode was on); under off (the zero
	// value) none.
	Attribution attribution.Mode
	// Meter settles a finished run's usage against the egress broker
	// (--usage-measurement); nil, the default, records nothing.
	Meter *usagemeter.Meter
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
	// Log receives diagnostics; nil discards.
	Log *slog.Logger
}

// Reconcile handles the singleton scheduler request and per-object runs.
func (r *RemediationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name == remSchedulerRequest {
		return r.schedule(ctx)
	}
	return r.run(ctx, req)
}

// schedule grants free slots to the highest-priority pending remediations.
func (r *RemediationReconciler) schedule(ctx context.Context) (ctrl.Result, error) {
	var list v1alpha1.RemediationList
	if err := r.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	running := 0
	var pending []schedule.Candidate
	for i := range list.Items {
		rem := &list.Items[i]
		switch rem.Status.Phase {
		case v1alpha1.RunRunning:
			running++
		case v1alpha1.RunPending, "":
			if !rem.DeletionTimestamp.IsZero() {
				continue
			}
			pending = append(pending, schedule.Candidate{
				Name:      rem.Name,
				Priority:  rem.Spec.Priority,
				QueuedAt:  rem.CreationTimestamp.Time,
				Expedited: r.expedited(ctx, rem.Namespace, rem.Spec.FindingRef.Name),
			})
		}
	}
	marked, err := r.markPaused(ctx, list.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	if r.Paused {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	if marked {
		// The pause just lifted and this pass rewrote runs' status. Granting
		// now would re-read them through a cache that may not have seen
		// those writes yet and fail the pass on a conflict, so leave the
		// grants to the pass their update events (or this requeue) trigger.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	maxConcurrent := r.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	for _, name := range schedule.Pick(pending, maxConcurrent-running, r.now(), r.Aging) {
		if err := r.grant(ctx, name); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// markPaused keeps the AgentsPaused condition true on every remediation the
// pause holds and removes it from every other, writing only a run whose
// condition changes: while agents are not paused, a run that never carried
// it is never written. It reports whether it wrote any run.
func (r *RemediationReconciler) markPaused(ctx context.Context, items []v1alpha1.Remediation) (bool, error) {
	marked := false
	for i := range items {
		rem := &items[i]
		if !rem.DeletionTimestamp.IsZero() {
			continue
		}
		conds := slices.Clone(rem.Status.Conditions)
		held := v1alpha1.HeldByPause(r.Paused, rem.Status.Phase, rem.Status.JobRef)
		if !v1alpha1.SyncAgentsPaused(&conds, held, r.now()) {
			continue
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cur v1alpha1.Remediation
			if err := r.Get(ctx, client.ObjectKeyFromObject(rem), &cur); err != nil {
				return client.IgnoreNotFound(err)
			}
			held = v1alpha1.HeldByPause(r.Paused, cur.Status.Phase, cur.Status.JobRef)
			if !cur.DeletionTimestamp.IsZero() || !v1alpha1.SyncAgentsPaused(&cur.Status.Conditions, held, r.now()) {
				return nil
			}
			marked = true
			return r.Status().Update(ctx, &cur)
		})
		if err != nil {
			return marked, err
		}
	}
	return marked, nil
}

// expedited reads the parent finding's expedite mark (cached client; a miss
// simply ranks the run normally).
func (r *RemediationReconciler) expedited(ctx context.Context, namespace, finding string) bool {
	if finding == "" {
		return false
	}
	var fnd v1alpha1.Finding
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: finding}, &fnd); err != nil {
		return false
	}
	return fnd.Spec.Expedite != nil
}

// grant moves one remediation to Running and its finding to Remediating
// (edge 11).
func (r *RemediationReconciler) grant(ctx context.Context, name string) error {
	var findingName string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var rem v1alpha1.Remediation
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, &rem); err != nil {
			return client.IgnoreNotFound(err)
		}
		if rem.Status.Phase != "" && rem.Status.Phase != v1alpha1.RunPending {
			return nil
		}
		now := metav1.NewTime(r.now())
		rem.Status.Phase = v1alpha1.RunRunning
		rem.Status.GrantedAt = &now
		findingName = rem.Spec.FindingRef.Name
		return r.Status().Update(ctx, &rem)
	})
	if err != nil || findingName == "" {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fnd v1alpha1.Finding
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: findingName}, &fnd); err != nil {
			return client.IgnoreNotFound(err)
		}
		if fnd.Status.Phase != v1alpha1.PhaseQueued {
			return nil
		}
		if err := v1alpha1.SetPhase(&fnd, v1alpha1.PhaseRemediating, r.now()); err != nil {
			return err
		}
		fnd.Status.ActiveRun = &v1alpha1.ActiveRun{Kind: v1alpha1.RunKindRemediation, Name: name}
		return r.Status().Update(ctx, &fnd)
	})
}

// run drives one remediation object.
func (r *RemediationReconciler) run(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rem v1alpha1.Remediation
	if err := r.Get(ctx, req.NamespacedName, &rem); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rem.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &rem)
	}
	switch rem.Status.Phase {
	case v1alpha1.RunRunning:
		if rem.Status.JobRef == nil {
			if r.Paused {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, r.launch(ctx, &rem)
		}
		return r.collect(ctx, &rem)
	case v1alpha1.RunComplete, v1alpha1.RunFailed:
		return r.measure(ctx, &rem)
	default:
		return ctrl.Result{}, nil
	}
}

// measure settles a finished remediation's usage against the egress broker
// (usagemeter): a no-op while the meter is off or once the usage is final.
// The harness is the one the spawner resolved and this controller launched
// it on, never the pod's own report of it.
func (r *RemediationReconciler) measure(ctx context.Context, rem *v1alpha1.Remediation) (ctrl.Result, error) {
	if rem.Status.Stage == nil || !r.Meter.Pending(rem.Status.Phase, &rem.Status.Stage.Usage) {
		return ctrl.Result{}, nil
	}
	out, wait := r.Meter.Settle(ctx, usagemeter.RunOf(ctx, r.Runner, rem.UID, rem.Status.JobRef,
		rem.CreationTimestamp.Time, rem.Spec.Parameters.Harness))
	if out == nil {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Remediation
		if err := r.Get(ctx, client.ObjectKeyFromObject(rem), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		if cur.Status.Stage == nil || cur.Status.Stage.Usage.Final {
			return nil
		}
		usagemeter.Apply(&cur.Status.Stage.Usage, *out)
		return r.Status().Update(ctx, &cur)
	})
}

// launch creates the remediation agent Job (credential-less artifact flow;
// the investigation report rides along as the analysis input).
func (r *RemediationReconciler) launch(ctx context.Context, rem *v1alpha1.Remediation) error {
	var fnd v1alpha1.Finding
	fndKey := types.NamespacedName{Namespace: rem.Namespace, Name: rem.Spec.FindingRef.Name}
	if err := r.Get(ctx, fndKey, &fnd); err != nil {
		return client.IgnoreNotFound(err)
	}
	var repo v1alpha1.Repository
	repoKey := types.NamespacedName{Namespace: rem.Namespace, Name: rem.Spec.RepositoryRef.Name}
	if err := r.Get(ctx, repoKey, &repo); err != nil {
		return err
	}
	if repo.Status.Artifact == nil {
		return fmt.Errorf("repository %s has no artifact", repo.Name)
	}
	var inv v1alpha1.Investigation
	invKey := types.NamespacedName{Namespace: rem.Namespace, Name: rem.Spec.InvestigationRef.Name}
	if err := r.Get(ctx, invKey, &inv); err != nil {
		return err
	}
	handoff, err := templates.RenderFindingIssue(&fnd)
	if err != nil {
		return err
	}

	// The spawner resolved the model to a harness (and thus a runner image and
	// credential). Re-check it is still enabled; an enabled-set change between
	// spawn and launch fails the run cleanly rather than creating a Job for a
	// runner that no longer exists.
	harnessID := rem.Spec.Parameters.Harness
	if harnessID == "" || !slices.Contains(r.Enabled, harnessID) {
		return r.fail(ctx, rem, "aborted",
			fmt.Sprintf("harness %q for model %q is not enabled", harnessID, rem.Spec.Parameters.Model), nil, nil)
	}

	repoName := ""
	if fnd.Spec.Repository != nil {
		repoName = fnd.Spec.Repository.Name
	}
	spec := jobs.Spec{
		Repo:                  repoName,
		Attempt:               int(rem.Spec.Attempt),
		Phase:                 "remediate",
		Harness:               harnessID,
		Model:                 rem.Spec.Parameters.Model,
		BaseSHA:               repo.Status.ResolvedSHA,
		IssueMarkdown:         handoff,
		InvestigationMarkdown: inv.Status.Report,
		PreviousAttempt:       agentresult.EncodePreviousAttempt(rem.Spec.PreviousAttempt),
		Kind:                  string(v1alpha1.RunKindRemediation),
		Owner:                 rem.Name,
		Finding:               fnd.Name,
		ArtifactURL:           repo.Status.Artifact.URL,
		ArtifactDigest:        repo.Status.Artifact.Digest,
		// The grant the spawner resolved. The pod treats its own automated
		// budget as a floor beneath this, so a run is never starved by what
		// the investigation predicted.
		MaxTurns:     rem.Spec.Parameters.MaxTurns,
		TokenBudget:  rem.Spec.Parameters.TokenBudget,
		AttributedTo: r.Attribution.Annotation(rem.Spec.AttributedTo),
	}
	if skipped := r.Images.Pin(&spec, &repo, &fnd); skipped != "" {
		// The repository declares an accepted image this launch cannot run.
		// The default image carries none of its toolchain, so a run there
		// cannot build or test the fix: hand the finding to a human unless
		// one already accepted exactly that by approving an earlier hold.
		accepted, err := r.untestedAccepted(ctx, rem)
		if err != nil {
			return err
		}
		if !accepted {
			return r.holdForImage(ctx, rem, skipped)
		}
		r.log().LogAttrs(ctx, slog.LevelInfo, "not running the repository-declared runner image",
			slog.String("remediation", rem.Name), slog.String("reason", skipped))
	}
	// The operator's network profile for the repository, if it gets one:
	// only on the accepted image Pin just copied, so a revived finding
	// (default image) gets none. What the Job carries is read back below.
	if _, skipped := r.Images.Profile(&spec, repo.Spec.URL); skipped != "" {
		r.log().LogAttrs(ctx, slog.LevelInfo, "not applying a network profile",
			slog.String("remediation", rem.Name), slog.String("reason", skipped))
	}
	// A registry grant naming a Forge's host is refused before any Job
	// exists, and so is a profile Create refuses: the same launch would be
	// refused again, so the run fails rather than retrying forever.
	err = r.Images.CheckForges(ctx, rem.Namespace, &spec)
	var jobName string
	var image v1alpha1.RunnerImageRef
	if err == nil {
		jobName, image, err = r.Runner.Create(ctx, spec)
	}
	switch {
	case errors.Is(err, jobs.ErrNetworkProfileRefused):
		return r.fail(ctx, rem, outcomeLaunchRefused, "the agent Job's network profile was refused: "+err.Error(),
			nil, nil)
	case err != nil:
		return fmt.Errorf("launch remediation job: %w", err)
	}
	// The image is recorded from what Create returned, never from the
	// Repository's pin: the changeset validator applies its
	// repository-image rules on this stamp. So is the network profile.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Remediation
		if err := r.Get(ctx, client.ObjectKeyFromObject(rem), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		cur.Status.JobRef = &v1alpha1.JobReference{Name: jobName}
		cur.Status.RunnerImage = &image
		return r.Status().Update(ctx, &cur)
	})
}

// collect applies the result once the Job finishes.
func (r *RemediationReconciler) collect(ctx context.Context, rem *v1alpha1.Remediation) (ctrl.Result, error) {
	st, err := r.Runner.Status(ctx, rem.Status.JobRef.Name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return ctrl.Result{}, r.fail(ctx, rem, "aborted", "agent job vanished before reporting", nil, nil)
		}
		return ctrl.Result{}, err
	}
	// A repository-image Job's prepare init refused to hand over: there is
	// no log to read, and the cluster cannot sandbox a repository image.
	if runnerguard.SandboxRefused(st) {
		return ctrl.Result{}, r.sandboxRefused(ctx, rem)
	}
	if !st.Done {
		// A default-image Job waits for the Job watch, as it always has; a
		// repository-image one is looked at again while its pod may be stuck
		// pulling, which never mutates the Job.
		requeue, pullFailure := runnerguard.Pending(st, r.now())
		if pullFailure != "" {
			return ctrl.Result{}, r.pullFailed(ctx, rem, pullFailure)
		}
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	out, err := r.Runner.Result(ctx, rem.Status.JobRef.Name)
	if errors.Is(err, jobs.ErrLogsGone) {
		// No retry can read it (an evicted pod, a removed container): the
		// run fails as one with no result, naming why the agent stopped.
		r.log().LogAttrs(ctx, slog.LevelWarn, "the agent log is gone; the run fails without a result",
			slog.String("remediation", rem.Name), slog.Any("error", err))
		return ctrl.Result{}, r.fail(ctx, rem, "aborted", st.Explain(jobs.LogsGoneDetail), nil, nil)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	// Persist before applying: this is the only moment the conversation is
	// still readable, since the Job's TTL takes the pod and its log with it.
	// A transcript that cannot be stored is logged and dropped — losing the
	// explanation must never cost the result it explains.
	transcript, err := transcriptstore.Persist(ctx, r.Client, rem.Namespace,
		transcriptLabels(rem), rem, remGVK, out.Turns)
	if err != nil {
		log.FromContext(ctx).Error(err, "persist transcript", "remediation", rem.Name)
	}

	var result *envelope.Remediation
	for _, e := range out.Events {
		switch e.Type {
		case envelope.TypeRemediation:
			result = e.Remediation
		case envelope.TypeFatal:
			return ctrl.Result{}, r.fail(ctx, rem, "aborted", e.Error, stageOf(result), transcript)
		}
	}
	switch {
	case result == nil:
		return ctrl.Result{}, r.fail(ctx, rem, "aborted", "agent job produced no remediation event", nil, transcript)
	case result.Outcome != envelope.OutcomeOK:
		return ctrl.Result{}, r.fail(ctx, rem, string(result.Outcome), result.Detail, &result.Stage, transcript)
	case !result.Success:
		return ctrl.Result{}, r.handOff(ctx, rem, result, transcript)
	default:
		return ctrl.Result{}, r.succeed(ctx, rem, result, transcript)
	}
}

// sandboxRefused ends a run whose sandbox probe found NetworkPolicy
// unenforced: the breaker trips, so no later launch in this process runs
// the repository's image, and the finding goes back to the queue without
// this attempt counting against MaxAttempts — the run is marked
// SandboxRefused, which fail() subtracts. The next launch finds the breaker
// tripped and is held for a human (holdForImage) rather than run untested
// on the default image, so the retry cannot loop.
func (r *RemediationReconciler) sandboxRefused(ctx context.Context, rem *v1alpha1.Remediation) error {
	r.Images.Breaker.Trip(ctx, rem.Status.JobRef.Name, rem.Spec.FindingRef.Name)
	result := &envelope.Remediation{Stage: agentresult.FailedStage(nil, "aborted", runnerguard.SandboxReason)}
	refused := func(cur *v1alpha1.Remediation) {
		meta.SetStatusCondition(&cur.Status.Conditions, runnerguard.RefusedCondition(cur.Generation))
	}
	if err := r.stampChild(ctx, rem, result, v1alpha1.RunFailed, nil, refused); err != nil {
		return err
	}
	return r.finishFinding(ctx, rem, result, v1alpha1.PhaseQueued, runnerguard.SandboxReason)
}

// holdForImage ends a run before its launch: the repository declares an
// accepted agent image, this launch cannot run it (skipped is Pin's reason),
// and nobody has accepted an untested fix for the finding. The run is
// marked held (so its attempt is not counted and a later launch knows a
// human accepted the default image) and the finding is handed to a human
// (edge Remediating→HandedOff); approving it revives it into a run on the
// default image, whose pull request says it was not built or tested.
func (r *RemediationReconciler) holdForImage(ctx context.Context, rem *v1alpha1.Remediation, skipped string) error {
	detail := runnerguard.HeldDetail(skipped)
	result := &envelope.Remediation{Stage: agentresult.FailedStage(nil, v1alpha1.OutcomeDefaultImageHeld, detail)}
	held := func(cur *v1alpha1.Remediation) {
		meta.SetStatusCondition(&cur.Status.Conditions, runnerguard.HeldCondition(cur.Generation, skipped))
	}
	if err := r.stampChild(ctx, rem, result, v1alpha1.RunFailed, nil, held); err != nil {
		return err
	}
	r.log().LogAttrs(ctx, slog.LevelInfo, "held remediation instead of running the default image",
		slog.String("remediation", rem.Name), slog.String("reason", skipped))
	return r.finishFinding(ctx, rem, result, v1alpha1.PhaseHandedOff, detail)
}

// untestedAccepted reports whether a human has accepted an untested,
// default-image remediation of rem's finding: an earlier attempt of the
// same finding was held (holdForImage), and only an approval brings a
// handed-off finding back to the queue.
func (r *RemediationReconciler) untestedAccepted(ctx context.Context, rem *v1alpha1.Remediation) (bool, error) {
	var siblings v1alpha1.RemediationList
	if err := r.List(ctx, &siblings, client.InNamespace(rem.Namespace),
		client.MatchingLabels{v1alpha1.LabelFinding: rem.Spec.FindingRef.Name}); err != nil {
		return false, fmt.Errorf("look for held attempts: %w", err)
	}
	for i := range siblings.Items {
		sib := &siblings.Items[i]
		if sib.Spec.FindingRef.UID == rem.Spec.FindingRef.UID && sib.Spec.Attempt < rem.Spec.Attempt &&
			runnerguard.Held(sib.Status.Conditions) {
			return true, nil
		}
	}
	return false, nil
}

// pullFailed ends a run whose repository-declared image cannot be pulled
// and never will be, then deletes the Job so its pod stops retrying the
// registry until the deadline.
func (r *RemediationReconciler) pullFailed(ctx context.Context, rem *v1alpha1.Remediation, detail string) error {
	if err := r.fail(ctx, rem, "aborted", detail, nil, nil); err != nil {
		return err
	}
	if err := r.Runner.Delete(ctx, rem.Status.JobRef.Name); err != nil && !kerrors.IsNotFound(err) {
		r.log().LogAttrs(ctx, slog.LevelWarn, "delete agent job after pull failure",
			slog.String("job", rem.Status.JobRef.Name), slog.Any("error", err))
	}
	return nil
}

// remGVK identifies the owner reference a transcript carries.
var remGVK = metav1.TypeMeta{
	APIVersion: v1alpha1.GroupVersion.String(),
	Kind:       "Remediation",
}

// transcriptLabels key a transcript to its run, so an operator can find one
// from the finding without walking owner references.
func transcriptLabels(rem *v1alpha1.Remediation) map[string]string {
	return map[string]string{
		v1alpha1.LabelFinding: rem.Spec.FindingRef.Name,
		v1alpha1.LabelOwner:   rem.Name,
		v1alpha1.LabelRunKind: string(v1alpha1.RunKindRemediation),
		v1alpha1.LabelAttempt: strconv.Itoa(int(rem.Spec.Attempt)),
	}
}

// succeed pushes the changeset, opens the PR, and moves the finding to
// InReview (edge 13). The changeset never touches any CR.
func (r *RemediationReconciler) succeed(
	ctx context.Context, rem *v1alpha1.Remediation, result *envelope.Remediation,
	transcript *v1alpha1.TranscriptRef,
) error {
	var fnd v1alpha1.Finding
	fndKey := types.NamespacedName{Namespace: rem.Namespace, Name: rem.Spec.FindingRef.Name}
	if err := r.Get(ctx, fndKey, &fnd); err != nil {
		return client.IgnoreNotFound(err)
	}
	if fnd.Spec.Repository == nil || result.Changeset == nil {
		return r.fail(ctx, rem, "aborted", "success without changeset or repository", &result.Stage, transcript)
	}
	// Validate before any forge call: the changeset is the pod's output,
	// and on a repository-declared image the pod's process is the image's.
	rules, err := r.changesetRules(ctx, rem)
	if err != nil {
		return err
	}
	if err := changeset.Validate(result.Changeset, rules); err != nil {
		return r.fail(ctx, rem, string(envelope.OutcomeChangesetRejected), err.Error(), &result.Stage, transcript)
	}
	branch := "patchy/" + fnd.Name
	// The commit message is the agent's, and lands on the default branch as
	// plain text when the pull request is merged or rebased: defanged, it
	// closes, references and mentions nothing there.
	cs := *result.Changeset
	cs.CommitMessage = templates.RemediationCommitMessage(cs.CommitMessage)
	commit, err := r.Forge.Push(ctx, rem.Namespace, fnd.Spec.Repository.URL, branch, &cs)
	if err != nil {
		return fmt.Errorf("push remediation branch: %w", err)
	}

	issueNumber := 0
	if fnd.Status.Tracking != nil {
		issueNumber = int(fnd.Status.Tracking.IssueNumber)
	}
	// The stored report carries its machine frontmatter; the PR body is
	// presentation, so render the markdown body only.
	reportBody := report.StripFrontmatter(result.ReportMarkdown)
	// A run on patchy's default image had none of the repository's
	// toolchain, so whatever the report says, patchy built and tested
	// nothing: the body says so, plainly, ahead of the report.
	untested := !ranRepositoryImage(rem.Status.RunnerImage)
	// Whether the agent reports it reproduced the bug before fixing it is
	// stated plainly, with its evidence, and its absence is a warning: the
	// claim is the agent's own, so the body attributes it to the agent.
	// The report is agent text: PRBody sanitises it, and a body it cannot
	// render falls back to patchy's own words, never to the raw report.
	// A run whose Job carried a network profile could fetch dependencies;
	// the body says so, from what the launch recorded off the Job, and
	// names the profile's routes as the operator's profile set holds them
	// now (the run records only the name).
	profile := networkProfileOf(rem.Status.RunnerImage)
	body, err := templates.PRBody(issueNumber, reportBody, untested,
		agentresult.Reproduction(result.Reproduction), profile, r.networkRoutes(profile))
	if err != nil {
		body = templates.PRBodyFallback(issueNumber)
	}
	number, url, err := r.Forge.EnsurePR(ctx, rem.Namespace, fnd.Spec.Repository.URL, branch,
		templates.FindingIssueTitle(&fnd), body)
	if err != nil {
		return fmt.Errorf("open remediation PR: %w", err)
	}

	if err := r.stampChild(ctx, rem, result, v1alpha1.RunComplete, transcript, func(cur *v1alpha1.Remediation) {
		cur.Status.Success = true
		cur.Status.Branch = branch
		cur.Status.PushedCommit = commit
		cur.Status.Confidence = agentresult.FormatConfidence(result.Confidence)
		cur.Status.PullRequest = &v1alpha1.PullRequestRef{Number: number, URL: url}
	}); err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := r.Get(ctx, fndKey, &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		cur.Status.Remediation = &v1alpha1.RemediationSummary{
			Name: rem.Name, Attempt: rem.Spec.Attempt,
			Outcome: string(result.Outcome), Success: true, Branch: branch,
			CompletedAt: timePtr(r.now()),
		}
		cur.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: number, URL: url, State: "open"}
		cur.Status.ActiveRun = nil
		if cur.Status.Phase == v1alpha1.PhaseRemediating {
			if err := v1alpha1.SetPhase(&cur, v1alpha1.PhaseInReview, r.now()); err != nil {
				return err
			}
		}
		return r.Status().Update(ctx, &cur)
	})
}

// handOff records an agent's "not safely fixable" verdict (edge 14).
func (r *RemediationReconciler) handOff(
	ctx context.Context, rem *v1alpha1.Remediation, result *envelope.Remediation,
	transcript *v1alpha1.TranscriptRef,
) error {
	if err := r.stampChild(ctx, rem, result, v1alpha1.RunComplete, transcript, func(cur *v1alpha1.Remediation) {
		cur.Status.Success = false
		cur.Status.Confidence = agentresult.FormatConfidence(result.Confidence)
	}); err != nil {
		return err
	}
	return r.finishFinding(ctx, rem, result, v1alpha1.PhaseHandedOff, "agent reported the finding is not safely fixable")
}

// stageOf is the stage of a remediation event that may not have arrived.
func stageOf(result *envelope.Remediation) *envelope.Stage {
	if result == nil {
		return nil
	}
	return &result.Stage
}

// fail retries (edge 12) or exhausts (edge 15). reported is the agent's own
// stage when it emitted one; its accounting survives the failure so the run's
// turns, tokens and cost still reach the child and the rollups — a failed run
// spent real money, and the estimate-against-actual averages depend on it
// being counted. Nil when no event arrived at all. Attempts the sandbox
// probe refused are not counted toward MaxAttempts.
func (r *RemediationReconciler) fail(
	ctx context.Context, rem *v1alpha1.Remediation, outcome, detail string, reported *envelope.Stage,
	transcript *v1alpha1.TranscriptRef,
) error {
	// Counted before the child is stamped: an error here leaves it Running
	// for the next reconcile instead of Failed with the finding unreleased.
	consumed, err := r.consumedAttempts(ctx, rem)
	if err != nil {
		return err
	}
	result := &envelope.Remediation{Stage: agentresult.FailedStage(reported, outcome, detail)}
	if err := r.stampChild(ctx, rem, result, v1alpha1.RunFailed, transcript, nil); err != nil {
		return err
	}
	maxAttempts := r.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 2
	}
	to := v1alpha1.PhaseQueued
	if consumed >= maxAttempts {
		to = v1alpha1.PhaseFailed
	}
	return r.finishFinding(ctx, rem, result, to, outcome+": "+detail)
}

// consumedAttempts is rem's attempt number less the finding's earlier
// attempts the sandbox probe refused or the launch held: their agent never
// ran.
func (r *RemediationReconciler) consumedAttempts(ctx context.Context, rem *v1alpha1.Remediation) (int32, error) {
	var siblings v1alpha1.RemediationList
	if err := r.List(ctx, &siblings, client.InNamespace(rem.Namespace),
		client.MatchingLabels{v1alpha1.LabelFinding: rem.Spec.FindingRef.Name}); err != nil {
		return 0, fmt.Errorf("count refused attempts: %w", err)
	}
	consumed := rem.Spec.Attempt
	for i := range siblings.Items {
		sib := &siblings.Items[i]
		if sib.Spec.FindingRef.UID == rem.Spec.FindingRef.UID && sib.Spec.Attempt < rem.Spec.Attempt &&
			(runnerguard.Refused(sib.Status.Conditions) || runnerguard.Held(sib.Status.Conditions)) {
			consumed--
		}
	}
	return consumed, nil
}

// finishFinding applies the post-run phase and summary to the finding.
func (r *RemediationReconciler) finishFinding(
	ctx context.Context, rem *v1alpha1.Remediation, result *envelope.Remediation,
	to v1alpha1.Phase, reason string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		key := types.NamespacedName{Namespace: rem.Namespace, Name: rem.Spec.FindingRef.Name}
		if err := r.Get(ctx, key, &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		if cur.Status.Phase != v1alpha1.PhaseRemediating {
			return nil
		}
		cur.Status.Remediation = &v1alpha1.RemediationSummary{
			Name: rem.Name, Attempt: rem.Spec.Attempt,
			Outcome: string(result.Outcome), Success: false,
			CompletedAt: timePtr(r.now()),
		}
		cur.Status.ActiveRun = nil
		cur.Status.LastFailureReason = agentresult.TruncateDetail(reason)
		if err := v1alpha1.SetPhase(&cur, to, r.now()); err != nil {
			return err
		}
		return r.Status().Update(ctx, &cur)
	})
}

// stampChild writes the run result onto the Remediation.
func (r *RemediationReconciler) stampChild(
	ctx context.Context, rem *v1alpha1.Remediation, result *envelope.Remediation,
	phase v1alpha1.RunPhase, transcript *v1alpha1.TranscriptRef, extra func(*v1alpha1.Remediation),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Remediation
		if err := r.Get(ctx, client.ObjectKeyFromObject(rem), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		cur.Status.Phase = phase
		cur.Status.Stage = agentresult.FromStage(&result.Stage)
		cur.Status.Stage.Transcript = transcript
		cur.Status.Report = agentresult.TruncateReport(result.ReportMarkdown)
		cur.Status.Reproduction = agentresult.Reproduction(result.Reproduction)
		if extra != nil {
			extra(&cur)
		}
		// The message is capped like the stage detail: the envelope's detail
		// is unbounded (a full git status, a stderr tail), and the CRD
		// refuses a condition message over 32768 bytes by rejecting the
		// whole status write, which would leave the run Running.
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionComplete,
			Status:             metav1.ConditionTrue,
			Reason:             nonEmptyReason(string(result.Outcome)),
			Message:            agentresult.TruncateDetail(result.Detail),
			ObservedGeneration: cur.Generation,
		})
		return r.Status().Update(ctx, &cur)
	})
}

// finalize cleans the child's Job/Secret before deletion (FinalizerJobs).
func (r *RemediationReconciler) finalize(ctx context.Context, rem *v1alpha1.Remediation) error {
	if rem.Status.JobRef != nil {
		if err := r.Runner.Delete(ctx, rem.Status.JobRef.Name); err != nil && !kerrors.IsNotFound(err) {
			return err
		}
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Remediation
		if err := r.Get(ctx, client.ObjectKeyFromObject(rem), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		fins := cur.GetFinalizers()
		out := fins[:0]
		for _, f := range fins {
			if f != v1alpha1.FinalizerJobs {
				out = append(out, f)
			}
		}
		if len(out) == len(fins) {
			return nil
		}
		cur.SetFinalizers(out)
		return r.Update(ctx, &cur)
	})
}

// SetupWithManager wires the reconciler and its scheduler fan-in.
func (r *RemediationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapJob := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		if obj.GetLabels()[v1alpha1.LabelRunKind] != string(v1alpha1.RunKindRemediation) {
			return nil
		}
		owner := obj.GetLabels()[v1alpha1.LabelOwner]
		if owner == "" {
			return nil
		}
		return []ctrl.Request{
			{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: owner}},
			{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: remSchedulerRequest}},
		}
	})
	mapSelf := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		return []ctrl.Request{
			{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}},
			{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: remSchedulerRequest}},
		}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Watches(&v1alpha1.Remediation{}, mapSelf).
		Watches(&batchv1.Job{}, mapJob).
		Named("remediation-run").
		Complete(r)
}

// nonEmptyReason keeps condition reasons non-empty.
func nonEmptyReason(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

// timePtr wraps a time for CRD fields.
func timePtr(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

func (r *RemediationReconciler) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *RemediationReconciler) log() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Log
}

// maxChangesetEntries is MaxChangesetEntries with its default applied.
func (r *RemediationReconciler) maxChangesetEntries() int {
	if r.MaxChangesetEntries <= 0 {
		return changeset.DefaultMaxEntries
	}
	return r.MaxChangesetEntries
}

// changesetRules gathers what rem's changeset is validated against: the
// Repository's pinned commit, the entry cap, and whether the
// repository-image rules apply. A Repository that has vanished leaves the
// base empty, which refuses the changeset rather than pushing it unchecked.
//
// The repository-image rules apply when either this run or the
// Investigation it acts on ran a repository-declared image. The
// remediation agent's input is not only the tree: its analysis is the
// Investigation's report and its parameters are the ones the spawner took
// from it, and a default-image remediation of a repository-image
// investigation (a human approved a held verdict, the breaker tripped
// between the two, or the controllers' flags differ) would otherwise follow
// that image's instructions exempt from the CI deny. An Investigation that
// cannot be found cannot vouch for itself, so the stricter rules apply.
func (r *RemediationReconciler) changesetRules(
	ctx context.Context, rem *v1alpha1.Remediation,
) (changeset.Rules, error) {
	rules := changeset.Rules{
		MaxEntries:      r.maxChangesetEntries(),
		RepositoryImage: ranRepositoryImage(rem.Status.RunnerImage),
	}
	if networkProfileOf(rem.Status.RunnerImage) != "" {
		// The Job carried a profile (the launch's read-back, never the
		// request): no file may leave the run naming the egress proxy.
		rules.Forbidden = r.Images.ProxyHostForms()
	}
	var repo v1alpha1.Repository
	key := types.NamespacedName{Namespace: rem.Namespace, Name: rem.Spec.RepositoryRef.Name}
	switch err := r.Get(ctx, key, &repo); {
	case err == nil:
		rules.Base = repo.Status.ResolvedSHA
	case !kerrors.IsNotFound(err):
		return rules, err
	}
	if rules.RepositoryImage {
		return rules, nil
	}
	var inv v1alpha1.Investigation
	key = types.NamespacedName{Namespace: rem.Namespace, Name: rem.Spec.InvestigationRef.Name}
	switch err := r.Get(ctx, key, &inv); {
	case err == nil:
		rules.RepositoryImage = ranRepositoryImage(inv.Status.RunnerImage)
	case kerrors.IsNotFound(err):
		rules.RepositoryImage = true
	default:
		return rules, err
	}
	return rules, nil
}

// ranRepositoryImage reports whether a run's launch stamp says its pod ran
// a repository-declared image. A run launched before the stamp existed ran
// the default image.
func ranRepositoryImage(ri *v1alpha1.RunnerImageRef) bool {
	return ri != nil && ri.Source == v1alpha1.RunnerImageSourceRepository
}

// outcomeLaunchRefused is a run whose Job was refused before it existed,
// in the intent runs' vocabulary.
const outcomeLaunchRefused = "launch_refused"

// networkRoutes are the routes of the profile called name in this
// controller's profile set: none for no profile, or one the set no longer
// holds, which leaves the pull request's line as step 1 wrote it.
func (r *RemediationReconciler) networkRoutes(name string) templates.NetworkRoutes {
	if p, ok := r.Images.Profiles.Get(name); ok && name != "" {
		return templates.RoutesOf(p)
	}
	return templates.NetworkRoutes{}
}

// networkProfileOf is the network profile a run's Job carried, as recorded
// at launch; "" when it carried none or nothing was recorded.
func networkProfileOf(ref *v1alpha1.RunnerImageRef) string {
	if ref == nil {
		return ""
	}
	return ref.NetworkProfile
}
