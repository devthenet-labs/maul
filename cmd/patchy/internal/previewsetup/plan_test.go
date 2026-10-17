// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"encoding/base64"
	"errors"
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"

	"sigs.k8s.io/yaml"
)

// randAnswers are valid, complete answers for the existing edge: hosts on
// two registrable domains, viewers, a registry, and one of every way R4
// reaches the gateway, on any platform.
func randAnswers(r *rand.Rand) Answers {
	a := Defaults()
	a.Edge = EdgeExisting
	ps := Platforms()
	a.Platform = Platform(ps[r.Intn(len(ps))])
	a.HostSuffix = randUnder(r, randDomain(r))
	site, _ := Site(a.HostSuffix)
	for {
		a.SignInHost = randUnder(r, randDomain(r))
		if s, _ := Site(a.SignInHost); s != site {
			break
		}
	}
	a.Dex.Issuer = "https://dex." + a.SignInHost
	for range 1 + r.Intn(3) {
		a.Viewers.Teams = append(a.Viewers.Teams, randLabel(r)+":"+randLabel(r))
	}
	if r.Intn(2) == 0 {
		a.Viewers.Users = append(a.Viewers.Users, "u"+strings.ReplaceAll(randLabel(r), "-", ""))
	}
	a.Registry = randDomain(r)
	if r.Intn(4) == 0 {
		a.Registry += ":5000"
	}
	if a.Platform == PlatformOther {
		a.NodeIsolation.Mode = []string{IsolationRequired, IsolationNone}[r.Intn(2)]
	}
	if a.Platform == PlatformEKS && r.Intn(2) == 0 {
		a.NodeIsolation.EKSAutoMode = EKSAutoMode{NodePool: randLabel(r), NodeClass: randLabel(r)}
	}
	switch r.Intn(5) {
	case 0:
		a.Existing.GatewayParent = randLabel(r) + "/" + randLabel(r)
	case 1:
		a.Existing.IngressClass = []string{"nginx", "traefik", "alb", "haproxy"}[r.Intn(4)]
	case 2:
		if a.Platform != PlatformEKS {
			a.Existing.IngressClass = "nginx"
		}
	case 3:
		randCloudflare(r, &a)
	default:
		randGatewayCert(r, &a)
	}
	a.Normalize()
	return a
}

// randCloudflare makes a the cloudflare-tunnel recipe: names Universal SSL
// covers (the suffix a whole zone, the sign-in host at most one label
// below its own), or any depth on zones with Advanced Certificate Manager.
func randCloudflare(r *rand.Rand, a *Answers) {
	a.Edge = EdgeCloudflareTunnel
	a.Existing = ExistingEdge{}
	a.Cloudflare = Cloudflare{TunnelID: randTunnelID(r), AdvancedCertificates: r.Intn(3) == 0}
	if r.Intn(2) == 0 {
		a.Cloudflare.Namespace = "t" + randLabel(r)
	}
	if !a.Cloudflare.AdvancedCertificates {
		a.HostSuffix, _ = Site(a.HostSuffix)
		site, _ := Site(a.SignInHost)
		a.SignInHost = site
		if r.Intn(2) == 0 {
			a.SignInHost = randLabel(r) + "." + site
		}
		a.Dex.Issuer = "https://dex." + a.SignInHost
	}
}

// randTunnelID is a lowercase UUID.
func randTunnelID(r *rand.Rand) string {
	const hexDigits = "0123456789abcdef"
	b := []byte("xxxxxxxx-xxxx-4xxx-8xxx-xxxxxxxxxxxx")
	for i, c := range b {
		if c == 'x' {
			b[i] = hexDigits[r.Intn(16)]
		}
	}
	return string(b)
}

// randGatewayCert makes a the gateway-tls or tailscale recipe, with any
// DNS-01 solver and a valid set of its options (each optional one present
// or not), so every way the solver can take a credential is drawn.
func randGatewayCert(r *rand.Rand, a *Answers) {
	a.Edge = []Edge{EdgeGatewayTLS, EdgeTailscale}[r.Intn(2)]
	a.Certificate, a.LoadBalancer, a.Tailscale = Certificate{}, LoadBalancer{}, Tailscale{}
	a.Cloudflare = Cloudflare{}
	dns := DNSProviders()
	a.Certificate.DNS = dns[r.Intn(len(dns))]
	opts := randDNSOptions(r, a.Certificate.DNS)
	some := func() bool { return r.Intn(2) == 0 }
	if a.Certificate.DNS == DNSManual && some() {
		a.Certificate.Issuer = "ClusterIssuer/" + randLabel(r)
	}
	a.Certificate.DNSOptions = opts
	if a.Certificate.DNS != DNSManual && some() {
		a.Certificate.ACMEEmail = "ops@" + randDomain(r)
	}
	a.Certificate.CertManagerInstalled = some()
	switch a.Edge {
	case EdgeGatewayTLS:
		if some() {
			a.LoadBalancer.Class = "example.com/lb"
		}
		if some() {
			a.LoadBalancer.InboundCIDRs = []string{"10.0.0.0/8", "192.0.2.1/32"}
		}
	case EdgeTailscale:
		a.Tailscale.OperatorInstalled = some()
		if some() {
			a.Tailscale.Addresses = []string{"100.64.0.1"}
		}
		if some() {
			a.Tailscale.Hostname, a.Tailscale.Tag = randLabel(r), "tag:"+randLabel(r)
		}
	}
}

// randDNSOptions are a valid set of a solver's options, each optional one
// present or not.
func randDNSOptions(r *rand.Rand, dns string) map[string]string {
	opts := map[string]string{}
	some := func() bool { return r.Intn(2) == 0 }
	switch dns {
	case "route53":
		opts["region"] = []string{"us-east-1", "eu-west-2", "ap-southeast-1"}[r.Intn(3)]
		if some() {
			opts["accessKeyID"] = "AKIA" + strings.Repeat("Q", 16)
		}
	case "google":
		opts["project"] = "p" + strings.Repeat("x", 4) + strings.ReplaceAll(randLabel(r), "-", "")
		if some() {
			opts["serviceAccountKey"] = "true"
		}
	case "azure":
		opts["subscriptionID"] = "00000000-0000-4000-8000-000000000001"
		opts["resourceGroupName"] = randLabel(r)
		if some() {
			opts["clientID"] = "00000000-0000-4000-8000-000000000002"
			opts["tenantID"] = "00000000-0000-4000-8000-000000000003"
		}
		if some() {
			opts["environment"] = azureEnvironments[r.Intn(len(azureEnvironments))]
		}
	case "rfc2136":
		opts["nameserver"] = randDomain(r) + ":53"
		opts["tsigKeyName"] = randLabel(r)
		if some() {
			opts["tsigAlgorithm"] = tsigAlgorithms[r.Intn(len(tsigAlgorithms))]
		}
	}
	return opts
}

// randCredential is a DNS credential for a's solver: a JSON key or a
// token, of random bytes either way.
func randCredential(r *rand.Rand, a Answers) Credential {
	b := make([]byte, 24)
	r.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	if a.DNSCredentialIsJSON() {
		return Credential{value: []byte(`{"type":"service_account","private_key":"` + tok + `"}`)}
	}
	return Credential{value: []byte(tok)}
}

// randSecret is a client secret of random bytes, the size a draw gives.
func randSecret(r *rand.Rand) ClientSecret {
	b := make([]byte, clientSecretBytes)
	r.Read(b)
	return ClientSecret{value: base64.RawURLEncoding.EncodeToString(b)}
}

// TestSecretNeverLeaves (seeded): whatever the answers, the Dex client
// secret, raw or base64-encoded, is in no file but the two Secret
// manifests, and in both of those; a DNS credential given is in no file
// but its own Secret manifest, and in that one exactly when the solver
// reads it.
func TestSecretNeverLeaves(t *testing.T) {
	cfg := quickConfig(500)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		a := randAnswers(r)
		args[0] = reflect.ValueOf(a)
		args[1] = reflect.ValueOf(randSecret(r).value)
		args[2] = reflect.ValueOf(randCredential(r, a).value)
		tunnel := make([]byte, 32)
		r.Read(tunnel)
		args[3] = reflect.ValueOf(tunnel)
	}
	prop := func(a Answers, secret string, dnsCred, tunnelSecret []byte) bool {
		s := Secrets{DexClient: ClientSecret{value: secret}}
		if a.DNSCredentialKey() != "" {
			s.DNS = Credential{value: dnsCred}
		}
		if a.Edge == EdgeCloudflareTunnel {
			s.Tunnel = testTunnel(a.Cloudflare.TunnelID, tunnelSecret)
		}
		files, err := Plan(a, s, testVersion)
		if err != nil {
			t.Logf("Plan(%+v): %v", a, err)
			return false
		}
		has := func(data, v []byte) bool {
			return bytes.Contains(data, v) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(v)))
		}
		for _, f := range files {
			dex := has(f.Data, []byte(secret))
			if dex != (f.Secret && f.Path != DNSSecretFile && f.Path != CloudflaredSecretFile) {
				t.Logf("%s: holds the Dex client secret %v", f.Path, dex)
				return false
			}
			dns := has(f.Data, dnsCred)
			if dns != (f.Path == DNSSecretFile) {
				t.Logf("%s: holds the DNS credential %v", f.Path, dns)
				return false
			}
			if a.Edge == EdgeCloudflareTunnel && !tunnelOnlyInItsSecret(t, f, s.Tunnel, tunnelSecret) {
				return false
			}
		}
		_, tunnelWritten := byPath(files)[CloudflaredSecretFile]
		if tunnelWritten != (a.Edge == EdgeCloudflareTunnel) {
			t.Logf("tunnel Secret written %v on --edge %s", tunnelWritten, a.Edge)
			return false
		}
		_, written := byPath(files)[DNSSecretFile]
		if written != (a.DNSCredentialKey() != "") {
			t.Logf("DNS Secret written %v, the solver reads one %q", written, a.DNSCredentialKey())
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestPlanIsByteStable (seeded): the same answers, secret and version
// render the same bytes, and answers.yaml reads back as the answers it
// was rendered from, so a re-run with no new flag changes nothing.
func TestPlanIsByteStable(t *testing.T) {
	cfg := quickConfig(300)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) { args[0] = reflect.ValueOf(randAnswers(r)) }
	prop := func(a Answers) bool {
		s := withTunnel(a, Secrets{DexClient: sentinel})
		first, err := Plan(a, s, testVersion)
		if err != nil {
			t.Logf("Plan: %v", err)
			return false
		}
		read, err := ParseAnswers([]byte(byPath(first)[AnswersFile]))
		if err != nil {
			t.Logf("ParseAnswers: %v", err)
			return false
		}
		read.Normalize()
		second, err := Plan(read, s, testVersion)
		if err != nil || len(first) != len(second) {
			t.Logf("re-plan: %v", err)
			return false
		}
		for i := range first {
			if first[i].Path != second[i].Path || !bytes.Equal(first[i].Data, second[i].Data) {
				t.Logf("%s differs on a re-run from answers.yaml:\n%s", first[i].Path,
					unifiedDiff(first[i].Path, first[i].Data, second[i].Data))
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestIssuerNamesItsCredential (seeded): the Issuer the recipe writes
// names the DNS Secret, under the one key the Secret holds, exactly when
// its solver reads a credential; with --dns manual no Issuer is written
// and the Certificate names the operator's.
func TestIssuerNamesItsCredential(t *testing.T) {
	cfg := quickConfig(400)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		a := randAnswers(r)
		randGatewayCert(r, &a)
		a.Normalize()
		args[0] = reflect.ValueOf(a)
		args[1] = reflect.ValueOf(randCredential(r, a).value)
	}
	prop := func(a Answers, cred []byte) bool {
		s := Secrets{DexClient: sentinel}
		key := a.DNSCredentialKey()
		if key != "" {
			s.DNS = Credential{value: cred}
		}
		files, err := Plan(a, s, testVersion)
		if err != nil {
			t.Logf("Plan(%+v): %v", a, err)
			return false
		}
		got := byPath(files)
		iss, written := got[IssuerFile]
		if written != (a.Certificate.DNS != DNSManual) {
			t.Logf("--dns %s: Issuer written %v", a.Certificate.DNS, written)
			return false
		}
		kind, name := a.Issuer()
		var cert struct {
			Spec struct {
				IssuerRef struct{ Kind, Name string } `json:"issuerRef"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(got[CertificateFile]), &cert); err != nil ||
			cert.Spec.IssuerRef.Kind != kind || cert.Spec.IssuerRef.Name != name {
			t.Logf("the Certificate names %+v, not %s/%s (%v)", cert.Spec.IssuerRef, kind, name, err)
			return false
		}
		if !written {
			return true
		}
		if !issuerKindHolds(t, a, iss, key, kind, name) {
			return false
		}
		refs := regexp.MustCompile(`key: (\S+)\n\s+name: `+DNSSecret+`\n`).FindAllStringSubmatch(iss, -1)
		if key == "" {
			if len(refs) > 0 || strings.Contains(iss, DNSSecret) {
				t.Logf("an ambient solver names the DNS Secret:\n%s", iss)
				return false
			}
			return true
		}
		for _, m := range refs {
			if m[1] != key {
				t.Logf("the Issuer names key %s, the Secret holds %s", m[1], key)
				return false
			}
		}
		var secret struct {
			Data map[string][]byte `json:"data"`
		}
		if err := yaml.Unmarshal([]byte(got[DNSSecretFile]), &secret); err != nil || len(refs) == 0 ||
			len(secret.Data) != 1 || !bytes.Equal(secret.Data[key], cred) {
			t.Logf("Issuer refs %v, Secret %v (%v)", refs, secret.Data, err)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestPlanRefuses(t *testing.T) {
	a := testAnswers()
	a.Platform = PlatformK3s
	a.Existing.IngressClass = "nginx"
	b := a
	b.Edge = EdgeCloudflareTunnel
	if _, err := Plan(b, Secrets{DexClient: sentinel}, testVersion); err == nil ||
		!strings.Contains(err.Error(), "--tunnel-credentials") {
		t.Errorf("--edge cloudflare-tunnel without a tunnel: %v", err)
	}
	for _, edge := range []Edge{EdgeGatewayTLS, EdgeTailscale} {
		b := a
		b.Edge = edge
		if _, err := Plan(b, Secrets{DexClient: sentinel}, testVersion); err == nil ||
			!strings.Contains(err.Error(), "--dns") {
			t.Errorf("--edge %s without --dns: %v", edge, err)
		}
	}
	b = a
	b.HostSuffix = ""
	if _, err := Plan(b, Secrets{DexClient: sentinel}, testVersion); err == nil ||
		!strings.Contains(err.Error(), "--host-suffix") {
		t.Errorf("an unanswered suffix: %v", err)
	}
	b = a
	b.SignInHost = "auth." + a.HostSuffix
	if _, err := Plan(b, Secrets{DexClient: sentinel}, testVersion); err == nil {
		t.Error("an invalid answer planned")
	}
	if _, err := Plan(a, Secrets{}, testVersion); err == nil {
		t.Error("planned without a client secret")
	}
}

func TestParseAnswersRefusesUnknownFields(t *testing.T) {
	if _, err := ParseAnswers([]byte("edge: existing\nhostSufix: typo.dev\n")); !errors.Is(err, ErrAnswers) {
		t.Errorf("ParseAnswers = %v, want ErrAnswers", err)
	}
}

// TestNextStepsEdgeTLS: NEXT-STEPS.md sends the operator to the chart's
// edge class and ACM only when both Ingresses take it. Regression: any EKS
// install was told so, though an ingress-nginx or alb class rendered the
// Ingresses with its own class.
func TestNextStepsEdgeTLS(t *testing.T) {
	for _, tc := range []struct {
		class     string
		edgeClass bool
		cert      bool
	}{
		{"", true, false},
		{"alb", false, false},
		{"nginx", false, true},
		{"traefik", false, true},
	} {
		a := testAnswers()
		a.Platform = PlatformEKS
		a.Existing.IngressClass = tc.class
		files := byPath(plan(t, a))
		next := files[NextStepsFile]
		if got := strings.Contains(next, "edgeIngressClass"); got != tc.edgeClass {
			t.Errorf("class %q: NEXT-STEPS.md names the chart's edge class %v, want %v", tc.class, got, tc.edgeClass)
		}
		if _, got := files[CertificateFile]; got != tc.cert {
			t.Errorf("class %q: Certificate generated %v, want %v", tc.class, got, tc.cert)
		}
		if got := strings.Contains(files[ValuesFile], "secretName: "+CertificateSecret); got != tc.cert {
			t.Errorf("class %q: Ingress TLS names the Certificate %v, want %v", tc.class, got, tc.cert)
		}
	}
}

// issuerKindHolds: an ambient identity is a ClusterIssuer (cert-manager
// lends it to no namespaced Issuer by default) that never claims to be
// namespaced; a stored credential stays in a namespaced Issuer beside its
// Secret. kind and name are a.Issuer(), what the Certificate names.
func issuerKindHolds(t *testing.T, a Answers, iss, key, kind, name string) bool {
	t.Helper()
	var meta struct {
		Kind     string `json:"kind"`
		Metadata struct{ Name, Namespace string }
	}
	if err := yaml.Unmarshal([]byte(iss), &meta); err != nil {
		t.Logf("the Issuer does not parse: %v", err)
		return false
	}
	wantKind, wantNS := "Issuer", a.Namespace
	if key == "" {
		wantKind, wantNS = "ClusterIssuer", ""
	}
	if meta.Kind != wantKind || meta.Metadata.Namespace != wantNS || meta.Metadata.Name != name ||
		kind != wantKind {
		t.Logf("--dns %s (credential key %q): %s %s/%s, want %s in %q", a.Certificate.DNS, key, meta.Kind,
			meta.Metadata.Namespace, meta.Metadata.Name, wantKind, wantNS)
		return false
	}
	if key == "" && strings.Contains(iss, "only this namespace") {
		t.Logf("a ClusterIssuer claims to be namespaced:\n%s", iss)
		return false
	}
	return true
}

// tunnelOnlyInItsSecret: the tunnel Secret file holds exactly the
// credentials file; any other file holds neither the tunnel secret (raw,
// or as the base64 the credentials file holds it in) nor the file.
func tunnelOnlyInItsSecret(t *testing.T, f File, c TunnelCredentials, secret []byte) bool {
	t.Helper()
	if f.Path == CloudflaredSecretFile {
		var sec struct {
			Data map[string][]byte `json:"data"`
		}
		if err := yaml.Unmarshal(f.Data, &sec); err != nil || len(sec.Data) != 1 ||
			!bytes.Equal(sec.Data[CloudflaredSecretKey], c.raw) {
			t.Logf("%s does not hold exactly the credentials file (%v)", f.Path, err)
			return false
		}
		return true
	}
	for _, v := range [][]byte{secret, []byte(base64.StdEncoding.EncodeToString(secret)), c.raw} {
		if bytes.Contains(f.Data, v) || bytes.Contains(f.Data, []byte(base64.StdEncoding.EncodeToString(v))) {
			t.Logf("%s: holds the tunnel secret", f.Path)
			return false
		}
	}
	return true
}
