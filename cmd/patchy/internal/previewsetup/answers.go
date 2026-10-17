// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Edge is how previews reach viewers: one recipe each (design §3, approach C).
type Edge string

const (
	// EdgeExisting is R4: an edge the cluster already runs (an Ingress
	// class, or a Gateway API Gateway) routes both names to the gateway.
	EdgeExisting Edge = "existing"
	// EdgeGatewayTLS is R2: the gateway terminates TLS itself, on a
	// LoadBalancer Service, with a cert-manager DNS-01 certificate.
	EdgeGatewayTLS Edge = "gateway-tls"
	// EdgeTailscale is R3: a tailnet device through the Tailscale operator.
	EdgeTailscale Edge = "tailscale"
	// EdgeCloudflareTunnel is R1: cloudflared in the cluster, TLS at
	// Cloudflare.
	EdgeCloudflareTunnel Edge = "cloudflare-tunnel"
)

// Edges lists every edge, in the order the help names them.
func Edges() []string {
	return []string{string(EdgeCloudflareTunnel), string(EdgeGatewayTLS), string(EdgeTailscale), string(EdgeExisting)}
}

// Platform is the cluster's kind, which picks the node isolation and
// registry pull guidance.
type Platform string

const (
	PlatformK3d   Platform = "k3d"
	PlatformK3s   Platform = "k3s"
	PlatformEKS   Platform = "eks"
	PlatformGKE   Platform = "gke"
	PlatformAKS   Platform = "aks"
	PlatformOther Platform = "other"
)

// Platforms lists every platform.
func Platforms() []string {
	return []string{string(PlatformK3d), string(PlatformK3s), string(PlatformEKS), string(PlatformGKE),
		string(PlatformAKS), string(PlatformOther)}
}

// The node isolation modes, preview.nodeIsolation.mode's.
const (
	IsolationRequired = "required"
	IsolationNone     = "none"
)

// How the existing edge gets its certificate.
const (
	// TLSCertificate: a cert-manager Certificate in the release namespace
	// whose Secret both Ingresses name (an ingress controller in the
	// cluster terminates TLS).
	TLSCertificate = "certificate"
	// TLSEdge: the edge holds the certificate itself (an ALB with ACM, a
	// Gateway listener), so nothing in the release namespace does.
	TLSEdge = "edge"
)

// The defaults every answer the operator need not give starts from.
const (
	DefaultRelease         = "patchy"
	DefaultNamespace       = "patchy"
	DefaultDexNamespace    = "dex"
	DefaultDexClientID     = "patchy-preview-auth"
	DefaultImagePathPrefix = "patchy/previews"
	// DefaultNodeLabel and DefaultTaintKey are patchy's own keys (owner
	// decision Q5), so operators stop inventing domains for them.
	DefaultNodeLabel      = "patchy.bitwisemedia.uk/previews"
	DefaultNodeLabelValue = "true"
	DefaultTaintKey       = "patchy.bitwisemedia.uk/preview-only"
	// DefaultAPIServerCIDR is the chart's render-time lookup of the API
	// server's Service and endpoint addresses (previewController.config
	// .apiServerCIDR).
	DefaultAPIServerCIDR = "auto"
	// DefaultCertificateIssuer is the namespaced Issuer the docs' wildcard
	// certificate names, and the one the gateway-tls recipe generates.
	DefaultCertificateIssuer = "Issuer/preview-dns01"
)

// Answers is every answer `setup previews` takes. answers.yaml records it,
// so a re-run starts from the last one; it never holds a secret.
type Answers struct {
	// CLIVersion is the CLI that wrote the files: recorded, never asked.
	CLIVersion string   `json:"cliVersion"`
	Edge       Edge     `json:"edge"`
	Platform   Platform `json:"platform"`
	// Release and Namespace are the patchy Helm release and its namespace.
	Release   string `json:"release"`
	Namespace string `json:"namespace"`
	// HostSuffix is preview.hostSuffix: previews are
	// https://<label>.<HostSuffix>.
	HostSuffix string `json:"hostSuffix"`
	// SignInHost is where viewers sign in: previewAuth.host, outside the
	// suffix and on another registrable domain.
	SignInHost string `json:"signInHost"`
	Dex        Dex    `json:"dex"`
	// Viewers are previewAuth.viewers. Empty needs NoViewers: an empty
	// list refuses every sign-in.
	Viewers   Viewers `json:"viewers"`
	NoViewers bool    `json:"noViewers,omitempty"`
	// Registry and ImagePathPrefix are where preview images come from:
	// <Registry>/<ImagePathPrefix>/<app>:sha-<commit>.
	Registry        string `json:"registry"`
	ImagePathPrefix string `json:"imagePathPrefix"`
	NodeIsolation   Nodes  `json:"nodeIsolation"`
	// APIServerCIDR is previewController.config.apiServerCIDR: auto, or
	// the kubernetes Service's /32.
	APIServerCIDR string `json:"apiServerCIDR"`
	// Existing is the existing-edge recipe's answers.
	Existing ExistingEdge `json:"existing,omitzero"`
}

// Dex is the relay's static client at the operator's Dex.
type Dex struct {
	Issuer    string `json:"issuer"`
	Namespace string `json:"namespace"`
	ClientID  string `json:"clientID"`
}

// Viewers are who may view previews: Dex GitHub groups (<org>:<team>) and
// GitHub logins, as the chart takes them (it prepends the claim prefixes).
type Viewers struct {
	Teams []string `json:"teams,omitempty"`
	Users []string `json:"users,omitempty"`
}

// Nodes is preview node isolation. An empty Mode is the platform's default.
type Nodes struct {
	Mode       string `json:"mode,omitempty"`
	Label      string `json:"label"`
	LabelValue string `json:"labelValue"`
	TaintKey   string `json:"taintKey"`
	// EKSAutoMode, on EKS Auto Mode, is the dedicated NodePool and its
	// NodeClass slot Pods select instead of Label (the chart's
	// preview.nodeIsolation.eksAutoMode); empty selects by Label.
	EKSAutoMode EKSAutoMode `json:"eksAutoMode,omitzero"`
}

// EKSAutoMode names an EKS Auto Mode NodePool and its NodeClass.
type EKSAutoMode struct {
	NodePool  string `json:"nodePool"`
	NodeClass string `json:"nodeClass"`
}

// ParseEKSAutoMode splits --eks-auto-mode's <nodePool>/<nodeClass>; ""
// is no Auto Mode pair.
func ParseEKSAutoMode(s string) (EKSAutoMode, error) {
	if s == "" {
		return EKSAutoMode{}, nil
	}
	pool, class, ok := strings.Cut(s, "/")
	if !ok || pool == "" || class == "" {
		return EKSAutoMode{}, fmt.Errorf("--eks-auto-mode %q is not <nodePool>/<nodeClass>", s)
	}
	return EKSAutoMode{NodePool: pool, NodeClass: class}, nil
}

// ExistingEdge is the existing-edge recipe's answers: exactly one of
// IngressClass and GatewayParent, except on EKS, where neither is the
// chart's edge class (the ALB the webhook and status page share).
type ExistingEdge struct {
	IngressClass string `json:"ingressClass,omitempty"`
	// GatewayParent is a Gateway API parent: <namespace>/<name>[/<section>].
	GatewayParent string `json:"gatewayParent,omitempty"`
	// TLS is TLSCertificate or TLSEdge; empty derives it (TLSEdge on a
	// Gateway parent, an ALB class or EKS, else TLSCertificate).
	TLS string `json:"tls,omitempty"`
	// CertificateIssuer is the cert-manager issuer the Certificate names,
	// <Issuer|ClusterIssuer>/<name>; empty is DefaultCertificateIssuer.
	CertificateIssuer string `json:"certificateIssuer,omitempty"`
}

// Defaults are the answers before the operator gives any.
func Defaults() Answers {
	return Answers{
		Release:         DefaultRelease,
		Namespace:       DefaultNamespace,
		Dex:             Dex{Namespace: DefaultDexNamespace, ClientID: DefaultDexClientID},
		ImagePathPrefix: DefaultImagePathPrefix,
		NodeIsolation: Nodes{Label: DefaultNodeLabel, LabelValue: DefaultNodeLabelValue,
			TaintKey: DefaultTaintKey},
		APIServerCIDR: DefaultAPIServerCIDR,
	}
}

// Normalize puts a in its one canonical form: hosts lowercased and without
// a trailing dot, viewers sorted and each listed once. Plan renders from
// it, so the same answers always give the same bytes.
func (a *Answers) Normalize() {
	a.HostSuffix = normHost(a.HostSuffix)
	a.SignInHost = normHost(a.SignInHost)
	a.Registry = strings.ToLower(strings.TrimSpace(a.Registry))
	a.Dex.Issuer = strings.TrimSpace(a.Dex.Issuer)
	a.Viewers.Teams = sortedUnique(a.Viewers.Teams)
	a.Viewers.Users = sortedUnique(a.Viewers.Users)
}

func normHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

func sortedUnique(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// IsolationMode is the node isolation mode in force: the answer, else the
// platform's default (required everywhere patchy knows how to add a
// dedicated node; "" for other, which must answer).
func (a Answers) IsolationMode() string {
	if a.NodeIsolation.Mode != "" {
		return a.NodeIsolation.Mode
	}
	if a.Platform == PlatformOther || a.Platform == "" {
		return ""
	}
	return IsolationRequired
}

// EdgeTLS is how the existing edge gets its certificate: the answer, else
// derived (design §4.3 R4: the edge holds it on ALB/ACM and a Gateway
// listener, a cert-manager Certificate otherwise). EKS alone says nothing:
// an ingress-nginx or Traefik class there terminates TLS in the cluster
// like anywhere else; only the chart's edge class (no --ingress-class on
// EKS) and an alb class are ALBs.
func (a Answers) EdgeTLS() string {
	e := a.Existing
	switch {
	case e.TLS != "":
		return e.TLS
	case e.GatewayParent != "", e.IngressClass == "alb", a.ChartEdgeClass():
		return TLSEdge
	}
	return TLSCertificate
}

// ChartEdgeClass reports whether both Ingresses take the chart's own edge
// class (edgeIngressClass, the ALB the webhook and status page share): an
// existing edge on EKS with no --ingress-class and no --gateway-parent.
func (a Answers) ChartEdgeClass() bool {
	return a.Edge == EdgeExisting && a.Platform == PlatformEKS && a.Existing.IngressClass == "" &&
		a.Existing.GatewayParent == ""
}

// Issuer is the cert-manager issuer the Certificate names.
func (a Answers) Issuer() (kind, name string) {
	ref := a.Existing.CertificateIssuer
	if ref == "" {
		ref = DefaultCertificateIssuer
	}
	kind, name, _ = strings.Cut(ref, "/")
	return kind, name
}

// Missing names the flags that answer what a still lacks, in flag order:
// what a non-interactive run must be given.
func (a Answers) Missing() []string {
	var out []string
	add := func(empty bool, flag string) {
		if empty {
			out = append(out, flag)
		}
	}
	add(a.Platform == "", "--platform")
	add(a.Edge == "", "--edge")
	add(a.HostSuffix == "", "--host-suffix")
	add(a.SignInHost == "", "--sign-in-host")
	add(a.Dex.Issuer == "", "--dex-issuer")
	add(len(a.Viewers.Teams) == 0 && len(a.Viewers.Users) == 0 && !a.NoViewers,
		"--viewer-team or --viewer-user (or --no-viewers)")
	add(a.Registry == "", "--registry")
	add(a.Platform == PlatformOther && a.NodeIsolation.Mode == "", "--node-isolation")
	add(a.Edge == EdgeExisting && a.Platform != PlatformEKS && a.Platform != "" &&
		a.Existing.IngressClass == "" && a.Existing.GatewayParent == "", "--ingress-class or --gateway-parent")
	return out
}

// Fullname is the chart's patchy.fullname for the release (no
// nameOverride or fullnameOverride): the release name when it already
// contains "patchy", else <release>-patchy.
func (a Answers) Fullname() string {
	name := a.Release
	if !strings.Contains(name, "patchy") {
		name += "-patchy"
	}
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimSuffix(name, "-")
}

// MarshalAnswers renders answers.yaml: a header, then a, keys sorted.
func MarshalAnswers(a Answers) ([]byte, error) {
	body, err := yaml.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("render answers.yaml: %w", err)
	}
	head := "# The answers `patchy setup previews` last rendered this directory from. A re-run starts\n" +
		"# from them; flags given then override them. No secret is ever recorded here.\n"
	return append([]byte(head), body...), nil
}

// ErrAnswers reports an answers.yaml that cannot be read back.
var ErrAnswers = errors.New("answers.yaml")

// ParseAnswers reads answers.yaml back. A field it does not know (from a
// newer CLI, or a typo) is refused rather than dropped.
func ParseAnswers(data []byte) (Answers, error) {
	var a Answers
	if err := yaml.UnmarshalStrict(data, &a); err != nil {
		return Answers{}, fmt.Errorf("%w: %w", ErrAnswers, err)
	}
	return a, nil
}
