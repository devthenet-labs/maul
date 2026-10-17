// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

// The upstream charts the recipes write values for, each pinned to one
// version (design §5.3): a pin is patchy's recommendation, so a stale one
// is a patchy-documented vulnerability. Each version is a constant of its
// own under a renovate comment, so a bump is one line; TestPinsAreTracked
// holds every pin to its comment, TestRenovateTracksPins holds
// .github/renovate.json5's regex manager to extracting each one (the org
// preset scans only Dockerfiles), and `mise run preview-pins` (CI's
// preview-pins job) checks that each version exists upstream and that the
// generated values and manifests are accepted by the chart and CRDs at
// that version.

// renovate: datasource=helm registryUrl=https://charts.jetstack.io depName=cert-manager
const certManagerVersion = "v1.21.2"

// renovate: datasource=helm registryUrl=https://pkgs.tailscale.com/helmcharts depName=tailscale-operator
const tailscaleOperatorVersion = "1.104.1"

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

// tailscaleOAuthSecret is the Secret the Tailscale operator's chart mounts
// its OAuth client from when its values name none (the chart's fullname,
// "operator", plus "-oauth"), with keys client_id and client_secret. The
// operator creates it from Tailscale's admin console (design §5.1: the
// wizard only references it).
const tailscaleOAuthSecret = "operator-oauth"

// TailscaleOperatorTag is the ACL tag the operator's own device carries,
// which must own the gateway device's tag.
const TailscaleOperatorTag = "tag:k8s-operator"

// upstreamValues renders one pinned chart's values under a header naming
// the pin and the install command.
func upstreamValues(p Pin, body map[string]any, comment ...string) ([]byte, error) {
	head := make([]string, 0, 4+len(comment))
	head = append(head,
		"Values for "+p.Chart+" "+p.Version+", the version patchy's recipe is written and tested for.",
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
