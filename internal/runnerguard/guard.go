// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerguard

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// Guard is one job controller's repository-image launch policy.
type Guard struct {
	// Enabled is the controller's --repository-images kill switch: while
	// false no launch copies a pinned image, so a flip takes effect at the
	// next Job without touching any CR.
	Enabled bool
	// Breaker refuses repository images once a sandbox probe has found
	// NetworkPolicy unenforced; nil never trips.
	Breaker *Breaker
	// ProfilesEnabled is the controller's --network-profiles-enabled kill
	// switch, Profiles the operator's profile set and ProxyBase the egress
	// proxy's http://<host> base URL (runnercfg.NetworkProfiles). While the
	// switch is off no launch requests a profile.
	ProfilesEnabled bool
	Profiles        netprofile.Set
	ProxyBase       *url.URL
	// Forges lists the hosts of the Forge CRs the controller can see, which
	// CheckForges holds a profile's registry grants to. Nil refuses every
	// profile with grants: a check that cannot run must not pass.
	Forges ForgeHosts
}

// ForgeHosts lists the hosts of the Forge CRs in a namespace
// (forge.Store.Hosts).
type ForgeHosts interface {
	Hosts(ctx context.Context, namespace string) ([]string, error)
}

// Reasons Pin reports for not using an image the Repository pinned.
const (
	SkipDisabled = "repository images are disabled"
	SkipBreaker  = "the sandbox breaker is tripped"
	SkipRevived  = "a human revived the finding, so it runs on the default image"
)

// Pin copies the Repository's pinned runner image into spec when this launch
// may run it. It reports why it did not when an image was pinned but is not
// used, and "" otherwise (copied, or nothing pinned: no declaration, a
// not-applicable devcontainer.json, or a rejected declaration — the last
// carries no image, which is how a Repository rejected under onReject
// default runs the default image without a human step).
//
// What Pin copies is a request, not a record: jobs.Client.Create decides
// whether injection actually happens and returns what the Job runs, and that
// return value is the only thing a launch may record.
func (g Guard) Pin(spec *jobs.Spec, repo *v1alpha1.Repository, fnd *v1alpha1.Finding) string {
	ri := repo.Status.RunnerImage
	if ri == nil || ri.Image == "" {
		return ""
	}
	switch {
	case !g.Enabled:
		return SkipDisabled
	case g.Breaker.Tripped():
		return SkipBreaker
	case Revived(fnd):
		return SkipRevived
	}
	spec.RunnerImage = ri.Image
	spec.RunnerSearchPath = ri.SearchPath
	spec.RunnerImageManifest = ri.Manifest
	return ""
}

// Further reasons PinFor reports: a launch that requires the repository's
// image has none to run.
const (
	SkipNoImage  = "the repository declares no runner image patchy can use"
	SkipRejected = "the repository's runner-image declaration was rejected"
)

// PinFor is Pin for a launch that requires the repository's image: an
// intent's build or revise run, which has no default-image fallback (the
// default image carries no toolchain, so a build there would open an
// untested pull request). It copies the Repository's pinned image into spec
// only when source-controller accepted one — Image set and not Rejected,
// since a Repository rejected under onReject default is Ready all the same
// — and the guard allows it, and returns "" exactly then. Otherwise it
// returns why not (SkipRejected, SkipNoImage, SkipDisabled or SkipBreaker,
// in that order) and leaves spec untouched, so the caller blocks instead of
// launching.
//
// It has no counterpart to Pin's revival rule, which moves a revived
// Finding to the default image: a launch that requires the repository's
// image has no default to move to, and an intent brought back by its
// trigger label is a new plan and a new approval, a fresh human decision,
// not a revival.
//
// Like Pin, what PinFor copies is a request: jobs.Client.Create decides and
// returns what the Job runs, and a caller requiring the image must still
// refuse a Job whose returned stamp is not the repository's.
func (g Guard) PinFor(spec *jobs.Spec, repo *v1alpha1.Repository) string {
	ri := repo.Status.RunnerImage
	switch {
	case ri != nil && ri.Rejected != "":
		return SkipRejected
	case ri == nil || ri.Image == "":
		return SkipNoImage
	case !g.Enabled:
		return SkipDisabled
	case g.Breaker.Tripped():
		return SkipBreaker
	}
	spec.RunnerImage = ri.Image
	spec.RunnerSearchPath = ri.SearchPath
	spec.RunnerImageManifest = ri.Manifest
	return ""
}

// Reasons Profile reports for not requesting a profile a repository
// would get.
const (
	SkipProfilesDisabled = "network profiles are disabled"
	SkipProfileAmbiguous = "the repository matches more than one network profile, so it gets none"
)

// Profile requests, on a spec that already carries the Repository's
// accepted image (Pin or PinFor copied it), the one operator network
// profile its repository matches, and returns the profile's name. It
// returns why not when a profile would otherwise be considered and is not
// applied, and ("", "") when there is nothing to say: no image (nothing
// was pinned, or Pin declined it: a revived finding, the kill switch, the
// breaker), a Job that may never carry a profile (jobs.ProfileAllowed: a
// remediate Job of kind remediation or a build Job of kind intent only),
// profiles off with none configured, or no profile matching. Profiles off
// with some configured, the breaker tripped, or a repository matching two
// profiles (fail closed) is a reason.
//
// Like Pin, what Profile sets is a request: jobs.Client.Create decides
// whether the profile is applied and returns what the Job carries, and that
// return value is the only thing a launch may record.
func (g Guard) Profile(spec *jobs.Spec, repoURL string) (applied, skipped string) {
	if spec.RunnerImage == "" || !jobs.ProfileAllowed(spec.Phase, spec.Kind) {
		return "", ""
	}
	switch {
	case !g.ProfilesEnabled || g.ProxyBase == nil:
		if g.Profiles.Len() == 0 {
			return "", ""
		}
		return "", SkipProfilesDisabled
	case g.Breaker.Tripped():
		return "", SkipBreaker
	}
	p, reason := g.Profiles.For(repoURL)
	switch reason {
	case "":
	case netprofile.ReasonAmbiguous:
		return "", SkipProfileAmbiguous
	default:
		return "", ""
	}
	spec.NetworkProfile = &jobs.NetworkProfile{
		Name:          p.Name,
		Route:         p.Route,
		Registries:    p.Registries,
		ProxyURL:      g.ProxyBase.Scheme + "://" + net.JoinHostPort(g.ProxyBase.Hostname(), strconv.Itoa(p.Port)),
		BakedModCache: p.BakedModCache,
	}
	return p.Name, ""
}

// CheckForges refuses, with an error wrapping jobs.ErrNetworkProfileRefused
// that names the host, a requested profile any of whose registry grants
// names a forge: a grant host (the registry's URL host, a file host, or a
// redirect pattern) that is, lies below or matches the host of a Forge CR
// in namespace. The chart and the proxy cannot see Forge CRs (a GitHub
// Enterprise host configured only in patchy-config), so the job
// controllers, which list them anyway, run this check at launch. A spec
// with no profile, or a profile without grants (step 1), is never checked
// and costs no list. A list failure is returned as is, for a retry; with
// no Forges lister configured, a profile with grants is refused.
func (g Guard) CheckForges(ctx context.Context, namespace string, spec *jobs.Spec) error {
	p := spec.NetworkProfile
	if p == nil || len(p.Registries) == 0 {
		return nil
	}
	if g.Forges == nil {
		return fmt.Errorf("%w: profile %s holds registry grants and no Forge list is configured to check "+
			"them against", jobs.ErrNetworkProfileRefused, p.Name)
	}
	hosts, err := g.Forges.Hosts(ctx, namespace)
	if err != nil {
		return fmt.Errorf("list the Forges a registry grant may not name: %w", err)
	}
	return ForgeRefusal(p, hosts)
}

// ForgeRefusal is CheckForges' decision over a listed set of Forge hosts:
// nil when no grant host is, lies below or matches one of them, an error
// wrapping jobs.ErrNetworkProfileRefused naming the host otherwise.
func ForgeRefusal(p *jobs.NetworkProfile, forgeHosts []string) error {
	if p == nil || len(forgeHosts) == 0 {
		return nil
	}
	for _, gr := range p.Registries {
		for _, h := range gr.Hosts {
			for _, f := range forgeHosts {
				// DeniedHosts below skips IP literals (the proxy judges
				// addresses); a Forge on an IP is still refused by name.
				if strings.EqualFold(h, f) {
					return fmt.Errorf("%w: profile %s grant %s names %s, the host of a Forge",
						jobs.ErrNetworkProfileRefused, p.Name, gr.Name, h)
				}
			}
		}
		if err := gr.DeniedHosts(netprofile.DenyContext{Hosts: forgeHosts}); err != nil {
			return fmt.Errorf("%w: profile %s: %w (a Forge's host)", jobs.ErrNetworkProfileRefused, p.Name, err)
		}
	}
	return nil
}

// ProxyHostForms are the spellings of the egress proxy's host that a
// changeset of a profile run may not contain (changeset.Rules.Forbidden):
// lockfiles and configuration must keep the registry's canonical URLs.
// For an in-cluster Service name <svc>.<ns>.svc.cluster.local they are
// that name, <svc>.<ns>.svc, <svc>.<ns> and http://<svc>; for any other
// host with a dot, the host itself; for a single-label host only
// http://<host>, since the bare word would refuse unrelated text. No
// proxy base is none.
func (g Guard) ProxyHostForms() []string {
	if g.ProxyBase == nil {
		return nil
	}
	host := strings.ToLower(g.ProxyBase.Hostname())
	if host == "" {
		return nil
	}
	if svcNS, ok := strings.CutSuffix(host, ".svc.cluster.local"); ok && strings.Count(svcNS, ".") == 1 {
		svc, _, _ := strings.Cut(svcNS, ".")
		return []string{host, svcNS + ".svc", svcNS, "http://" + svc}
	}
	if !strings.Contains(host, ".") {
		return []string{"http://" + host}
	}
	return []string{host}
}

// Revived reports whether a human brought the finding back from a terminal
// phase — approve on a HandedOff finding, retry on a Failed one — which the
// append-only phase log records as an earlier HandedOff or Failed entry.
// Such a finding runs on the default image from then on: whatever sent it
// to a human (an incompatible image, an ignore verdict held because the run
// used the repository's image, exhausted attempts) would otherwise be
// retried in the same environment. A remediation launch still holds such a
// finding for a human (HeldCondition) until one accepts an untested run on
// the default image; an investigation, read-only, just runs there.
func Revived(fnd *v1alpha1.Finding) bool {
	for _, pt := range fnd.Status.PhaseTimes {
		if pt.Phase == v1alpha1.PhaseHandedOff || pt.Phase == v1alpha1.PhaseFailed {
			return true
		}
	}
	return false
}

// PullGrace is how long a repository-image pod may sit waiting on its agent
// container before a collector judges the wait; PullPoll paces the looks
// after it until the container has started.
const (
	PullGrace = 2 * time.Minute
	PullPoll  = 30 * time.Second
)

// Pending decides what a collector does with a Job that has not finished.
// For a default-image Job, or a finished one, it returns nothing: the Job
// watch reports completion, as it always has. For a repository-image Job it
// returns a requeue until the pull grace (timed from the Job's creation) has
// passed — whatever the pod reports, since a pod that has not yet reported a
// container status is indistinguishable from one about to fail its pull —
// then a failure detail once the agent container is stuck on a pull failure
// that will not heal (InvalidImageName, CreateContainerConfigError, or a pull
// error saying the manifest is unknown or not found), a slower requeue until
// the agent container has started, and nothing once it has. The slower
// requeue covers a container waiting on anything else and a pod with no
// container status at all (unscheduled, or waiting for a node past the
// grace): a pull that fails once it is scheduled never mutates the Job, so
// only these looks would see it. Any other pull error keeps waiting for the
// Job's activeDeadlineSeconds.
func Pending(st jobs.Status, now time.Time) (time.Duration, string) {
	if st.Done || st.RunnerImageSource != v1alpha1.RunnerImageSourceRepository {
		return 0, ""
	}
	if elapsed := now.Sub(st.Created); elapsed < PullGrace {
		return PullGrace - elapsed, ""
	}
	if deterministicPull(st.Waiting, st.WaitingMessage) {
		detail := st.Waiting
		if st.WaitingMessage != "" {
			detail += ": " + st.WaitingMessage
		}
		return 0, "agent image pull failed: " + detail
	}
	if !st.AgentStarted {
		return PullPoll, ""
	}
	return 0, ""
}

// deterministicPull reports a waiting state no amount of backoff will clear.
// The kubelet alternates ErrImagePull with ImagePullBackOff, and newer ones
// carry the registry's error on both, so the message is read on either.
func deterministicPull(reason, message string) bool {
	switch reason {
	case "InvalidImageName", "CreateContainerConfigError":
		return true
	case "ErrImagePull", "ImagePullBackOff":
		msg := strings.ToLower(message)
		return strings.Contains(msg, "manifest unknown") || strings.Contains(msg, "not found")
	}
	return false
}

// SandboxUnenforced leads the failure reason a collector records when a
// repository-image Job's prepare init refused to hand over; SandboxReason is
// the whole of it.
const (
	SandboxUnenforced = "SandboxUnenforced"
	SandboxReason     = SandboxUnenforced + ": the sandbox probe found the agent pod's egress open " +
		"(prepare exited 78): NetworkPolicy is not enforced, so repository-declared runner images " +
		"are refused until the controller restarts"
)

// RefusedCondition is the condition a collector sets on a run whose Job
// SandboxRefused judged refused; the run's attempt then does not count
// toward MaxAttempts.
func RefusedCondition(generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               v1alpha1.ConditionSandboxRefused,
		Status:             metav1.ConditionTrue,
		Reason:             SandboxUnenforced,
		Message:            SandboxReason,
		ObservedGeneration: generation,
	}
}

// Refused reports whether a run's conditions record that the sandbox probe
// refused it (RefusedCondition), so its attempt is not counted.
func Refused(conds []metav1.Condition) bool {
	return meta.IsStatusConditionTrue(conds, v1alpha1.ConditionSandboxRefused)
}

// SandboxRefused reports whether a repository-image Job's prepare init
// exited with jobs.ExitSandboxUnenforced: the probe found egress open, the
// agent container never started, and there is no log to read. A default
// Job's init is never judged here: it runs the probe only in its wait mode,
// which exits 0 whatever it finds, and its prepare script guards even that
// line with an echo of the exit status, so a default init cannot exit
// ExitSandboxUnenforced. Keep both true before loosening the annotation gate.
func SandboxRefused(st jobs.Status) bool {
	return st.RunnerImageSource == v1alpha1.RunnerImageSourceRepository &&
		st.InitExitCode != nil && *st.InitExitCode == jobs.ExitSandboxUnenforced
}

// HeldReason leads the condition a remediation launch records when it holds
// a finding instead of falling back to the default image.
const HeldReason = "RepositoryImageUnavailable"

// HeldDetail is why a launch held a finding whose repository declares an
// accepted image this launch cannot run; skipped is Pin's reason.
func HeldDetail(skipped string) string {
	return "the repository declares an agent image but this run cannot use it (" + skipped + "), and " +
		"patchy's default image carries none of the repository's toolchain, so a fix there could not be " +
		"built or tested; approve the finding to remediate it on the default image anyway, untested"
}

// HeldCondition is the condition a remediation launch sets on a run it held
// (v1alpha1.ConditionDefaultImageHeld); the run's attempt then does not
// count toward MaxAttempts, and a later launch for the finding runs the
// default image.
func HeldCondition(generation int64, skipped string) metav1.Condition {
	return metav1.Condition{
		Type:               v1alpha1.ConditionDefaultImageHeld,
		Status:             metav1.ConditionTrue,
		Reason:             HeldReason,
		Message:            HeldDetail(skipped),
		ObservedGeneration: generation,
	}
}

// Held reports whether a run's conditions record that its launch was held
// (HeldCondition) rather than run on the default image.
func Held(conds []metav1.Condition) bool {
	return meta.IsStatusConditionTrue(conds, v1alpha1.ConditionDefaultImageHeld)
}
