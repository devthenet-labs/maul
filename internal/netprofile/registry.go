// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The ecosystems a registry serves.
const (
	EcosystemGo   = "go"
	EcosystemNPM  = "npm"
	EcosystemPyPI = "pypi"
)

// Ecosystems is every ecosystem, in the fixed order the job environment
// and the prompts list them.
var Ecosystems = []string{EcosystemGo, EcosystemNPM, EcosystemPyPI}

// The credential types of a registry (auth.type).
const (
	AuthNone         = "none"
	AuthBearer       = "bearer"
	AuthBasic        = "basic"
	AuthCodeArtifact = "codeartifact"
)

// Registry limits; the chart schema carries the same numbers.
const (
	MaxRegistries = 16
	// MaxRegistryHosts bounds fileHosts and redirectHosts each.
	MaxRegistryHosts = 8
	// MaxArtifactBytesLimit is the highest maxArtifactBytes (4 GiB).
	MaxArtifactBytesLimit = int64(4) << 30
	// MaxHealthPackageLen bounds healthPackage.
	MaxHealthPackageLen = 214
	// DefaultCodeArtifactDuration and MinCodeArtifactDuration bound a
	// CodeArtifact token's lifetime in seconds.
	DefaultCodeArtifactDuration = 43200
	MinCodeArtifactDuration     = 900
	// MinInternalPrefixLen is the widest internal CIDR (/24).
	MinInternalPrefixLen = 24
	maxPodLabels         = 16
)

var (
	// registryURLPattern is the chart schema's registry URL pattern.
	registryURLPattern = regexp.MustCompile(`^https://[a-z0-9.-]+(:[0-9]{1,5})?/[A-Za-z0-9._~/+-]*$`)
	// codeArtifactDomain, codeArtifactOwner and awsRegion are the
	// CodeArtifact identifiers' grammars.
	codeArtifactDomain = regexp.MustCompile(`^[a-z][a-z0-9-]{0,48}[a-z0-9]$`)
	codeArtifactOwner  = regexp.MustCompile(`^[0-9]{12}$`)
	awsRegion          = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	// dnsLabel is a DNS-1123 label (a namespace).
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// labelKey and labelValue are simplified Kubernetes label grammars.
	labelKey = regexp.MustCompile(
		`^([a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)
	labelValue = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?)?$`)
)

// Auth is a registry's credential, by type only: a Secret's name or value
// never appears in the registries JSON. A bearer credential is read from
// <credentials dir>/<registry>/token, a basic one from .../username and
// .../password.
type Auth struct {
	Type         string        `json:"type"`
	CodeArtifact *CodeArtifact `json:"codeartifact,omitempty"`
}

// CodeArtifact names the domain a codeartifact registry mints its token
// for.
type CodeArtifact struct {
	Domain          string `json:"domain"`
	DomainOwner     string `json:"domainOwner"`
	Region          string `json:"region"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
}

// Duration is the token lifetime asked for, in seconds:
// DurationSeconds, or DefaultCodeArtifactDuration when unset.
func (c CodeArtifact) Duration() int {
	if c.DurationSeconds == 0 {
		return DefaultCodeArtifactDuration
	}
	return c.DurationSeconds
}

// Internal opts a registry in to a private or in-cluster address.
type Internal struct {
	// CIDRs are the only addresses the registry's dialer admits, on the
	// URL's port: IPv4, /24 or narrower.
	CIDRs []string `json:"cidrs"`
	// PodSelector, for an in-cluster registry, is the NetworkPolicy peer
	// the chart renders; TargetPort its container port.
	PodSelector *PodSelector `json:"podSelector,omitempty"`
	TargetPort  int          `json:"targetPort,omitempty"`
}

// PodSelector is an in-cluster registry's pods.
type PodSelector struct {
	Namespace   string            `json:"namespace"`
	MatchLabels map[string]string `json:"matchLabels"`
}

// Registry is one operator-defined upstream registry
// (egressProxy.registries.<name>), as the chart renders it for the proxy.
type Registry struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
	// URL is the https base; for pypi the index URL ending /simple/.
	URL  string `json:"url"`
	Auth Auth   `json:"auth"`
	// FileHosts are exact hosts a pypi index links files to, fetched
	// without the credential.
	FileHosts []string `json:"fileHosts,omitempty"`
	// RedirectHosts are exact hosts or *.suffix patterns a 3xx may lead
	// to, followed without the credential.
	RedirectHosts []string `json:"redirectHosts,omitempty"`
	// Internal admits a private or in-cluster address.
	Internal *Internal `json:"internal,omitempty"`
	// PrivateModules and PublicChecksumLookups are a go registry's
	// checksum choice; both required for go, refused otherwise.
	PrivateModules        *[]string `json:"privateModules,omitempty"`
	PublicChecksumLookups *bool     `json:"publicChecksumLookups,omitempty"`
	// MaxArtifactBytes caps an archive; 0 is the proxy's default.
	MaxArtifactBytes int64 `json:"maxArtifactBytes,omitempty"`
	// HealthPackage is a package the credential can always read, for the
	// credential self-check; "" turns the self-check off.
	HealthPackage string `json:"healthPackage,omitempty"`
	// CA is set when a CA bundle is mounted for the registry: its
	// transport then trusts only that bundle.
	CA bool `json:"ca,omitempty"`
}

// RegistryContext is what a registry is judged against beyond itself.
type RegistryContext struct {
	// DenyHosts are further denied hosts (DenyContext.Hosts): the
	// profiles' forge hosts and the egress broker's base-URL overrides.
	DenyHosts []string
	// AlwaysDeny are further ranges no internal CIDR may overlap: cluster
	// DNS, the API-server endpoints, the Kubernetes Service IP.
	AlwaysDeny []netip.Prefix
	// ClusterCIDRs are the cluster's pod and Service ranges: an internal
	// CIDR inside them must be a /32.
	ClusterCIDRs []netip.Prefix
}

// BaseURL is the parsed URL. It is valid after Validate.
func (r Registry) BaseURL() (*url.URL, error) { return url.Parse(r.URL) }

// Hostname is the URL's host without its port; "" if the URL does not
// parse.
func (r Registry) Hostname() string {
	u, err := url.Parse(r.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Hosts are every host the registry route may contact: the URL's host,
// the file hosts and the redirect hosts (patterns as written). The chart
// copies them onto each grant, for the job controllers' Forge check.
func (r Registry) Hosts() []string {
	out := []string{r.Hostname()}
	out = append(out, r.FileHosts...)
	return append(out, r.RedirectHosts...)
}

// InternalPrefixes are the parsed internal CIDRs (none for a public
// registry). They are valid after Validate.
func (r Registry) InternalPrefixes() []netip.Prefix {
	if r.Internal == nil {
		return nil
	}
	out := make([]netip.Prefix, 0, len(r.Internal.CIDRs))
	for _, c := range r.Internal.CIDRs {
		if p, err := netip.ParsePrefix(c); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

// denyContext is the registry's own DenyContext.
func (r Registry) denyContext(rc RegistryContext) DenyContext {
	return DenyContext{Hosts: rc.DenyHosts, Internal: r.Internal != nil, InternalCIDRs: r.InternalPrefixes()}
}

// Validate checks a registry on its own (the chart's render guards 12-19,
// in Go): name, ecosystem, URL, credential, hosts, internal admission and
// the go checksum choice.
func (r Registry) Validate(rc RegistryContext) error {
	if !namePattern.MatchString(r.Name) {
		return fmt.Errorf("registry name %q is not a DNS label of at most %d characters starting with a letter",
			r.Name, MaxNameLen)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("registry %s: %s", r.Name, fmt.Sprintf(format, args...))
	}
	if !validEcosystem(r.Ecosystem) {
		return fail("ecosystem %q is not go, npm or pypi", r.Ecosystem)
	}
	if err := r.validateInternal(rc); err != nil {
		return fail("%v", err)
	}
	u, err := r.validateURL()
	if err != nil {
		return fail("%v", err)
	}
	ctx := r.denyContext(rc)
	if err := DeniedHost(u.Hostname(), ctx); err != nil {
		return fail("url: %v", err)
	}
	if err := r.validateAuth(u.Hostname()); err != nil {
		return fail("%v", err)
	}
	if err := r.validateHosts(ctx); err != nil {
		return fail("%v", err)
	}
	if err := r.validateGo(); err != nil {
		return fail("%v", err)
	}
	if r.MaxArtifactBytes < 0 || r.MaxArtifactBytes > MaxArtifactBytesLimit {
		return fail("maxArtifactBytes %d is outside 0-%d", r.MaxArtifactBytes, MaxArtifactBytesLimit)
	}
	if err := r.validateHealthPackage(); err != nil {
		return fail("%v", err)
	}
	return nil
}

func validEcosystem(e string) bool {
	return e == EcosystemGo || e == EcosystemNPM || e == EcosystemPyPI
}

// validateURL is the URL's shape (guard 14 included): https, the schema's
// pattern, no empty, . or .. segment, a pypi index ending /simple/, and a
// port other than 443 only on an internal registry.
func (r Registry) validateURL() (*url.URL, error) {
	if !registryURLPattern.MatchString(r.URL) {
		return nil, fmt.Errorf("url %q is not https://<lowercase host>[:port]/<path>", r.URL)
	}
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, fmt.Errorf("url %q: %w", r.URL, err)
	}
	if u.Hostname() == "" || !hostPattern.MatchString(u.Hostname()) || !validLabels(u.Hostname()) {
		return nil, fmt.Errorf("url %q: host is not a lowercase host name", r.URL)
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	for i, s := range segs {
		if s == "." || s == ".." || (s == "" && i != len(segs)-1) {
			return nil, fmt.Errorf("url %q: path has an empty, . or .. segment", r.URL)
		}
	}
	if r.Ecosystem == EcosystemPyPI && !strings.HasSuffix(u.Path, "/simple/") {
		return nil, fmt.Errorf("url %q: a pypi registry's url is its index URL, ending /simple/", r.URL)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > MaxPort {
			return nil, fmt.Errorf("url %q: port %q is outside 1-%d", r.URL, p, MaxPort)
		}
		if n != 443 && r.Internal == nil {
			return nil, fmt.Errorf("url %q: port %d needs internal (a public registry is reached on 443 only)",
				r.URL, n)
		}
	}
	return u, nil
}

// Port is the URL's port, 443 when it names none.
func (r Registry) Port() int {
	u, err := url.Parse(r.URL)
	if err != nil || u.Port() == "" {
		return 443
	}
	n, _ := strconv.Atoi(u.Port())
	return n
}

// validateAuth is guard 12 and the Go half of 16: a known type; the
// codeartifact block on codeartifact only, never on go, its identifiers
// well-formed and the URL host exactly the domain's endpoint.
func (r Registry) validateAuth(host string) error {
	switch r.Auth.Type {
	case AuthNone, AuthBearer, AuthBasic:
		if r.Auth.CodeArtifact != nil {
			return fmt.Errorf("auth.codeartifact is set on auth.type %s", r.Auth.Type)
		}
		return nil
	case AuthCodeArtifact:
	default:
		return fmt.Errorf("auth.type %q is not none, bearer, basic or codeartifact", r.Auth.Type)
	}
	if r.Ecosystem == EcosystemGo {
		return errors.New("codeartifact has no Go module support: a codeartifact registry is npm or pypi")
	}
	ca := r.Auth.CodeArtifact
	if ca == nil {
		return errors.New("auth.type codeartifact needs auth.codeartifact (domain, domainOwner, region)")
	}
	switch {
	case !codeArtifactDomain.MatchString(ca.Domain):
		return fmt.Errorf("codeartifact domain %q is not a CodeArtifact domain name", ca.Domain)
	case !codeArtifactOwner.MatchString(ca.DomainOwner):
		return fmt.Errorf("codeartifact domainOwner %q is not a 12-digit account id", ca.DomainOwner)
	case !awsRegion.MatchString(ca.Region):
		return fmt.Errorf("codeartifact region %q is not an AWS region", ca.Region)
	case ca.DurationSeconds != 0 &&
		(ca.DurationSeconds < MinCodeArtifactDuration || ca.DurationSeconds > DefaultCodeArtifactDuration):
		return fmt.Errorf("codeartifact durationSeconds %d is outside %d-%d", ca.DurationSeconds,
			MinCodeArtifactDuration, DefaultCodeArtifactDuration)
	}
	prefix := ca.Domain + "-" + ca.DomainOwner + ".d.codeartifact." + ca.Region
	if host != prefix+".amazonaws.com" && host != prefix+".on.aws" {
		return fmt.Errorf("url host %s is not the CodeArtifact endpoint of domain %s, owner %s, region %s "+
			"(%s.amazonaws.com)", host, ca.Domain, ca.DomainOwner, ca.Region, prefix)
	}
	if r.Port() != 443 {
		return errors.New("a codeartifact url names no port other than 443")
	}
	return nil
}

// validateHosts is guard 19: file hosts are exact hosts, redirect hosts
// exact hosts or *.suffix patterns, none denied, at most MaxRegistryHosts
// each.
func (r Registry) validateHosts(ctx DenyContext) error {
	if len(r.FileHosts) > MaxRegistryHosts || len(r.RedirectHosts) > MaxRegistryHosts {
		return fmt.Errorf("fileHosts and redirectHosts list at most %d entries each", MaxRegistryHosts)
	}
	for _, h := range r.FileHosts {
		if err := validExactHost(h); err != nil {
			return fmt.Errorf("fileHosts: %w", err)
		}
		if err := DeniedHost(h, ctx); err != nil {
			return fmt.Errorf("fileHosts: %w", err)
		}
	}
	for _, h := range r.RedirectHosts {
		if err := ValidHostOrPattern(h, ctx); err != nil {
			return fmt.Errorf("redirectHosts: %w", err)
		}
	}
	return nil
}

// validExactHost is a lowercase host name, not an address.
func validExactHost(h string) error {
	if len(h) > maxHostLen || !hostPattern.MatchString(h) || !validLabels(h) {
		return fmt.Errorf("%q is not a lowercase host name", h)
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return fmt.Errorf("%q is an address, not a host name", h)
	}
	return nil
}

// ValidHostOrPattern judges a redirect host: an exact host DeniedHost
// admits, or a *.suffix pattern DeniedPattern admits.
func ValidHostOrPattern(h string, ctx DenyContext) error {
	if strings.HasPrefix(h, "*.") {
		return DeniedPattern(h, ctx)
	}
	if err := validExactHost(h); err != nil {
		return err
	}
	return DeniedHost(h, ctx)
}

// validateGo is guards 17 and 18 on the registry: a go registry states
// privateModules (possibly empty) and publicChecksumLookups; no other
// ecosystem may.
func (r Registry) validateGo() error {
	if r.Ecosystem != EcosystemGo {
		if r.PrivateModules != nil || r.PublicChecksumLookups != nil {
			return errors.New("privateModules and publicChecksumLookups are for go registries only")
		}
		return nil
	}
	if r.PrivateModules == nil || r.PublicChecksumLookups == nil {
		return errors.New("a go registry must state both privateModules (a list, possibly empty) and " +
			"publicChecksumLookups: there is no default")
	}
	return validModuleList("privateModules", *r.PrivateModules)
}

// validModuleList checks a list of module prefixes.
func validModuleList(field string, ms []string) error {
	if len(ms) > MaxModules {
		return fmt.Errorf("%s lists %d prefixes; at most %d", field, len(ms), MaxModules)
	}
	for _, m := range ms {
		if err := ValidPrefix(m); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	return nil
}

// validateHealthPackage: at most MaxHealthPackageLen bytes, and a package
// name of the ecosystem (it is sent upstream).
func (r Registry) validateHealthPackage() error {
	h := r.HealthPackage
	if h == "" {
		return nil
	}
	if len(h) > MaxHealthPackageLen {
		return fmt.Errorf("healthPackage is over %d bytes", MaxHealthPackageLen)
	}
	var ok bool
	switch r.Ecosystem {
	case EcosystemNPM:
		_, _, err := NPMName(h)
		ok = err == nil
	case EcosystemPyPI:
		ok = PyName(h)
	case EcosystemGo:
		ok = ValidPrefix(h) == nil && !strings.HasSuffix(h, "/")
	}
	if !ok {
		return fmt.Errorf("healthPackage %q is not a %s package name", h, r.Ecosystem)
	}
	return nil
}

// validateInternal is guards 15 and 17's internal half: IPv4 CIDRs of /24
// or narrower, none overlapping the always-denied set, a /32 inside the
// cluster CIDRs, and a targetPort only beside a podSelector.
func (r Registry) validateInternal(rc RegistryContext) error {
	in := r.Internal
	if in == nil {
		return nil
	}
	if len(in.CIDRs) == 0 {
		return errors.New("internal.cidrs is required: the dialer admits only these addresses")
	}
	for _, c := range in.CIDRs {
		if err := validInternalCIDR(c, rc); err != nil {
			return err
		}
	}
	if in.TargetPort < 0 || in.TargetPort > MaxPort {
		return fmt.Errorf("internal.targetPort %d is outside 0-%d", in.TargetPort, MaxPort)
	}
	if in.TargetPort != 0 && in.PodSelector == nil {
		return errors.New("internal.targetPort is the podSelector rule's port: it needs internal.podSelector")
	}
	if in.PodSelector != nil {
		return in.PodSelector.validate()
	}
	return nil
}

// validInternalCIDR is one internal CIDR: a masked IPv4 /24 or narrower,
// clear of the always-denied set, and a /32 inside the cluster CIDRs.
func validInternalCIDR(c string, rc RegistryContext) error {
	p, err := netip.ParsePrefix(c)
	if err != nil || !p.Addr().Is4() || p.Bits() < MinInternalPrefixLen || p.Masked() != p {
		return fmt.Errorf("internal.cidrs entry %q is not a masked IPv4 CIDR of /%d or narrower",
			c, MinInternalPrefixLen)
	}
	for _, set := range [][]netip.Prefix{AlwaysDeniedCIDRs, rc.AlwaysDeny} {
		for _, d := range set {
			if d.Overlaps(p) {
				return fmt.Errorf("internal.cidrs entry %s overlaps the always-denied %s", p, d)
			}
		}
	}
	for _, cc := range rc.ClusterCIDRs {
		if cc.Overlaps(p) && p.Bits() != 32 {
			return fmt.Errorf("internal.cidrs entry %s is inside the cluster range %s: list the ClusterIP "+
				"as a /32", p, cc)
		}
	}
	return nil
}

// validate is an in-cluster registry's pod selector: a namespace and 1 to
// maxPodLabels labels.
func (ps PodSelector) validate() error {
	if !dnsLabel.MatchString(ps.Namespace) {
		return fmt.Errorf("internal.podSelector.namespace %q is not a namespace name", ps.Namespace)
	}
	if len(ps.MatchLabels) == 0 || len(ps.MatchLabels) > maxPodLabels {
		return fmt.Errorf("internal.podSelector.matchLabels must hold 1-%d labels", maxPodLabels)
	}
	for k, v := range ps.MatchLabels {
		if len(k) > 316 || !labelKey.MatchString(k) || !labelValue.MatchString(v) {
			return fmt.Errorf("internal.podSelector.matchLabels %q=%q is not a label", k, v)
		}
	}
	return nil
}

// RegistrySet is a validated registry set, sorted by name. The zero value
// is empty.
type RegistrySet struct {
	registries []Registry
}

// NewRegistrySet validates registries (each on its own, then unique names,
// at most MaxRegistries).
func NewRegistrySet(registries []Registry, rc RegistryContext) (RegistrySet, error) {
	if len(registries) > MaxRegistries {
		return RegistrySet{}, fmt.Errorf("%d registries; at most %d are allowed", len(registries), MaxRegistries)
	}
	names := map[string]bool{}
	out := make([]Registry, 0, len(registries))
	for _, r := range registries {
		if err := r.Validate(rc); err != nil {
			return RegistrySet{}, err
		}
		if names[r.Name] {
			return RegistrySet{}, fmt.Errorf("registry %s is named twice", r.Name)
		}
		names[r.Name] = true
		out = append(out, r.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return RegistrySet{registries: out}, nil
}

// ParseRegistries decodes the registries JSON (an array, as the chart
// renders egressProxy.registries), refusing unknown fields and trailing
// data, and validates it with NewRegistrySet. Empty input, "null" and "[]"
// are an empty set.
func ParseRegistries(data []byte, rc RegistryContext) (RegistrySet, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return RegistrySet{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var regs []Registry
	if err := dec.Decode(&regs); err != nil {
		return RegistrySet{}, fmt.Errorf("registries: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return RegistrySet{}, errors.New("registries: trailing data after the JSON array")
	}
	set, err := NewRegistrySet(regs, rc)
	if err != nil {
		return RegistrySet{}, fmt.Errorf("registries: %w", err)
	}
	return set, nil
}

// Len is the number of registries.
func (s RegistrySet) Len() int { return len(s.registries) }

// Registries returns a copy of the registries, sorted by name.
func (s RegistrySet) Registries() []Registry {
	out := make([]Registry, len(s.registries))
	for i, r := range s.registries {
		out[i] = r.clone()
	}
	return out
}

// Get returns the registry called name.
func (s RegistrySet) Get(name string) (Registry, bool) {
	for _, r := range s.registries {
		if r.Name == name {
			return r.clone(), true
		}
	}
	return Registry{}, false
}

// clone copies every list and pointer a caller could mutate.
func (r Registry) clone() Registry {
	r.FileHosts = append([]string(nil), r.FileHosts...)
	r.RedirectHosts = append([]string(nil), r.RedirectHosts...)
	if r.Auth.CodeArtifact != nil {
		ca := *r.Auth.CodeArtifact
		r.Auth.CodeArtifact = &ca
	}
	if r.Internal != nil {
		in := *r.Internal
		in.CIDRs = append([]string(nil), in.CIDRs...)
		if in.PodSelector != nil {
			ps := *in.PodSelector
			ps.MatchLabels = make(map[string]string, len(ps.MatchLabels))
			for k, v := range r.Internal.PodSelector.MatchLabels {
				ps.MatchLabels[k] = v
			}
			in.PodSelector = &ps
		}
		r.Internal = &in
	}
	if r.PrivateModules != nil {
		pm := append([]string{}, (*r.PrivateModules)...)
		r.PrivateModules = &pm
	}
	if r.PublicChecksumLookups != nil {
		b := *r.PublicChecksumLookups
		r.PublicChecksumLookups = &b
	}
	return r
}
