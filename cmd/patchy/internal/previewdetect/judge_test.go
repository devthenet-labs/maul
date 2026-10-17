// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewdetect

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	corev1 "k8s.io/api/core/v1"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewcheck"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
)

// seed makes every property deterministic, so the gate never flakes.
const seed = 20261017

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

func TestPlatformOf(t *testing.T) {
	n := func(name, provider string, labels ...string) Node {
		l := map[string]string{}
		for _, k := range labels {
			l[k] = "x"
		}
		return Node{Name: name, ProviderID: provider, Labels: l}
	}
	for _, tc := range []struct {
		name  string
		nodes []Node
		want  previewsetup.Platform
	}{
		{"none", nil, ""},
		{"k3d", []Node{n("k3d-dev-server-0", "k3s://k3d-dev-server-0"), n("k3d-dev-agent-0", "k3s://k3d-dev-agent-0")},
			previewsetup.PlatformK3d},
		{"k3s", []Node{n("box", "k3s://box")}, previewsetup.PlatformK3s},
		{"eks", []Node{n("ip-10-0-0-1", "aws:///us-east-1a/i-0abc", "eks.amazonaws.com/nodegroup")},
			previewsetup.PlatformEKS},
		{"eks auto mode", []Node{n("i-0abc", "aws:///us-east-1a/i-0abc", labelEKSComputeType)},
			previewsetup.PlatformEKS},
		{"kubeadm on ec2", []Node{n("ip-10-0-0-1", "aws:///us-east-1a/i-0abc")}, ""},
		{"gke", []Node{n("gke-a", "gce://proj/us-central1-a/gke-a", labelGKENodePool)}, previewsetup.PlatformGKE},
		{"aks", []Node{n("aks-a", "azure:///subscriptions/x", labelAKSCluster)}, previewsetup.PlatformAKS},
		{"kind", []Node{n("kind-control-plane", "kind://docker/kind/kind-control-plane")}, ""},
		{"no provider id", []Node{n("bare", "")}, ""},
		{"mixed", []Node{n("box", "k3s://box"), n("k3d-a", "k3s://k3d-a")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := PlatformOf(tc.nodes)
			if got != tc.want {
				t.Errorf("PlatformOf = %q (%s), want %q", got, why, tc.want)
			}
			if why == "" {
				t.Error("no reason given")
			}
		})
	}
}

func TestEnforcer(t *testing.T) {
	for _, tc := range []struct {
		images   []string
		platform previewsetup.Platform
		want     string
		ebpf     bool
	}{
		{[]string{"quay.io/cilium/cilium:v1.16.0@sha256:abc"}, previewsetup.PlatformEKS, "Cilium", true},
		{[]string{"docker.io/calico/node:v3.28"}, previewsetup.PlatformAKS, "Calico", false},
		{[]string{"602401143452.dkr.ecr.us-east-1.amazonaws.com/amazon/aws-network-policy-agent:v1.1"},
			previewsetup.PlatformEKS, "the VPC CNI network policy agent", false},
		{[]string{"gke.gcr.io/anetd:v1"}, previewsetup.PlatformGKE, "GKE Dataplane V2 (Cilium)", true},
		{nil, previewsetup.PlatformK3d, "kube-router, built into k3s", true},
		{[]string{"rancher/mirrored-coredns-coredns:1.12.0"}, previewsetup.PlatformEKS, "", false},
		// A registry host or tag naming an enforcer is not that enforcer.
		{[]string{"cilium.example/coredns:calico"}, "", "", false},
	} {
		got, ebpf := enforcer(tc.images, tc.platform)
		if !strings.HasPrefix(got, tc.want) || (tc.want == "" && got != "") || ebpf != tc.ebpf {
			t.Errorf("enforcer(%v, %s) = %q, %v; want %q, %v", tc.images, tc.platform, got, ebpf, tc.want, tc.ebpf)
		}
	}
}

// findings are a k3d cluster's, with a release that already serves
// previews.
func findings() Findings {
	return Judge(Observation{
		Nodes:         []Node{{Name: "k3d-dev-server-0", ProviderID: "k3s://k3d-dev-server-0"}},
		APIServiceIPs: []string{"10.43.0.1"}, APIEndpoints: []string{"172.18.0.2", "fd00::1"},
		ClusterDNS: "10.43.0.10", CertManager: Present, Tailscale: Present,
		Release: previewcheck.Settings{
			Preview: previewcheck.ControllerConfig{From: "patchy/p", Data: map[string]string{
				KeyPreviewHostSuffix: "acme-previews.dev", KeyPreviewLegacy: "false"}},
			Auth: previewcheck.ControllerConfig{From: "patchy/a", Data: map[string]string{
				KeyAuthIssuer: "https://preview-auth.patchy.acme.dev", KeyAuthDexIssuer: "https://dex.acme.dev",
				KeyAuthGateway: "true"}},
		},
	})
}

func TestJudge(t *testing.T) {
	f := findings()
	if f.Platform != previewsetup.PlatformK3d || !f.EBPF {
		t.Errorf("Platform = %q, EBPF = %v", f.Platform, f.EBPF)
	}
	if !reflect.DeepEqual(f.APIEndpointCIDRs, []string{"172.18.0.2/32"}) || f.ClusterDNSCIDR != "10.43.0.10/32" {
		t.Errorf("addresses = %v, %q", f.APIEndpointCIDRs, f.ClusterDNSCIDR)
	}
	want := Release{Previews: true, SignIn: true, HostSuffix: "acme-previews.dev",
		SignInHost: "preview-auth.patchy.acme.dev", DexIssuer: "https://dex.acme.dev", Gateway: true}
	if f.Release != want {
		t.Errorf("Release = %+v, want %+v", f.Release, want)
	}
}

func TestApplyFillsWhatIsUnanswered(t *testing.T) {
	a := previewsetup.Defaults()
	a.Edge = previewsetup.EdgeTailscale
	filled := Apply(findings(), &a, func(string) bool { return false })
	if a.Platform != previewsetup.PlatformK3d || a.HostSuffix != "acme-previews.dev" ||
		a.SignInHost != "preview-auth.patchy.acme.dev" || a.Dex.Issuer != "https://dex.acme.dev" ||
		!a.Certificate.CertManagerInstalled || !a.Tailscale.OperatorInstalled {
		t.Errorf("Apply left %+v", a)
	}
	// k3d keeps auto: kube-router judges after the Service is translated.
	if a.APIServerCIDR != previewsetup.DefaultAPIServerCIDR {
		t.Errorf("APIServerCIDR = %q, want auto on k3d", a.APIServerCIDR)
	}
	if len(filled) != 6 {
		t.Errorf("filled = %q", filled)
	}
}

func TestApplyEKSKeepsTheServiceAddress(t *testing.T) {
	f := Judge(Observation{
		Nodes: []Node{{Name: "i-1", ProviderID: "aws:///us-east-1a/i-1",
			Labels: map[string]string{labelEKSComputeType: "auto"}}},
		APIServiceIPs: []string{"172.20.0.1"}, APIEndpoints: []string{"10.0.1.5", "10.0.2.6"},
		Images: []string{"amazon/aws-network-policy-agent:v1.1"},
	})
	a := previewsetup.Defaults()
	Apply(f, &a, func(string) bool { return false })
	if a.APIServerCIDR != "172.20.0.1/32" || !f.EKSAutoMode {
		t.Errorf("APIServerCIDR = %q, EKSAutoMode = %v", a.APIServerCIDR, f.EKSAutoMode)
	}
	// With Cilium the Service /32 alone matches nothing: auto stays.
	f.EBPF = true
	b := previewsetup.Defaults()
	Apply(f, &b, func(string) bool { return false })
	if b.APIServerCIDR != previewsetup.DefaultAPIServerCIDR {
		t.Errorf("APIServerCIDR = %q with an eBPF enforcer, want auto", b.APIServerCIDR)
	}
}

// answerFlags are the flags Apply may fill.
var answerFlags = []string{"platform", "host-suffix", "sign-in-host", "dex-issuer", "api-server-cidr",
	"cert-manager-installed", "tailscale-operator-installed"}

// genAnswers draws answers from small pools, each field empty or not.
func genAnswers(r *rand.Rand) previewsetup.Answers {
	pick := func(xs ...string) string { return xs[r.Intn(len(xs))] }
	a := previewsetup.Defaults()
	a.Platform = previewsetup.Platform(pick("", "k3s", "eks", "other"))
	a.Edge = previewsetup.Edge(pick("", "existing", "gateway-tls", "tailscale", "cloudflare-tunnel"))
	a.HostSuffix = pick("", "given-previews.dev")
	a.SignInHost = pick("", "auth.given.dev")
	a.Dex.Issuer = pick("", "https://dex.given.dev")
	a.APIServerCIDR = pick("auto", "10.96.0.1/32")
	a.Certificate.CertManagerInstalled = r.Intn(2) == 0
	a.Tailscale.OperatorInstalled = r.Intn(2) == 0
	return a
}

// genFindings draws findings, each fact found or not.
func genFindings(r *rand.Rand) Findings {
	pick := func(xs ...string) string { return xs[r.Intn(len(xs))] }
	presence := func() Presence { return Presence(r.Intn(3)) }
	return Findings{
		Platform:       previewsetup.Platform(pick("", "k3d", "eks", "gke")),
		APIServerCIDRs: [][]string{nil, {"172.20.0.1/32"}, {"172.20.0.1/32", "172.20.0.2/32"}}[r.Intn(3)],
		EBPF:           r.Intn(2) == 0,
		CertManager:    presence(), Tailscale: presence(),
		Release: Release{HostSuffix: pick("", "found-previews.dev"), SignInHost: pick("", "auth.found.dev"),
			DexIssuer: pick("", "https://dex.found.dev")},
	}
}

// TestApplyProperties (seeded): Apply never changes an answer whose flag
// was given, never replaces an answer already made (only empties, auto and
// a false "installed" are open), and is idempotent: a second Apply changes
// nothing and reports nothing.
func TestApplyProperties(t *testing.T) {
	prop := func(s int64) bool {
		r := rand.New(rand.NewSource(s))
		a, f := genAnswers(r), genFindings(r)
		given := map[string]bool{}
		for _, fl := range answerFlags {
			given[fl] = r.Intn(3) == 0
		}
		before := a
		Apply(f, &a, func(fl string) bool { return given[fl] })
		fields := map[string][2]any{
			"platform":                     {before.Platform, a.Platform},
			"host-suffix":                  {before.HostSuffix, a.HostSuffix},
			"sign-in-host":                 {before.SignInHost, a.SignInHost},
			"dex-issuer":                   {before.Dex.Issuer, a.Dex.Issuer},
			"api-server-cidr":              {before.APIServerCIDR, a.APIServerCIDR},
			"cert-manager-installed":       {before.Certificate.CertManagerInstalled, a.Certificate.CertManagerInstalled},
			"tailscale-operator-installed": {before.Tailscale.OperatorInstalled, a.Tailscale.OperatorInstalled},
		}
		open := map[string]bool{
			"platform": before.Platform == "", "host-suffix": before.HostSuffix == "",
			"sign-in-host": before.SignInHost == "", "dex-issuer": before.Dex.Issuer == "",
			"api-server-cidr":              before.APIServerCIDR == previewsetup.DefaultAPIServerCIDR,
			"cert-manager-installed":       !before.Certificate.CertManagerInstalled,
			"tailscale-operator-installed": !before.Tailscale.OperatorInstalled,
		}
		for fl, pair := range fields {
			if pair[0] != pair[1] && (given[fl] || !open[fl]) {
				t.Logf("seed %d: %s changed %v -> %v (given %v, open %v)", s, fl, pair[0], pair[1], given[fl], open[fl])
				return false
			}
		}
		// Nothing outside the fields Apply owns changes.
		x, y := before, a
		x.Platform, x.HostSuffix, x.SignInHost, x.Dex.Issuer, x.APIServerCIDR = "", "", "", "", ""
		y.Platform, y.HostSuffix, y.SignInHost, y.Dex.Issuer, y.APIServerCIDR = "", "", "", "", ""
		x.Certificate.CertManagerInstalled, y.Certificate.CertManagerInstalled = false, false
		x.Tailscale.OperatorInstalled, y.Tailscale.OperatorInstalled = false, false
		if !reflect.DeepEqual(x, y) {
			t.Logf("seed %d: Apply changed another answer", s)
			return false
		}
		again := a
		if more := Apply(f, &again, func(fl string) bool { return given[fl] }); len(more) != 0 ||
			!reflect.DeepEqual(again, a) {
			t.Logf("seed %d: not idempotent: %q", s, more)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(2000)); err != nil {
		t.Error(err)
	}
}

func TestReport(t *testing.T) {
	f := findings()
	f.Nodes = append(f.Nodes,
		Node{Name: "k3d-dev-agent-0", ProviderID: "k3s://k3d-dev-agent-0",
			Labels: map[string]string{previewsetup.DefaultNodeLabel: "true"}},
	)
	a := previewsetup.Defaults()
	a.Platform = previewsetup.PlatformK3s
	got := strings.Join(Report(f, a), "\n")
	for _, want := range []string{
		"platform: k3d",
		"warning: --platform is k3s, but the nodes say k3d",
		"API server: Service 10.43.0.1/32, endpoints 172.18.0.2/32; --api-server-cidr auto",
		"cluster DNS: 10.43.0.10/32",
		"cert-manager: installed; Tailscale operator: installed",
		"NetworkPolicy: enforced by kube-router",
		"preview nodes: none carries",
		"warning: k3d-dev-agent-0 carries " + previewsetup.DefaultNodeLabel + "=true without the taint",
		"previews served by the preview gateway already",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Report lacks %q:\n%s", want, got)
		}
	}
	f.Release.LegacyIngress = true
	if got := strings.Join(Report(f, a), "\n"); !strings.Contains(got, "legacy load balancer path") {
		t.Errorf("Report says nothing of the legacy path:\n%s", got)
	}
}

func TestReportSingleNode(t *testing.T) {
	f := Judge(Observation{Nodes: []Node{{Name: "box", ProviderID: "k3s://box"}}})
	a := previewsetup.Defaults()
	if got := strings.Join(Report(f, a), "\n"); !strings.Contains(got, "warning: one node") {
		t.Errorf("Report lacks the one-node warning:\n%s", got)
	}
	a.NodeIsolation.Mode = previewsetup.IsolationNone
	if got := strings.Join(Report(f, a), "\n"); strings.Contains(got, "one node") {
		t.Errorf("Report warns of one node with isolation off:\n%s", got)
	}
}

// TestReleaseKeysAreTheChartsKeys pins the ConfigMap keys detection reads to
// the chart's templates, which write them.
func TestReleaseKeysAreTheChartsKeys(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "charts", "patchy", "templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	for _, k := range []string{KeyPreviewHostSuffix, KeyPreviewLegacy, KeyAuthIssuer, KeyAuthDexIssuer,
		KeyAuthGateway} {
		if !strings.Contains(all.String(), `"`+k+`"`) {
			t.Errorf("no chart template writes %s", k)
		}
	}
}

func TestPreviewNodes(t *testing.T) {
	taint := corev1.Taint{Key: previewsetup.DefaultTaintKey, Value: "true", Effect: corev1.TaintEffectNoExecute}
	f := Findings{Nodes: []Node{
		{Name: "both", Labels: map[string]string{previewsetup.DefaultNodeLabel: "true"}, Taints: []corev1.Taint{taint}},
		{Name: "label", Labels: map[string]string{previewsetup.DefaultNodeLabel: "true"}},
		{Name: "taint", Taints: []corev1.Taint{taint}},
		{Name: "noschedule", Labels: map[string]string{previewsetup.DefaultNodeLabel: "true"},
			Taints: []corev1.Taint{{Key: previewsetup.DefaultTaintKey, Effect: corev1.TaintEffectNoSchedule}}},
		{Name: "neither"},
	}}
	both, label, tnt := f.PreviewNodes(previewsetup.Defaults())
	if !slices.Equal(both, []string{"both"}) || !slices.Equal(label, []string{"label", "noschedule"}) ||
		!slices.Equal(tnt, []string{"taint"}) {
		t.Errorf("PreviewNodes = %v, %v, %v", both, label, tnt)
	}
}
