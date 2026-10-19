// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"math/rand"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// seed makes every property deterministic: the gate never flakes, and a
// failure reproduces.
const seed = 20261017

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// randLabel is a lowercase DNS label starting with a letter.
func randLabel(r *rand.Rand) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789-"
	b := make([]byte, 1+r.Intn(10))
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	b[0] = 'a' + byte(r.Intn(26))
	b[len(b)-1] = 'a' + byte(r.Intn(26))
	return string(b)
}

// tlds are public suffixes of one and two labels, ICANN and private, so
// publicsuffix decides the registrable domain of each differently.
var tlds = []string{"com", "dev", "net", "org", "io", "co.uk", "com.au", "github.io", "test"}

// randDomain is <label>.<tld>: a registrable domain. A draw that is a
// public suffix itself (hu.net, say: the list holds private suffixes
// under com and net) is drawn again.
func randDomain(r *rand.Rand) string {
	for {
		d := randLabel(r) + "." + tlds[r.Intn(len(tlds))]
		if site, err := Site(d); err == nil && site == d {
			return d
		}
	}
}

// randUnder is zero to two labels in front of domain.
func randUnder(r *rand.Rand, domain string) string {
	for range r.Intn(3) {
		domain = randLabel(r) + "." + domain
	}
	return domain
}

// randHostish is a string over the host alphabet and its near misses.
func randHostish(r *rand.Rand) string {
	const chars = "abz09-.A_ *"
	b := make([]byte, r.Intn(24))
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}

// schemaHostSuffix is preview.hostSuffix's pattern in the chart's schema.
var schemaHostSuffix = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)

// TestValidHostAgreesWithTheGateway (seeded): the wizard accepts a host
// exactly when the gateway's own grammar does (previewauth.NewHosts), and
// every host it accepts the chart's schema accepts too, so a value the
// wizard writes always renders and always serves.
func TestValidHostAgreesWithTheGateway(t *testing.T) {
	const other = "sign-in.other-site.test"
	cfg := quickConfig(20000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		h := randHostish(r)
		if r.Intn(2) == 0 {
			h = randUnder(r, randDomain(r))
		}
		args[0] = reflect.ValueOf(h)
	}
	prop := func(h string) bool {
		if Under(other, h) {
			return true
		}
		_, err := previewauth.NewHosts(h, other)
		if ValidHost(h) != (err == nil) {
			t.Logf("%q: ValidHost %v, NewHosts %v", h, ValidHost(h), err)
			return false
		}
		if ValidHost(h) && !schemaHostSuffix.MatchString(h) {
			t.Logf("%q: valid but the chart's schema refuses it", h)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestCheckHostsProperties (seeded): a sign-in host under the suffix (or
// equal to it) is always refused, acknowledged or not; for any two valid
// hosts outside each other, CheckHosts passes exactly when their
// registrable domains differ, and acknowledged (--shared-domain) exactly
// when the suffix has one; SharesSite is exactly that sharing.
func TestCheckHostsProperties(t *testing.T) {
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		suffix := randUnder(r, randDomain(r))
		args[0] = reflect.ValueOf(suffix)
		args[1] = reflect.ValueOf(randLabel(r) + "." + suffix)
		var signIn string
		switch r.Intn(3) {
		case 0: // on the suffix's own registrable domain, outside it
			site, _ := Site(suffix)
			signIn = randUnder(r, site)
		default:
			signIn = randUnder(r, randDomain(r))
		}
		args[2] = reflect.ValueOf(signIn)
	}
	prop := func(suffix, under, signIn string) bool {
		if CheckHosts(suffix, under, false) == nil || CheckHosts(suffix, suffix, false) == nil ||
			CheckHosts(suffix, under, true) == nil || CheckHosts(suffix, suffix, true) == nil {
			t.Logf("%s: a sign-in host under or at it passed", suffix)
			return false
		}
		if Under(signIn, suffix) {
			return CheckHosts(suffix, signIn, false) != nil && CheckHosts(suffix, signIn, true) != nil
		}
		a, errA := Site(suffix)
		b, errB := Site(signIn)
		want := errA == nil && (errB != nil || a != b)
		if got := CheckHosts(suffix, signIn, false) == nil; got != want {
			t.Logf("CheckHosts(%s, %s) passed=%v, want %v (sites %s, %s)", suffix, signIn, got, want, a, b)
			return false
		}
		// Acknowledged, only the suffix's own registrable domain matters.
		if got := CheckHosts(suffix, signIn, true) == nil; got != (errA == nil) {
			t.Logf("CheckHosts(%s, %s, shared) passed=%v, want %v", suffix, signIn, got, errA == nil)
			return false
		}
		if SharesSite(suffix, signIn) != (errA == nil && errB == nil && a == b) {
			t.Logf("SharesSite(%s, %s) = %v (sites %s, %s)", suffix, signIn, SharesSite(suffix, signIn), a, b)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestCheckIssuerProperties (seeded): a Dex issuer on the suffix's
// registrable domain is refused unless acknowledged, and under the suffix
// always; one on another domain passes.
func TestCheckIssuerProperties(t *testing.T) {
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		suffix := randUnder(r, randDomain(r))
		other := randDomain(r)
		for s, _ := Site(suffix); other == s; {
			other = randDomain(r)
		}
		args[0] = reflect.ValueOf(suffix)
		args[1] = reflect.ValueOf(randUnder(r, other))
		args[2] = reflect.ValueOf(randLabel(r))
	}
	prop := func(suffix, other, label string) bool {
		site, _ := Site(suffix)
		if CheckIssuer("https://"+label+"."+site, suffix, false) == nil {
			t.Logf("issuer on %s passed for suffix %s", site, suffix)
			return false
		}
		if !Under(label+"."+site, suffix) && CheckIssuer("https://"+label+"."+site, suffix, true) != nil {
			t.Logf("issuer on %s refused for suffix %s, acknowledged", site, suffix)
			return false
		}
		if CheckIssuer("https://"+label+"."+suffix+"/dex", suffix, false) == nil ||
			CheckIssuer("https://"+label+"."+suffix+"/dex", suffix, true) == nil {
			return false
		}
		if s, _ := Site(other); s == site {
			return true
		}
		if err := CheckIssuer("https://"+other+"/dex", suffix, false); err != nil {
			t.Logf("issuer on %s refused for suffix %s: %v", other, suffix, err)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestCheckHosts(t *testing.T) {
	for _, tc := range []struct {
		suffix, signIn, want string
	}{
		{"acme-previews.dev", "preview-auth.patchy.acme.dev", ""},
		{"previews.acme.co.uk", "sign-in.acme-auth.co.uk", ""},
		// The owner's one-domain install is refused (Q3).
		{"preview.patchy.devthe.net", "preview-auth.patchy.devthe.net", "share the registrable domain devthe.net"},
		{"acme-previews.dev", "auth.acme-previews.dev", "sits under"},
		{"acme-previews.dev", "acme-previews.dev", "sits under"},
		{"co.uk", "auth.acme.dev", "no registrable domain"},
		// A private public suffix under a generic TLD: the shrunk draw
		// that once slipped out of randDomain as a "registrable domain".
		{"hu.net", "auth.acme.dev", "no registrable domain"},
		{"Acme.dev", "auth.acme.dev", "--host-suffix"},
		{"acme.dev", "localhost", "--sign-in-host"},
		{"*.acme.dev", "auth.other.dev", "--host-suffix"},
	} {
		err := CheckHosts(tc.suffix, tc.signIn, false)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("CheckHosts(%s, %s) = %v", tc.suffix, tc.signIn, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("CheckHosts(%s, %s) = %v, want %q", tc.suffix, tc.signIn, err, tc.want)
		}
	}
}

func TestCheckIssuer(t *testing.T) {
	for _, tc := range []struct{ issuer, want string }{
		{"https://dex.patchy.acme.dev", ""},
		{"https://dex.patchy.acme.dev:5556/dex", ""},
		{"http://dex.patchy.acme.dev", "not an https URL"},
		{"https://user:pw@dex.acme.dev", "not an https URL"},
		{"https://dex.acme.dev?x=1", "not an https URL"},
		{"https://10.0.0.1", "not a DNS name"},
		{"https://dex.acme-previews.dev", "sits under"},
		{"https://dex.previews.dev", ""},
	} {
		err := CheckIssuer(tc.issuer, "acme-previews.dev", false)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("CheckIssuer(%s) = %v", tc.issuer, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("CheckIssuer(%s) = %v, want %q", tc.issuer, err, tc.want)
		}
	}
	if err := CheckIssuer("https://dex.acme.dev", "previews.acme.dev", false); err == nil ||
		!strings.Contains(err.Error(), "registrable domain acme.dev") ||
		!strings.Contains(err.Error(), "--shared-domain acknowledged") {
		t.Errorf("an issuer same-site with the suffix: %v", err)
	}
	if err := CheckIssuer("https://dex.acme.dev", "previews.acme.dev", true); err != nil {
		t.Errorf("an issuer same-site with the suffix, acknowledged: %v", err)
	}
}

// TestCheckHostsShared: the owner's one-domain install passes once
// acknowledged; a sign-in host under the suffix never does.
func TestCheckHostsShared(t *testing.T) {
	for _, tc := range []struct {
		suffix, signIn, want string
	}{
		{"preview.patchy.devthe.net", "preview-auth.patchy.devthe.net", ""},
		{"previews.devthe.net", "devthe.net", ""},
		{"acme-previews.dev", "preview-auth.patchy.acme.dev", ""},
		{"previews.devthe.net", "auth.previews.devthe.net", "sits under"},
		{"devthe.net", "auth.devthe.net", "sits under"},
		{"co.uk", "auth.acme.dev", "no registrable domain"},
	} {
		err := CheckHosts(tc.suffix, tc.signIn, true)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("CheckHosts(%s, %s, shared) = %v", tc.suffix, tc.signIn, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("CheckHosts(%s, %s, shared) = %v, want %q", tc.suffix, tc.signIn, err, tc.want)
		}
	}
	if err := CheckHosts("preview.patchy.devthe.net", "preview-auth.patchy.devthe.net", false); err == nil ||
		!strings.Contains(err.Error(), "--shared-domain acknowledged") {
		t.Errorf("an unacknowledged shared domain does not name the flag: %v", err)
	}
}

func TestParseGatewayParent(t *testing.T) {
	for in, want := range map[string][3]string{
		"gateway-system/edge":                {"gateway-system", "edge", ""},
		"gateway-system/edge/https-previews": {"gateway-system", "edge", "https-previews"},
	} {
		ns, name, section, err := ParseGatewayParent(in)
		if err != nil || [3]string{ns, name, section} != want {
			t.Errorf("ParseGatewayParent(%s) = %s %s %s %v", in, ns, name, section, err)
		}
	}
	for _, in := range []string{"edge", "a/b/c/d", "Gateway/edge", "ns/Edge!", "ns/edge/HTTPS", "/edge"} {
		if _, _, _, err := ParseGatewayParent(in); err == nil {
			t.Errorf("ParseGatewayParent(%s) passed", in)
		}
	}
}

// TestValidateReportsEveryProblem: every bad answer is named at once.
func TestValidateReportsEveryProblem(t *testing.T) {
	a := testAnswers()
	a.Platform = "openshift"
	a.Edge = "ngrok"
	a.Release = "Patchy"
	a.Namespace = "patchy_ns"
	a.Viewers = Viewers{Teams: []string{"acme"}, Users: []string{"-octocat"}}
	a.Registry = "ghcr.io/acme"
	a.ImagePathPrefix = "/previews"
	a.NodeIsolation = Nodes{Mode: "maybe", Label: "bad key!", LabelValue: "", TaintKey: "x/y/z"}
	a.APIServerCIDR = "10.0.0.0/8"
	a.Dex.ClientID = ""
	err := a.Validate()
	if err == nil {
		t.Fatal("Validate passed")
	}
	for _, want := range []string{"--platform", "--edge", "--release", "-n ", "--viewer-team", "--viewer-user",
		"--registry \"ghcr.io/acme\" holds a path", "--image-path-prefix", "--node-isolation", "--node-label key",
		"--node-label value", "--taint-key", "--api-server-cidr", "--dex-client-id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate does not name %s:\n%v", want, err)
		}
	}
}

func TestValidateExisting(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Answers)
		want string
	}{
		"both":           {func(a *Answers) { a.Existing.GatewayParent = "gw/edge" }, "exclusive"},
		"reserved class": {func(a *Answers) { a.Existing.IngressClass = "alb-preview" }, "reserved"},
		"bad class":      {func(a *Answers) { a.Existing.IngressClass = "Nginx" }, "--ingress-class"},
		"bad tls":        {func(a *Answers) { a.Existing.TLS = "acm" }, "--edge-tls"},
		"bad issuer":     {func(a *Answers) { a.Certificate.Issuer = "Issuer" }, "--certificate-issuer"},
		"issuer kind":    {func(a *Answers) { a.Certificate.Issuer = "Foo/bar" }, "--certificate-issuer"},
		"certificate on a gateway": {func(a *Answers) {
			a.Existing = ExistingEdge{GatewayParent: "gw/edge", TLS: TLSCertificate}
		}, "listener"},
		"viewers both ways": {func(a *Answers) { a.NoViewers = true }, "exclusive"},
	} {
		a := testAnswers()
		a.Platform = PlatformK3s
		a.Existing.IngressClass = "nginx"
		tc.edit(&a)
		if err := a.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want %q", name, err, tc.want)
		}
	}
}

func TestMissing(t *testing.T) {
	got := Defaults().Missing()
	want := []string{"--platform", "--edge", "--host-suffix", "--sign-in-host", "--dex-issuer",
		"--viewer-team or --viewer-user (or --no-viewers)", "--registry"}
	if !slices.Equal(got, want) {
		t.Errorf("Missing() = %v, want %v", got, want)
	}
	a := testAnswers()
	a.Platform = PlatformOther
	if got := a.Missing(); !slices.Equal(got, []string{"--node-isolation", "--ingress-class or --gateway-parent"}) {
		t.Errorf("other platform: Missing() = %v", got)
	}
	a.Platform = PlatformEKS
	if got := a.Missing(); len(got) != 0 {
		t.Errorf("EKS takes the edge class: Missing() = %v", got)
	}
	a.NoViewers, a.Viewers = true, Viewers{}
	if got := a.Missing(); len(got) != 0 {
		t.Errorf("--no-viewers: Missing() = %v", got)
	}
}

func TestDerived(t *testing.T) {
	a := testAnswers()
	for p, want := range map[Platform]string{PlatformK3d: IsolationRequired, PlatformGKE: IsolationRequired,
		PlatformOther: "", "": ""} {
		a.Platform = p
		if got := a.IsolationMode(); got != want {
			t.Errorf("%s: IsolationMode = %q, want %q", p, got, want)
		}
	}
	for _, tc := range []struct {
		platform Platform
		e        ExistingEdge
		want     string
	}{
		{PlatformK3s, ExistingEdge{IngressClass: "traefik"}, TLSCertificate},
		{PlatformK3s, ExistingEdge{IngressClass: "alb"}, TLSEdge},
		{PlatformEKS, ExistingEdge{}, TLSEdge},
		{PlatformEKS, ExistingEdge{IngressClass: "alb"}, TLSEdge},
		// Regression: EKS alone made every class edge-held, so ingress-nginx
		// or Traefik there served its default certificate.
		{PlatformEKS, ExistingEdge{IngressClass: "nginx"}, TLSCertificate},
		{PlatformEKS, ExistingEdge{IngressClass: "traefik"}, TLSCertificate},
		{PlatformEKS, ExistingEdge{IngressClass: "nginx", TLS: TLSEdge}, TLSEdge},
		{PlatformEKS, ExistingEdge{IngressClass: "nginx", TLS: TLSCertificate}, TLSCertificate},
		{PlatformGKE, ExistingEdge{GatewayParent: "gw/edge"}, TLSEdge},
	} {
		a.Platform, a.Existing = tc.platform, tc.e
		if got := a.EdgeTLS(); got != tc.want {
			t.Errorf("%s %+v: EdgeTLS = %s, want %s", tc.platform, tc.e, got, tc.want)
		}
	}
	for release, want := range map[string]string{"patchy": "patchy", "patchy-dev": "patchy-dev",
		"previews": "previews-patchy"} {
		a.Release = release
		if got := a.Fullname(); got != want {
			t.Errorf("Fullname(%s) = %s, want %s", release, got, want)
		}
	}
}

func TestNormalize(t *testing.T) {
	a := Answers{HostSuffix: " Acme-Previews.DEV. ", SignInHost: "Auth.Acme.dev", Registry: "GHCR.io",
		Viewers: Viewers{Teams: []string{"acme:qa", " ", "acme:dev", "acme:qa"}, Users: []string{"b", "a", "b"}}}
	a.Normalize()
	if a.HostSuffix != "acme-previews.dev" || a.SignInHost != "auth.acme.dev" || a.Registry != "ghcr.io" ||
		!slices.Equal(a.Viewers.Teams, []string{"acme:dev", "acme:qa"}) ||
		!slices.Equal(a.Viewers.Users, []string{"a", "b"}) {
		t.Errorf("Normalize = %+v", a)
	}
}

func TestChartVersion(t *testing.T) {
	for in, want := range map[string]string{"v0.12.40": "0.12.40", "0.12.40": "0.12.40", "v1.2.3-rc.1": "1.2.3-rc.1",
		"dev": "", "v1.2": ""} {
		if got := ChartVersion(in); got != want {
			t.Errorf("ChartVersion(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateEKSAutoMode(t *testing.T) {
	a := testAnswers()
	a.Platform = PlatformEKS
	a.NodeIsolation.EKSAutoMode = EKSAutoMode{NodePool: "previews", NodeClass: "previews"}
	if err := a.Validate(); err != nil {
		t.Fatalf("Auto Mode on EKS: %v", err)
	}
	for name, tc := range map[string]struct {
		edit func(*Answers)
		want string
	}{
		"not eks":   {func(a *Answers) { a.Platform = PlatformGKE; a.Existing.IngressClass = "nginx" }, "--platform eks"},
		"mode none": {func(a *Answers) { a.NodeIsolation.Mode = IsolationNone }, "--node-isolation none"},
		"bad name":  {func(a *Answers) { a.NodeIsolation.EKSAutoMode.NodePool = "Previews" }, "Previews"},
	} {
		b := a
		tc.edit(&b)
		if err := b.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want an error naming %q", name, err, tc.want)
		}
	}
	for in, ok := range map[string]bool{"": true, "pool/class": true, "pool": false, "/class": false, "pool/": false} {
		if _, err := ParseEKSAutoMode(in); (err == nil) != ok {
			t.Errorf("ParseEKSAutoMode(%q) = %v", in, err)
		}
	}
}
