// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// forwardedHopsDirect is the gateway's forwardedHops when nothing appends
// X-Forwarded-For in front of it: an L4 load balancer that keeps the
// client's address (externalTrafficPolicy Local) or a tailnet proxy
// (design §5.3: fixed per recipe, never asked).
const forwardedHopsDirect = 0

// tailnetPrefixes are the addresses Tailscale gives devices: the CGNAT
// range and its IPv6 unique local prefix.
var tailnetPrefixes = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48")}

// tailscaleTagPattern is a Tailscale ACL tag.
var tailscaleTagPattern = regexp.MustCompile(`^tag:[a-z0-9][a-z0-9-]{0,62}$`)

// gatewayCertificate is what the gateway-tls and tailscale recipes share
// (design §4.3 R2, R3): the wildcard Certificate covering both names, the
// Issuer that answers its DNS-01 challenge (unless the operator brings
// their own), cert-manager's pinned values (unless it runs), and the
// gateway terminating TLS itself with the Certificate's Secret.
func gatewayCertificate(a Answers, v values) ([]File, error) {
	var files []File
	cert, err := certificate(a)
	if err != nil {
		return nil, err
	}
	files = append(files, File{Path: CertificateFile, Data: cert})
	if a.WritesIssuer() {
		iss, err := issuer(a)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: IssuerFile, Data: iss})
	}
	if !a.Certificate.CertManagerInstalled {
		cm, err := certManagerValues()
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: CertManager.ValuesFile, Data: cm})
	}
	gw := v.at("previewAuth", "gateway")
	gw["forwardedHops"] = forwardedHopsDirect
	v.at("previewAuth", "gateway", "tls")["secrets"] = []any{map[string]any{
		"secretName": CertificateSecret,
		"hosts":      []string{"*." + a.HostSuffix, a.SignInHost},
	}}
	// The gateway's own exposure alone: an Ingress or HTTPRoute in the
	// operator's values would be a second one, which the chart refuses
	// beside tls.secrets.
	v.at("previewAuth", "gateway", "ingress")["enabled"] = false
	v.at("previewAuth", "gateway", "httpRoute")["parentRefs"] = []any{}
	return files, nil
}

// gatewayTLSRecipe is R2 (design §4.3): the gateway's Service is a
// LoadBalancer of its own and terminates TLS, so no ingress controller is
// needed. externalTrafficPolicy Local keeps the viewer's address as the
// gateway's peer, which forwardedHops 0 and inboundCIDRs judge.
func gatewayTLSRecipe(a Answers, v values) ([]File, error) {
	files, err := gatewayCertificate(a, v)
	if err != nil {
		return nil, err
	}
	gw := v.at("previewAuth", "gateway")
	gw["inboundCIDRs"] = stringsOrEmpty(a.LoadBalancer.InboundCIDRs)
	gw["publishedAddresses"] = []string{}
	svc := v.at("previewAuth", "gateway", "service")
	svc["type"] = "LoadBalancer"
	svc["externalTrafficPolicy"] = "Local"
	svc["loadBalancerClass"] = a.LoadBalancer.Class
	return files, nil
}

// tailscaleRecipe is R3 (design §4.3): the Tailscale operator gives the
// gateway's ClusterIP Service a tailnet device of its own, so nothing
// listens on the host and nothing is public. The tailnet policy is the
// allowlist (inboundCIDRs stays empty: the gateway sees the proxy's
// address, not the viewer's), and sign-in stays required (§5.2).
func tailscaleRecipe(a Answers, v values) ([]File, error) {
	files, err := gatewayCertificate(a, v)
	if err != nil {
		return nil, err
	}
	if !a.Tailscale.OperatorInstalled {
		ts, err := tailscaleOperatorValues()
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: TailscaleOperator.ValuesFile, Data: ts})
	}
	gw := v.at("previewAuth", "gateway")
	gw["inboundCIDRs"] = []string{}
	gw["publishedAddresses"] = stringsOrEmpty(a.Tailscale.Addresses)
	svc := v.at("previewAuth", "gateway", "service")
	svc["type"] = "ClusterIP"
	svc["loadBalancerClass"] = ""
	svc["annotations"] = map[string]any{
		"tailscale.com/expose":   "true",
		"tailscale.com/hostname": a.TailscaleHostname(),
		"tailscale.com/tags":     a.TailscaleTag(),
	}
	return files, nil
}

// validateLoadBalancer checks the gateway-tls recipe's answers.
func (a Answers) validateLoadBalancer() []error {
	var errs []error
	lb := a.LoadBalancer
	if lb.Class != "" && len(validation.IsQualifiedName(lb.Class)) > 0 {
		errs = append(errs, fmt.Errorf("--load-balancer-class %q is not a load balancer class (a qualified "+
			"name, such as service.k8s.aws/nlb)", lb.Class))
	}
	for _, c := range lb.InboundCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil || p != p.Masked() {
			errs = append(errs, fmt.Errorf("--inbound-cidr %q is not a CIDR in its network form", c))
		}
	}
	return errs
}

// validateTailscale checks the tailscale recipe's answers.
func (a Answers) validateTailscale() []error {
	var errs []error
	t := a.Tailscale
	if t.Hostname != "" && len(validation.IsDNS1123Label(t.Hostname)) > 0 {
		errs = append(errs, fmt.Errorf("--tailscale-hostname %q is not a DNS label", t.Hostname))
	}
	if t.Tag != "" && !tailscaleTagPattern.MatchString(t.Tag) {
		errs = append(errs, fmt.Errorf("--tailscale-tag %q is not a tag (tag:<name>, lowercase)", t.Tag))
	}
	if a.TailscaleTag() == TailscaleOperatorTag {
		errs = append(errs, errors.New("--tailscale-tag "+TailscaleOperatorTag+" is the operator's own: give the "+
			"previews a tag of their own, so the tailnet policy can grant viewers that device alone"))
	}
	for _, s := range t.Addresses {
		ip, err := netip.ParseAddr(s)
		if err != nil || !inTailnet(ip) {
			errs = append(errs, fmt.Errorf("--tailscale-address %q is not a tailnet address (%s)", s,
				joinPrefixes(tailnetPrefixes)))
		}
	}
	return errs
}

func inTailnet(ip netip.Addr) bool {
	for _, p := range tailnetPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func joinPrefixes(ps []netip.Prefix) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, " or ")
}
