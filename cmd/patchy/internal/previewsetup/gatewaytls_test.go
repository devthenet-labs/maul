// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gatewayAnswers are complete gateway-tls answers on k3s with Cloudflare.
func gatewayAnswers() Answers {
	a := testAnswers()
	a.Edge = EdgeGatewayTLS
	a.Platform = PlatformK3s
	a.Certificate = Certificate{DNS: "cloudflare"}
	return a
}

func TestValidateGatewayCert(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Answers)
		want string
	}{
		"unknown dns":   {func(a *Answers) { a.Certificate.DNS = "godaddy" }, "--dns \"godaddy\" is none of"},
		"unknown key":   {func(a *Answers) { a.Certificate.DNSOptions = map[string]string{"zone": "x"} }, "takes none"},
		"region needed": {func(a *Answers) { a.Certificate.DNS = "route53" }, "needs --dns-option region="},
		"bad region": {func(a *Answers) {
			a.Certificate = Certificate{DNS: "route53", DNSOptions: map[string]string{"region": "Mars"}}
		}, "region=\"Mars\" is not an AWS region"},
		"temporary key": {func(a *Answers) {
			a.Certificate = Certificate{DNS: "route53", DNSOptions: map[string]string{"region": "us-east-1",
				"accessKeyID": "ASIAIOSFODNN7EXAMPLE"}}
		}, "accessKeyID="},
		"azure half a principal": {func(a *Answers) {
			a.Certificate = Certificate{DNS: "azure", DNSOptions: map[string]string{
				"subscriptionID": "00000000-0000-4000-8000-000000000001", "resourceGroupName": "dns",
				"clientID": "00000000-0000-4000-8000-000000000002"}}
		}, "clientID and tenantID together"},
		"rfc2136 bad port": {func(a *Answers) {
			a.Certificate = Certificate{DNS: "rfc2136", DNSOptions: map[string]string{
				"nameserver": "ns1.acme.dev:99999", "tsigKeyName": "k"}}
		}, "nameserver="},
		"rfc2136 weak algorithm": {func(a *Answers) {
			a.Certificate = Certificate{DNS: "rfc2136", DNSOptions: map[string]string{
				"nameserver": "192.0.2.53", "tsigKeyName": "k", "tsigAlgorithm": "HMACMD5"}}
		}, "tsigAlgorithm="},
		"issuer with a provider": {func(a *Answers) { a.Certificate.Issuer = "ClusterIssuer/le" },
			"use --dns manual"},
		"manual options": {func(a *Answers) {
			a.Certificate = Certificate{DNS: DNSManual, DNSOptions: map[string]string{"region": "us-east-1"}}
		}, "--dns-option with --dns manual"},
		"manual email": {func(a *Answers) { a.Certificate = Certificate{DNS: DNSManual, ACMEEmail: "a@b.dev"} },
			"--acme-email with --dns manual"},
		"bad email":     {func(a *Answers) { a.Certificate.ACMEEmail = "Ops <ops@acme.dev>" }, "--acme-email"},
		"bad lb class":  {func(a *Answers) { a.LoadBalancer.Class = "not a class" }, "--load-balancer-class"},
		"host bits set": {func(a *Answers) { a.LoadBalancer.InboundCIDRs = []string{"10.0.0.1/8"} }, "--inbound-cidr"},
		"not a cidr":    {func(a *Answers) { a.LoadBalancer.InboundCIDRs = []string{"office"} }, "--inbound-cidr"},
		"public address on the tailnet": {func(a *Answers) {
			a.Edge = EdgeTailscale
			a.Tailscale.Addresses = []string{"203.0.113.9"}
		}, "--tailscale-address"},
		"operator's tag": {func(a *Answers) { a.Edge = EdgeTailscale; a.Tailscale.Tag = TailscaleOperatorTag },
			"the operator's own"},
		"bad tag":      {func(a *Answers) { a.Edge = EdgeTailscale; a.Tailscale.Tag = "previews" }, "--tailscale-tag"},
		"bad hostname": {func(a *Answers) { a.Edge = EdgeTailscale; a.Tailscale.Hostname = "Previews" }, "hostname"},
	} {
		a := gatewayAnswers()
		tc.edit(&a)
		if err := a.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want %q", name, err, tc.want)
		}
	}
}

func TestValidateGatewayCertAccepts(t *testing.T) {
	for name, edit := range map[string]func(*Answers){
		"cloudflare":   func(*Answers) {},
		"manual":       func(a *Answers) { a.Certificate = Certificate{DNS: DNSManual, Issuer: "ClusterIssuer/le"} },
		"manual alone": func(a *Answers) { a.Certificate = Certificate{DNS: DNSManual} },
		"ipv6 tailnet": func(a *Answers) {
			a.Edge = EdgeTailscale
			a.Tailscale.Addresses = []string{"fd7a:115c:a1e0::1", "100.100.1.2"}
		},
		"rfc2136 address": func(a *Answers) {
			a.Certificate = Certificate{DNS: "rfc2136", DNSOptions: map[string]string{
				"nameserver": "[2001:db8::53]:5353", "tsigKeyName": "patchy.acme.dev."}}
		},
		// The existing edge never reads the DNS answers a re-run may still
		// carry from another edge.
		"existing ignores dns": func(a *Answers) {
			a.Edge = EdgeExisting
			a.Existing.IngressClass = "nginx"
			a.Certificate.DNS = "godaddy"
		},
	} {
		a := gatewayAnswers()
		edit(&a)
		a.Normalize()
		if err := a.Validate(); err != nil {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
}

func TestMissingDNS(t *testing.T) {
	for _, edge := range []Edge{EdgeGatewayTLS, EdgeTailscale} {
		a := gatewayAnswers()
		a.Edge = edge
		a.Certificate.DNS = ""
		if got := a.Missing(); !slices.Equal(got, []string{"--dns"}) {
			t.Errorf("%s: Missing() = %v, want [--dns]", edge, got)
		}
	}
}

func TestNormalizeGatewayAnswers(t *testing.T) {
	a := gatewayAnswers()
	a.LoadBalancer.InboundCIDRs = []string{"10.1.2.0/24", " 2001:DB8::/32", "10.1.2.0/24"}
	a.Tailscale.Addresses = []string{"FD7A:115C:A1E0::1", "100.64.0.1"}
	a.Certificate.DNS = " Route53 "
	a.Certificate.DNSOptions = map[string]string{}
	a.Normalize()
	if want := []string{"10.1.2.0/24", "2001:db8::/32"}; !slices.Equal(a.LoadBalancer.InboundCIDRs, want) {
		t.Errorf("inbound CIDRs = %v, want %v", a.LoadBalancer.InboundCIDRs, want)
	}
	if want := []string{"100.64.0.1", "fd7a:115c:a1e0::1"}; !slices.Equal(a.Tailscale.Addresses, want) {
		t.Errorf("addresses = %v, want %v", a.Tailscale.Addresses, want)
	}
	if a.Certificate.DNS != "route53" || a.Certificate.DNSOptions != nil {
		t.Errorf("certificate = %+v", a.Certificate)
	}
}

func TestDNSCredentialKey(t *testing.T) {
	for _, tc := range []struct {
		edge Edge
		dns  string
		opts map[string]string
		want string
	}{
		{EdgeGatewayTLS, "cloudflare", nil, "api-token"},
		{EdgeTailscale, "route53", map[string]string{"region": "us-east-1"}, ""},
		{EdgeTailscale, "route53", map[string]string{"region": "us-east-1", "accessKeyID": "AKIAIOSFODNN7EXAMPLE"},
			"secret-access-key"},
		{EdgeGatewayTLS, "google", map[string]string{"project": "acme-dns"}, ""},
		{EdgeGatewayTLS, "google", map[string]string{"project": "acme-dns", "serviceAccountKey": "true"}, "key.json"},
		{EdgeGatewayTLS, "azure", nil, ""},
		{EdgeGatewayTLS, "azure", map[string]string{"clientID": "x"}, "client-secret"},
		{EdgeGatewayTLS, "digitalocean", nil, "access-token"},
		{EdgeGatewayTLS, "rfc2136", nil, "tsig-secret"},
		{EdgeGatewayTLS, DNSManual, nil, ""},
		{EdgeExisting, "cloudflare", nil, ""},
	} {
		a := gatewayAnswers()
		a.Edge = tc.edge
		a.Certificate = Certificate{DNS: tc.dns, DNSOptions: tc.opts}
		if got := a.DNSCredentialKey(); got != tc.want {
			t.Errorf("%s %s %v: key %q, want %q", tc.edge, tc.dns, tc.opts, got, tc.want)
		}
	}
}

func TestParseCredential(t *testing.T) {
	a := gatewayAnswers()
	c, err := ParseCredential(a, []byte("  tok3n-value\n"))
	if err != nil || string(c.bytes()) != "tok3n-value" {
		t.Errorf("a token file: %q, %v", c.bytes(), err)
	}
	for name, raw := range map[string]string{
		"empty":      " \n",
		"two lines":  "QZXfirst\nQZXsecond\n",
		"inner tab":  "QZXa\tQZXb",
		"oversized":  strings.Repeat("QZX", maxCredential),
		"a NUL byte": "QZXtok\x00QZXen",
	} {
		_, err := ParseCredential(a, []byte(raw))
		if !errors.Is(err, ErrCredential) {
			t.Errorf("%s: %v, want ErrCredential", name, err)
		} else if strings.Contains(err.Error(), "QZX") {
			t.Errorf("%s: the refusal quotes the credential: %v", name, err)
		}
	}
	a.Certificate = Certificate{DNS: "google", DNSOptions: map[string]string{"project": "acme-dns",
		"serviceAccountKey": "true"}}
	key := "{\n  \"type\": \"service_account\"\n}\n"
	if c, err := ParseCredential(a, []byte(key)); err != nil || string(c.bytes()) != key {
		t.Errorf("a JSON key is kept as it is: %q, %v", c.bytes(), err)
	}
	if _, err := ParseCredential(a, []byte("not json")); !errors.Is(err, ErrCredential) {
		t.Errorf("a key that is no JSON: %v", err)
	}
}

func TestCredentialRedacted(t *testing.T) {
	c := Credential{value: []byte("hunter2-dns-token")}
	for _, out := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s %q", c, c, c, c, c),
		fmt.Sprint(Secrets{DNS: c}), fmt.Errorf("wrapped: %v", c).Error()} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("formatting leaks the credential: %s", out)
		}
	}
}

func TestResolveDNSCredential(t *testing.T) {
	dir := t.TempDir()
	a := gatewayAnswers()
	given := Credential{value: []byte("first-token")}

	if c, err := ResolveDNSCredential(dir, a, Credential{}); err != nil || !c.IsZero() {
		t.Fatalf("nothing given, nothing on disk: %v, %v", c, err)
	}
	if c, err := ResolveDNSCredential(dir, a, given); err != nil || string(c.bytes()) != "first-token" {
		t.Fatalf("given: %v", err)
	}
	files, err := Plan(a, Secrets{DexClient: sentinel, DNS: given}, testVersion)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compare(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(c, Accept{}); err != nil {
		t.Fatal(err)
	}
	// A re-run without a credential reads the written one back.
	if c, err := ResolveDNSCredential(dir, a, Credential{}); err != nil || string(c.bytes()) != "first-token" {
		t.Errorf("read back: %q, %v", c.bytes(), err)
	}
	// One given replaces it.
	if c, err := ResolveDNSCredential(dir, a, Credential{value: []byte("second")}); err != nil ||
		string(c.bytes()) != "second" {
		t.Errorf("a new one given: %q, %v", c.bytes(), err)
	}
	// Another provider's key on disk is not this solver's credential.
	b := a
	b.Certificate = Certificate{DNS: "digitalocean"}
	if _, err := ResolveDNSCredential(dir, b, Credential{}); err == nil ||
		!strings.Contains(err.Error(), "--dns-credentials-file") {
		t.Errorf("another provider's Secret read back: %v", err)
	}
	// A solver that takes none refuses one given, and ignores the file.
	b.Certificate = Certificate{DNS: "route53", DNSOptions: map[string]string{"region": "us-east-1"}}
	if _, err := ResolveDNSCredential(dir, b, given); !errors.Is(err, ErrCredential) {
		t.Errorf("a credential for an ambient solver: %v", err)
	}
	if c, err := ResolveDNSCredential(dir, b, Credential{}); err != nil || !c.IsZero() {
		t.Errorf("an ambient solver read a credential back: %v", err)
	}
	// Never read through a symbolic link.
	link := t.TempDir()
	if err := os.MkdirAll(filepath.Join(link, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, DNSSecretFile), filepath.Join(link, DNSSecretFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDNSCredential(link, a, Credential{}); err == nil {
		t.Error("read a credential through a symbolic link")
	}
}

// TestGatewayCertFiles: each recipe writes the upstream values only for
// what does not run yet, and the Issuer only when it answers the
// challenge itself.
func TestGatewayCertFiles(t *testing.T) {
	common := []string{AnswersFile, NextStepsFile, ValuesFile, DexClientSecretFile, RelayDexSecretFile, CertificateFile}
	for name, want := range map[string][]string{
		"gateway-tls-cloudflare-k3s":   {IssuerFile, CertManager.ValuesFile, DNSSecretFile},
		"gateway-tls-route53-eks":      {IssuerFile, CertManager.ValuesFile},
		"gateway-tls-google-gke":       {IssuerFile, CertManager.ValuesFile},
		"gateway-tls-digitalocean-k3s": {IssuerFile, CertManager.ValuesFile},
		"tailscale-route53-k3d": {IssuerFile, CertManager.ValuesFile, TailscaleOperator.ValuesFile,
			DNSSecretFile},
		"tailscale-manual-k3s": nil,
	} {
		files := planWith(t, goldenCases()[name], goldenSecrets(name))
		var got []string
		for _, f := range files {
			got = append(got, f.Path)
		}
		want = append(slices.Clone(common), want...)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: files = %v, want %v", name, got, want)
		}
	}
}

// TestNextStepsCreatesAnUnwrittenCredential: a solver that reads a
// credential the operator did not give gets the command to create it, from
// a file; one that reads none says so.
func TestNextStepsCreatesAnUnwrittenCredential(t *testing.T) {
	next := byPath(planWith(t, goldenCases()["gateway-tls-digitalocean-k3s"], Secrets{DexClient: sentinel}))
	if !strings.Contains(next[NextStepsFile], "create secret generic "+DNSSecret+" --from-file=access-token=") {
		t.Errorf("NEXT-STEPS.md does not say how to create the credential:\n%s", next[NextStepsFile])
	}
	next = byPath(planWith(t, goldenCases()["gateway-tls-route53-eks"], Secrets{DexClient: sentinel}))
	if !strings.Contains(next[NextStepsFile], "It takes no credential") ||
		strings.Contains(next[NextStepsFile], "create secret generic "+DNSSecret) {
		t.Errorf("an ambient solver is told to create a credential:\n%s", next[NextStepsFile])
	}
}
