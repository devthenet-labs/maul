// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import "strings"

// The upstream charts the recipes write values for, and the images their
// manifests run, each pinned to one version (design §5.3): a pin is
// maul's recommendation, so a stale one is a maul-documented
// vulnerability. Each version (an image's tag and digest) is a constant of
// its own under a renovate comment, so a bump is one line;
// TestPinsAreTracked holds every pin to its comment, TestRenovateTracksPins
// holds .github/renovate.json5's regex manager to extracting each one (the
// org preset scans only Dockerfiles), and `mise run preview-pins` (CI's
// preview-pins job) checks that each version exists upstream and that the
// generated values and manifests are accepted by the chart, the CRDs and
// the image at that version.

// renovate: datasource=helm registryUrl=https://charts.jetstack.io depName=cert-manager
const certManagerVersion = "v1.21.2"

// renovate: datasource=helm registryUrl=https://pkgs.tailscale.com/helmcharts depName=tailscale-operator
const tailscaleOperatorVersion = "1.104.1"

// renovate: datasource=docker depName=docker.io/cloudflare/cloudflared
const cloudflaredImage = "2026.10.1@sha256:a15a3b50b8dabd57ac2090e9484c0247dba9dfa1f0df4d298d4317f9d69eb2d5"

// Pin is an upstream Helm chart at the version the recipes are written
// for, and where NEXT-STEPS.md installs it.
type Pin struct {
	// Chart and Repo name the chart: `helm install <Release> <Chart>
	// --repo <Repo> --version <Version>`.
	Chart   string
	Repo    string
	Version string
	// Release and Namespace are where NEXT-STEPS.md installs it.
	Release   string
	Namespace string
	// ValuesFile is the values file the recipe writes for it.
	ValuesFile string
}

// The pinned charts.
var (
	CertManager = Pin{
		Chart: "cert-manager", Repo: "https://charts.jetstack.io", Version: certManagerVersion,
		Release: "cert-manager", Namespace: "cert-manager", ValuesFile: "upstream/cert-manager.values.yaml",
	}
	TailscaleOperator = Pin{
		Chart: "tailscale-operator", Repo: "https://pkgs.tailscale.com/helmcharts", Version: tailscaleOperatorVersion,
		Release: "tailscale-operator", Namespace: "tailscale", ValuesFile: "upstream/tailscale-operator.values.yaml",
	}
)

// Pins lists every pinned chart.
func Pins() []Pin { return []Pin{CertManager, TailscaleOperator} }

// ImagePin is an upstream container image a recipe's manifests run, pinned
// to one release tag and that tag's multi-platform index digest: the tag
// says which release it is, the digest is what the kubelet pulls.
type ImagePin struct {
	// Repository is the image's repository, registry host included.
	Repository string
	// Tag and Digest are the release and its index digest (sha256:...).
	Tag, Digest string
}

// Ref is the image reference the manifests name: repository, tag and digest.
func (p ImagePin) Ref() string { return p.Repository + ":" + p.Tag + "@" + p.Digest }

// Cloudflared is the cloudflare-tunnel recipe's connector image.
var Cloudflared = imagePin("docker.io/cloudflare/cloudflared", cloudflaredImage)

// ImagePins lists every pinned image.
func ImagePins() []ImagePin { return []ImagePin{Cloudflared} }

// imagePin splits a pin constant, <tag>@<digest>.
func imagePin(repo, pinned string) ImagePin {
	tag, digest, _ := strings.Cut(pinned, "@")
	return ImagePin{Repository: repo, Tag: tag, Digest: digest}
}

// tailscaleOAuthSecret is the Secret the Tailscale operator's chart mounts
// its OAuth client from when its values name none (the chart's fullname,
// "operator", plus "-oauth"), with keys client_id and client_secret. The
// operator creates it from Tailscale's admin console (design §5.1: the
// wizard only references it).
const tailscaleOAuthSecret = "operator-oauth"

// TailscaleOperatorTag is the ACL tag the operator's own device carries,
// which must own the gateway device's tag.
const TailscaleOperatorTag = "tag:k8s-operator"

// TailscaleProxyTag is the tag the operator gives every proxy device it
// creates by default. The tailnet policy names it as "another tagged
// device" the previews' device must refuse, so the previews need a tag
// distinct from it as well as from the operator's own.
const TailscaleProxyTag = "tag:k8s"

// upstreamValues renders one pinned chart's values under a header naming
// the pin and the install command.
func upstreamValues(p Pin, body map[string]any, comment ...string) ([]byte, error) {
	head := make([]string, 0, 4+len(comment))
	head = append(head,
		"Values for "+p.Chart+" "+p.Version+", the version maul's recipe is written and tested for.",
		"Install or upgrade it with:",
		"  helm upgrade --install "+p.Release+" "+p.Chart+" --repo "+p.Repo+" --version "+p.Version+" \\",
		"    -n "+p.Namespace+" --create-namespace -f "+p.ValuesFile)
	return manifest(body, append(head, comment...)...)
}

// certManagerValues are cert-manager's values: its CRDs installed by the
// chart and kept on uninstall (deleting them would delete every
// Certificate in the cluster).
func certManagerValues() ([]byte, error) {
	return upstreamValues(CertManager, map[string]any{
		"crds": map[string]any{"enabled": true, "keep": true},
	})
}

// tailscaleOperatorValues are the Tailscale operator's values: its OAuth
// client from the Secret the operator creates, its own tag, and nothing
// the recipe does not use (no IngressClass, no API server proxy).
func tailscaleOperatorValues() ([]byte, error) {
	return upstreamValues(TailscaleOperator, map[string]any{
		"oauth":          map[string]any{"clientId": "", "clientSecret": ""},
		"operatorConfig": map[string]any{"defaultTags": []string{TailscaleOperatorTag}},
		"ingressClass":   map[string]any{"enabled": false},
		"apiServerProxyConfig": map[string]any{
			"mode": "false", "allowImpersonation": "false",
		},
	}, "Its OAuth client comes from Secret "+tailscaleOAuthSecret+" in namespace "+TailscaleOperator.Namespace+
		" (keys client_id and client_secret),",
		"created beforehand: NEXT-STEPS.md says how.")
}
