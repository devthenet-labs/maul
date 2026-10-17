// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The grammars the chart's schema holds the same values to
// (charts/patchy/values.schema.json); a value the wizard accepts must
// render.
var (
	teamPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*:[^\s:][^\s]*$`)
	userPattern     = regexp.MustCompile(`^[A-Za-z0-9](-?[A-Za-z0-9]){0,38}$`)
	registryPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)
	prefixPattern   = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// sectionPattern is a Gateway API SectionName.
	sectionPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// maxRelease is Helm's limit on a release name.
const maxRelease = 53

// ValidHost reports whether h is a host the gateway's grammar accepts
// (internal/previewauth's): a lowercase DNS name of two or more labels,
// each 1 to 63 letters, digits and inner hyphens, at most 251 characters
// in all (room for the wildcard's "*.").
func ValidHost(h string) bool {
	if len(h) == 0 || len(h) > 251 {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// Under reports whether host is suffix or a name under it.
func Under(host, suffix string) bool {
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

// Site is h's registrable domain (eTLD+1, as publicsuffix decides it), or
// an error for a name that has none (a public suffix itself).
func Site(h string) (string, error) {
	return publicsuffix.EffectiveTLDPlusOne(h)
}

// CheckHosts holds the preview host suffix and the sign-in host to what
// the gateway's isolation assumes (design §0, G11, owner decision Q3):
// both valid hosts, the sign-in host outside the suffix, and the two on
// different registrable domains. Preview code is same-site with every name
// on the suffix's registrable domain, so a sign-in host there would hand
// it the viewer's session cookies' site: refused, never warned.
func CheckHosts(suffix, signIn string) error {
	if !ValidHost(suffix) {
		return fmt.Errorf("--host-suffix %q is not a lowercase DNS name of two or more labels", suffix)
	}
	if !ValidHost(signIn) {
		return fmt.Errorf("--sign-in-host %q is not a lowercase DNS name of two or more labels", signIn)
	}
	if Under(signIn, suffix) {
		return fmt.Errorf("--sign-in-host %s sits under --host-suffix %s: the preview wildcard would cover it, and "+
			"preview code would be same-origin with sign-in", signIn, suffix)
	}
	site, err := Site(suffix)
	if err != nil {
		return fmt.Errorf("--host-suffix %s has no registrable domain of its own (%v): previews must sit under a "+
			"domain you register", suffix, err)
	}
	if s, err := Site(signIn); err == nil && s == site {
		return fmt.Errorf("--sign-in-host %s and --host-suffix %s share the registrable domain %s, so preview code "+
			"would run same-site with sign-in: put previews on a domain of their own (a second, cheap domain; on "+
			"Cloudflare it is also what makes the free certificate cover them)", signIn, suffix, site)
	}
	return nil
}

// CheckIssuer holds the Dex issuer to an https URL whose host is neither
// under the preview suffix nor on its registrable domain: Dex holds the
// viewer's GitHub session, which preview code must not share a site with.
func CheckIssuer(issuer, suffix string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("--dex-issuer %q is not an https URL without credentials, query or fragment", issuer)
	}
	host := strings.ToLower(u.Hostname())
	if _, err := netip.ParseAddr(host); err == nil || !ValidHost(host) {
		return fmt.Errorf("--dex-issuer %q: its host is not a DNS name", issuer)
	}
	if !ValidHost(suffix) {
		return nil // CheckHosts reports the suffix
	}
	if Under(host, suffix) {
		return fmt.Errorf("--dex-issuer %s sits under --host-suffix %s: preview code would be same-origin with Dex",
			issuer, suffix)
	}
	site, err := Site(suffix)
	if err != nil {
		return nil // CheckHosts reports it
	}
	if s, err := Site(host); err == nil && s == site {
		return fmt.Errorf("--dex-issuer %s shares the registrable domain %s with --host-suffix %s, so preview code "+
			"would run same-site with Dex: put previews on a domain of their own", issuer, site, suffix)
	}
	return nil
}

// ParseGatewayParent splits a Gateway API parent, <namespace>/<name> or
// <namespace>/<name>/<section>.
func ParseGatewayParent(s string) (namespace, name, section string, err error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return "", "", "", fmt.Errorf("--gateway-parent %q is not <namespace>/<name>[/<section>]", s)
	}
	namespace, name = parts[0], parts[1]
	if len(parts) == 3 {
		section = parts[2]
	}
	switch {
	case len(validation.IsDNS1123Label(namespace)) > 0:
		return "", "", "", fmt.Errorf("--gateway-parent %q: namespace %q is not a DNS label", s, namespace)
	case len(validation.IsDNS1123Subdomain(name)) > 0:
		return "", "", "", fmt.Errorf("--gateway-parent %q: name %q is not a DNS subdomain", s, name)
	case len(parts) == 3 && (len(section) > 253 || !sectionPattern.MatchString(section)):
		return "", "", "", fmt.Errorf("--gateway-parent %q: section %q is not a listener name", s, section)
	}
	return namespace, name, section, nil
}

// Validate reports every answer a run would be refused for, at once:
// what the chart's schema or render would refuse, and what the gateway's
// isolation forbids. Missing answers are Missing's, not reported here.
func (a Answers) Validate() error {
	var errs []error
	for _, part := range []func() []error{a.validateNames, a.validateSignIn, a.validateImages, a.validateNodes,
		a.validateCertificate} {
		errs = append(errs, part()...)
	}
	switch a.Edge {
	case EdgeExisting:
		errs = append(errs, a.validateExisting()...)
	case EdgeGatewayTLS:
		errs = append(errs, a.validateLoadBalancer()...)
	case EdgeTailscale:
		errs = append(errs, a.validateTailscale()...)
	}
	errs = append(errs, a.validateCloudflare()...)
	return errors.Join(errs...)
}

// validateNames checks the enumerations and the Kubernetes names.
func (a Answers) validateNames() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if a.Edge != "" && !slices.Contains(Edges(), string(a.Edge)) {
		bad("--edge %q is none of %s", a.Edge, strings.Join(Edges(), ", "))
	}
	if a.Platform != "" && !slices.Contains(Platforms(), string(a.Platform)) {
		bad("--platform %q is none of %s", a.Platform, strings.Join(Platforms(), ", "))
	}
	if e := validation.IsDNS1123Label(a.Release); len(e) > 0 || len(a.Release) > maxRelease {
		bad("--release %q is not a Helm release name (a DNS label of at most %d characters)", a.Release, maxRelease)
	}
	for _, ns := range [][2]string{{"-n", a.Namespace}, {"--dex-namespace", a.Dex.Namespace}} {
		if e := validation.IsDNS1123Label(ns[1]); len(e) > 0 {
			bad("%s %q is not a namespace name: %s", ns[0], ns[1], strings.Join(e, "; "))
		}
	}
	if a.APIServerCIDR != DefaultAPIServerCIDR {
		if p, err := netip.ParsePrefix(a.APIServerCIDR); err != nil || !p.Addr().Is4() || p.Bits() != 32 {
			bad("--api-server-cidr %q is neither auto nor an IPv4 /32 (the kubernetes Service's address)",
				a.APIServerCIDR)
		}
	}
	return errs
}

// validateSignIn checks the hosts, Dex and the viewers.
func (a Answers) validateSignIn() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if a.HostSuffix != "" && a.SignInHost != "" {
		if err := CheckHosts(a.HostSuffix, a.SignInHost); err != nil {
			errs = append(errs, err)
		}
	}
	if a.Dex.Issuer != "" {
		if err := CheckIssuer(a.Dex.Issuer, a.HostSuffix); err != nil {
			errs = append(errs, err)
		}
	}
	if !clientIDPattern.MatchString(a.Dex.ClientID) {
		bad("--dex-client-id %q is not a client ID (letters, digits, '.', '_' and '-', at most 128)", a.Dex.ClientID)
	}
	for _, t := range a.Viewers.Teams {
		if !teamPattern.MatchString(t) {
			bad("--viewer-team %q is not <org>:<team-slug>", t)
		}
	}
	for _, u := range a.Viewers.Users {
		if !userPattern.MatchString(u) {
			bad("--viewer-user %q is not a GitHub login", u)
		}
	}
	if a.NoViewers && (len(a.Viewers.Teams) > 0 || len(a.Viewers.Users) > 0) {
		bad("--no-viewers and --viewer-team/--viewer-user are exclusive")
	}
	return errs
}

// validateImages checks where preview images come from.
func (a Answers) validateImages() []error {
	var errs []error
	if a.Registry != "" {
		if host, path, ok := strings.Cut(a.Registry, "/"); ok {
			errs = append(errs, fmt.Errorf("--registry %q holds a path: give the host (%s) to --registry and put "+
				"%s at the front of --image-path-prefix", a.Registry, host, path))
		} else if !registryPattern.MatchString(a.Registry) {
			errs = append(errs, fmt.Errorf("--registry %q is not a registry host (lowercase, with an optional port)",
				a.Registry))
		}
	}
	if !prefixPattern.MatchString(a.ImagePathPrefix) {
		errs = append(errs, fmt.Errorf("--image-path-prefix %q is not a registry path (lowercase segments joined "+
			"by '/')", a.ImagePathPrefix))
	}
	return errs
}

func (a Answers) validateNodes() []error {
	var errs []error
	n := a.NodeIsolation
	switch n.Mode {
	case "", IsolationRequired, IsolationNone:
	default:
		errs = append(errs, fmt.Errorf("--node-isolation %q is none of required, none", n.Mode))
	}
	if e := validation.IsQualifiedName(n.Label); len(e) > 0 {
		errs = append(errs, fmt.Errorf("--node-label key %q: %s", n.Label, strings.Join(e, "; ")))
	}
	if e := validation.IsValidLabelValue(n.LabelValue); len(e) > 0 || n.LabelValue == "" {
		errs = append(errs, fmt.Errorf("--node-label value %q is not a label value", n.LabelValue))
	}
	if e := validation.IsQualifiedName(n.TaintKey); len(e) > 0 {
		errs = append(errs, fmt.Errorf("--taint-key %q: %s", n.TaintKey, strings.Join(e, "; ")))
	}
	if eks := n.EKSAutoMode; eks != (EKSAutoMode{}) {
		if a.Platform != PlatformEKS {
			errs = append(errs, fmt.Errorf("--eks-auto-mode is for --platform eks, not %q", a.Platform))
		}
		if a.IsolationMode() == IsolationNone {
			errs = append(errs, errors.New("--eks-auto-mode with --node-isolation none: Auto Mode names the "+
				"dedicated preview nodes, which mode none has none of"))
		}
		for _, name := range []string{eks.NodePool, eks.NodeClass} {
			if v := validation.IsDNS1123Label(name); len(v) > 0 {
				errs = append(errs, fmt.Errorf("--eks-auto-mode name %q: %s", name, strings.Join(v, "; ")))
			}
		}
	}
	return errs
}

func (a Answers) validateExisting() []error {
	var errs []error
	e := a.Existing
	if e.IngressClass != "" && e.GatewayParent != "" {
		errs = append(errs, errors.New("--ingress-class and --gateway-parent are exclusive: the edge is an "+
			"Ingress controller or a Gateway, not both"))
	}
	if e.IngressClass != "" {
		if v := validation.IsDNS1123Subdomain(e.IngressClass); len(v) > 0 {
			errs = append(errs, fmt.Errorf("--ingress-class %q: %s", e.IngressClass, strings.Join(v, "; ")))
		}
		if e.IngressClass == "alb-preview" {
			errs = append(errs, errors.New("--ingress-class alb-preview is reserved for the slot namespaces"))
		}
	}
	if e.GatewayParent != "" {
		if _, _, _, err := ParseGatewayParent(e.GatewayParent); err != nil {
			errs = append(errs, err)
		}
	}
	switch e.TLS {
	case "", TLSEdge:
	case TLSCertificate:
		if e.GatewayParent != "" {
			errs = append(errs, errors.New("--edge-tls certificate with --gateway-parent: a Gateway's listener "+
				"holds the certificate, in the Gateway's namespace"))
		}
	default:
		errs = append(errs, fmt.Errorf("--edge-tls %q is none of certificate, edge", e.TLS))
	}
	return errs
}

func validSubdomain(name string) bool { return len(validation.IsDNS1123Subdomain(name)) == 0 }
