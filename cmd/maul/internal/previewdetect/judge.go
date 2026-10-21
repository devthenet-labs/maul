// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewdetect

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/devthenet-labs/maul/cmd/maul/internal/previewsetup"
)

// The release ConfigMap settings detection reads: the chart's own keys
// (preview-controller's and the relay's), pinned to the chart's templates by
// TestReleaseKeysAreTheChartsKeys.
const (
	KeyPreviewHostSuffix = "MAUL_PREVIEW_HOST_SUFFIX"
	KeyPreviewLegacy     = "MAUL_PREVIEW_LEGACY_INGRESS"
	KeyAuthIssuer        = "MAUL_PREVIEW_AUTH_ISSUER"
	KeyAuthDexIssuer     = "MAUL_PREVIEW_AUTH_DEX_ISSUER_URL"
	KeyAuthGateway       = "MAUL_PREVIEW_AUTH_GATEWAY"
)

// The node labels that name a managed platform.
const (
	labelEKSPrefix      = "eks.amazonaws.com/"
	labelEKSComputeType = "eks.amazonaws.com/compute-type"
	labelGKENodePool    = "cloud.google.com/gke-nodepool"
	labelAKSCluster     = "kubernetes.azure.com/cluster"
	k3dNodePrefix       = "k3d-"
)

// Release is what the maul release's ConfigMaps say about previews.
type Release struct {
	// Unknown reports that the release's ConfigMaps could not be read, so
	// nothing below is known (and Previews false says nothing).
	Unknown bool
	// Previews and SignIn report whether preview-controller and the relay
	// are configured at all.
	Previews, SignIn bool
	HostSuffix       string
	// SignInHost is the relay's host (previewAuth.host), from its issuer.
	SignInHost string
	DexIssuer  string
	// Gateway is the relay's gateway switch, LegacyIngress
	// preview-controller's legacy slot Ingress switch (true when unset, its
	// flag's default).
	Gateway, LegacyIngress bool
}

// Findings are what detection concluded.
type Findings struct {
	// Platform is "" when the nodes do not agree on one maul knows.
	Platform       previewsetup.Platform
	PlatformReason string
	EKSAutoMode    bool
	Nodes          []Node
	// APIServerCIDRs are the kubernetes Service's /32s, APIEndpointCIDRs
	// the API server's own addresses as /32s.
	APIServerCIDRs   []string
	APIEndpointCIDRs []string
	ClusterDNSCIDR   string
	IngressClasses   []IngressClass
	GatewayClasses   []string
	CertManager      Presence
	Tailscale        Presence
	// NetworkPolicy names what enforces NetworkPolicy, "" when nothing
	// maul recognises was seen enforcing it.
	NetworkPolicy string
	// NetworkPolicyWarning says why an enforcer that was seen does not
	// enforce (the VPC CNI's ships switched off), when NetworkPolicy is "".
	NetworkPolicyWarning string
	// EBPF reports an enforcer that judges NetworkPolicy after the Service
	// address is translated (so the kubernetes Service's /32 alone matches
	// nothing): kube-router and Cilium.
	EBPF    bool
	Release Release
	Notes   []string
}

// Judge decides Findings from an Observation. Pure.
func Judge(o Observation) Findings {
	f := Findings{
		Nodes:            o.Nodes,
		APIServerCIDRs:   hostCIDRs(o.APIServiceIPs),
		APIEndpointCIDRs: hostCIDRs(o.APIEndpoints),
		IngressClasses:   o.IngressClasses,
		GatewayClasses:   o.GatewayClasses,
		CertManager:      o.CertManager,
		Tailscale:        o.Tailscale,
		Notes:            o.Notes,
	}
	if c := hostCIDRs([]string{o.ClusterDNS}); len(c) == 1 {
		f.ClusterDNSCIDR = c[0]
	}
	f.Platform, f.PlatformReason = PlatformOf(o.Nodes)
	for _, n := range o.Nodes {
		if n.Labels[labelEKSComputeType] == "auto" {
			f.EKSAutoMode = true
		}
	}
	f.NetworkPolicy, f.EBPF, f.NetworkPolicyWarning = enforcer(o, f.Platform, f.EKSAutoMode)
	f.Release = release(o)
	return f
}

// PlatformOf names the platform every node agrees on, and why; "" when
// there is no node, or they disagree, or one is no platform maul knows.
func PlatformOf(nodes []Node) (previewsetup.Platform, string) {
	if len(nodes) == 0 {
		return "", "no node was read"
	}
	var got previewsetup.Platform
	for _, n := range nodes {
		p := nodePlatform(n)
		switch {
		case p == "":
			return "", fmt.Sprintf("node %s (provider ID %q) is on no platform maul recognises", n.Name,
				n.ProviderID)
		case got != "" && p != got:
			return "", fmt.Sprintf("the nodes disagree (%s and %s)", got, p)
		}
		got = p
	}
	scheme, _, _ := strings.Cut(nodes[0].ProviderID, "://")
	return got, fmt.Sprintf("%d node%s, provider ID %s://", len(nodes), plural(len(nodes)), scheme)
}

// nodePlatform is one node's platform, from its provider ID and labels.
func nodePlatform(n Node) previewsetup.Platform {
	scheme, _, ok := strings.Cut(n.ProviderID, "://")
	if !ok {
		return ""
	}
	switch scheme {
	case "k3s":
		if strings.HasPrefix(n.Name, k3dNodePrefix) {
			return previewsetup.PlatformK3d
		}
		return previewsetup.PlatformK3s
	case "aws":
		for k := range n.Labels {
			if strings.HasPrefix(k, labelEKSPrefix) {
				return previewsetup.PlatformEKS
			}
		}
	case "gce":
		if _, ok := n.Labels[labelGKENodePool]; ok {
			return previewsetup.PlatformGKE
		}
	case "azure":
		if _, ok := n.Labels[labelAKSCluster]; ok {
			return previewsetup.PlatformAKS
		}
	}
	return ""
}

// enforcers are the NetworkPolicy enforcers recognised by an image's
// repository path, first match wins, and whether each judges after the
// Service address is translated. The VPC CNI's agent is judged by its
// switches too (vpcCNI), since its image is there whether or not it
// enforces.
var enforcers = []struct {
	match, name string
	ebpf        bool
}{
	{"cilium", "Cilium", true},
	{"anetd", "GKE Dataplane V2 (Cilium)", true},
	{"kube-router", "kube-router", true},
	{"calico", "Calico", false},
	{policyAgentImage, vpcCNIAgent, false},
	{"antrea", "Antrea", false},
	{"weave", "Weave Net", false},
}

const (
	vpcCNIAgent      = "the VPC CNI network policy agent"
	vpcCNIController = "the VPC CNI network policy controller (EKS Auto Mode)"
)

// enforcer names what enforces NetworkPolicy from kube-system's images (and,
// for the VPC CNI, its switches), whether it judges after translation, and
// otherwise why an enforcer seen does not enforce. k3s and k3d run
// kube-router inside k3s itself, with no Pod of its own; EKS Auto Mode runs
// the VPC CNI's agent on the node, with no Pod either.
func enforcer(o Observation, platform previewsetup.Platform, autoMode bool) (string, bool, string) {
	for _, e := range enforcers {
		for _, img := range o.Images {
			if !strings.Contains(imageName(img), e.match) {
				continue
			}
			if e.name == vpcCNIAgent {
				if why := vpcCNIOff(o, true); why != "" {
					return "", false, why
				}
			}
			return e.name, e.ebpf, ""
		}
	}
	switch {
	case platform == previewsetup.PlatformK3s || platform == previewsetup.PlatformK3d:
		return "kube-router, built into k3s (unless it runs with --disable-network-policy)", true, ""
	case platform == previewsetup.PlatformEKS && autoMode:
		if why := vpcCNIOff(o, false); why != "" {
			return "", false, why
		}
		return vpcCNIController, false, ""
	}
	return "", false, ""
}

// vpcCNIOff says what keeps the VPC CNI from enforcing NetworkPolicy, ""
// when nothing does: its network policy controller's ConfigMap switch,
// and, where the agent runs as a Pod, the agent's own flag, both off by
// default.
func vpcCNIOff(o Observation, agentPod bool) string {
	var off []string
	if on, err := strconv.ParseBool(o.VPCCNIPolicyController); err != nil || !on {
		off = append(off, fmt.Sprintf("kube-system/%s does not set %s: \"true\"", vpcCNIConfigMap,
			KeyVPCCNIPolicyController))
	}
	if agentPod && !slices.ContainsFunc(o.PolicyAgentArgs, agentEnabled) {
		off = append(off, "its agent does not run with --enable-network-policy=true (the add-on's "+
			"enableNetworkPolicy)")
	}
	if len(off) == 0 {
		return ""
	}
	return "the VPC CNI is present but enforces no NetworkPolicy: " + strings.Join(off, ", and ")
}

// agentEnabled reports a policy agent command line that switches
// enforcement on.
func agentEnabled(args []string) bool {
	on := false
	for _, a := range args {
		switch v, ok := strings.CutPrefix(strings.TrimSpace(a), "--enable-network-policy"); {
		case ok && v == "":
			on = true
		case ok && strings.HasPrefix(v, "="):
			b, err := strconv.ParseBool(v[1:])
			on = err == nil && b
		}
	}
	return on
}

// imageName is an image reference's repository path, without the registry
// host, tag or digest (calico/node, amazon/aws-network-policy-agent).
func imageName(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if host, rest, ok := strings.Cut(ref, "/"); ok && (strings.ContainsAny(host, ".:") || host == "localhost") {
		ref = rest
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return strings.ToLower(ref)
}

// release reads the release's preview settings.
func release(o Observation) Release {
	s := o.Release
	r := Release{Unknown: s.Preview.Err != nil || s.Auth.Err != nil, Previews: s.Preview.Found(),
		SignIn: s.Auth.Found(), LegacyIngress: true}
	if r.Previews {
		r.HostSuffix = strings.TrimSpace(s.Preview.Data[KeyPreviewHostSuffix])
		if v, ok := s.Preview.Data[KeyPreviewLegacy]; ok {
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				r.LegacyIngress = b
			}
		}
	}
	if r.SignIn {
		if u, err := url.Parse(strings.TrimSpace(s.Auth.Data[KeyAuthIssuer])); err == nil {
			r.SignInHost = u.Hostname()
		}
		r.DexIssuer = strings.TrimSpace(s.Auth.Data[KeyAuthDexIssuer])
		r.Gateway = s.Auth.Bool(KeyAuthGateway)
	}
	return r
}

// hostCIDRs are the IPv4 addresses among ips, each as a /32.
func hostCIDRs(ips []string) []string {
	var out []string
	for _, s := range ips {
		ip, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil || !ip.Is4() || ip.IsUnspecified() {
			continue
		}
		out = append(out, netip.PrefixFrom(ip, 32).String())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Given reports whether an answer flag (its name without the dashes) was
// given on the command line.
type Given func(flag string) bool

// Apply lays f over a: only what is still unanswered (empty, or the API
// server CIDR's default auto, or a false "already installed"), and never an
// answer whose flag was given. It returns what it filled, one line each.
// Apply is idempotent.
func Apply(f Findings, a *previewsetup.Answers, given Given) []string {
	var filled []string
	set := func(flag string, unanswered bool, value, why string, do func()) {
		if !unanswered || given(flag) || value == "" {
			return
		}
		do()
		line := "--" + flag + " " + value
		if why != "" {
			line += " (" + why + ")"
		}
		filled = append(filled, line)
	}
	set("platform", a.Platform == "", string(f.Platform), f.PlatformReason,
		func() { a.Platform = f.Platform })
	set("host-suffix", a.HostSuffix == "", f.Release.HostSuffix, "the release's preview.hostSuffix",
		func() { a.HostSuffix = f.Release.HostSuffix })
	set("sign-in-host", a.SignInHost == "", f.Release.SignInHost, "the release's previewAuth.host",
		func() { a.SignInHost = f.Release.SignInHost })
	set("dex-issuer", a.Dex.Issuer == "", f.Release.DexIssuer, "the release's previewAuth.dex.issuerURL",
		func() { a.Dex.Issuer = f.Release.DexIssuer })
	if cidr := f.serviceCIDR(a.Platform); cidr != "" {
		set("api-server-cidr", a.APIServerCIDR == previewsetup.DefaultAPIServerCIDR, cidr,
			"the kubernetes Service; EKS rotates the API server's own addresses",
			func() { a.APIServerCIDR = cidr })
	}
	set("cert-manager-installed", a.GatewayTerminatesTLS() && !a.Certificate.CertManagerInstalled &&
		f.CertManager == Present, "true", "its "+CertManagerCRD+" CRD",
		func() { a.Certificate.CertManagerInstalled = true })
	set("tailscale-operator-installed", a.Edge == previewsetup.EdgeTailscale && !a.Tailscale.OperatorInstalled &&
		f.Tailscale == Present, "true", "its "+TailscaleCRD+" CRD",
		func() { a.Tailscale.OperatorInstalled = true })
	return filled
}

// serviceCIDR is the API server CIDR detection would answer instead of
// auto: the kubernetes Service's one /32 on EKS, where the chart's notes
// keep it (auto's endpoint addresses rotate there), unless an enforcer that
// judges after translation runs (then the Service /32 alone matches
// nothing). Elsewhere auto, a render-time lookup of both, is kept.
func (f Findings) serviceCIDR(p previewsetup.Platform) string {
	if p != previewsetup.PlatformEKS || f.EBPF || len(f.APIServerCIDRs) != 1 {
		return ""
	}
	return f.APIServerCIDRs[0]
}

// PreviewNodes sorts the nodes by the preview label and taint a names:
// both (preview nodes), the label alone (any Pod may run beside preview
// code there) and the taint alone.
func (f Findings) PreviewNodes(a previewsetup.Answers) (both, labelOnly, taintOnly []string) {
	for _, n := range f.Nodes {
		label := n.Labels[a.NodeIsolation.Label] == a.NodeIsolation.LabelValue && a.NodeIsolation.Label != ""
		taint := slices.ContainsFunc(n.Taints, func(t corev1.Taint) bool {
			return t.Key == a.NodeIsolation.TaintKey && t.Effect == corev1.TaintEffectNoExecute
		})
		switch {
		case label && taint:
			both = append(both, n.Name)
		case label:
			labelOnly = append(labelOnly, n.Name)
		case taint:
			taintOnly = append(taintOnly, n.Name)
		}
	}
	return both, labelOnly, taintOnly
}

// Report is what detection found, one line each, for the operator: the
// facts, then the warnings that concern answers a (complete or not).
func Report(f Findings, a previewsetup.Answers) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if f.Platform != "" {
		add("platform: %s (%s)", f.Platform, f.PlatformReason)
	} else {
		add("platform: not recognised: %s", f.PlatformReason)
	}
	if a.Platform != "" && f.Platform != "" && a.Platform != f.Platform {
		add("warning: --platform is %s, but the nodes say %s", a.Platform, f.Platform)
	}
	if f.EKSAutoMode {
		add("EKS Auto Mode nodes: name the preview NodePool and NodeClass with --eks-auto-mode")
	}
	add("API server: Service %s, endpoints %s; --api-server-cidr %s", listOr(f.APIServerCIDRs, "unknown"),
		listOr(f.APIEndpointCIDRs, "unknown"), a.APIServerCIDR)
	if f.ClusterDNSCIDR != "" {
		add("cluster DNS: %s (the chart's clusterDNSCIDR, if your values do not set it)", f.ClusterDNSCIDR)
	}
	classes := make([]string, 0, len(f.IngressClasses))
	for _, c := range f.IngressClasses {
		s := c.Name
		if c.Default {
			s += " (default)"
		}
		classes = append(classes, s)
	}
	add("Ingress classes: %s; Gateway classes: %s", listOr(classes, "none"), listOr(f.GatewayClasses, "none"))
	add("cert-manager: %s; Tailscale operator: %s", f.CertManager, f.Tailscale)
	switch {
	case f.NetworkPolicy != "":
		add("NetworkPolicy: enforced by %s", f.NetworkPolicy)
	case f.NetworkPolicyWarning != "":
		add("warning: NetworkPolicy: %s; previews rely on it", f.NetworkPolicyWarning)
	default:
		add("warning: NetworkPolicy: no enforcer recognised in kube-system; previews rely on it, so check that " +
			"your CNI enforces it")
	}
	out = append(out, nodeReport(f, a)...)
	r := f.Release
	switch {
	case r.Unknown:
		add("release in namespace %s: unknown, its ConfigMaps could not be read (see the note below)",
			a.Namespace)
	case !r.Previews:
		add("release in namespace %s: previews not configured yet", a.Namespace)
	case r.LegacyIngress:
		add("warning: release in namespace %s still serves previews through the legacy load balancer path "+
			"(legacyIngress true): move it to the gateway as docs/configuration/preview-gateway.md describes; "+
			"this command does not", a.Namespace)
	case r.SignIn && r.Gateway:
		add("release in namespace %s: previews served by the preview gateway already", a.Namespace)
	}
	for _, n := range f.Notes {
		add("note: %s", n)
	}
	return out
}

// nodeReport is the preview node lines.
func nodeReport(f Findings, a previewsetup.Answers) []string {
	if a.IsolationMode() == previewsetup.IsolationNone || len(f.Nodes) == 0 ||
		a.NodeIsolation.EKSAutoMode != (previewsetup.EKSAutoMode{}) {
		return nil
	}
	var out []string
	both, labelOnly, taintOnly := f.PreviewNodes(a)
	sel := a.NodeIsolation.Label + "=" + a.NodeIsolation.LabelValue
	taint := a.NodeIsolation.TaintKey + "=true:NoExecute"
	if len(both) > 0 {
		out = append(out, fmt.Sprintf("preview nodes (%s, %s): %s", sel, taint, strings.Join(both, ", ")))
	} else {
		out = append(out, fmt.Sprintf("preview nodes: none carries %s and %s yet; NEXT-STEPS.md says how to "+
			"add one", sel, taint))
	}
	if len(labelOnly) > 0 {
		out = append(out, fmt.Sprintf("warning: %s carr%s %s without the taint: any Pod may run beside preview "+
			"code there", strings.Join(labelOnly, ", "), pluralVerb(len(labelOnly)), sel))
	}
	if len(taintOnly) > 0 {
		out = append(out, fmt.Sprintf("note: %s carr%s the taint %s without the label", strings.Join(taintOnly, ", "),
			pluralVerb(len(taintOnly)), taint))
	}
	if (f.Platform == previewsetup.PlatformK3s || f.Platform == previewsetup.PlatformK3d) && len(f.Nodes) == 1 {
		out = append(out, "warning: one node: add an agent node for previews, or answer --node-isolation none "+
			"(previews then share the node with everything else)")
	}
	return out
}

// String names a Presence.
func (p Presence) String() string {
	switch p {
	case Present:
		return "installed"
	case Absent:
		return "not installed"
	}
	return "unknown"
}

func listOr(s []string, none string) string {
	if len(s) == 0 {
		return none
	}
	return strings.Join(s, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pluralVerb(n int) string {
	if n == 1 {
		return "ies"
	}
	return "y"
}
