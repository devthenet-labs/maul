// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/forge"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/runnerimage"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolpod"
)

// kindIntent is the intent Jobs' maul.run/kind, which a build Job's spec
// carries (the profile admission policy reads it).
const kindIntent = "intent"

// start renders pv's tool pod: within the tool pod bound, from the build's
// own image and profile, with the relay's workspace config, its key kept
// across passes; then the verification runs. Waiting for room is bounded by
// the verification's own timeout: past it the round could not have run
// anyway (Infra tool-pods-saturated). A request the tool pod builder
// refuses is Stale; one the build no longer matches is Stale too.
func (r *Reconciler) start(ctx context.Context, pv *v1alpha1.PreviewVerification, s *subject) (ctrl.Result,
	error) {
	name := JobName(pv)
	waited := r.now().Sub(pv.CreationTimestamp.Time)
	job, _, err := r.Tools.Get(ctx, name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job != nil && job.Labels[v1alpha1.LabelPreviewVerification] != string(pv.UID) {
		// Another verification's, under this one's name: never adopted.
		r.log().LogAttrs(ctx, slog.LevelWarn, "a tool Job of another verification holds this one's name; deleted",
			slog.String("verification", pv.Name), slog.String("job", name))
		return r.wait(), r.Tools.Delete(ctx, name)
	}
	if job == nil {
		if full, err := r.saturated(ctx, pv); err != nil || full {
			if err != nil {
				return ctrl.Result{}, err
			}
			if waited >= pv.Spec.Timeout.Duration+grace {
				return ctrl.Result{}, r.settle(ctx, pv, v1alpha1.VerificationInfra,
					v1alpha1.VerificationReasonToolPodsSaturated)
			}
			if pv.Status.Reason != v1alpha1.VerificationReasonToolPodsSaturated {
				pv.Status.Reason = v1alpha1.VerificationReasonToolPodsSaturated
				return r.wait(), r.Status().Update(ctx, pv)
			}
			return r.wait(), nil
		}
	}
	spec, goEnv, reason, err := r.toolPodSpec(ctx, pv, s)
	switch {
	case err != nil:
		return r.retryOrInfra(ctx, pv, waited, "the tool pod was not rendered", err)
	case reason != "":
		return ctrl.Result{}, r.settle(ctx, pv, reasonPhase(reason), reason)
	}
	if reason, err := r.create(ctx, pv, spec, goEnv); reason != "" {
		return ctrl.Result{}, r.settle(ctx, pv, reasonPhase(reason), reason)
	} else if err != nil {
		return r.retryOrInfra(ctx, pv, waited, "the tool pod was not created", err)
	}
	t := metav1.NewTime(r.now())
	pv.Status.Phase, pv.Status.Reason, pv.Status.StartedAt = v1alpha1.VerificationRunning, "", &t
	return r.wait(), r.Status().Update(ctx, pv)
}

// retryOrInfra returns err for a retry while pv may still run, and past
// its own timeout settles it Infra tools-not-ready: a failure that keeps
// recurring never becomes a verdict, nor a wait without end.
func (r *Reconciler) retryOrInfra(ctx context.Context, pv *v1alpha1.PreviewVerification, waited time.Duration,
	msg string, err error) (ctrl.Result, error) {
	if waited < pv.Spec.Timeout.Duration+grace {
		return ctrl.Result{}, err
	}
	r.log().LogAttrs(ctx, slog.LevelWarn, msg, slog.String("verification", pv.Name), slog.Any("error", err))
	return ctrl.Result{}, r.settle(ctx, pv, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsNotReady)
}

// reasonPhase is the phase a start-time reason settles in: a request that
// no longer holds is Stale, maul's own trouble Infra.
func reasonPhase(reason v1alpha1.PreviewVerificationReason) v1alpha1.PreviewVerificationPhase {
	if reason == v1alpha1.VerificationReasonStaleCriteria {
		return v1alpha1.VerificationStale
	}
	return v1alpha1.VerificationInfra
}

// saturated reports the tool pod bound reached: as many live tool Jobs
// (not finished, not deleting) of other verifications as --intent-max-tool-pods.
func (r *Reconciler) saturated(ctx context.Context, pv *v1alpha1.PreviewVerification) (bool, error) {
	list, err := r.Tools.List(ctx)
	if err != nil {
		return false, err
	}
	live := 0
	for i := range list {
		j := &list[i]
		if _, done := jobFinished(j); !done && j.DeletionTimestamp.IsZero() &&
			j.Labels[v1alpha1.LabelPreviewVerification] != string(pv.UID) {
			live++
		}
	}
	return live >= max(r.Settings.MaxToolPods, 1), nil
}

// toolPodSpec is pv's tool pod, without its Data and Key, and the image's
// Go directories: the workspace's trees on the build's own image, its
// profile from the very build Spec a launch would create (ProfileOf), held
// to what the workspace recorded. A reason says why there is none.
func (r *Reconciler) toolPodSpec(ctx context.Context, pv *v1alpha1.PreviewVerification, s *subject) (
	jobs.ToolPodSpec, map[string]string, v1alpha1.PreviewVerificationReason, error) {
	ws := pv.Spec.Workspace
	build, repo, why, err := r.buildSpec(ctx, pv, s)
	if err != nil || why != "" {
		if why != "" {
			r.log().LogAttrs(ctx, slog.LevelWarn, "the agents tool pod is not rendered",
				slog.String("verification", pv.Name), slog.String("detail", why))
		}
		return jobs.ToolPodSpec{}, nil, v1alpha1.VerificationReasonStaleCriteria, err
	}
	spec := jobs.ToolPodSpec{
		Name: JobName(pv), Verification: string(pv.UID), Repo: build.Repo,
		ToolsImage: r.Settings.ToolsImage, Image: ws.SandboxImage, SearchPath: ws.SandboxSearchPath,
		Trees: []jobs.Tree{
			{Key: jobs.ToolSideHead, URL: s.repository.URL, BaseSHA: ws.HeadSHA,
				ArtifactURL: s.head.Status.Artifact.URL, ArtifactDigest: s.head.Status.Artifact.Digest},
			{Key: jobs.ToolSideBase, URL: s.repository.URL, BaseSHA: ws.BaseSHA,
				ArtifactURL: s.base.Status.Artifact.URL, ArtifactDigest: s.base.Status.Artifact.Digest},
		},
		Timeout:     pv.Spec.Timeout.Duration,
		DeadlineMax: v1alpha1.MaxAgentsVerificationTimeout + deadlineMargin,
		Resources:   r.Settings.Resources,
	}
	spec.ProfileOf(build)
	return spec, repo.Status.RunnerImage.GoEnv, "", nil
}

// buildSpec is the build Job Spec a launch of the latest build of the
// workspace's repository would create now, through the calls it makes in
// its order (PinFor on the build's own Repository, then Profile and the
// Forge check), and that Repository. why says why it no longer matches the
// workspace: no build, another image or search path, another profile.
func (r *Reconciler) buildSpec(ctx context.Context, pv *v1alpha1.PreviewVerification, s *subject) (
	jobs.Spec, *v1alpha1.Repository, string, error) {
	ws := pv.Spec.Workspace
	run, repo, err := r.buildRepository(ctx, s.intent, s.repository.URL)
	if err != nil || repo == nil {
		return jobs.Spec{}, nil, "no build of " + s.repository.URL + " is the intent's own", err
	}
	_, slug, err := forge.ParseRepoURL(s.repository.URL)
	if err != nil {
		return jobs.Spec{}, nil, "the repository URL does not parse: " + err.Error(), nil
	}
	harness := r.Settings.Harness
	if run.Spec.Harness.IsCodex() {
		harness = r.Settings.CodexHarness
	}
	spec := jobs.Spec{Repo: slug.String(), Phase: string(agentrun.PhaseBuild), Kind: kindIntent, Harness: harness}
	if skipped := r.Images.PinFor(&spec, repo); skipped != "" {
		return jobs.Spec{}, nil, "the build's image is not runnable: " + skipped, nil
	}
	if spec.RunnerImage != ws.SandboxImage || spec.RunnerSearchPath != ws.SandboxSearchPath {
		return jobs.Spec{}, nil, "the build's accepted image is no longer the workspace's", nil
	}
	if applied, _ := r.Images.Profile(&spec, s.repository.URL); applied != ws.NetworkProfile {
		return jobs.Spec{}, nil, fmt.Sprintf("the build now gets network profile %q, the workspace recorded %q",
			applied, ws.NetworkProfile), nil
	}
	switch err := r.Images.CheckForges(ctx, pv.Namespace, &spec); {
	case errors.Is(err, jobs.ErrNetworkProfileRefused):
		return jobs.Spec{}, nil, "the build's network profile is refused: " + err.Error(), nil
	case err != nil:
		// The Forge list could not be read: the build's launch would
		// retry, so this one does.
		return jobs.Spec{}, nil, "", err
	}
	return spec, repo, "", nil
}

// buildRepository is the latest build run of repoURL among in's
// (LatestBuildRun, the gate's own choice), and the Repository it was built
// on, its runner-image anchor: nil when it has none, or one that run does
// not own.
func (r *Reconciler) buildRepository(ctx context.Context, in *v1alpha1.Intent, repoURL string) (
	*v1alpha1.IntentRun, *v1alpha1.Repository, error) {
	var list v1alpha1.IntentRunList
	if err := r.List(ctx, &list, client.InNamespace(in.Namespace),
		client.MatchingLabels{v1alpha1.LabelIntent: in.Name}); err != nil {
		return nil, nil, err
	}
	runs := make([]*v1alpha1.IntentRun, 0, len(list.Items))
	for i := range list.Items {
		runs = append(runs, &list.Items[i])
	}
	run := LatestBuildRun(in, repoURL, runs)
	if run == nil {
		return nil, nil, nil
	}
	var repo v1alpha1.Repository
	key := types.NamespacedName{Namespace: in.Namespace, Name: run.Spec.Repository.RepositoryRef.Name}
	switch err := r.APIReader.Get(ctx, key, &repo); {
	case kerrors.IsNotFound(err):
		return run, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("read the build's repository %s: %w", key.Name, err)
	}
	if !controlledBy(repo.OwnerReferences, run.UID) || repo.Status.RunnerImage == nil {
		return run, nil, nil
	}
	return run, &repo, nil
}

// create writes the relay's data and key into spec and creates the tool
// pod. The key of a Secret an earlier pass created is kept (CreateToolPod
// refuses a Secret whose data differs), so a retry adopts what is there;
// a Secret of another verification under the name is deleted first. A spec
// the builder refuses is stale-criteria, an API refusal (a quota, an
// admission policy) tools-unscheduled; any other failure is an error.
func (r *Reconciler) create(ctx context.Context, pv *v1alpha1.PreviewVerification, spec jobs.ToolPodSpec,
	goEnv map[string]string) (v1alpha1.PreviewVerificationReason, error) {
	key, uid, found, err := r.Tools.Key(ctx, spec.Name)
	switch {
	case err != nil:
		return "", err
	case found && uid != string(pv.UID):
		return "", errors.Join(r.Tools.Delete(ctx, spec.Name),
			fmt.Errorf("tool Secret %s%s was another verification's; deleted", spec.Name, jobs.ToolsSecretSuffix))
	case found:
		spec.Key = key
	default:
		spec.Key = toolpod.NewKey()
	}
	env, err := r.Tools.SandboxEnv(spec)
	if err != nil {
		return r.refused(ctx, pv, err)
	}
	if spec.Data, err = r.relayData(pv, spec, relayEnv(env, goEnv)); err != nil {
		r.log().LogAttrs(ctx, slog.LevelWarn, "the relay's config is refused", slog.String("verification", pv.Name),
			slog.Any("error", err))
		return v1alpha1.VerificationReasonStaleCriteria, nil
	}
	if err := r.Tools.Create(ctx, spec); err != nil {
		return r.refused(ctx, pv, err)
	}
	return "", nil
}

// refused maps a tool pod creation failure: a refused spec is
// stale-criteria, the API server's refusal tools-unscheduled, anything
// else an error to retry.
func (r *Reconciler) refused(ctx context.Context, pv *v1alpha1.PreviewVerification, err error) (
	v1alpha1.PreviewVerificationReason, error) {
	reason := v1alpha1.PreviewVerificationReason("")
	switch {
	case errors.Is(err, jobs.ErrToolPodRefused):
		reason = v1alpha1.VerificationReasonStaleCriteria
	case kerrors.IsForbidden(err) || kerrors.IsInvalid(err):
		reason = v1alpha1.VerificationReasonToolsUnscheduled
	default:
		return "", err
	}
	r.log().LogAttrs(ctx, slog.LevelWarn, "the tool pod was refused", slog.String("verification", pv.Name),
		slog.String("reason", string(reason)), slog.Any("error", err))
	return reason, nil
}

// relayData is the relay's data: its workspace config (validated as the
// relay validates it) and the outcomes under evaluation.
func (r *Reconciler) relayData(pv *v1alpha1.PreviewVerification, spec jobs.ToolPodSpec,
	env []toolexec.EnvVar) (map[string]string, error) {
	ws := pv.Spec.Workspace
	_, _, wall := toolpod.Deadlines(spec.Timeout, spec.DeadlineMax)
	w := &previewtools.Workspace{SearchPath: ws.SandboxSearchPath, Env: env,
		BuildTimeoutSeconds: int(r.buildTimeout(ws) / time.Second)}
	switch {
	case ws.Surface.CLI != nil:
		w.Build, w.Binary = slices.Clone(ws.Surface.CLI.Build), ws.Surface.CLI.Binary
	case ws.Surface.Library != nil:
		w.Language = string(ws.Surface.Library.Language)
	}
	cfg := previewtools.Config{Mode: previewtools.ModeWorkspace, Workspace: w,
		MaxToolCalls: int(pv.Spec.Tools.MaxToolCalls), TimeoutSeconds: int(wall / time.Second),
		ReplayOnly: pv.Spec.Rung == v1alpha1.RungReplay}
	for _, c := range pv.Spec.Checks {
		cfg.Checks = append(cfg.Checks, previewtools.StoredCheck{ID: c.ID, Digest: c.Digest, Body: c.Body,
			Expect: string(c.Expect)})
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return toolpod.ConfigData(cfg, pv.Spec.Outcomes)
}

// buildTimeout is each tree's build timeout: the cli surface's as the
// workspace resolved it (a library's the default), never over
// --intent-tool-build-timeout-max.
func (r *Reconciler) buildTimeout(ws *v1alpha1.VerificationWorkspace) time.Duration {
	t := v1alpha1.DefaultCLIBuildTimeout
	if ws.Surface.CLI != nil && ws.Surface.CLI.BuildTimeout.Duration > 0 {
		t = ws.Surface.CLI.BuildTimeout.Duration
	}
	if m := r.Settings.BuildTimeoutMax; m > 0 && t > m {
		t = m
	}
	return t
}

// relayEnv is the environment the relay sends every program: the sandbox's
// as jobs renders it (the build's profile included), less PATH (the relay
// sends the search path) and every name the exec shim owns or refuses, then
// each of the image's Go directories the rendered environment does not set
// that the shim admits. A program sees nothing wider than the build.
func relayEnv(rendered []corev1.EnvVar, goEnv map[string]string) []toolexec.EnvVar {
	var out []toolexec.EnvVar
	set := map[string]bool{}
	for _, e := range rendered {
		set[e.Name] = true
		if e.Name != "PATH" && e.ValueFrom == nil && toolexec.AllowedEnv(e.Name) {
			out = append(out, toolexec.EnvVar{Name: e.Name, Value: e.Value})
		}
	}
	for _, name := range []string{"GOMODCACHE", "GOPATH", "GOROOT"} {
		if v, ok := goEnv[name]; ok && !set[name] && runnerimage.GoEnvName(name) && toolexec.AllowedEnv(name) {
			out = append(out, toolexec.EnvVar{Name: name, Value: v})
		}
	}
	return out
}

// jobFinished is when j reached a terminal condition, and whether it has.
func jobFinished(j *batchv1.Job) (time.Time, bool) {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}
