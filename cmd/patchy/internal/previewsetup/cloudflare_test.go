// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// cloudflareAnswers are complete cloudflare-tunnel answers on k3s: names
// Universal SSL covers.
func cloudflareAnswers() Answers {
	a := cloudflareCases()["cloudflare-tunnel-k3s"]
	a.Normalize()
	return a
}

// TestUniversalSSLDepthProperties (seeded, design §9: "first-level iff
// suffix == registrable domain"): every preview host <label>.<suffix> is
// covered exactly when the suffix is its own registrable domain; a
// registrable domain and its first-level names are always covered, nothing
// deeper is; and CheckCloudflareDepth passes exactly when both the
// previews and the sign-in host are covered, or always with ACM.
func TestUniversalSSLDepthProperties(t *testing.T) {
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(randUnder(r, randDomain(r)))
		args[1] = reflect.ValueOf(randLabel(r))
		args[2] = reflect.ValueOf(randUnder(r, randDomain(r)))
	}
	prop := func(suffix, label, signIn string) bool {
		site, err := Site(suffix)
		if err != nil {
			t.Logf("%s: no site: %v", suffix, err)
			return false
		}
		if got, want := UniversalSSLCovers(label+"."+suffix), suffix == site; got != want {
			t.Logf("%s.%s covered %v, suffix is its site %v", label, suffix, got, want)
			return false
		}
		if !UniversalSSLCovers(site) || !UniversalSSLCovers(label+"."+site) ||
			UniversalSSLCovers(label+"."+label+"."+site) {
			t.Logf("%s: the apex and first level must be covered, the second never", site)
			return false
		}
		want := UniversalSSLCovers(label+"."+suffix) && UniversalSSLCovers(signIn)
		if got := CheckCloudflareDepth(suffix, signIn, false) == nil; got != want {
			t.Logf("CheckCloudflareDepth(%s, %s) passed %v, want %v", suffix, signIn, got, want)
			return false
		}
		if CheckCloudflareDepth(suffix, signIn, true) != nil {
			t.Logf("CheckCloudflareDepth(%s, %s, acm) refused", suffix, signIn)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestCheckCloudflareDepth(t *testing.T) {
	for _, tc := range []struct {
		suffix, signIn string
		acm            bool
		want           string // "" passes
	}{
		{"acme-previews.dev", "preview-auth.acme.dev", false, ""},
		{"acme-previews.dev", "acme.dev", false, ""},
		{"acme-previews.co.uk", "auth.acme.com.au", false, ""},
		// The design's own example: a second-level wildcard, which free
		// Universal SSL leaves without a valid certificate.
		{"preview.acme-apps.dev", "preview-auth.acme.dev", false, "--host-suffix preview.acme-apps.dev is below"},
		{"acme-previews.dev", "preview-auth.patchy.acme.dev", false, "--sign-in-host preview-auth.patchy.acme.dev"},
		{"preview.acme-apps.dev", "preview-auth.patchy.acme.dev", true, ""},
	} {
		err := CheckCloudflareDepth(tc.suffix, tc.signIn, tc.acm)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("CheckCloudflareDepth(%s, %s, %v) = %v, want %q", tc.suffix, tc.signIn, tc.acm, err, tc.want)
		}
		if err != nil && !strings.Contains(err.Error(), "--cloudflare-acm") {
			t.Errorf("the refusal names no way out: %v", err)
		}
	}
}

// TestCloudflareOneDomainRefused (owner decision Q3): previews and sign-in
// on one registrable domain are refused on Cloudflare too, with the second
// domain as the way out; no flag lets it through.
func TestCloudflareOneDomainRefused(t *testing.T) {
	for _, acm := range []bool{false, true} {
		a := cloudflareAnswers()
		a.HostSuffix, a.SignInHost = "previews.acme.dev", "preview-auth.acme.dev"
		a.Dex.Issuer = "https://dex.patchy.example.org"
		a.Cloudflare.AdvancedCertificates = acm
		err := a.Validate()
		if err == nil || !strings.Contains(err.Error(), "share the registrable domain acme.dev") ||
			!strings.Contains(err.Error(), "a second, cheap domain") {
			t.Errorf("acm %v: one domain: %v", acm, err)
		}
	}
}

func TestValidateCloudflare(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Answers)
		want string
	}{
		"release namespace": {func(a *Answers) { a.Cloudflare.Namespace = a.Namespace },
			"is the release namespace"},
		"bad namespace": {func(a *Answers) { a.Cloudflare.Namespace = "Edge_Tunnel" }, "--cloudflared-namespace"},
		// The manifest relabels its namespace restricted and rolling back
		// deletes it, so a namespace something else lives in is refused.
		"dex namespace": {func(a *Answers) { a.Cloudflare.Namespace = a.Dex.Namespace }, "is Dex's namespace"},
		"kube-system": {func(a *Answers) { a.Cloudflare.Namespace = "kube-system" },
			"one of Kubernetes' own namespaces"},
		"kube-node-lease": {func(a *Answers) { a.Cloudflare.Namespace = "kube-node-lease" },
			"one of Kubernetes' own namespaces"},
		"default": {func(a *Answers) { a.Cloudflare.Namespace = "default" }, "one of Kubernetes' own namespaces"},
		"cert-manager": {func(a *Answers) { a.Cloudflare.Namespace = CertManager.Namespace },
			"cert-manager's namespace"},
		"tailscale": {func(a *Answers) { a.Cloudflare.Namespace = TailscaleOperator.Namespace },
			"the Tailscale operator's namespace"},
		// A load balancer's answers would be recorded and silently dropped.
		"inbound cidr": {func(a *Answers) { a.LoadBalancer.InboundCIDRs = []string{"203.0.113.0/24"} },
			"--inbound-cidr is for --edge gateway-tls"},
		"load balancer class": {func(a *Answers) { a.LoadBalancer.Class = "metallb" },
			"--load-balancer-class is for --edge gateway-tls"},
		"bad tunnel id": {func(a *Answers) { a.Cloudflare.TunnelID = "6FF42AE2-765D-4ADF-8112-31C55C1551EF" },
			"not a lowercase UUID"},
		"deep suffix": {func(a *Answers) { a.HostSuffix = "preview.acme-apps.dev" }, "--cloudflare-acm"},
		"acm elsewhere": {func(a *Answers) {
			a.Edge, a.Cloudflare = EdgeExisting, Cloudflare{AdvancedCertificates: true}
			a.Existing.IngressClass = "nginx"
		}, "--cloudflare-acm is for --edge cloudflare-tunnel"},
		"namespace elsewhere": {func(a *Answers) {
			a.Edge, a.Cloudflare = EdgeExisting, Cloudflare{Namespace: "edge"}
			a.Existing.IngressClass = "nginx"
		}, "--cloudflared-namespace is for --edge cloudflare-tunnel"},
	} {
		a := cloudflareAnswers()
		tc.edit(&a)
		if err := a.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want %q", name, err, tc.want)
		}
	}
	if err := cloudflareAnswers().Validate(); err != nil {
		t.Errorf("valid answers: %v", err)
	}
}

func TestMissingTunnelCredentials(t *testing.T) {
	a := cloudflareAnswers()
	a.Cloudflare.TunnelID = ""
	if m := a.Missing(); !slices.Equal(m, []string{"--tunnel-credentials"}) {
		t.Errorf("Missing = %v", m)
	}
}

// credentialsJSON is a credentials file as cloudflared writes one, with
// edit applied to its fields.
func credentialsJSON(edit func(map[string]string)) []byte {
	m := map[string]string{"AccountTag": "0123456789abcdef0123456789abcdef", "TunnelID": testTunnelID,
		"TunnelSecret": base64.StdEncoding.EncodeToString(tunnelSentinel)}
	if edit != nil {
		edit(m)
	}
	var parts []string
	for _, k := range []string{"AccountTag", "TunnelSecret", "TunnelID", "Endpoint", "Extra"} {
		if v, ok := m[k]; ok {
			parts = append(parts, fmt.Sprintf("%q:%q", k, v))
		}
	}
	return []byte("{" + strings.Join(parts, ",") + "}\n")
}

func TestParseTunnelCredentials(t *testing.T) {
	c, err := ParseTunnelCredentials(credentialsJSON(func(m map[string]string) { m["Endpoint"] = "" }))
	if err != nil || c.TunnelID() != testTunnelID || c.IsZero() {
		t.Fatalf("a credentials file: %v, %v", c.TunnelID(), err)
	}
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"cert.pem": {[]byte("-----BEGIN ARGO TUNNEL TOKEN-----\nabc\n-----END ARGO TUNNEL TOKEN-----\n"),
			"cert.pem"},
		"not json":    {[]byte("tunnel: x"), "not one JSON object"},
		"two objects": {append(credentialsJSON(nil), credentialsJSON(nil)...), "not one JSON object"},
		"unknown field": {credentialsJSON(func(m map[string]string) { m["Extra"] = "x" }),
			`unknown field "Extra"`},
		"bad tag": {credentialsJSON(func(m map[string]string) { m["AccountTag"] = "acme" }), "AccountTag"},
		"no tag":  {credentialsJSON(func(m map[string]string) { delete(m, "AccountTag") }), "AccountTag"},
		"upper id": {credentialsJSON(func(m map[string]string) { m["TunnelID"] = strings.ToUpper(testTunnelID) }),
			"TunnelID"},
		"short secret": {credentialsJSON(func(m map[string]string) {
			m["TunnelSecret"] = base64.StdEncoding.EncodeToString([]byte("short"))
		}), "TunnelSecret"},
		"not base64": {credentialsJSON(func(m map[string]string) { m["TunnelSecret"] = "%%%" }), "TunnelSecret"},
		"too big":    {[]byte(strings.Repeat(" ", maxCredential+1)), "more than"},
	} {
		_, err := ParseTunnelCredentials(tc.raw)
		if !errors.Is(err, ErrTunnelCredentials) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(tunnelSentinel)) {
			t.Errorf("%s: the refusal quotes the tunnel secret: %v", name, err)
		}
	}
}

func TestTunnelCredentialsRedacted(t *testing.T) {
	c := testTunnel(testTunnelID, tunnelSentinel)
	secret := base64.StdEncoding.EncodeToString(tunnelSentinel)
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s %q", c, c, c, c, c),
		fmt.Sprint(Secrets{Tunnel: c}), fmt.Sprintf("%+v", Secrets{Tunnel: c})} {
		if strings.Contains(s, secret) || strings.Contains(s, "AccountTag") {
			t.Errorf("formatting leaks the credentials: %s", s)
		}
	}
	if b, _ := c.MarshalJSON(); strings.Contains(string(b), secret) {
		t.Error("MarshalJSON leaks the credentials")
	}
}

// writeTunnelSecret writes a's tunnel Secret manifest under dir for c, as
// an earlier run would have.
func writeTunnelSecret(t *testing.T, dir string, a Answers, c TunnelCredentials) {
	t.Helper()
	a.Cloudflare.TunnelID = c.TunnelID()
	files, err := tunnelSecretFile(a, Secrets{Tunnel: c})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, filepath.FromSlash(CloudflaredSecretFile))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, files[0].Data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveTunnelCredentials(t *testing.T) {
	a := cloudflareAnswers()
	given := testTunnel(testTunnelID, tunnelSentinel)
	empty := t.TempDir()
	if got, err := ResolveTunnelCredentials(empty, a, given); err != nil || got.TunnelID() != testTunnelID {
		t.Errorf("given: %v, %v", got.TunnelID(), err)
	}
	if got, err := ResolveTunnelCredentials(empty, a, TunnelCredentials{}); err != nil || !got.IsZero() {
		t.Errorf("none: %v, %v", got.TunnelID(), err)
	}
	other := a
	other.Edge = EdgeExisting
	if _, err := ResolveTunnelCredentials(empty, other, given); !errors.Is(err, ErrTunnelCredentials) {
		t.Errorf("given to another edge: %v", err)
	}

	dir := t.TempDir()
	writeTunnelSecret(t, dir, a, given)
	got, err := ResolveTunnelCredentials(dir, a, TunnelCredentials{})
	if err != nil || got.TunnelID() != testTunnelID || string(got.raw) != string(given.raw) {
		t.Errorf("read back: %v, %v", got.TunnelID(), err)
	}
	moved := a
	moved.Cloudflare.TunnelID = "00000000-0000-4000-8000-000000000000"
	if _, err := ResolveTunnelCredentials(dir, moved, TunnelCredentials{}); !errors.Is(err, ErrTunnelMismatch) {
		t.Errorf("on disk for another tunnel: %v", err)
	}
	// A new file given wins over the one on disk, whatever its tunnel.
	newer := testTunnel(moved.Cloudflare.TunnelID, tunnelSentinel)
	if got, err := ResolveTunnelCredentials(dir, a, newer); err != nil || got.TunnelID() != newer.TunnelID() {
		t.Errorf("a new file: %v, %v", got.TunnelID(), err)
	}
}

// TestPlanRefusesAnotherTunnel: credentials for one tunnel never render
// beside a config naming another.
func TestPlanRefusesAnotherTunnel(t *testing.T) {
	a := cloudflareAnswers()
	s := Secrets{DexClient: sentinel, Tunnel: testTunnel("00000000-0000-4000-8000-000000000000", tunnelSentinel)}
	if _, err := Plan(a, s, testVersion); !errors.Is(err, ErrTunnelMismatch) {
		t.Errorf("Plan = %v, want ErrTunnelMismatch", err)
	}
	if _, err := Plan(a, Secrets{DexClient: sentinel}, testVersion); err == nil ||
		!strings.Contains(err.Error(), "--tunnel-credentials") {
		t.Errorf("Plan without credentials = %v", err)
	}
}

// TestCloudflaredConfigRolls: the Pod template carries the config's
// digest, so applying a changed config (a new tunnel) rolls cloudflared,
// and the same answers keep the same digest.
func TestCloudflaredConfigRolls(t *testing.T) {
	digest := func(a Answers) string {
		t.Helper()
		m := cloudflaredObjects(t, byPath(plan(t, a))[CloudflaredFile])
		ann, _, _ := unstructured.NestedString(m["Deployment"].Object, "spec", "template", "metadata", "annotations",
			cloudflaredConfigAnnotation)
		if len(ann) != 64 {
			t.Fatalf("no config digest on the Pod template: %q", ann)
		}
		return ann
	}
	a := cloudflareAnswers()
	b := a
	b.Cloudflare.TunnelID = "00000000-0000-4000-8000-000000000000"
	if first, again := digest(a), digest(a); first != again || first == digest(b) {
		t.Error("the config digest does not follow the config")
	}
}

// TestCloudflaredHasNoKubernetesCredential: cloudflared runs with no
// ServiceAccount token, as the image's non-root user, read-only.
func TestCloudflaredHasNoKubernetesCredential(t *testing.T) {
	m := cloudflaredObjects(t, byPath(plan(t, cloudflareAnswers()))[CloudflaredFile])
	pod := m["Deployment"].Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false ||
		m["ServiceAccount"].Object["automountServiceAccountToken"] != false {
		t.Error("cloudflared mounts a ServiceAccount token")
	}
	s := fmt.Sprint(pod)
	for _, want := range []string{"runAsNonRoot:true", "readOnlyRootFilesystem:true", "allowPrivilegeEscalation:false",
		"drop:[ALL]", "image:" + Cloudflared.Ref()} {
		if !strings.Contains(s, want) {
			t.Errorf("cloudflared's Pod lacks %s", want)
		}
	}
	if ns := m["Namespace"].GetLabels()["pod-security.kubernetes.io/enforce"]; ns != "restricted" {
		t.Errorf("cloudflared's namespace enforces %q", ns)
	}
}

// TestCloudflareEmptiesIngressFrom: cloudflared reaches the gateway's 8082
// from its own Pod addresses, so an ingressFrom the operator narrowed to a
// load balancer's subnets is overridden in the values layered after theirs,
// and NEXT-STEPS.md says so.
func TestCloudflareEmptiesIngressFrom(t *testing.T) {
	files := byPath(plan(t, cloudflareAnswers()))
	var v map[string]any
	if err := yaml.Unmarshal([]byte(files[ValuesFile]), &v); err != nil {
		t.Fatal(err)
	}
	pa, _ := v["previewAuth"].(map[string]any)
	got, ok := pa["ingressFrom"].([]any)
	if !ok || len(got) != 0 {
		t.Errorf("previewAuth.ingressFrom = %#v, want an explicit []", pa["ingressFrom"])
	}
	if !strings.Contains(files[NextStepsFile], "`previewAuth.ingressFrom: []`") {
		t.Error("NEXT-STEPS.md does not explain the emptied ingressFrom")
	}
}

// TestCloudflaredEgressIsAnyPublicAddress: cloudflared's internet egress
// names no Cloudflare range, so NEXT-STEPS.md must not claim it reaches
// Cloudflare alone.
func TestCloudflaredEgressIsAnyPublicAddress(t *testing.T) {
	files := byPath(plan(t, cloudflareAnswers()))
	np := fmt.Sprint(cloudflaredObjects(t, files[CloudflaredFile])["NetworkPolicy"].Object)
	if !strings.Contains(np, "cidr:0.0.0.0/0") || !strings.Contains(np, "cidr:::/0") {
		t.Fatalf("cloudflared's egress is no longer every public address; revisit the wording: %s", np)
	}
	steps := files[NextStepsFile]
	if !strings.Contains(steps, "any public address on 443 and 7844") ||
		strings.Contains(steps, "the gateway and Cloudflare (443") {
		t.Error("NEXT-STEPS.md describes cloudflared's egress as Cloudflare's alone")
	}
}
