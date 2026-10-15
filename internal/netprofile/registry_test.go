// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"net/netip"
	"strings"
	"testing"
)

// registriesJSON is the spec's example registries (section 4.1), as the
// chart renders them for the proxy: secret names never appear.
const registriesJSON = `[
 {"name":"art-npm","ecosystem":"npm","url":"https://acme.jfrog.io/artifactory/api/npm/npm-virtual/",
  "auth":{"type":"bearer"},"redirectHosts":["*.s3.us-east-1.amazonaws.com"],"healthPackage":"left-pad"},
 {"name":"art-go","ecosystem":"go","url":"https://acme.jfrog.io/artifactory/api/go/go-virtual",
  "auth":{"type":"basic"},"privateModules":["acme.example/"],"publicChecksumLookups":false},
 {"name":"nexus-pypi","ecosystem":"pypi","url":"https://nexus.corp.internal:8443/repository/pypi-all/simple/",
  "auth":{"type":"basic"},"fileHosts":["files.pythonhosted.org"],"ca":true,
  "internal":{"cidrs":["10.20.30.40/32"],"podSelector":{"namespace":"tools","matchLabels":{"app":"nexus"}},
   "targetPort":8081}},
 {"name":"ca-npm","ecosystem":"npm",
  "url":"https://patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com/npm/npm-store/",
  "auth":{"type":"codeartifact","codeartifact":{"domain":"patchy-gate","domainOwner":"377946145366",
   "region":"us-east-1","durationSeconds":43200}},"maxArtifactBytes":1073741824}]`

func TestParseRegistriesSpecExample(t *testing.T) {
	set, err := ParseRegistries([]byte(registriesJSON), RegistryContext{DenyHosts: []string{"github.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 4 {
		t.Fatalf("Len = %d", set.Len())
	}
	names := make([]string, 0, set.Len())
	for _, r := range set.Registries() {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "art-go,art-npm,ca-npm,nexus-pypi" {
		t.Fatalf("registries not sorted by name: %v", names)
	}
	goReg, _ := set.Get("art-go")
	if goReg.PrivateModules == nil || len(*goReg.PrivateModules) != 1 || goReg.PublicChecksumLookups == nil ||
		*goReg.PublicChecksumLookups {
		t.Fatalf("go checksum choice = %+v", goReg)
	}
	nexus, _ := set.Get("nexus-pypi")
	if nexus.Port() != 8443 || nexus.Hostname() != "nexus.corp.internal" ||
		len(nexus.InternalPrefixes()) != 1 || !nexus.CA {
		t.Fatalf("nexus = %+v", nexus)
	}
	if got := strings.Join(nexus.Hosts(), ","); got != "nexus.corp.internal,files.pythonhosted.org" {
		t.Fatalf("Hosts = %s", got)
	}
	ca, _ := set.Get("ca-npm")
	if ca.Auth.CodeArtifact.Duration() != 43200 || ca.Port() != 443 {
		t.Fatalf("ca = %+v", ca)
	}
	if _, ok := set.Get("nope"); ok {
		t.Fatal("Get found a registry that is not in the set")
	}
	// A copy read back cannot change the set.
	got, _ := set.Get("nexus-pypi")
	got.Internal.CIDRs[0] = "0.0.0.0/0"
	got.Internal.PodSelector.MatchLabels["app"] = "evil"
	(*goReg.PrivateModules)[0] = "evil.example/"
	again, _ := set.Get("nexus-pypi")
	goAgain, _ := set.Get("art-go")
	if again.Internal.CIDRs[0] != "10.20.30.40/32" || again.Internal.PodSelector.MatchLabels["app"] != "nexus" ||
		(*goAgain.PrivateModules)[0] != "acme.example/" {
		t.Fatal("a registry read back shares state with the set")
	}
}

func TestParseRegistriesEmptyAndShape(t *testing.T) {
	for _, in := range []string{"", " ", "null", "[]"} {
		set, err := ParseRegistries([]byte(in), RegistryContext{})
		if err != nil || set.Len() != 0 {
			t.Errorf("ParseRegistries(%q) = %d, %v", in, set.Len(), err)
		}
	}
	for in, want := range map[string]string{
		`[{"name":"a","ecosystem":"npm","url":"https://r.example.com/","auth":{"type":"none"},
		   "secretName":"x"}]`: "unknown field",
		`[{"name":"a","ecosystem":"npm","url":"https://r.example.com/",
		   "auth":{"type":"bearer","secretName":"x"}}]`: "unknown field",
		`{"name":"a"}`: "cannot unmarshal",
		`[] []`:        "trailing data",
		`[{"name":"a","ecosystem":"npm","url":"https://r.example.com/","auth":{"type":"none"}},
		  {"name":"a","ecosystem":"pypi","url":"https://r.example.com/simple/","auth":{"type":"none"}}]`: "named twice",
	} {
		if _, err := ParseRegistries([]byte(in), RegistryContext{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseRegistries(%s) = %v, want %q", in, err, want)
		}
	}
	many := make([]Registry, MaxRegistries+1)
	for i := range many {
		many[i] = validNPMRegistry()
		many[i].Name = "r" + string(rune('a'+i))
	}
	if _, err := NewRegistrySet(many, RegistryContext{}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("NewRegistrySet of %d = %v", len(many), err)
	}
}

func validNPMRegistry() Registry {
	return Registry{Name: "art-npm", Ecosystem: EcosystemNPM,
		URL: "https://acme.jfrog.io/artifactory/api/npm/npm-virtual/", Auth: Auth{Type: AuthBearer}}
}

func validGoRegistry() Registry {
	private := []string{"acme.example/"}
	public := false
	return Registry{Name: "art-go", Ecosystem: EcosystemGo, URL: "https://acme.jfrog.io/artifactory/api/go/go-virtual",
		Auth: Auth{Type: AuthBasic}, PrivateModules: &private, PublicChecksumLookups: &public}
}

func validCodeArtifact() Registry {
	return Registry{Name: "ca-pypi", Ecosystem: EcosystemPyPI,
		URL: "https://patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com/pypi/pypi-store/simple/",
		Auth: Auth{Type: AuthCodeArtifact, CodeArtifact: &CodeArtifact{Domain: "patchy-gate",
			DomainOwner: "377946145366", Region: "us-east-1"}}}
}

func validInternal() Registry {
	return Registry{Name: "nexus", Ecosystem: EcosystemPyPI,
		URL: "https://nexus.tools.svc.cluster.local:8443/repository/pypi/simple/", Auth: Auth{Type: AuthBasic},
		Internal: &Internal{CIDRs: []string{"172.20.5.6/32"},
			PodSelector: &PodSelector{Namespace: "tools", MatchLabels: map[string]string{"app": "nexus"}},
			TargetPort:  8081}}
}

// registryContext is a cluster with a pod/Service range, cluster DNS and
// an attribution API-server endpoint.
var registryContext = RegistryContext{
	DenyHosts:    []string{"github.com", "ghe.acme.example"},
	AlwaysDeny:   []netip.Prefix{netip.MustParsePrefix("172.20.0.10/32"), netip.MustParsePrefix("10.0.5.7/32")},
	ClusterCIDRs: []netip.Prefix{netip.MustParsePrefix("172.20.0.0/16"), netip.MustParsePrefix("100.64.0.0/16")},
}

func TestRegistryValidateAccepts(t *testing.T) {
	for name, r := range map[string]Registry{
		"npm bearer":          validNPMRegistry(),
		"go basic":            validGoRegistry(),
		"codeartifact pypi":   validCodeArtifact(),
		"internal in-cluster": validInternal(),
		"codeartifact on.aws": func() Registry {
			r := validCodeArtifact()
			r.URL = "https://patchy-gate-377946145366.d.codeartifact.us-east-1.on.aws/pypi/pypi-store/simple/"
			return r
		}(),
		"public none": {Name: "pub", Ecosystem: EcosystemNPM, URL: "https://registry.npmjs.org/",
			Auth: Auth{Type: AuthNone}},
		"explicit 443": {Name: "pub", Ecosystem: EcosystemNPM, URL: "https://registry.npmjs.org:443/",
			Auth: Auth{Type: AuthNone}},
		"go empty private, public lookups": func() Registry {
			r := validGoRegistry()
			empty, yes := []string{}, true
			r.PrivateModules, r.PublicChecksumLookups = &empty, &yes
			return r
		}(),
		"internal IP literal": {Name: "nexus", Ecosystem: EcosystemNPM, URL: "https://10.20.30.40:8443/npm/",
			Auth: Auth{Type: AuthBasic}, Internal: &Internal{CIDRs: []string{"10.20.30.0/24"}}},
		"max artifact 4 GiB": func() Registry {
			r := validNPMRegistry()
			r.MaxArtifactBytes = MaxArtifactBytesLimit
			return r
		}(),
		"health packages": func() Registry {
			r := validNPMRegistry()
			r.HealthPackage = "@acme/health"
			return r
		}(),
		"duration 900": func() Registry {
			r := validCodeArtifact()
			r.Auth.CodeArtifact.DurationSeconds = 900
			return r
		}(),
	} {
		if err := r.Validate(registryContext); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestRegistryValidateGuards mirrors the chart's render guards 12-19, one
// refusal each, in Go.
func TestRegistryValidateGuards(t *testing.T) {
	tests := []struct {
		guard  string
		base   func() Registry
		mutate func(*Registry)
		want   string
	}{
		{"name", validNPMRegistry, func(r *Registry) { r.Name = "Art" }, "DNS label"},
		{"ecosystem", validNPMRegistry, func(r *Registry) { r.Ecosystem = "maven" }, "ecosystem"},
		{"12 codeartifact on go", validGoRegistry, func(r *Registry) {
			r.Auth = Auth{Type: AuthCodeArtifact, CodeArtifact: &CodeArtifact{Domain: "d", DomainOwner: "1"}}
		}, "no Go module support"},
		{"12 codeartifact host mismatch", validCodeArtifact, func(r *Registry) { r.Auth.CodeArtifact.Region = "us-west-2" },
			"not the CodeArtifact endpoint"},
		{"12 codeartifact owner mismatch", validCodeArtifact, func(r *Registry) {
			r.Auth.CodeArtifact.DomainOwner = "111111111111"
		}, "not the CodeArtifact endpoint"},
		{"12 codeartifact other host", validCodeArtifact, func(r *Registry) {
			r.URL = "https://acme.jfrog.io/pypi/simple/"
		}, "not the CodeArtifact endpoint"},
		{"12 codeartifact without block", validCodeArtifact, func(r *Registry) { r.Auth.CodeArtifact = nil },
			"needs auth.codeartifact"},
		{"12 codeartifact owner digits", validCodeArtifact, func(r *Registry) { r.Auth.CodeArtifact.DomainOwner = "37794" },
			"12-digit"},
		{"12 codeartifact duration low", validCodeArtifact, func(r *Registry) {
			r.Auth.CodeArtifact.DurationSeconds = 899
		}, "durationSeconds"},
		{"12 codeartifact duration high", validCodeArtifact, func(r *Registry) {
			r.Auth.CodeArtifact.DurationSeconds = 43201
		}, "durationSeconds"},
		{"12 codeartifact block on bearer", validNPMRegistry, func(r *Registry) {
			r.Auth.CodeArtifact = &CodeArtifact{}
		}, "auth.codeartifact is set"},
		{"13 forge host", validNPMRegistry, func(r *Registry) { r.URL = "https://github.com/npm/" }, "deny list"},
		{"13 context forge", validNPMRegistry, func(r *Registry) { r.URL = "https://npm.ghe.acme.example/" }, "deny list"},
		{"13 githubusercontent", validNPMRegistry, func(r *Registry) {
			r.URL = "https://npm.pkg.githubusercontent.com/"
		}, "deny list"},
		{"13 model api", validNPMRegistry, func(r *Registry) { r.URL = "https://api.anthropic.com/" }, "deny list"},
		{"13 bedrock", validNPMRegistry, func(r *Registry) {
			r.URL = "https://bedrock-runtime.us-east-1.amazonaws.com/"
		}, "deny list"},
		{"13 vertex", validNPMRegistry, func(r *Registry) { r.URL = "https://us-east5-aiplatform.googleapis.com/" },
			"deny list"},
		{"13 kubernetes", validInternal, func(r *Registry) {
			r.URL = "https://kubernetes.default.svc.cluster.local:8443/simple/"
		}, "deny list"},
		{"13 svc without internal", validNPMRegistry, func(r *Registry) { r.URL = "https://nexus.tools.svc/npm/" },
			"in-cluster"},
		{"13 IP literal without internal", validNPMRegistry, func(r *Registry) { r.URL = "https://1.2.3.4/npm/" },
			"IP literal"},
		{"13 IP literal outside cidrs", validInternal, func(r *Registry) { r.URL = "https://10.9.9.9:8443/simple/" },
			"IP literal"},
		{"14 non-443 without internal", validNPMRegistry, func(r *Registry) {
			r.URL = "https://acme.jfrog.io:8443/npm/"
		}, "needs internal"},
		{"14 codeartifact port", validCodeArtifact, func(r *Registry) {
			r.URL = "https://patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com:8443/pypi/x/simple/"
			r.Internal = &Internal{CIDRs: []string{"10.1.2.3/32"}}
		}, "port"},
		{"15 IMDS", validInternal, func(r *Registry) { r.Internal.CIDRs = []string{"169.254.169.254/32"} },
			"always-denied"},
		{"15 loopback", validInternal, func(r *Registry) { r.Internal.CIDRs = []string{"127.0.0.0/24"} }, "always-denied"},
		{"15 cluster DNS", validInternal, func(r *Registry) { r.Internal.CIDRs = []string{"172.20.0.0/24"} },
			"always-denied"},
		{"15 api server", validInternal, func(r *Registry) { r.Internal.CIDRs = []string{"10.0.5.0/24"} },
			"always-denied"},
		{"15 /24 inside cluster CIDRs", validInternal, func(r *Registry) { r.Internal.CIDRs = []string{"172.20.5.0/24"} },
			"as a /32"},
		{"15 too wide", validInternal, func(r *Registry) { r.Internal.CIDRs = []string{"10.20.0.0/16"} }, "/24 or narrower"},
		{"15 IPv6", validInternal, func(r *Registry) { r.Internal.CIDRs = []string{"fd00::/120"} }, "IPv4"},
		{"15 unmasked", validInternal, func(r *Registry) { r.Internal.CIDRs = []string{"10.20.30.1/24"} }, "masked"},
		{"15 no cidrs", validInternal, func(r *Registry) { r.Internal.CIDRs = nil }, "cidrs"},
		{"16 unknown auth", validNPMRegistry, func(r *Registry) { r.Auth.Type = "token" }, "auth.type"},
		{"16 empty auth", validNPMRegistry, func(r *Registry) { r.Auth.Type = "" }, "auth.type"},
		{"17 privateModules on npm", validNPMRegistry, func(r *Registry) {
			pm := []string{"acme.example/"}
			r.PrivateModules = &pm
		}, "go registries only"},
		{"17 publicChecksumLookups on pypi", validCodeArtifact, func(r *Registry) {
			b := true
			r.PublicChecksumLookups = &b
		}, "go registries only"},
		{"17 targetPort without podSelector", validInternal, func(r *Registry) { r.Internal.PodSelector = nil },
			"needs internal.podSelector"},
		{"17 podSelector namespace", validInternal, func(r *Registry) { r.Internal.PodSelector.Namespace = "Tools" },
			"namespace"},
		{"17 podSelector labels", validInternal, func(r *Registry) { r.Internal.PodSelector.MatchLabels = nil },
			"matchLabels"},
		{"18 go without privateModules", validGoRegistry, func(r *Registry) { r.PrivateModules = nil }, "no default"},
		{"18 go without publicChecksumLookups", validGoRegistry, func(r *Registry) { r.PublicChecksumLookups = nil },
			"no default"},
		{"18 bad private prefix", validGoRegistry, func(r *Registry) {
			pm := []string{"golang.org/toolchain"}
			r.PrivateModules = &pm
		}, "toolchain"},
		{"19 denied fileHost", validCodeArtifact, func(r *Registry) {
			r.FileHosts = []string{"objects.githubusercontent.com"}
		}, "fileHosts"},
		{"19 wildcard fileHost", validCodeArtifact, func(r *Registry) { r.FileHosts = []string{"*.cdn.example.com"} },
			"fileHosts"},
		{"19 IP fileHost", validCodeArtifact, func(r *Registry) { r.FileHosts = []string{"1.2.3.4"} }, "fileHosts"},
		{"19 redirect amazonaws", validNPMRegistry, func(r *Registry) { r.RedirectHosts = []string{"*.amazonaws.com"} },
			"redirectHosts"},
		{"19 redirect googleapis", validNPMRegistry, func(r *Registry) {
			r.RedirectHosts = []string{"*.googleapis.com"}
		}, "redirectHosts"},
		{"19 redirect two labels", validNPMRegistry, func(r *Registry) { r.RedirectHosts = []string{"*.example.com"} },
			"redirectHosts"},
		{"19 redirect denied exact", validNPMRegistry, func(r *Registry) {
			r.RedirectHosts = []string{"api.anthropic.com"}
		}, "redirectHosts"},
		{"19 too many redirect hosts", validNPMRegistry, func(r *Registry) {
			r.RedirectHosts = make([]string, MaxRegistryHosts+1)
		}, "at most"},
		{"url http", validNPMRegistry, func(r *Registry) { r.URL = "http://acme.jfrog.io/npm/" }, "https"},
		{"url no path", validNPMRegistry, func(r *Registry) { r.URL = "https://acme.jfrog.io" }, "https"},
		{"url query", validNPMRegistry, func(r *Registry) { r.URL = "https://acme.jfrog.io/npm/?x=1" }, "https"},
		{"url userinfo", validNPMRegistry, func(r *Registry) { r.URL = "https://u:p@acme.jfrog.io/npm/" }, "https"},
		{"url uppercase", validNPMRegistry, func(r *Registry) { r.URL = "https://Acme.jfrog.io/npm/" }, "https"},
		{"url dotdot", validNPMRegistry, func(r *Registry) { r.URL = "https://acme.jfrog.io/npm/../x/" }, "segment"},
		{"url double slash", validNPMRegistry, func(r *Registry) { r.URL = "https://acme.jfrog.io/npm//x/" }, "segment"},
		{"pypi not simple", validCodeArtifact, func(r *Registry) {
			r.URL = "https://patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com/pypi/pypi-store/"
		}, "/simple/"},
		{"max artifact bytes", validNPMRegistry, func(r *Registry) { r.MaxArtifactBytes = MaxArtifactBytesLimit + 1 },
			"maxArtifactBytes"},
		{"negative artifact bytes", validNPMRegistry, func(r *Registry) { r.MaxArtifactBytes = -1 },
			"maxArtifactBytes"},
		{"health package long", validNPMRegistry, func(r *Registry) {
			r.HealthPackage = strings.Repeat("a", MaxHealthPackageLen+1)
		}, "healthPackage"},
		{"health package bad npm", validNPMRegistry, func(r *Registry) { r.HealthPackage = "../x" }, "healthPackage"},
		{"health package bad pypi", validCodeArtifact, func(r *Registry) { r.HealthPackage = "six/../x" },
			"healthPackage"},
		{"health package bad go", validGoRegistry, func(r *Registry) { r.HealthPackage = "acme" }, "healthPackage"},
	}
	for _, tt := range tests {
		t.Run(tt.guard, func(t *testing.T) {
			r := tt.base()
			tt.mutate(&r)
			err := r.Validate(registryContext)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want one naming %q", err, tt.want)
			}
			if _, err := NewRegistrySet([]Registry{r}, registryContext); err == nil {
				t.Fatal("NewRegistrySet accepted a registry Validate refuses")
			}
		})
	}
}
