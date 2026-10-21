// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
	"github.com/devthenet-labs/maul/cmd/maul/internal/imagecheck"
	"github.com/devthenet-labs/maul/cmd/maul/internal/previewcheck"
	"github.com/devthenet-labs/maul/internal/forge"
	"github.com/devthenet-labs/maul/internal/runnerimage"
)

// leafPattern is the one path segment preview-controller admits after its
// image prefix (controller/preview's namePattern).
var leafPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// shaTagPattern is the immutable tag a preview runs: sha-<40 hex>.
var shaTagPattern = regexp.MustCompile(`^sha-[0-9a-f]{40}$`)

// Config is what a run reads with.
type Config struct {
	// Reader reads the cluster with the caller's own kubeconfig.
	Reader client.Reader
	// Namespace and Project name the Project to check.
	Namespace string
	Project   string
	// GitHub reads the repositories with the caller's own identity.
	GitHub GitHub
	// Keychain authenticates registry reads; the CLI passes
	// resolve.NewKeychain, the caller's own cloud and docker credentials.
	Keychain authn.Keychain
	// Resolver looks the preview host up; nil is net.DefaultResolver.
	Resolver previewcheck.Resolver
	// DialTLS completes the preview TLS handshake; nil is
	// previewcheck.DialTLS.
	DialTLS previewcheck.TLSDialer
	// HTTP sends the preview sign-in checks' requests, never following a
	// redirect; nil is previewcheck.NewHTTPDoer.
	HTTP previewcheck.HTTPDoer
}

// run is one check of one Project.
type run struct {
	cfg    Config
	p      *v1alpha1.Project
	report Report
	// ready is the Project's Ready condition when it describes the current
	// spec, else nil.
	ready *metav1.Condition
	heads map[string]headResult
}

type headResult struct {
	head Head
	err  error
}

// Run checks a Project. The error is the Project itself not being
// readable, as the API server reported it (not found, forbidden); every
// other failure to find something out is a SKIP line with its reason.
func Run(ctx context.Context, cfg Config) (Report, error) {
	var p v1alpha1.Project
	if err := cfg.Reader.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: cfg.Project}, &p); err != nil {
		return Report{}, err
	}
	r := &run{cfg: cfg, p: &p, heads: map[string]headResult{},
		report: Report{Project: p.Name, Namespace: p.Namespace}}
	r.verdict()
	r.forges(ctx)
	r.labels()
	s := loadSettings(ctx, cfg.Reader, p.Namespace)
	r.agentImages(ctx, s)
	r.previews(ctx, s)
	return r.report, nil
}

func (r *run) add(check, repository string, status checkreport.Status, format string, args ...any) {
	r.report.add(check, repository, status, fmt.Sprintf(format, args...))
}

// verdict reports intent-controller's own conditions.
func (r *run) verdict() {
	p := r.p
	ready := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady)
	switch {
	case ready == nil:
		r.add(CheckReady, "", checkreport.Fail, "intent-controller has not reported on this Project: is it deployed "+
			"(intentController.enabled) and watching namespace %s?", p.Namespace)
	case ready.ObservedGeneration < p.Generation:
		r.add(CheckReady, "", checkreport.Fail, "Ready describes generation %d and the Project is at generation %d: "+
			"intent-controller has not validated the current spec yet; run this again in a minute",
			ready.ObservedGeneration, p.Generation)
	case ready.Status != metav1.ConditionTrue:
		r.ready = ready
		r.add(CheckReady, "", checkreport.Fail, "%s: %s", ready.Reason, ready.Message)
	case p.Spec.Suspend:
		r.ready = ready
		r.add(CheckReady, "", checkreport.Fail, "%s: %s; but spec.suspend is true, so no intent is discovered or "+
			"launched", ready.Reason, ready.Message)
	default:
		r.ready = ready
		r.add(CheckReady, "", checkreport.Pass, "%s: %s", ready.Reason, ready.Message)
	}

	conflict := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionIntentNameConflict)
	switch {
	case conflict == nil:
		r.add(CheckIntentNames, "", checkreport.Skip, "not reported yet: intent-controller reports it after polling "+
			"the intent repository, which it does only while the Project is Ready")
	case conflict.Status == metav1.ConditionTrue:
		r.add(CheckIntentNames, "", checkreport.Fail, "%s", conflict.Message)
	default:
		r.add(CheckIntentNames, "", checkreport.Pass, "%s", conflict.Message)
	}
}

// isReady reports a Ready condition that is True for the current spec.
func (r *run) isReady() bool {
	return r.ready != nil && r.ready.Status == metav1.ConditionTrue
}

// forges resolves the intent repository and every app repository to the
// Forge that covers it, from the Forge CRs alone, and reports that Forge's
// own Ready condition (source-controller's credential check).
func (r *run) forges(ctx context.Context) {
	var list v1alpha1.ForgeList
	listErr := r.cfg.Reader.List(ctx, &list, client.InNamespace(r.p.Namespace))
	for _, t := range r.repositories() {
		if listErr != nil {
			r.add(CheckForge, t.key, checkreport.Skip, "cannot list Forges: %v", listErr)
			continue
		}
		res, err := forge.Resolve(list.Items, t.url)
		if err != nil {
			r.add(CheckForge, t.key, checkreport.Fail, "%v; a Forge in namespace %s must cover it (spec.orgs, "+
				"spec.repositories)", err, r.p.Namespace)
			continue
		}
		f := res.Forge
		covers := fmt.Sprintf("Forge %s covers %s/%s", f.Name, res.Repo.Owner, res.Repo.Name)
		switch ready := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionReady); {
		case ready == nil:
			r.add(CheckForge, t.key, checkreport.Fail, "%s, but source-controller has not validated its credential yet",
				covers)
		case ready.Status != metav1.ConditionTrue:
			r.add(CheckForge, t.key, checkreport.Fail, "%s, but it is not Ready: %s: %s", covers, ready.Reason,
				ready.Message)
		default:
			r.add(CheckForge, t.key, checkreport.Pass, "%s and is Ready (%s)", covers, ready.Reason)
		}
	}
}

// target is one repository a check is about.
type target struct{ key, url string }

// repositories are the intent repository, then every app repository.
func (r *run) repositories() []target {
	out := make([]target, 0, 1+len(r.p.Spec.Repositories))
	out = append(out, target{IntentRepository, r.p.Spec.IntentRepository})
	for _, repo := range r.p.Spec.Repositories {
		out = append(out, target{repo.Name, repo.URL})
	}
	return out
}

// labelRefusal opens the Ready message intent-controller writes when the
// App cannot create a label (AppNotInstalled, "the label %q cannot be
// created on %s: ..."), which is how a label failure is told from the
// App's other refusals.
const labelRefusal = "the label "

// labels reports the trigger and approve labels from the Ready condition:
// intent-controller ensures them last, so they exist exactly when it is
// True.
func (r *run) labels() {
	p := r.p
	approve := p.Spec.Labels.Approve
	if approve == "" {
		approve = v1alpha1.DefaultApproveLabel
	}
	where := p.Spec.IntentRepository
	if _, repo, err := forge.ParseRepoURL(where); err == nil {
		where = repo.Owner + "/" + repo.Name
	}
	switch {
	case r.isReady():
		r.add(CheckLabels, "", checkreport.Pass, "%q (trigger) and %q (approve) exist on %s",
			v1alpha1.ProjectTriggerLabel(p), approve, where)
	case r.ready != nil && r.ready.Reason == v1alpha1.ReasonAppNotInstalled &&
		strings.HasPrefix(r.ready.Message, labelRefusal):
		r.add(CheckLabels, "", checkreport.Fail, "%s", r.ready.Message)
	default:
		r.add(CheckLabels, "", checkreport.Skip, "not proven: intent-controller ensures %q and %q on %s only once "+
			"every check before them passes (see ready)", v1alpha1.ProjectTriggerLabel(p), approve, where)
	}
}

// head is a repository's default-branch head, read once per run.
func (r *run) head(ctx context.Context, repoURL string) (Head, error) {
	if h, ok := r.heads[repoURL]; ok {
		return h.head, h.err
	}
	var h headResult
	if r.cfg.GitHub == nil {
		h.err = errors.New("no GitHub reader")
	} else {
		h.head, h.err = r.cfg.GitHub.Head(ctx, repoURL)
	}
	r.heads[repoURL] = h
	return h.head, h.err
}

// agentImages checks each repository's declared agent image.
func (r *run) agentImages(ctx context.Context, s settings) {
	require := r.p.Spec.RequireRepositoryImage == nil || *r.p.Spec.RequireRepositoryImage
	pol, polErr := s.policy()
	if s.Source.Err != nil {
		polErr = s.Source.Err
	}
	for _, repo := range r.p.Spec.Repositories {
		r.agentImage(ctx, repo, require, s.Source, pol, polErr)
	}
}

func (r *run) agentImage(ctx context.Context, repo v1alpha1.ProjectRepository, require bool,
	cfg previewcheck.ControllerConfig, pol imagePolicy, polErr error) {
	key := repo.Name
	head, err := r.head(ctx, repo.URL)
	if err != nil {
		r.add(CheckAgentImage, key, checkreport.Skip, "cannot read the default branch: %v", err)
		return
	}
	at := head.Branch + "@" + shortSHA(head.SHA)
	files, err := r.declarations(ctx, repo.URL, head.SHA)
	if err != nil {
		r.add(CheckAgentImage, key, checkreport.Skip, "cannot read the declaration at %s: %v", at, err)
		return
	}
	decl, err := runnerimage.Declare(files)
	if err != nil {
		r.add(CheckAgentImage, key, checkreport.Fail, "%s at %s is refused: %v", runnerimage.AgentYAMLPath, at, err)
		return
	}
	if decl.Outcome != runnerimage.OutcomeDeclared {
		why := fmt.Sprintf("%s declares no agent image: neither %s nor %s exists as a regular file "+
			"(source-controller ignores a symlink)", at, runnerimage.AgentYAMLPath, runnerimage.DevcontainerPath)
		if decl.Outcome == runnerimage.OutcomeNotApplicable {
			why = fmt.Sprintf("%s declares no agent image: %s is not for maul (%s)", at, decl.Manifest, decl.Reason)
		}
		if require {
			r.add(CheckAgentImage, key, checkreport.Fail, "%s, and the Project requires one (requireRepositoryImage), "+
				"so every build and revise run blocks with ImageRequired", why)
		} else {
			r.add(CheckAgentImage, key, checkreport.Skip, "%s; build and revise runs use the default runner image "+
				"(requireRepositoryImage: false)", why)
		}
		return
	}
	declared := fmt.Sprintf("%s (%s at %s)", decl.Image, decl.Manifest, at)
	r.judgeAgentImage(ctx, key, decl.Image, declared, require, cfg, pol, polErr)
}

// judgeAgentImage judges one declared image under source-controller's
// policy; declared names it and where it was declared, for the reasons.
func (r *run) judgeAgentImage(ctx context.Context, key, image, declared string, require bool,
	cfg previewcheck.ControllerConfig, pol imagePolicy, polErr error) {
	switch {
	case polErr != nil:
		r.add(CheckAgentImage, key, checkreport.Skip, "%s: cannot read source-controller's policy: %v; "+
			"`maul check image %s --allow <registry path>` checks it by hand", declared, polErr, image)
		return
	case !cfg.Found():
		r.add(CheckAgentImage, key, checkreport.Skip, "%s: no source-controller ConfigMap in namespace %s, so its "+
			"policy is unknown; `maul check image %s` checks it by hand", declared, r.p.Namespace, image)
		return
	case !pol.enabled && require:
		r.add(CheckAgentImage, key, checkreport.Fail, "%s: source-controller does not resolve repository-declared "+
			"images (%s is not true in %s; the chart's agent.repositoryImages.enabled), so every build and "+
			"revise run blocks with ImageRequired", declared, keyRepositoryImages, pol.from)
		return
	case !pol.enabled:
		r.add(CheckAgentImage, key, checkreport.Skip, "%s: source-controller does not resolve repository-declared "+
			"images, so runs use the default runner image (requireRepositoryImage: false)", declared)
		return
	}
	rep, err := imagecheck.Static(ctx, imagecheck.StaticConfig{
		Reference: image, Policy: pol.policy, MaxBytes: pol.maxBytes, PublicKey: pol.key,
		KeyName: "source-controller's cosign key", Keychain: r.cfg.Keychain,
	})
	if err != nil {
		r.add(CheckAgentImage, key, checkreport.Skip, "%s: %v", declared, err)
		return
	}
	var failed, failedNames []string
	for _, c := range rep.Checks {
		if c.Status == checkreport.Fail {
			failed = append(failed, c.Name+": "+c.Reason)
			failedNames = append(failedNames, c.Name)
		}
	}
	if slices.Equal(failedNames, []string{imagecheck.CheckResolve}) {
		// Only the registry read failed: tell an image that is not there
		// from credentials of the caller's that cannot read it.
		_, err := r.fetch(ctx, image)
		switch code := registryCode(err); {
		case err == nil:
		case code == transport.NameUnknownErrorCode || code == transport.ManifestUnknownErrorCode || code == "404":
			r.add(CheckAgentImage, key, checkreport.Fail, "%s is not published: %v; the repository's agent image "+
				"publisher must push it before an intent builds there (is its last run green?)", declared, err)
			return
		default:
			r.add(CheckAgentImage, key, checkreport.Skip, "%s passes source-controller's allowlist from %s, but "+
				"your registry credentials cannot read it: %v", declared, pol.from, err)
			return
		}
	}
	if len(failed) > 0 {
		r.add(CheckAgentImage, key, checkreport.Fail, "%s fails source-controller's policy from %s: %s; "+
			"`maul check image %s` lists every check", declared, pol.from, strings.Join(failed, "; "), image)
		return
	}
	if pol.unverified != "" {
		// Every other check passed, but the one source-controller may
		// still refuse it on was not run: that is not a PASS.
		r.add(CheckAgentImage, key, checkreport.Skip, "%s pins to %s and passes source-controller's other checks "+
			"from %s, but not its signature check: %s; `maul check image %s --cosign-key <file>` verifies it "+
			"with the key source-controller mounts", declared, rep.Image, pol.from, pol.unverified, image)
		return
	}
	r.add(CheckAgentImage, key, checkreport.Pass, "%s pins to %s and passes source-controller's policy from %s "+
		"(%s); read with your registry credentials, not source-controller's", declared, rep.Image, pol.from,
		pol.signature)
}

// declarations reads the two declaration files at sha.
func (r *run) declarations(ctx context.Context, repoURL, sha string) (runnerimage.Files, error) {
	var files runnerimage.Files
	for _, f := range []struct {
		path string
		into *runnerimage.File
	}{
		{runnerimage.AgentYAMLPath, &files.AgentYAML},
		{runnerimage.DevcontainerPath, &files.Devcontainer},
	} {
		data, size, found, err := r.cfg.GitHub.File(ctx, repoURL, sha, f.path, runnerimage.MaxDeclarationBytes)
		if err != nil {
			return runnerimage.Files{}, err
		}
		*f.into = runnerimage.File{Present: found, Size: size, Data: data}
	}
	return files, nil
}

// previews checks a previewed Project's preview path end to end.
func (r *run) previews(ctx context.Context, s settings) {
	p := r.p
	previews := v1alpha1.EffectivePreviews(p)
	if len(previews) == 0 {
		reason := "the Project previews no repository (neither spec.preview nor any repositories[].preview is set)"
		status := checkreport.Skip
		if p.Spec.Preview != nil || slices.ContainsFunc(p.Spec.Repositories,
			func(repo v1alpha1.ProjectRepository) bool { return repo.Preview != nil }) {
			reason = "the Project's preview configuration is one the schema refuses (both spec.preview and " +
				"repositories[].preview, a repeated path, or more than 4), so it previews nothing"
			status = checkreport.Fail
		}
		r.add(CheckPreviews, "", status, "%s", reason)
		r.add(CheckPreviewDNS, "", checkreport.Skip, "no preview")
		r.add(CheckPreviewTLS, "", checkreport.Skip, "no preview")
		return
	}
	prefix := s.Preview.Data[keyPreviewImagePrefix]
	suffix := s.Preview.Data[keyPreviewHostSuffix]
	switch {
	case s.Intent.Err != nil || s.Preview.Err != nil:
		r.add(CheckPreviews, "", checkreport.Skip, "cannot read the controllers' settings: %v",
			errors.Join(s.Intent.Err, s.Preview.Err))
	case !s.Intent.Bool(keyIntentPreviewsEnabled):
		r.add(CheckPreviews, "", checkreport.Fail, "intent-controller writes no Preview: %s is not true in %s "+
			"(the chart's previewController.enabled)", keyIntentPreviewsEnabled, orNone(s.Intent.From, p.Namespace))
	case !s.Preview.Found():
		r.add(CheckPreviews, "", checkreport.Fail, "no preview-controller ConfigMap in namespace %s: is "+
			"previewController.enabled set in the maul chart?", p.Namespace)
	case prefix == "" || suffix == "":
		r.add(CheckPreviews, "", checkreport.Fail, "%s sets no %s or %s", s.Preview.From, keyPreviewImagePrefix,
			keyPreviewHostSuffix)
	default:
		r.add(CheckPreviews, "", checkreport.Pass, "intent-controller writes Previews, and preview-controller serves "+
			"each intent at <intent>.%s from images under %s", suffix, prefix)
	}
	// A Preview exists only for an intent with a pull request, and every
	// component with one runs its head. The default-branch head runs only
	// in a component the intent leaves unchanged, so only with more than
	// one (v1alpha1.DesiredPreviewComponents).
	baseRuns := len(previews) > 1
	for _, rp := range previews {
		r.previewImage(ctx, rp, prefix, baseRuns)
	}
	pc := previewcheck.New(previewcheck.Config{Reader: r.cfg.Reader, Namespace: p.Namespace, Project: p.Name,
		Resolver: r.cfg.Resolver, DialTLS: r.cfg.DialTLS, HTTP: r.cfg.HTTP})
	pc.Host(ctx, s.Settings, suffix)
	pc.Auth(ctx, s.Settings, suffix)
	for _, l := range pc.Lines() {
		// previewcheck has made each reason printable already.
		r.report.Checks = append(r.report.Checks, Check{Name: l.Name, Status: l.Status, Reason: l.Reason})
	}
}

// previewImage checks one previewed repository's image repository: under
// the prefix, one leaf segment, and sha-<default-branch head> published,
// which only fails the check when baseRuns says a preview runs that tag.
func (r *run) previewImage(ctx context.Context, rp v1alpha1.RepositoryPreview, prefix string, baseRuns bool) {
	key, img := rp.Name, rp.Preview.ImageRepository
	if prefix != "" {
		leaf, under := strings.CutPrefix(img, prefix)
		switch {
		case !under:
			r.add(CheckPreviewImage, key, checkreport.Fail, "%s is not under preview-controller's image prefix %s, "+
				"so it refuses to render the Preview", img, prefix)
			return
		case !leafPattern.MatchString(leaf):
			r.add(CheckPreviewImage, key, checkreport.Fail, "%s: %q after the prefix must be one lowercase DNS label "+
				"(letters, digits and inner dashes), or preview-controller refuses to render the Preview", img, leaf)
			return
		}
	}
	repo, err := name.NewRepository(img)
	if err != nil {
		r.add(CheckPreviewImage, key, checkreport.Fail, "%s is not an image repository: %v", img, err)
		return
	}
	head, headErr := r.head(ctx, rp.URL)
	if headErr != nil {
		// The head names the tag to look for; without it, the most that
		// can be said is whether the repository is there and holds
		// commit tags at all.
		shaTags, err := r.shaTags(ctx, repo)
		switch {
		case err != nil:
			r.add(CheckPreviewImage, key, checkreport.Skip, "cannot read the default branch (%v), and %s",
				headErr, r.unreadable(img, err))
		default:
			r.add(CheckPreviewImage, key, checkreport.Skip, "%s is reachable and holds %d sha-<commit> tag%s, but "+
				"which commit heads the default branch is unknown: %v", img, shaTags, plural(shaTags), headErr)
		}
		return
	}
	tag := "sha-" + head.SHA
	desc, err := r.fetch(ctx, img+":"+tag)
	switch code := registryCode(err); {
	case err == nil:
		r.add(CheckPreviewImage, key, checkreport.Pass, "%s:%s, the head of %s, is published (%s); read with your "+
			"registry credentials, not the preview nodes'", img, tag, head.Branch, desc.Digest)
	case code == transport.NameUnknownErrorCode:
		r.add(CheckPreviewImage, key, checkreport.Fail, "the image repository %s does not exist: %v", img, err)
	case code == transport.ManifestUnknownErrorCode || code == "404":
		others := ""
		if n, err := r.shaTags(ctx, repo); err == nil {
			others = fmt.Sprintf(" (it holds %d other sha-<commit> tag%s)", n, plural(n))
		}
		if !baseRuns {
			r.add(CheckPreviewImage, key, checkreport.Skip, "%s:%s, the head of %s, is not published%s, but no "+
				"preview runs it: with one previewed repository, a preview runs only the pull request's head, so "+
				"whether the runtime publisher pushes that is known only once an intent opens one (is the "+
				"PREVIEW_PUBLISH_ENABLED repository variable 'true'?)", img, tag, head.Branch, others)
			return
		}
		r.add(CheckPreviewImage, key, checkreport.Fail, "%s:%s, the head of %s, is not published%s: a preview "+
			"runs sha-<commit> images, the pull request's head and, for a component the intent leaves unchanged, "+
			"the %s head, so the runtime publisher must push both (is the PREVIEW_PUBLISH_ENABLED repository "+
			"variable 'true', and the last publish run green? a push minutes old may still be publishing)",
			img, tag, head.Branch, others, head.Branch)
	default:
		r.add(CheckPreviewImage, key, checkreport.Skip, "%s", r.unreadable(img, err))
	}
}

// unreadable says a registry read was refused or failed with the caller's
// credentials, which is not the same as the preview nodes' being refused.
func (r *run) unreadable(img string, err error) string {
	return fmt.Sprintf("your registry credentials cannot read %s: %v", img, err)
}

// shaTags counts the sha-<40 hex> tags a repository holds, read with the
// caller's registry credentials.
func (r *run) shaTags(ctx context.Context, repo name.Repository) (int, error) {
	tags, err := remote.List(repo, remote.WithContext(ctx), remote.WithAuthFromKeychain(r.keychain()))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tags {
		if shaTagPattern.MatchString(t) {
			n++
		}
	}
	return n, nil
}

// fetch reads one manifest with the caller's registry credentials.
func (r *run) fetch(ctx context.Context, reference string) (*remote.Descriptor, error) {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return nil, err
	}
	return remote.Get(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(r.keychain()))
}

// keychain is the caller's registry credentials; with none configured,
// registries are read anonymously.
func (r *run) keychain() authn.Keychain {
	if r.cfg.Keychain == nil {
		return authn.NewMultiKeychain()
	}
	return r.cfg.Keychain
}

// shortSHA is a commit's abbreviation for a reason.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// plural is "s" unless n is one.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// registryCode classifies a registry error: its first error code, "404"
// for a bare not-found (a HEAD has no body), or "" for anything else (a
// credential or network failure).
func registryCode(err error) transport.ErrorCode {
	var te *transport.Error
	switch {
	case !errors.As(err, &te):
		return ""
	case len(te.Errors) > 0:
		return te.Errors[0].Code
	case te.StatusCode == http.StatusNotFound:
		return "404"
	}
	return ""
}

// orNone names the ConfigMaps a setting was read from, or says there were
// none.
func orNone(from, namespace string) string {
	if from == "" {
		return "any intent-controller ConfigMap in namespace " + namespace
	}
	return from
}
