// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package runnercfg wires the per-harness agent-runner fleet from controller
// flags: the image and credential Secret for each harness, which harnesses a
// deployment enables, and the startup validation that every allowlisted model
// can actually be run. Both job controllers (investigation, remediation) share
// it so the flag surface and enablement rules never drift apart.
package runnercfg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/model"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/provider"
	"github.com/bitwise-media-group/patchy/internal/resourceclass"
	"github.com/bitwise-media-group/patchy/internal/usagemeter"
)

// RegisterFlags adds the per-harness runner flags shared by both job
// controllers: an image per harness (unset = that runner is not configured),
// its credential Secret name/key/env, and the harness restrict list. Mutually
// exclusive with RegisterEvolveFlags within one binary — both register the
// shared credential flags.
func RegisterFlags(f *pflag.FlagSet) {
	f.String("claude-agent-image", "", "claude-agent-runner image (claude CLI); unset disables the claude runner")
	f.String("codex-agent-image", "", "codex-agent-runner image (codex CLI); unset disables the codex runner")
	f.String("copilot-agent-image", "",
		"copilot-agent-runner image (copilot CLI); unset disables the copilot runner")
	f.String("fake-agent-image", "", "fake agent image for dev/e2e (replays fixtures, no credential)")

	registerSharedFlags(f)
}

// RegisterEvolveFlags adds the evolve-runner fleet flags for the evaluation
// controller: an evolve-runner image per harness plus the same shared
// credential Secret flags the finding runners use — one Secret per model
// vendor in the agents namespace, whichever fleet exercises it.
func RegisterEvolveFlags(f *pflag.FlagSet) {
	f.String("evolve-claude-image", "",
		"evolve-runner image with the claude CLI; unset disables the claude evaluation runner")
	f.String("evolve-codex-image", "",
		"evolve-runner image with the codex CLI; unset disables the codex evaluation runner")
	f.String("evolve-copilot-image", "",
		"evolve-runner image with the copilot CLI; unset disables the copilot evaluation runner")
	f.String("evolve-fake-image", "", "fake evolve runner for dev/e2e (replays fixtures, no credential)")

	registerSharedFlags(f)
}

// registerSharedFlags adds the credential and restrict flags common to both
// runner fleets, plus the broker/provider flags brokered claude runners are
// configured with.
func registerSharedFlags(f *pflag.FlagSet) {
	f.String("broker-url", "",
		"egress credential broker base URL (required for the claude runner; claude runs proxy-only)")
	f.String("broker-token-audience", jobs.DefaultBrokerAudience,
		"audience the agent pods' projected broker caller tokens are bound to")
	f.String("claude-provider", provider.Anthropic,
		"model provider behind the broker for claude runs: "+strings.Join(provider.ClaudeNames, ", "))
	f.String("claude-provider-region", "", "provider region (required for bedrock and vertex)")
	f.String("claude-provider-region-prefix", "",
		"bedrock inference-profile prefix override when it cannot be derived from the region")
	f.String("claude-provider-project-id", "", "GCP project id (required for vertex)")
	f.String("claude-model-map", "",
		"canonical=provider-id model overrides, comma separated (required for foundry: deployment names)")
	f.String("claude-provider-env", "",
		"extra NAME=value agent-pod env for the provider, comma separated (credential names rejected)")

	// The --claude-secret* flags stay registered for back-compat, but the
	// claude runner is brokered now and ignores them (a migration notice is
	// logged when they are customized). Codex/copilot still use theirs.
	f.String("claude-secret", "patchy-anthropic", "IGNORED: the claude model credential lives in the egress broker")
	f.String("claude-secret-key", "api-key", "IGNORED: the claude model credential lives in the egress broker")
	f.String("claude-secret-env", "ANTHROPIC_API_KEY",
		"IGNORED: the claude model credential lives in the egress broker")

	f.String("codex-secret", "patchy-openai", "Secret (agent namespace) holding the OpenAI credential")
	f.String("codex-secret-key", "api-key", "key within the OpenAI credential Secret")
	f.String("codex-secret-env", "OPENAI_API_KEY", secretEnvUsage("OpenAI", model.HarnessCodex))

	// Copilot's credential is a GitHub token rather than a model API key, so
	// the default Secret is named for the harness, not a vendor.
	f.String("copilot-secret", "patchy-copilot", "Secret (agent namespace) holding the GitHub Copilot credential")
	f.String("copilot-secret-key", "token", "key within the GitHub Copilot credential Secret")
	f.String("copilot-secret-env", "COPILOT_GITHUB_TOKEN", secretEnvUsage("GitHub Copilot", model.HarnessCopilot))

	f.String("harnesses", "",
		"restrict enabled harnesses to this comma list (default: any harness whose credential Secret exists)")
}

// Runners builds the configured runner fleet from the flags. A harness is a
// candidate runner only when its image flag is set. The claude runner is
// always brokered — its model traffic goes through the egress credential
// broker and no credential enters the pod; codex/copilot keep the Secret
// channel, whose env var is validated against the harness's accepted
// credential channels so a typo'd --codex-secret-env fails at startup rather
// than in the pod.
func Runners(opts *cli.Options) (map[string]jobs.Runner, error) {
	runners := map[string]jobs.Runner{}

	if img := opts.String("claude-agent-image"); img != "" {
		r, err := claudeRunner(opts, img)
		if err != nil {
			return nil, err
		}
		// The finding runner is the one harness that may run a
		// repository-declared image: brokered, so its pod holds no
		// credential for that image to read. It contributes agent-runner
		// and its CLI; codex, copilot (a real credential in-pod) and the
		// fake leave Inject nil and always run their own image.
		r.Inject = claudeInject()
		runners[model.HarnessClaude] = r
	}
	if img := opts.String("codex-agent-image"); img != "" {
		env := opts.String("codex-secret-env")
		if !accepts(model.HarnessCodex, env) {
			return nil, fmt.Errorf("--codex-secret-env %q is not a credential the codex harness accepts (one of %v)",
				env, envKeys(model.HarnessCodex))
		}
		runners[model.HarnessCodex] = jobs.Runner{
			Image: img, Secret: opts.String("codex-secret"),
			SecretKey: opts.String("codex-secret-key"), SecretEnv: env,
		}
	}
	if img := opts.String("copilot-agent-image"); img != "" {
		env := opts.String("copilot-secret-env")
		if !accepts(model.HarnessCopilot, env) {
			return nil, fmt.Errorf("--copilot-secret-env %q is not a credential the copilot harness accepts (one of %v)",
				env, envKeys(model.HarnessCopilot))
		}
		runners[model.HarnessCopilot] = jobs.Runner{
			Image: img, Secret: opts.String("copilot-secret"),
			SecretKey: opts.String("copilot-secret-key"), SecretEnv: env,
		}
	}
	if img := opts.String("fake-agent-image"); img != "" {
		runners[model.HarnessFake] = jobs.Runner{Image: img} // no credential
	}

	if len(runners) == 0 {
		return nil, errors.New("no agent runner configured (set at least one of " +
			"--claude-agent-image / --codex-agent-image / --copilot-agent-image / --fake-agent-image)")
	}
	return runners, nil
}

// claudeInject is what the claude runner image contributes to a Job that
// runs a repository-declared image: agent-runner and the claude CLI, where
// the Dockerfile puts them.
func claudeInject() []string {
	inject := []string{"agent-runner"}
	if h, ok := harness.ByID(model.HarnessClaude); ok {
		inject = append(inject, h.CLI()...)
	}
	return inject
}

// RegisterRepositoryImageFlags adds the repository-declared runner image
// flags both job controllers share: the kill switch, and the
// ephemeral-storage wall a repository image's Job requires.
func RegisterRepositoryImageFlags(f *pflag.FlagSet) {
	f.Bool("repository-images", false,
		"run a Repository's pinned repository-declared runner image (brokered claude runner only); "+
			"off runs every Job on its harness's own image")
	f.String("agent-ephemeral-storage", "",
		"ephemeral-storage request and limit on both agent containers, a quantity such as 8Gi "+
			"(required with --repository-images)")
}

// RepositoryImages reads those flags: whether repository images are on,
// and the ephemeral-storage quantity for every agent Job (empty leaves the
// Job without one, exactly as before the flag existed). A repository image
// without the wall on disk is refused at startup rather than at every Job,
// and so is a quantity of zero or less, a wall no pod could write under.
func RepositoryImages(opts *cli.Options) (enabled bool, ephemeralStorage string, err error) {
	enabled, ephemeralStorage = opts.Bool("repository-images"), opts.String("agent-ephemeral-storage")
	if ephemeralStorage != "" {
		q, err := resource.ParseQuantity(ephemeralStorage)
		if err != nil {
			return false, "", fmt.Errorf("--agent-ephemeral-storage %q: %w", ephemeralStorage, err)
		}
		if q.Sign() <= 0 {
			return false, "", fmt.Errorf("--agent-ephemeral-storage %q must be positive", ephemeralStorage)
		}
	}
	if enabled && ephemeralStorage == "" {
		return false, "", errors.New("--agent-ephemeral-storage is required with --repository-images: " +
			"it bounds the disk a repository-declared image can fill through the pod's emptyDirs")
	}
	return enabled, ephemeralStorage, nil
}

// RegisterNetworkProfileFlags adds the operator network profile flags the
// two controllers that launch write-stage Jobs share (remediation and
// intent; investigation and evaluation never register them): the kill
// switch, the profile set's JSON (netprofile.Parse) and the egress proxy's
// base URL, to which each profile's port is joined. It is registered beside
// RegisterRepositoryImageFlags, which it requires.
func RegisterNetworkProfileFlags(f *pflag.FlagSet) {
	f.Bool("network-profiles-enabled", false,
		"apply operator network profiles to remediate and intent build Jobs that run an accepted repository "+
			"image (requires --repository-images); off applies none from the next Job on")
	f.String("network-profiles", "",
		"the operator network profiles as JSON (an array; the chart renders agent.networkProfiles); "+
			"ignored while --network-profiles-enabled is off")
	f.String("egress-proxy-url", "",
		"the egress proxy's base URL, http://<host> with no port or path (each profile's port is joined to it); "+
			"required with --network-profiles-enabled")
}

// NetworkProfiles reads those flags: whether profiles are on, the parsed
// profile set and the proxy's base URL. Off, the other two are ignored
// (nothing is parsed, so a ConfigMap still carrying profiles while the kill
// switch is thrown starts) and the set is empty. On, startup fails naming
// the flag without --repository-images (a profile only ever applies to a
// Job running an accepted repository image), with no profile, without the
// proxy URL or with one that is not http://<host> alone, or when a profile
// does not validate (an RE2 pattern, a module prefix, a port, ...).
func NetworkProfiles(opts *cli.Options) (enabled bool, set netprofile.Set, base *url.URL, err error) {
	if !opts.Bool("network-profiles-enabled") {
		return false, netprofile.Set{}, nil, nil
	}
	if !opts.Bool("repository-images") {
		return false, netprofile.Set{}, nil, errors.New("--network-profiles-enabled requires --repository-images: " +
			"a network profile only ever applies to a Job running an accepted repository image")
	}
	if set, err = netprofile.Parse([]byte(opts.String("network-profiles"))); err != nil {
		return false, netprofile.Set{}, nil, fmt.Errorf("--network-profiles: %w", err)
	}
	if set.Len() == 0 {
		return false, netprofile.Set{}, nil, errors.New("--network-profiles is empty: " +
			"--network-profiles-enabled needs at least one profile")
	}
	raw := opts.String("egress-proxy-url")
	if raw == "" {
		return false, netprofile.Set{}, nil, errors.New("--egress-proxy-url is required with --network-profiles-enabled")
	}
	if base, err = proxyBase(raw); err != nil {
		return false, netprofile.Set{}, nil, fmt.Errorf("--egress-proxy-url: %w", err)
	}
	return true, set, base, nil
}

// proxyBase admits http://<host> alone: a lowercase DNS name or an IP
// literal, with no port (each profile brings its own), path, query,
// fragment or user.
func proxyBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", raw, err)
	}
	if u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.Port() != "" || u.Path != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("%q is not http://<host> with no port, path, query, fragment or user", raw)
	}
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return u, nil
	}
	if host == "" || host != strings.ToLower(host) || len(validation.IsDNS1123Subdomain(host)) > 0 {
		return nil, fmt.Errorf("%q: host %q is not a lowercase DNS name or an IP", raw, host)
	}
	return u, nil
}

// agentResourceFlags are the default's flags, in resourceclass's order:
// requests.cpu, requests.memory, limits.cpu, limits.memory.
var agentResourceFlags = []string{"agent-cpu-request", "agent-memory-request", "agent-cpu-limit",
	"agent-memory-limit"}

// RegisterAgentResourceFlags adds the agent Jobs' default CPU and memory,
// shared by every controller that launches them (investigation,
// remediation, intent and evaluation): each a quantity on both containers
// of every Job, unset leaving that one off. All unset is today's Jobs, with
// no CPU or memory requests or limits at all. An intent build, revise or
// check-fix run whose repository picks a resource class replaces all four
// with the class's (intent-controller's --intent-resource-classes).
func RegisterAgentResourceFlags(f *pflag.FlagSet) {
	f.String("agent-cpu-request", "", "CPU request on both containers of every agent Job, a quantity such as 500m; "+
		"unset requests none")
	f.String("agent-memory-request", "", "memory request on both containers of every agent Job, a quantity such "+
		"as 2Gi; unset requests none")
	f.String("agent-cpu-limit", "", "CPU limit on both containers of every agent Job; unset sets none (a limit "+
		"throttles browser and Node test suites)")
	f.String("agent-memory-limit", "", "memory limit on both containers of every agent Job, at or above the "+
		"request; unset sets none")
}

// AgentResources reads the default's flags into the resources every agent
// Job gets. Each set quantity must parse and pass resourceclass's checks
// (positive, within the sanity bounds, each request at or below its limit),
// or startup fails naming it: a size the API server would refuse is never
// discovered by a Job.
func AgentResources(opts *cli.Options) (resourceclass.Resources, error) {
	var r resourceclass.Resources
	targets := []**resource.Quantity{&r.Requests.CPU, &r.Requests.Memory, &r.Limits.CPU, &r.Limits.Memory}
	for i, name := range agentResourceFlags {
		v := strings.TrimSpace(opts.String(name))
		if v == "" {
			continue
		}
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return resourceclass.Resources{}, fmt.Errorf("--%s %q: %w", name, v, err)
		}
		*targets[i] = &q
	}
	if err := r.Validate(); err != nil {
		return resourceclass.Resources{}, fmt.Errorf("agent resources (--%s): %w",
			strings.Join(agentResourceFlags, ", --"), err)
	}
	return r, nil
}

// RegisterAgentDNSFlag adds --agent-dns, shared by every controller that
// launches agent Jobs (investigation, remediation, intent and evaluation):
// how the agent pods resolve names.
func RegisterAgentDNSFlag(f *pflag.FlagSet) {
	f.String("agent-dns", string(jobs.DNSCluster),
		"how agent pods resolve names: cluster (the cluster resolver) or none (no resolver: the hosts a Job's "+
			"pod dials, the artifact server and the egress broker, are resolved by this controller at launch and "+
			"pinned in the pod's hosts file; brokered claude and the fake harness only)")
}

// AgentsPausedFlag is the switch RegisterAgentsPausedFlag adds.
const AgentsPausedFlag = "agents-paused"

// RegisterAgentsPausedFlag adds --agents-paused, shared by every controller
// that launches agent Jobs (investigation, remediation, intent and
// evaluation): the cluster-wide pause. While it is set the controller's
// scheduler grants no slot and launches no Job; Jobs already running finish
// and are collected as usual, and pending work waits, in order, until the
// switch is lifted.
func RegisterAgentsPausedFlag(f *pflag.FlagSet) {
	f.Bool(AgentsPausedFlag, false,
		"launch no new agent Jobs (running Jobs finish and are collected; pending work waits until this is unset)")
}

// RegisterAttributionFlag adds --attribution-mode, shared by every
// controller that launches agent Jobs (investigation, remediation, intent
// and evaluation): off (the default) stamps nothing; attribute stamps each
// new run's spec.attributedTo and every agent Job's attributed-to
// annotation (design L1).
func RegisterAttributionFlag(f *pflag.FlagSet) {
	f.String(attribution.Flag, string(attribution.ModeOff),
		"record whom each agent run is for: off stamps nothing; attribute stamps each new run's "+
			"spec.attributedTo and every agent Job's and pod's "+v1alpha1.AnnotationAttributedTo+" annotation")
}

// RegisterUsageFlags adds the --usage-* flags (usagemeter.RegisterFlags),
// shared by every controller that launches agent Jobs: off by default.
func RegisterUsageFlags(f *pflag.FlagSet) { usagemeter.RegisterFlags(f) }

// UsageMeter builds the meter the --usage-* flags configure: nil while
// --usage-measurement is off, a startup error when it is on without a
// usable broker URL.
func UsageMeter(opts *cli.Options, log *slog.Logger) (*usagemeter.Meter, error) {
	return usagemeter.New(usagemeter.Config{
		On:        opts.Bool(usagemeter.FlagMeasurement),
		BrokerURL: opts.String(usagemeter.FlagBrokerURL),
		TokenFile: opts.String(usagemeter.FlagTokenFile),
	}, log)
}

// Attribution reads --attribution-mode, refusing an unknown mode at
// startup rather than running unattributed by mistake.
func Attribution(opts *cli.Options) (attribution.Mode, error) {
	mode, err := attribution.ParseMode(opts.String(attribution.Flag))
	if err != nil {
		return "", fmt.Errorf("--%s: %w", attribution.Flag, err)
	}
	return mode, nil
}

// AgentDNS reads --agent-dns and holds it to the harnesses this controller
// runs: under none, a harness whose pod dials its model API by name (codex,
// copilot: a credential in the pod, no broker) could never reach it, so
// startup fails naming it rather than every one of its Jobs failing.
// enabled is the resolved harness set; a configured runner left disabled
// never runs, so it does not count.
func AgentDNS(opts *cli.Options, runners map[string]jobs.Runner, enabled []string) (jobs.DNSMode, error) {
	mode, err := jobs.ParseDNSMode(opts.String("agent-dns"))
	if err != nil {
		return "", fmt.Errorf("--agent-dns: %w", err)
	}
	if mode != jobs.DNSNone {
		return mode, nil
	}
	for _, id := range enabled {
		if jobs.NeedsResolver(runners[id]) {
			return "", fmt.Errorf("--agent-dns %s: the %s harness dials its model API by name and needs a resolver; "+
				"disable it (--harnesses) or keep --agent-dns %s", jobs.DNSNone, id, jobs.DNSCluster)
		}
	}
	return mode, nil
}

// EvolveRunners builds the evolve-runner fleet from the flags, mirroring
// Runners: a harness is a candidate only when its evolve image flag is set.
// The claude evolve runner is brokered like its finding sibling; codex and
// copilot reuse the shared --<harness>-secret flags, with the credential env
// var validated against the harness's accepted channels.
func EvolveRunners(opts *cli.Options) (map[string]jobs.Runner, error) {
	runners := map[string]jobs.Runner{}

	if img := opts.String("evolve-claude-image"); img != "" {
		r, err := claudeRunner(opts, img)
		if err != nil {
			return nil, err
		}
		runners[model.HarnessClaude] = r
	}
	// Flag names derive from the harness ids: evolve-<id>-image and
	// <id>-secret{,-key,-env}.
	for _, h := range []string{model.HarnessCodex, model.HarnessCopilot} {
		img := opts.String("evolve-" + h + "-image")
		if img == "" {
			continue
		}
		env := opts.String(h + "-secret-env")
		if !accepts(h, env) {
			return nil, fmt.Errorf("--%s-secret-env %q is not a credential the %s harness accepts (one of %v)",
				h, env, h, envKeys(h))
		}
		runners[h] = jobs.Runner{
			Image: img, Secret: opts.String(h + "-secret"),
			SecretKey: opts.String(h + "-secret-key"), SecretEnv: env,
		}
	}
	if img := opts.String("evolve-fake-image"); img != "" {
		runners[model.HarnessFake] = jobs.Runner{Image: img} // no credential
	}

	if len(runners) == 0 {
		return nil, errors.New("no evolve runner configured (set at least one of " +
			"--evolve-claude-image / --evolve-codex-image / --evolve-copilot-image / --evolve-fake-image)")
	}
	return runners, nil
}

// claudeRunner builds the brokered claude runner shared by both fleets: the
// provider config from the flags, the derived model map, and the gateway env
// the pod gets. --broker-url is required — claude-without-broker is not a
// configuration. The legacy --claude-secret* flags are ignored; customizing
// them earns a migration notice rather than an error.
func claudeRunner(opts *cli.Options, img string) (jobs.Runner, error) {
	pcfg, err := ClaudeProviderConfig(opts)
	if err != nil {
		return jobs.Runner{}, err
	}
	mm, err := provider.EffectiveModelMap(pcfg, model.Builtins())
	if err != nil {
		return jobs.Runner{}, err
	}
	if opts.String("claude-secret") != "patchy-anthropic" ||
		opts.String("claude-secret-key") != "api-key" ||
		opts.String("claude-secret-env") != "ANTHROPIC_API_KEY" {
		opts.Log.Warn("--claude-secret* is ignored: claude runs proxy-only and the model credential " +
			"lives in the egress broker; move the Secret to the broker's namespace and drop these flags")
	}
	return jobs.Runner{Image: img, Brokered: true, Env: provider.Env(pcfg, mm)}, nil
}

// ClaudeProviderConfig builds and validates the claude provider config from
// the shared flags.
func ClaudeProviderConfig(opts *cli.Options) (provider.Config, error) {
	if opts.String("broker-url") == "" {
		return provider.Config{}, errors.New(
			"--broker-url is required when a claude runner is configured (claude runs proxy-only)")
	}
	mm, err := parseKVList(opts.String("claude-model-map"))
	if err != nil {
		return provider.Config{}, fmt.Errorf("--claude-model-map: %w", err)
	}
	extra, err := parseKVList(opts.String("claude-provider-env"))
	if err != nil {
		return provider.Config{}, fmt.Errorf("--claude-provider-env: %w", err)
	}
	cfg := provider.Config{
		Name:         opts.String("claude-provider"),
		BrokerURL:    opts.String("broker-url"),
		Region:       opts.String("claude-provider-region"),
		RegionPrefix: opts.String("claude-provider-region-prefix"),
		ProjectID:    opts.String("claude-provider-project-id"),
		ModelMap:     mm,
		ExtraEnv:     extra,
	}
	if err := cfg.Validate(); err != nil {
		return provider.Config{}, err
	}
	// Validate refuses the gateway and credential names; the names a Job sets
	// itself are the jobs package's to know, and provider cannot import it.
	// Both Job builders drop them from the runner env anyway, so refusing them
	// here turns a silent drop into an error the operator sees. Each fleet's
	// names are refused on every controller: the chart stamps one provider
	// env into all of them, and neither fleet has a use for the other's.
	perJob := slices.Concat(jobs.PerJobEnvNames(), jobs.EvalJobEnvNames())
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		if slices.Contains(perJob, k) {
			return provider.Config{}, fmt.Errorf(
				"--claude-provider-env: %s is set per Job by patchy; it cannot be set here", k)
		}
	}
	return cfg, nil
}

// parseKVList parses a comma-separated NAME=value flag into a map.
func parseKVList(s string) (map[string]string, error) {
	items := SplitList(s)
	if len(items) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("entry %q is not NAME=value", item)
		}
		out[k] = v
	}
	return out, nil
}

// Restrict parses the --harnesses restrict list; empty means auto-detect.
func Restrict(opts *cli.Options) []string { return SplitList(opts.String("harnesses")) }

// SplitList splits a comma-separated flag value, trimming blanks.
func SplitList(s string) []string {
	var out []string
	for tok := range strings.SplitSeq(s, ",") {
		if tok = strings.TrimSpace(tok); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// Resolve probes the configured runners' credentials, computes the enabled
// harness set, and validates coverage: the allowlist must be fully runnable,
// every requiredModel (the investigate/remediate defaults, canonical ids)
// must resolve to an enabled harness, and a brokered claude runner's model
// map must cover every claude-resolving model — a foundry gap is a startup
// error here, not a mid-run failure. It also probes the broker's readiness,
// non-fatally: the broker Deployment may simply not be up yet, and a
// deployment-ordering deadlock would be worse than a warning. Returns the
// sorted enabled harness ids.
func Resolve(ctx context.Context, opts *cli.Options, cs kubernetes.Interface, namespace string,
	runners map[string]jobs.Runner, restrict, allowlist []string, requiredModels ...string) ([]string, error) {
	return resolve(ctx, opts, cs, namespace, runners, restrict, allowlist, true, requiredModels...)
}

// ResolveWithoutBrokerProbe performs the same startup validation as Resolve
// without a controller-side readiness request to the broker. Use it where a
// NetworkPolicy deliberately allows agent pods, but not the controller, to
// reach the broker. Broker reachability is not a launch or readiness gate.
func ResolveWithoutBrokerProbe(ctx context.Context, opts *cli.Options, cs kubernetes.Interface, namespace string,
	runners map[string]jobs.Runner, restrict, allowlist []string, requiredModels ...string) ([]string, error) {
	return resolve(ctx, opts, cs, namespace, runners, restrict, allowlist, false, requiredModels...)
}

func resolve(ctx context.Context, opts *cli.Options, cs kubernetes.Interface, namespace string,
	runners map[string]jobs.Runner, restrict, allowlist []string, probe bool, requiredModels ...string) ([]string, error) {
	enabled, err := jobs.ResolveRunners(ctx, cs, namespace, runners, restrict)
	if err != nil {
		return nil, err
	}
	if len(enabled) == 0 {
		return nil, errors.New("no harness enabled: no configured runner has its credential Secret in the agent namespace")
	}
	set := harness.EnabledSet(enabled)
	if err := harness.ValidateAllowlist(model.Builtins(), allowlist, set); err != nil {
		return nil, fmt.Errorf("model allowlist: %w", err)
	}
	for _, id := range requiredModels {
		if id == "" {
			continue
		}
		if _, _, err := ResolveHarness(id, enabled); err != nil {
			return nil, err
		}
	}
	if r, ok := runners[model.HarnessClaude]; ok && r.Brokered {
		pcfg, err := ClaudeProviderConfig(opts)
		if err != nil {
			return nil, err
		}
		mm, err := provider.EffectiveModelMap(pcfg, model.Builtins())
		if err != nil {
			return nil, err
		}
		required := append(slices.Clone(allowlist), requiredModels...)
		if err := provider.ValidateCoverage(pcfg, model.Builtins(), mm, required); err != nil {
			return nil, err
		}
		if probe {
			probeBroker(ctx, pcfg.BrokerURL, opts.Log)
		}
	}
	return enabled, nil
}

// probeBroker checks the broker's readiness endpoint once at startup, purely
// advisorily: it surfaces a missing model credential or an unreachable
// broker in the controller's log without making deployment order fatal.
func probeBroker(ctx context.Context, brokerURL string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	readyz := strings.TrimRight(brokerURL, "/") + "/readyz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, readyz, nil)
	if err != nil {
		log.Warn("egress broker probe failed", "url", readyz, "error", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn("egress broker not reachable yet; claude jobs will fail until it is",
			"url", readyz, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.Warn("egress broker is not ready; check its /readyz and credential sources",
			"url", readyz, "status", resp.StatusCode)
	}
}

// ResolveHarness resolves a canonical model id to its harness and CLI model-id
// given the enabled set, erroring when the model is unknown or unrunnable.
func ResolveHarness(canonical string, enabled []string) (harnessID, cliModelID string, err error) {
	m, ok := model.ModelByID(model.Builtins(), canonical)
	if !ok {
		return "", "", fmt.Errorf("model %q is not in the model registry", canonical)
	}
	h, cliID, ok := harness.ResolveModel(m, harness.EnabledSet(enabled))
	if !ok {
		return "", "", fmt.Errorf("model %q needs one of harnesses %v enabled, but only %v are",
			canonical, m.SupportedHarnessIDs(), enabled)
	}
	return h, cliID, nil
}

// secretEnvUsage renders a --<harness>-secret-env help string from the
// harness's own accepted channels, so the flag can never advertise a set that
// the startup validation below disagrees with — the two drifted apart once
// already, leaving the claude flag naming two of its three channels.
func secretEnvUsage(vendor, harnessID string) string {
	return fmt.Sprintf("env var the %s credential is injected as (one of %s)",
		vendor, strings.Join(envKeys(harnessID), ", "))
}

func accepts(harnessID, env string) bool { return slices.Contains(envKeys(harnessID), env) }

func envKeys(harnessID string) []string {
	if h, ok := harness.ByID(harnessID); ok {
		return h.EnvKeys()
	}
	return nil
}
