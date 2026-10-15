// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// ErrDeniedHost is wrapped by every DeniedHost and DeniedPattern refusal.
var ErrDeniedHost = errors.New("host is on the egress proxy's deny list")

// ForgeHosts are the forge hosts no registry, file host or redirect target
// may be. Each entry denies the host and every name below it. Forges known
// only through Forge CRs are added by the job controllers at launch
// (DenyContext.Hosts).
var ForgeHosts = []string{"github.com", "githubusercontent.com"}

// ModelAPIHosts are the model APIs the egress broker reaches, and their
// neighbours. Each entry denies the host and every name below it; the
// regional forms the broker builds from a region or resource are the label
// rules in deniedLabel. A test in cmd/egress-broker instantiates every
// default base URL the broker can build and asserts DeniedHost refuses it.
var ModelAPIHosts = []string{
	"anthropic.com",
	"openai.com",
	"openai.azure.com",
	"services.ai.azure.com",
	"cognitiveservices.azure.com",
	"aiplatform.googleapis.com",
	"generativelanguage.googleapis.com",
}

// clusterHosts are the in-cluster names of the Kubernetes API, denied
// exactly: unlike the other tables they are prefixes of their own subtrees
// (kubernetes.default.svc is below no kubernetes.default in DNS terms), so
// each name is listed. The control that holds whatever the name is the
// dialer's: the Kubernetes Service IP is always denied.
var clusterHosts = []string{
	"kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local",
}

// clusterSuffixes are in-cluster DNS names, refused unless the registry is
// marked internal.
var clusterSuffixes = []string{"svc", "cluster.local"}

// TooWideSuffixes are shared multi-tenant suffixes: a redirectHosts pattern
// equal to one of them, or a parent of one, would admit any tenant's name.
var TooWideSuffixes = []string{
	"amazonaws.com",
	"s3.amazonaws.com",
	"cloudfront.net",
	"googleapis.com",
	"storage.googleapis.com",
	"appspot.com",
	"azure.com",
	"windows.net",
	"blob.core.windows.net",
	"azureedge.net",
	"githubusercontent.com",
	"github.io",
	"fastly.net",
	"akamaized.net",
	"r2.cloudflarestorage.com",
}

// MinPatternLabels is the fewest labels a "*.suffix" pattern's suffix may
// have.
const MinPatternLabels = 3

// AlwaysDeniedCIDRs are the address ranges no registry may ever be
// admitted to, even when it lists them under internal.cidrs: this network,
// loopback, link-local (IMDS, EKS Pod Identity), multicast and reserved.
// The proxy adds the Kubernetes Service IP and its --always-deny-cidrs.
var AlwaysDeniedCIDRs = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// DenyContext is what DeniedHost and DeniedPattern judge beyond the fixed
// table.
type DenyContext struct {
	// Hosts are further denied hosts, each with every name below it: the
	// profiles' forge hosts, the egress broker's base-URL overrides and,
	// in the job controllers, the hosts of every Forge CR.
	Hosts []string
	// Internal admits in-cluster DNS names (*.svc, *.cluster.local): the
	// registry is marked internal.
	Internal bool
	// InternalCIDRs admit an IP-literal host inside them: the registry's
	// own internal.cidrs.
	InternalCIDRs []netip.Prefix
}

// canonicalHost lowercases h and drops one trailing dot, so a redirect's
// Location is judged the way a resolver would read it.
func canonicalHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// under reports whether host is suffix or a name below it.
func under(host, suffix string) bool {
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

// DeniedHost reports why host may not be a registry, file or redirect host
// (nil when it may): a forge, a model API, the Kubernetes API, an
// in-cluster name without Internal, any of ctx.Hosts, or an IP literal
// outside ctx.InternalCIDRs (or inside AlwaysDeniedCIDRs). The host is
// lowercased and a trailing dot dropped first.
func DeniedHost(host string, ctx DenyContext) error {
	h := canonicalHost(host)
	if h == "" {
		return fmt.Errorf("%w: empty host", ErrDeniedHost)
	}
	if strings.HasPrefix(h, "[") || strings.Contains(h, ":") {
		return fmt.Errorf("%w: %s is an IPv6 literal", ErrDeniedHost, host)
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return deniedAddr(a, ctx)
	}
	for _, set := range [][]string{ForgeHosts, ModelAPIHosts, ctx.Hosts} {
		for _, d := range set {
			if d = canonicalHost(d); d != "" && under(h, d) {
				return fmt.Errorf("%w: %s is %s or below it", ErrDeniedHost, host, d)
			}
		}
	}
	for _, d := range clusterHosts {
		if h == d {
			return fmt.Errorf("%w: %s is the Kubernetes API", ErrDeniedHost, host)
		}
	}
	if why := deniedLabel(h); why != "" {
		return fmt.Errorf("%w: %s is %s", ErrDeniedHost, host, why)
	}
	if !ctx.Internal {
		for _, s := range clusterSuffixes {
			if under(h, s) {
				return fmt.Errorf("%w: %s is an in-cluster name and the registry is not internal", ErrDeniedHost, host)
			}
		}
	}
	return nil
}

// deniedAddr judges an IP-literal host: admitted only inside the
// registry's internal CIDRs and never inside AlwaysDeniedCIDRs.
func deniedAddr(a netip.Addr, ctx DenyContext) error {
	a = a.Unmap()
	for _, p := range AlwaysDeniedCIDRs {
		if p.Contains(a) {
			return fmt.Errorf("%w: %s is in the always-denied range %s", ErrDeniedHost, a, p)
		}
	}
	for _, p := range ctx.InternalCIDRs {
		if p.Contains(a) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is an IP literal outside the registry's internal CIDRs", ErrDeniedHost, a)
}

// deniedLabel is the two label rules the broker's regional hosts need, each
// fixed to one label position so a pattern such as
// *.s3.us-east-1.amazonaws.com, whose free labels sit elsewhere, never
// overlaps them:
//   - Bedrock: <bedrock…>.<region>.amazonaws.com, and its VPC endpoint
//     form <…>.<bedrock…>.<region>.vpce.amazonaws.com;
//   - Vertex: <region>-aiplatform.googleapis.com.
func deniedLabel(h string) string {
	labels := strings.Split(h, ".")
	n := len(labels)
	if strings.HasSuffix(h, ".amazonaws.com") {
		if n >= 4 && strings.HasPrefix(labels[n-4], "bedrock") {
			return "a Bedrock endpoint (bedrock*.<region>.amazonaws.com)"
		}
		if n >= 5 && labels[n-3] == "vpce" && strings.HasPrefix(labels[n-5], "bedrock") {
			return "a Bedrock VPC endpoint"
		}
	}
	if n == 3 && labels[1] == "googleapis" && labels[2] == "com" && strings.HasSuffix(labels[0], "-aiplatform") {
		return "a regional Vertex AI endpoint (*-aiplatform.googleapis.com)"
	}
	return ""
}

// DeniedPattern reports why a redirectHosts wildcard "*.suffix" may not be
// used (nil when it may). The suffix must be a host name of at least
// MinPatternLabels labels; it may not be denied itself, have a denied name
// below it (any name the pattern can match must be one DeniedHost admits),
// sit below a denied suffix, or be equal to or a parent of a
// TooWideSuffixes entry. An exact host (no "*.") is DeniedHost's to judge.
func DeniedPattern(pattern string, ctx DenyContext) error {
	suffix, ok := strings.CutPrefix(pattern, "*.")
	if !ok {
		return fmt.Errorf("%w: %q is not a *.suffix pattern", ErrDeniedHost, pattern)
	}
	if !hostPattern.MatchString(suffix) || !validLabels(suffix) {
		return fmt.Errorf("%w: %q: %q is not a lowercase host name", ErrDeniedHost, pattern, suffix)
	}
	if strings.Count(suffix, ".")+1 < MinPatternLabels {
		return fmt.Errorf("%w: %q has fewer than %d labels after the *", ErrDeniedHost, pattern, MinPatternLabels)
	}
	if _, err := netip.ParseAddr(suffix); err == nil {
		return fmt.Errorf("%w: %q wildcards an address", ErrDeniedHost, pattern)
	}
	for _, w := range TooWideSuffixes {
		if under(w, suffix) {
			return fmt.Errorf("%w: %q covers the shared suffix %s", ErrDeniedHost, pattern, w)
		}
	}
	// A pattern matches every name strictly below suffix, so it is safe
	// only if suffix itself is admitted with its subtree free of denials:
	// a denied entry at or above suffix denies the whole subtree, one
	// below suffix is a name the pattern would match.
	for _, set := range [][]string{ForgeHosts, ModelAPIHosts, ctx.Hosts} {
		for _, d := range set {
			if d = canonicalHost(d); d == "" {
				continue
			}
			if under(suffix, d) || under(d, suffix) {
				return fmt.Errorf("%w: %q overlaps the denied %s", ErrDeniedHost, pattern, d)
			}
		}
	}
	for _, d := range clusterHosts {
		if under(d, suffix) {
			return fmt.Errorf("%w: %q can match the Kubernetes API name %s", ErrDeniedHost, pattern, d)
		}
	}
	if !ctx.Internal {
		for _, s := range clusterSuffixes {
			if under(suffix, s) {
				return fmt.Errorf("%w: %q is an in-cluster name", ErrDeniedHost, pattern)
			}
		}
	}
	if why := patternMeetsLabelRule(suffix); why != "" {
		return fmt.Errorf("%w: %q can match %s", ErrDeniedHost, pattern, why)
	}
	return nil
}

// patternMeetsLabelRule reports whether some name *.suffix matches is
// refused by deniedLabel. The pattern chooses the labels in front of
// suffix freely, so this asks whether a free label can land in a rule's
// fixed position, or the suffix itself already has the rule's shape.
func patternMeetsLabelRule(suffix string) string {
	labels := strings.Split(suffix, ".")
	n := len(labels)
	if strings.HasSuffix("."+suffix, ".amazonaws.com") {
		// bedrock*.<region>.amazonaws.com: the bedrock label is fourth
		// from the end, so any suffix of at most three labels leaves it
		// free; a longer suffix fixes it.
		if n <= 3 || strings.HasPrefix(labels[n-4], "bedrock") {
			return "a Bedrock endpoint"
		}
		// <…>.<bedrock…>.<region>.vpce.amazonaws.com: the bedrock label is
		// fifth from the end.
		if labels[n-3] == "vpce" && (n <= 4 || strings.HasPrefix(labels[n-5], "bedrock")) {
			return "a Bedrock VPC endpoint"
		}
	}
	// <region>-aiplatform.googleapis.com has exactly three labels, and a
	// pattern's names have at least four.
	return ""
}

// validLabels reports whether every label of h is a DNS label: no empty
// label, none longer than 63, none starting or ending with "-".
func validLabels(h string) bool {
	for l := range strings.SplitSeq(h, ".") {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return false
		}
	}
	return true
}

// isAddr reports whether h is an IP literal.
func isAddr(h string) bool {
	_, err := netip.ParseAddr(h)
	return err == nil
}
