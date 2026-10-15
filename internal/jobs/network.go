// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/agentrun"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// NetworkProfile is the operator network profile a launching controller
// asks a Job to carry (Spec.NetworkProfile): the dependency routes through
// the egress proxy, the public go route and any registry grants. Applied,
// it adds exactly three things to the Job and nothing else: the profile
// label on the Job and its pod (the identity both NetworkPolicies on the
// profile's port select), the proxy's host in the pod's hosts file under
// DNSNone, and the package managers' environment in the agent container
// (profileEnv, then TunnelEnv when the profile lists tunnel hosts).
type NetworkProfile struct {
	// Name is the profile's name (netprofile's grammar); the label is
	// netprofile.LabelKey(Name).
	Name string
	// Route is the public dependency route: netprofile.RouteGo,
	// netprofile.RouteTunnel (a tunnel to Hosts only), or "" for a profile
	// that holds registry grants only.
	Route string
	// Registries are the profile's registry grants, as the operator's
	// profile set holds them (at most one per ecosystem). Each becomes its
	// ecosystem's environment (§5 of the step-2 spec); none, as on every
	// step-1 profile, leaves the Job exactly as it was.
	Registries []netprofile.Grant
	// ProxyURL is the egress proxy's listener for this profile:
	// http://<host>:<port>, with no path. The controller joins the proxy's
	// base URL and the profile's port.
	ProxyURL string
	// BakedModCache is the repository image's GOMODCACHE, served read-only
	// after the proxy; empty means the network only. Go route only.
	BakedModCache string
	// Hosts are the profile's tunnel hosts (step 3), exact public names
	// the proxy opens a CONNECT tunnel to on 443; none means no tunnel.
	// With hosts the Job also gets the tunnel environment (TunnelEnv).
	Hosts []string
	// Clients are the client presets (netprofile.Clients) applied beside
	// the tunnel; only with Hosts.
	Clients []string
}

// ErrNetworkProfileRefused wraps every refusal of a Spec's NetworkProfile: a
// profile on a Job that may never carry one, or one that does not hold
// together. Each is a contradiction in what the controller asked for, never
// a transient failure, so a caller settles the launch instead of retrying.
var ErrNetworkProfileRefused = errors.New("jobs: network profile refused")

// The run kinds a profile may meet; NameFor's discriminators.
const (
	kindRemediation = string(v1alpha1.RunKindRemediation)
	kindIntent      = "intent"
)

// profilePair is one (Phase, Kind) a Job is built for.
type profilePair struct{ phase, kind string }

// allowedProfilePairs are the only Jobs a profile may be applied to: a
// Finding's remediate run and an intent's build (which covers revise and
// check-fix, both build-phase Jobs). The pair is judged as one: a build
// Job of the remediation kind, or a remediate Job of the intent kind, is
// neither.
var allowedProfilePairs = map[profilePair]bool{
	{string(agentrun.PhaseRemediate), kindRemediation}: true,
	{string(agentrun.PhaseBuild), kindIntent}:          true,
}

// ProfileAllowed reports whether a Job of phase and kind may carry a
// network profile at all: exactly (remediate, remediation) or (build,
// intent), judged as a pair. Create refuses a profile anywhere else; a
// launching controller asks before it requests one.
func ProfileAllowed(phase, kind string) bool { return allowedProfilePairs[profilePair{phase, kind}] }

// profileRefusal reports why a Spec's NetworkProfile must not be built
// into any Job, or nil when it may (or when there is none). It is judged
// before injection is decided, so a profile asked for on the wrong Job is a
// refusal even where it would have been dropped: investigate, plan and
// evaluation Jobs never carry one, nor a Job reading Trees. The profile
// itself must hold together, since its fields become the pod's label and
// its go environment: a name in netprofile's grammar, the go route, an
// http proxy URL of a host and an explicit port alone, and an absolute,
// clean baked cache directory or none.
func profileRefusal(spec Spec) error {
	p := spec.NetworkProfile
	if p == nil {
		return nil
	}
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !allowedProfilePairs[profilePair{spec.Phase, spec.Kind}] {
		refuse("a %s Job of kind %s never carries a network profile; only a remediate Job of kind %s or a "+
			"build Job of kind %s may", spec.Phase, spec.Kind, kindRemediation, kindIntent)
	}
	if len(spec.Trees) > 0 {
		refuse("a Job with trees never carries a network profile")
	}
	if err := netprofile.ValidName(p.Name); err != nil {
		refuse("%v", err)
	}
	switch {
	case p.Route == "" && len(p.Registries) == 0:
		refuse("a profile with no route must hold registry grants; it would open nothing")
	case p.Route != "" && p.Route != netprofile.RouteGo && p.Route != netprofile.RouteTunnel:
		refuse("route %q is not supported (only %q or %q)", p.Route, netprofile.RouteGo, netprofile.RouteTunnel)
	}
	errs = append(errs, grantRefusals(p)...)
	errs = append(errs, tunnelRefusals(p)...)
	if err := validProxyURL(p.ProxyURL); err != nil {
		refuse("%v", err)
	}
	if b := p.BakedModCache; b != "" && (!strings.HasPrefix(b, "/") || path.Clean(b) != b ||
		strings.ContainsAny(b, ",\x00") || strings.ContainsFunc(b, isSpaceOrControl)) {
		refuse("baked module cache %q is not an absolute clean path", b)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrNetworkProfileRefused, errors.Join(errs...))
}

// grantRefusals are why a profile's registry grants do not hold together:
// a grant that does not validate, two grants of one ecosystem, or grants
// the agent could not be told of (profileEnv must be able to encode them).
func grantRefusals(p *NetworkProfile) []error {
	var errs []error
	seen := map[string]bool{}
	for _, g := range p.Registries {
		if err := g.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		if seen[g.Ecosystem] {
			errs = append(errs, fmt.Errorf("two %s grants: a package manager takes one index from the environment",
				g.Ecosystem))
		}
		seen[g.Ecosystem] = true
	}
	if len(errs) == 0 && len(p.Registries) > 0 {
		if _, err := netprofile.EncodePodGrants(podGrants(p)); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// isSpaceOrControl reports a byte no path in GOPROXY's list may hold.
func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }

// validProxyURL admits http://<host>:<port> and nothing more: a DNS-1123
// host (or an IP literal), an explicit port, and no user, path, query or
// fragment, since the go command's GOPROXY entry is this URL plus /go.
func validProxyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("proxy URL %q: %w", raw, err)
	}
	if u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return fmt.Errorf("proxy URL %q is not http://<host>:<port> with no path, query, fragment or user", raw)
	}
	host, port := u.Hostname(), u.Port()
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return fmt.Errorf("proxy URL %q has no explicit port", raw)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if host != strings.ToLower(host) || len(validation.IsDNS1123Subdomain(host)) > 0 {
		return fmt.Errorf("proxy URL %q: host %q is not a lowercase DNS name or an IP", raw, host)
	}
	return nil
}

// appliesProfile reports whether a Job carries its Spec's profile: only
// with Config.NetworkProfiles on, a profile asked for, the Job
// running the repository's image (injects: the repository-image kill
// switch, a pinned image and a runner with binaries to inject), on an
// allowed (Phase, Kind) pair and with no trees. Anything else that
// profileRefusal let through is dropped silently, exactly like an
// unhonoured RunnerImage: the caller cannot know Create's injection
// decision, and the read-back (networkProfileOf) reports what runs.
func (c *Client) appliesProfile(runner Runner, spec Spec) bool {
	return c.cfg.NetworkProfiles && spec.NetworkProfile != nil && c.injects(runner, spec) &&
		allowedProfilePairs[profilePair{spec.Phase, spec.Kind}] && len(spec.Trees) == 0
}

// patternNone is the value of every go pattern variable a profile owns:
// non-empty, so the go command never falls through to the image's
// $GOROOT/go.env for it, and matching no real module path, since .invalid
// is a reserved top-level domain.
const patternNone = "patchy.invalid"

// Where a profile Job's go command keeps its writable state: the root
// filesystem is read-only, and the checksum database's state lives under
// GOPATH, not GOMODCACHE.
const (
	profileGoPath     = "/tmp/go"
	profileGoModCache = profileGoPath + "/pkg/mod"
)

// goProfileNames are the names goProfileEnv owns on a profile Job, in the
// order it sets them. None is reserved (an image's ENV may name them, and
// every scaffolded image sets GOPROXY=off, GOMODCACHE and GOTOOLCHAIN); a
// profile Job owns them by dropping whatever else set them and appending
// its own value, the injectEnv way.
var goProfileNames = []string{
	"GOENV", "GOPROXY", "GOSUMDB", "GONOSUMDB", "GONOPROXY", "GOPRIVATE", "GOINSECURE",
	"GOAUTH", "GOTOOLCHAIN", "GOPATH", "GOMODCACHE", netprofile.EnvProfile,
}

// goProfileEnv is the go command's environment on a profile Job's agent
// container, every value non-empty: the user go/env file off; the proxy
// first and the image's baked cache (read-only, file://) after it, with no
// direct and no off, so version lists come from the network and a refusal
// falls through to what the image holds; the checksum database reached
// through the proxy; every pattern variable set to a value that matches
// nothing, so neither an image ENV nor $GOROOT/go.env can route a module
// around the proxy or the checksum database; no credential helper; no
// toolchain download; GOPATH and GOMODCACHE on the writable /tmp; and the
// profile's name for agent-runner's prompt.
//
// With a go registry grant, GOPROXY lists the registry's route first, then
// the public route when the profile has it, then the baked cache; and
// GONOSUMDB is the grant's private module prefixes (never looked up in the
// public checksum database) or, with none, the pattern that matches
// nothing. GOPRIVATE and GONOPROXY stay unmatched: a real pattern there
// means direct, which a profile pod cannot reach.
func goProfileEnv(p *NetworkProfile) []corev1.EnvVar {
	var entries []string
	gonosumdb := patternNone
	if g, ok := grantFor(p, netprofile.EcosystemGo); ok {
		entries = append(entries, registryURL(p, g))
		if len(g.PrivateModules) > 0 {
			gonosumdb = strings.Join(g.PrivateModules, ",")
		}
	}
	if p.Route == netprofile.RouteGo {
		entries = append(entries, p.ProxyURL+"/go")
	}
	if p.BakedModCache != "" {
		entries = append(entries, (&url.URL{Scheme: "file", Path: path.Join(p.BakedModCache, "cache/download")}).String())
	}
	values := map[string]string{
		"GOENV":               "off",
		"GOPROXY":             strings.Join(entries, ","),
		"GOSUMDB":             "sum.golang.org",
		"GONOSUMDB":           gonosumdb,
		"GONOPROXY":           patternNone,
		"GOPRIVATE":           patternNone,
		"GOINSECURE":          patternNone,
		"GOAUTH":              "off",
		"GOTOOLCHAIN":         "local",
		"GOPATH":              profileGoPath,
		"GOMODCACHE":          profileGoModCache,
		netprofile.EnvProfile: p.Name,
	}
	env := make([]corev1.EnvVar, 0, len(goProfileNames))
	for _, name := range goProfileNames {
		env = append(env, corev1.EnvVar{Name: name, Value: values[name]})
	}
	return env
}

// Where a profile Job's npm and pip keep their caches: the root filesystem
// is read-only.
const (
	profileNPMCache = "/tmp/npm-cache"
	profilePipCache = "/tmp/pip-cache"
)

// pipListNone is the value of every pip list variable a profile owns: one
// space, which pip splits to an empty list, where an empty value would
// fall through to a config file's (fact 5 of the step-2 spec).
const pipListNone = " "

// npmProfileEnv is npm's environment for an npm grant: the registry's
// proxy route (an environment variable beats every .npmrc), tarballs
// fetched through it whatever host a lockfile names while the lockfile
// keeps the canonical URL (replace-registry-host always), no audit, fund or
// update-notifier requests, install scripts off unless the grant runs
// them, the cache on /tmp, and strict TLS for anything an image would turn
// it off for.
func npmProfileEnv(p *NetworkProfile, g netprofile.Grant) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "NPM_CONFIG_REGISTRY", Value: registryURL(p, g) + "/"},
		{Name: "NPM_CONFIG_REPLACE_REGISTRY_HOST", Value: "always"},
		{Name: "NPM_CONFIG_AUDIT", Value: "false"},
		{Name: "NPM_CONFIG_FUND", Value: "false"},
		{Name: "NPM_CONFIG_UPDATE_NOTIFIER", Value: "false"},
		{Name: "NPM_CONFIG_IGNORE_SCRIPTS", Value: strconv.FormatBool(!g.NPMScripts)},
		{Name: "NPM_CONFIG_CACHE", Value: profileNPMCache},
		{Name: "NPM_CONFIG_STRICT_SSL", Value: "true"},
	}
}

// pipProfileEnv is pip's environment for a pypi grant: no pip.conf of any
// level, the registry's simple index through the proxy as the only index
// (the list variables a single space, never empty), the proxy's plain-http
// host trusted, wheels only unless the grant allows source builds, no
// prompts, no version check, and the cache on /tmp.
func pipProfileEnv(p *NetworkProfile, g netprofile.Grant) []corev1.EnvVar {
	onlyBinary := ":all:"
	if g.PipSdists {
		onlyBinary = ":none:"
	}
	return []corev1.EnvVar{
		{Name: "PIP_CONFIG_FILE", Value: "/dev/null"},
		{Name: "PIP_INDEX_URL", Value: registryURL(p, g) + "/simple/"},
		{Name: "PIP_EXTRA_INDEX_URL", Value: pipListNone},
		{Name: "PIP_FIND_LINKS", Value: pipListNone},
		{Name: "PIP_TRUSTED_HOST", Value: proxyHostPort(p.ProxyURL)},
		{Name: "PIP_ONLY_BINARY", Value: onlyBinary},
		{Name: "PIP_NO_INPUT", Value: "1"},
		{Name: "PIP_DISABLE_PIP_VERSION_CHECK", Value: "1"},
		{Name: "PIP_CACHE_DIR", Value: profilePipCache},
	}
}

// registryURL is a grant's route on the profile's listener, with no
// trailing slash.
func registryURL(p *NetworkProfile, g netprofile.Grant) string {
	return p.ProxyURL + "/r/" + g.Name
}

// proxyHostPort is the proxy URL's host:port, the form pip's trusted-host
// matches an http index by.
func proxyHostPort(proxyURL string) string {
	if u, err := url.Parse(proxyURL); err == nil {
		return u.Host
	}
	return proxyURL
}

// grantFor is the profile's grant of the ecosystem, if it has one.
func grantFor(p *NetworkProfile, ecosystem string) (netprofile.Grant, bool) {
	for _, g := range p.Registries {
		if g.Ecosystem == ecosystem {
			return g, true
		}
	}
	return netprofile.Grant{}, false
}

// profileEnv is the route and grant half of a profile Job's agent
// environment (TunnelEnv is the tunnel half, appended after it): a
// tunnel-only profile without grants, its name alone, so the image's go
// environment stays untouched and the Go route honestly absent; a
// step-1 profile (the public go route, no grants) exactly goProfileEnv; a
// profile with grants, the go environment when it has the public route or
// a go grant (without its trailing profile name), then npm's and pip's for
// their grants, then the profile's name, its ecosystems and its grants for
// agent-runner's prompt. No value is empty.
func profileEnv(p *NetworkProfile) []corev1.EnvVar {
	if len(p.Registries) == 0 {
		if p.Route == netprofile.RouteTunnel {
			return []corev1.EnvVar{{Name: netprofile.EnvProfile, Value: p.Name}}
		}
		return goProfileEnv(p)
	}
	var env []corev1.EnvVar
	_, goGrant := grantFor(p, netprofile.EcosystemGo)
	if p.Route == netprofile.RouteGo || goGrant {
		goEnv := goProfileEnv(p)
		env = append(env, goEnv[:len(goEnv)-1]...) // the profile's name goes last, below
	}
	if g, ok := grantFor(p, netprofile.EcosystemNPM); ok {
		env = append(env, npmProfileEnv(p, g)...)
	}
	if g, ok := grantFor(p, netprofile.EcosystemPyPI); ok {
		env = append(env, pipProfileEnv(p, g)...)
	}
	pod := podGrants(p)
	grants, err := netprofile.EncodePodGrants(pod)
	if err != nil {
		// profileRefusal validated every grant and their ecosystems first,
		// and nothing builds a Job it refused.
		panic(fmt.Sprintf("jobs: encode the grants profileRefusal admitted: %v", err))
	}
	return append(env,
		corev1.EnvVar{Name: netprofile.EnvProfile, Value: p.Name},
		corev1.EnvVar{Name: netprofile.EnvEcosystems, Value: netprofile.EcosystemList(p.Route == netprofile.RouteGo, pod)},
		corev1.EnvVar{Name: netprofile.EnvGrants, Value: grants},
	)
}

// podGrants is what the agent is told about the profile's grants.
func podGrants(p *NetworkProfile) []netprofile.PodGrant {
	out := make([]netprofile.PodGrant, 0, len(p.Registries))
	for _, g := range p.Registries {
		out = append(out, netprofile.PodGrantOf(g))
	}
	return out
}

// applyProfileEnv owns the profile's names on env (profileEnv, then the
// tunnel environment, TunnelEnv's, nil without hosts): every entry of those
// names (an operator's Config.Env, a runner's Env, an injectEnv blank) is
// dropped, then the profile's values are appended once each, in order. A
// name the profile does not set (npm's on a profile without an npm grant,
// HTTPS_PROXY's blank on a profile without hosts) is left as it was.
func applyProfileEnv(env []corev1.EnvVar, p *NetworkProfile, tunnel []corev1.EnvVar) []corev1.EnvVar {
	own := slices.Concat(profileEnv(p), tunnel)
	return append(slices.DeleteFunc(env, func(e corev1.EnvVar) bool {
		return slices.ContainsFunc(own, func(o corev1.EnvVar) bool { return o.Name == e.Name })
	}), own...)
}

// networkProfileOf reads the profile a Job carries off the Job that exists,
// created or adopted: the name of its one network label set to "true", or
// "" when it has none (or, never written by Create, more than one).
func networkProfileOf(job *batchv1.Job) string {
	var names []string
	for _, k := range slices.Sorted(maps.Keys(job.Labels)) {
		if name, ok := strings.CutPrefix(k, netprofile.LabelPrefix); ok && job.Labels[k] == netprofile.LabelValue {
			names = append(names, name)
		}
	}
	if len(names) != 1 {
		return ""
	}
	return names[0]
}
