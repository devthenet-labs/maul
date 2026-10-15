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
// asks a Job to carry (Spec.NetworkProfile): one dependency route through
// the egress proxy. Applied, it adds exactly three things to the Job and
// nothing else: the profile label on the Job and its pod (the identity both
// NetworkPolicies on the profile's port select), the proxy's host in the
// pod's hosts file under DNSNone, and the go command's environment in the
// agent container (goProfileEnv).
type NetworkProfile struct {
	// Name is the profile's name (netprofile's grammar); the label is
	// netprofile.LabelKey(Name).
	Name string
	// Route is the dependency route; netprofile.RouteGo only.
	Route string
	// ProxyURL is the egress proxy's listener for this profile:
	// http://<host>:<port>, with no path. The controller joins the proxy's
	// base URL and the profile's port.
	ProxyURL string
	// BakedModCache is the repository image's GOMODCACHE, served read-only
	// after the proxy; empty means the network only.
	BakedModCache string
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
	if p.Route != netprofile.RouteGo {
		refuse("route %q is not supported (only %q)", p.Route, netprofile.RouteGo)
	}
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
func goProfileEnv(p *NetworkProfile) []corev1.EnvVar {
	goproxy := p.ProxyURL + "/go"
	if p.BakedModCache != "" {
		goproxy += "," + (&url.URL{Scheme: "file", Path: path.Join(p.BakedModCache, "cache/download")}).String()
	}
	values := map[string]string{
		"GOENV":               "off",
		"GOPROXY":             goproxy,
		"GOSUMDB":             "sum.golang.org",
		"GONOSUMDB":           patternNone,
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

// applyProfileEnv owns goProfileNames on env: every entry of those names
// (an operator's Config.Env, a runner's Env, an injectEnv blank) is
// dropped, then the profile's values are appended once each, in order.
func applyProfileEnv(env []corev1.EnvVar, p *NetworkProfile) []corev1.EnvVar {
	return append(slices.DeleteFunc(env, func(e corev1.EnvVar) bool {
		return slices.Contains(goProfileNames, e.Name)
	}), goProfileEnv(p)...)
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
