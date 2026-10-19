// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"encoding/base64"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden trees under testdata/golden")

// testVersion is the CLI version the golden trees are rendered as.
const testVersion = "v0.12.40"

// sentinel is the Dex client secret the golden trees carry: no secret at
// all, and easy to find in a file it must never reach.
var sentinel = ClientSecret{value: "SENTINEL-not-a-secret-0123456789abcdef"}

// testAnswers are documentation values: previews on a registrable domain
// of their own, sign-in and Dex on another.
func testAnswers() Answers {
	a := Defaults()
	a.Edge = EdgeExisting
	a.HostSuffix = "acme-previews.dev"
	a.SignInHost = "preview-auth.patchy.acme.dev"
	a.Dex.Issuer = "https://dex.patchy.acme.dev"
	a.Viewers = Viewers{Teams: []string{"acme:qa"}, Users: []string{"octocat"}}
	a.Registry = "ghcr.io"
	a.ImagePathPrefix = "acme/patchy/previews"
	return a
}

// goldenCases are the trees pinned byte for byte: every way R4 reaches the
// gateway, across the platforms' guidance.
func goldenCases() map[string]Answers {
	cases := map[string]Answers{}
	traefik := testAnswers()
	traefik.Platform = PlatformK3s
	traefik.Existing.IngressClass = "traefik"
	cases["existing-traefik-k3s"] = traefik

	k3d := testAnswers()
	k3d.Platform = PlatformK3d
	k3d.Registry = "registry.localhost:5000"
	k3d.ImagePathPrefix = DefaultImagePathPrefix
	k3d.Existing.IngressClass = "traefik"
	cases["existing-traefik-k3d"] = k3d

	nginx := testAnswers()
	nginx.Platform = PlatformGKE
	nginx.Registry = "europe-west2-docker.pkg.dev"
	nginx.Existing.IngressClass = "nginx"
	nginx.Certificate.Issuer = "ClusterIssuer/letsencrypt-dns01"
	cases["existing-nginx-gke"] = nginx

	alb := testAnswers()
	alb.Platform = PlatformEKS
	alb.Registry = "123456789012.dkr.ecr.eu-west-2.amazonaws.com"
	alb.ImagePathPrefix = DefaultImagePathPrefix
	alb.APIServerCIDR = "172.20.0.1/32"
	cases["existing-alb-eks"] = alb

	// ingress-nginx on EKS terminates TLS in the cluster: a Certificate,
	// never the chart's edge class or ACM.
	eksNginx := testAnswers()
	eksNginx.Platform = PlatformEKS
	eksNginx.Registry = "123456789012.dkr.ecr.eu-west-2.amazonaws.com"
	eksNginx.Existing.IngressClass = "nginx"
	cases["existing-nginx-eks"] = eksNginx

	autoMode := testAnswers()
	autoMode.Platform = PlatformEKS
	autoMode.Registry = "123456789012.dkr.ecr.eu-west-2.amazonaws.com"
	autoMode.Existing.IngressClass = "alb"
	autoMode.NodeIsolation.EKSAutoMode = EKSAutoMode{NodePool: "patchy-previews", NodeClass: "patchy-previews"}
	cases["existing-alb-eks-auto-mode"] = autoMode

	route := testAnswers()
	route.Platform = PlatformAKS
	route.Registry = "acme.azurecr.io"
	route.Existing.GatewayParent = "gateway-system/edge/https-previews"
	cases["existing-gateway-api-aks"] = route

	other := testAnswers()
	other.Platform = PlatformOther
	other.Release = "previews"
	other.Namespace = "patchy-system"
	other.NodeIsolation.Mode = IsolationNone
	other.NoViewers = true
	other.Viewers = Viewers{}
	other.Existing.IngressClass = "haproxy"
	cases["existing-haproxy-other"] = other

	for name, a := range gatewayCertCases() {
		cases[name] = a
	}
	for name, a := range cloudflareCases() {
		cases[name] = a
	}
	for name, a := range sharedDomainCases() {
		cases[name] = a
	}
	return cases
}

// sharedDomainCases are runs with one registrable domain for previews,
// sign-in and Dex, acknowledged (--shared-domain acknowledged): the
// owner's desktop on a tailnet, and a Cloudflare tunnel, which then needs
// Advanced Certificate Manager.
func sharedDomainCases() map[string]Answers {
	cases := map[string]Answers{}
	shared := func(a Answers) Answers {
		a.HostSuffix, a.SignInHost, a.Dex.Issuer = "previews.acme.dev", "preview-auth.acme.dev", "https://dex.acme.dev"
		a.SharedDomainAck = SharedDomainAcknowledged
		return a
	}
	desk := shared(testAnswers())
	desk.Edge, desk.Platform = EdgeTailscale, PlatformK3d
	desk.Certificate = Certificate{DNS: "route53", DNSOptions: map[string]string{"region": "us-east-1",
		"accessKeyID": "AKIAIOSFODNN7EXAMPLE"}}
	desk.Registry = "123456789012.dkr.ecr.us-east-1.amazonaws.com"
	desk.ImagePathPrefix = DefaultImagePathPrefix
	desk.Tailscale.Addresses = []string{"100.101.12.58"}
	cases["tailscale-shared-domain-k3d"] = desk

	cf := shared(testAnswers())
	cf.Edge, cf.Platform = EdgeCloudflareTunnel, PlatformK3s
	cf.Cloudflare = Cloudflare{TunnelID: testTunnelID, AdvancedCertificates: true}
	cases["cloudflare-tunnel-shared-domain-k3s"] = cf

	// Only Dex shares the previews' registrable domain: the sign-in host is
	// on another, so the certificate's two names still sit in two zones.
	dex := testAnswers()
	dex.Edge, dex.Platform = EdgeGatewayTLS, PlatformK3s
	dex.HostSuffix, dex.SignInHost, dex.Dex.Issuer = "previews.acme.dev", "preview-auth.acme-auth.dev",
		"https://dex.acme.dev"
	dex.SharedDomainAck = SharedDomainAcknowledged
	dex.Certificate = Certificate{DNS: "digitalocean"}
	cases["gateway-tls-dex-shared-domain-k3s"] = dex
	return cases
}

// TestNextStepsDexOnlySharedZones: when only Dex shares the previews'
// registrable domain, NEXT-STEPS.md never says the previews and the
// sign-in host sit on one zone, on any edge that names the zones: the
// DNS-01 solver or Cloudflare must still answer for two.
func TestNextStepsDexOnlySharedZones(t *testing.T) {
	dexOnly := func(a Answers) Answers {
		a.HostSuffix, a.SignInHost, a.Dex.Issuer = "previews.acme.dev", "preview-auth.acme-auth.dev",
			"https://dex.acme.dev"
		a.SharedDomainAck = SharedDomainAcknowledged
		return a
	}
	gw := dexOnly(testAnswers())
	gw.Edge, gw.Platform, gw.Certificate = EdgeGatewayTLS, PlatformK3s, Certificate{DNS: "digitalocean"}
	existing := dexOnly(testAnswers())
	existing.Platform = PlatformGKE
	existing.Existing.IngressClass = "nginx"
	existing.Certificate.Issuer = "ClusterIssuer/letsencrypt-dns01"
	cf := dexOnly(testAnswers())
	cf.Edge, cf.Platform = EdgeCloudflareTunnel, PlatformK3s
	cf.Cloudflare = Cloudflare{TunnelID: testTunnelID, AdvancedCertificates: true}
	for name, a := range map[string]Answers{"gateway-tls": gw, "existing": existing, "cloudflare-tunnel": cf} {
		t.Run(name, func(t *testing.T) {
			a.Normalize()
			if err := a.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if a.SharedSite() != "acme.dev" {
				t.Fatalf("SharedSite = %q, want acme.dev", a.SharedSite())
			}
			next := byPath(planWith(t, a, withTunnel(a, Secrets{DexClient: sentinel})))[NextStepsFile]
			flat := strings.Join(strings.Fields(next), " ")
			for _, wrong := range []string{"Both names sit on `acme.dev`", "Both names sit on the zone",
				"accepted previews sharing with sign-in"} {
				if strings.Contains(flat, wrong) {
					t.Errorf("NEXT-STEPS.md says %q though the sign-in host is on acme-auth.dev", wrong)
				}
			}
			if !strings.Contains(flat, "Dex (`dex.acme.dev`)") {
				t.Error("NEXT-STEPS.md does not name Dex as what shares acme.dev")
			}
			if name != "cloudflare-tunnel" && !strings.Contains(flat, "different registrable domains") {
				t.Error("NEXT-STEPS.md does not say the certificate's names sit on two registrable domains")
			}
			if name == "cloudflare-tunnel" && !strings.Contains(flat, "`acme-auth.dev` for the sign-in host") {
				t.Error("NEXT-STEPS.md does not name the sign-in host's own zone")
			}
		})
	}
}

// testTunnelID is the tunnel the golden cloudflare-tunnel trees run.
const testTunnelID = "6ff42ae2-765d-4adf-8112-31c55c1551ef"

// cloudflareCases are the cloudflare-tunnel (R1) trees: previews on a
// whole zone and a first-level sign-in host, which Universal SSL covers;
// and deeper names on zones with Advanced Certificate Manager.
func cloudflareCases() map[string]Answers {
	cases := map[string]Answers{}
	free := testAnswers()
	free.Edge = EdgeCloudflareTunnel
	free.Platform = PlatformK3s
	free.SignInHost = "preview-auth.acme.dev"
	free.Cloudflare.TunnelID = testTunnelID
	cases["cloudflare-tunnel-k3s"] = free

	acm := testAnswers()
	acm.Edge = EdgeCloudflareTunnel
	acm.Platform = PlatformEKS
	acm.Registry = "123456789012.dkr.ecr.eu-west-2.amazonaws.com"
	acm.HostSuffix = "preview.acme-apps.dev"
	acm.Cloudflare = Cloudflare{TunnelID: testTunnelID, Namespace: "edge-tunnel", AdvancedCertificates: true}
	cases["cloudflare-tunnel-acm-eks"] = acm
	return cases
}

// tunnelSentinel is the tunnel secret the golden trees carry: no secret
// at all (32 bytes, so it parses), and easy to find in a file it must
// never reach.
var tunnelSentinel = []byte("TUNNEL-SENTINEL-not-a-secret-012")

// testTunnel is a credentials file for tunnel id holding secret, as
// `cloudflared tunnel create` writes one.
func testTunnel(id string, secret []byte) TunnelCredentials {
	raw := `{"AccountTag":"0123456789abcdef0123456789abcdef","TunnelSecret":"` +
		base64.StdEncoding.EncodeToString(secret) + `","TunnelID":"` + id + `","Endpoint":""}`
	c, err := ParseTunnelCredentials([]byte(raw))
	if err != nil {
		panic(err)
	}
	return c
}

// withTunnel gives s the sentinel tunnel credentials for a's tunnel when
// a is the cloudflare-tunnel recipe, which cannot render without them.
func withTunnel(a Answers, s Secrets) Secrets {
	if a.Edge == EdgeCloudflareTunnel && s.Tunnel.IsZero() {
		s.Tunnel = testTunnel(a.Cloudflare.TunnelID, tunnelSentinel)
	}
	return s
}

// gatewayCertCases are the gateway-tls (R2) and tailscale (R3) trees: one
// of every DNS-01 solver, with and without a credential the wizard writes,
// and with cert-manager and the Tailscale operator already running.
func gatewayCertCases() map[string]Answers {
	cases := map[string]Answers{}
	tls := func(p Platform, dns string, opts map[string]string) Answers {
		a := testAnswers()
		a.Edge = EdgeGatewayTLS
		a.Platform = p
		a.Certificate = Certificate{DNS: dns, DNSOptions: opts}
		return a
	}
	cf := tls(PlatformK3s, "cloudflare", nil)
	cf.Certificate.ACMEEmail = "ops@acme.dev"
	cf.LoadBalancer.InboundCIDRs = []string{"203.0.113.0/24", "198.51.100.7/32"}
	cases["gateway-tls-cloudflare-k3s"] = cf

	r53 := tls(PlatformEKS, "route53", map[string]string{"region": "eu-west-2"})
	r53.Registry = "123456789012.dkr.ecr.eu-west-2.amazonaws.com"
	r53.LoadBalancer.Class = "service.k8s.aws/nlb"
	cases["gateway-tls-route53-eks"] = r53

	gcp := tls(PlatformGKE, "google", map[string]string{"project": "acme-previews", "serviceAccountKey": "true"})
	gcp.Registry = "europe-west2-docker.pkg.dev"
	cases["gateway-tls-google-gke"] = gcp

	az := tls(PlatformAKS, "azure", map[string]string{"subscriptionID": "00000000-0000-4000-8000-000000000001",
		"resourceGroupName": "dns", "clientID": "00000000-0000-4000-8000-000000000002",
		"tenantID": "00000000-0000-4000-8000-000000000003"})
	az.Registry = "acme.azurecr.io"
	cases["gateway-tls-azure-aks"] = az

	do := tls(PlatformK3s, "digitalocean", nil)
	cases["gateway-tls-digitalocean-k3s"] = do

	rfc := tls(PlatformOther, "rfc2136", map[string]string{"nameserver": "ns1.acme.dev:53",
		"tsigKeyName": "patchy-previews"})
	rfc.NodeIsolation.Mode = IsolationRequired
	cases["gateway-tls-rfc2136-other"] = rfc

	// The desktop (owner decision Q7): k3d, Route53 finding each name's
	// zone, an IAM user's key, and the device's address once it appeared.
	desk := tls(PlatformK3d, "route53", map[string]string{"region": "us-east-1",
		"accessKeyID": "AKIAIOSFODNN7EXAMPLE"})
	desk.Edge = EdgeTailscale
	desk.Registry = "123456789012.dkr.ecr.us-east-1.amazonaws.com"
	desk.ImagePathPrefix = DefaultImagePathPrefix
	desk.Tailscale.Addresses = []string{"100.101.12.58"}
	cases["tailscale-route53-k3d"] = desk

	own := tls(PlatformK3s, DNSManual, nil)
	own.Edge = EdgeTailscale
	own.Certificate.Issuer = "ClusterIssuer/letsencrypt-dns01"
	own.Certificate.CertManagerInstalled = true
	own.Tailscale = Tailscale{Hostname: "acme-previews", Tag: "tag:previews", OperatorInstalled: true}
	cases["tailscale-manual-k3s"] = own
	return cases
}

// dnsSentinel is the DNS credential the golden trees carry where the
// wizard writes one: no credential at all.
var dnsSentinel = Credential{value: []byte("DNS-SENTINEL-not-a-credential-fedcba9876543210")}

// withDNSCredential are the cases whose operator gave a DNS credential.
var withDNSCredential = map[string]bool{
	"gateway-tls-cloudflare-k3s":  true,
	"gateway-tls-azure-aks":       true,
	"gateway-tls-rfc2136-other":   true,
	"tailscale-route53-k3d":       true,
	"tailscale-shared-domain-k3d": true,
}

// goldenSecrets are the secrets case name renders with.
func goldenSecrets(name string) Secrets {
	s := Secrets{DexClient: sentinel}
	if withDNSCredential[name] {
		s.DNS = dnsSentinel
	}
	return withTunnel(goldenCases()[name], s)
}

// plan renders a with the sentinel secret, failing the test on an error.
func plan(t *testing.T, a Answers) []File {
	t.Helper()
	return planWith(t, a, withTunnel(a, Secrets{DexClient: sentinel}))
}

// planWith renders a with s, failing the test on an error.
func planWith(t *testing.T, a Answers, s Secrets) []File {
	t.Helper()
	a.Normalize()
	files, err := Plan(a, s, testVersion)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return files
}

func byPath(files []File) map[string]string {
	m := make(map[string]string, len(files))
	for _, f := range files {
		m[f.Path] = string(f.Data)
	}
	return m
}

// readTree reads every file under dir, by slash path.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	want := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		want[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("read golden tree %s (run with -update to create): %v", dir, err)
	}
	return want
}

// TestGolden pins every generated file, byte for byte, per case.
// TestGoldenValuesRender renders the same trees' values with the chart.
func TestGolden(t *testing.T) {
	for name, a := range goldenCases() {
		t.Run(name, func(t *testing.T) {
			files := planWith(t, a, goldenSecrets(name))
			dir := filepath.Join("testdata", "golden", name)
			if *update {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				c, err := Compare(dir, files)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Write(c, Accept{}); err != nil {
					t.Fatal(err)
				}
			}
			got, want := byPath(files), readTree(t, dir)
			for p, data := range got {
				switch w, ok := want[p]; {
				case !ok:
					t.Errorf("%s is generated but not in %s (run with -update to accept)", p, dir)
				case w != data:
					t.Errorf("%s differs from %s (run with -update to accept):\n%s", p, dir,
						unifiedDiff(p, []byte(w), []byte(data)))
				}
			}
			for p := range want {
				if _, ok := got[p]; !ok {
					t.Errorf("%s is in %s but no longer generated (run with -update to accept)", p, dir)
				}
			}
		})
	}
}

// TestPlanFileSets: every tree has the common files and its edge's own.
func TestPlanFileSets(t *testing.T) {
	common := []string{AnswersFile, NextStepsFile, ValuesFile, DexClientSecretFile, RelayDexSecretFile}
	for name, want := range map[string][]string{
		"existing-traefik-k3s":       {CertificateFile},
		"existing-nginx-gke":         {CertificateFile},
		"existing-alb-eks":           nil,
		"existing-nginx-eks":         {CertificateFile},
		"existing-alb-eks-auto-mode": nil,
		"existing-gateway-api-aks":   {SignInRouteFile},
		"cloudflare-tunnel-k3s":      {CloudflaredFile, CloudflaredSecretFile},
		"cloudflare-tunnel-acm-eks":  {CloudflaredFile, CloudflaredSecretFile},
	} {
		files := plan(t, goldenCases()[name])
		var got []string
		for _, f := range files {
			got = append(got, f.Path)
		}
		want = append(slices.Clone(common), want...)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: files = %v, want %v", name, got, want)
		}
		if !slices.IsSortedFunc(files, func(x, y File) int { return strings.Compare(x.Path, y.Path) }) {
			t.Errorf("%s: files are not sorted by path", name)
		}
		for _, f := range files {
			if f.Secret != strings.HasPrefix(f.Path, "secrets/") {
				t.Errorf("%s: %s Secret = %v", name, f.Path, f.Secret)
			}
		}
	}
}
