// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

// The chart renders egressProxy.registries into the JSON ParseRegistries
// reads, and its values.schema.json and render guards (maul.egressProxyGuard
// and maul.egressRegistriesGuard) judge each registry and grant before the
// proxy sees them. These tests keep the chart's copies of the rules in step
// with this package's: the schema's bounds and patterns, the deny table the
// guards spell out as literal lists, and the deny helpers' verdicts.

// registrySchema is the part of values.schema.json the registry rules live
// in.
type registrySchema struct {
	Definitions struct {
		Registry struct {
			Required   []string `json:"required"`
			Properties struct {
				Ecosystem struct {
					Enum []string `json:"enum"`
				} `json:"ecosystem"`
				URL  stringSchema `json:"url"`
				Auth struct {
					Properties struct {
						Type struct {
							Enum []string `json:"enum"`
						} `json:"type"`
						CodeArtifact struct {
							Required   []string `json:"required"`
							Properties struct {
								Domain          stringSchema `json:"domain"`
								DomainOwner     stringSchema `json:"domainOwner"`
								Region          stringSchema `json:"region"`
								DurationSeconds struct {
									Minimum int `json:"minimum"`
									Maximum int `json:"maximum"`
								} `json:"durationSeconds"`
							} `json:"properties"`
						} `json:"codeartifact"`
					} `json:"properties"`
				} `json:"auth"`
				FileHosts struct {
					MaxItems int          `json:"maxItems"`
					Items    stringSchema `json:"items"`
				} `json:"fileHosts"`
				RedirectHosts struct {
					MaxItems int          `json:"maxItems"`
					Items    stringSchema `json:"items"`
				} `json:"redirectHosts"`
				Internal struct {
					Properties struct {
						CIDRs struct {
							MinItems int          `json:"minItems"`
							Items    stringSchema `json:"items"`
						} `json:"cidrs"`
						PodSelector struct {
							Properties struct {
								Namespace   stringSchema `json:"namespace"`
								MatchLabels struct {
									MinProperties int `json:"minProperties"`
									MaxProperties int `json:"maxProperties"`
								} `json:"matchLabels"`
							} `json:"properties"`
						} `json:"podSelector"`
						TargetPort struct {
							Minimum int `json:"minimum"`
							Maximum int `json:"maximum"`
						} `json:"targetPort"`
					} `json:"properties"`
				} `json:"internal"`
				PrivateModules struct {
					MaxItems int          `json:"maxItems"`
					Items    stringSchema `json:"items"`
				} `json:"privateModules"`
				MaxArtifactBytes struct {
					Minimum int64 `json:"minimum"`
					Maximum int64 `json:"maximum"`
				} `json:"maxArtifactBytes"`
				HealthPackage stringSchema `json:"healthPackage"`
			} `json:"properties"`
			If struct {
				Properties struct {
					Ecosystem struct {
						Const string `json:"const"`
					} `json:"ecosystem"`
				} `json:"properties"`
			} `json:"if"`
			Then struct {
				Required []string `json:"required"`
			} `json:"then"`
		} `json:"registry"`
		RegistryGrant struct {
			Properties struct {
				Packages struct {
					MaxItems int `json:"maxItems"`
				} `json:"packages"`
				PackagePolicy struct {
					Enum []string `json:"enum"`
				} `json:"packagePolicy"`
			} `json:"properties"`
			OneOf []struct {
				Required []string `json:"required"`
			} `json:"oneOf"`
		} `json:"registryGrant"`
		NetworkProfile struct {
			AnyOf []struct {
				Required []string `json:"required"`
			} `json:"anyOf"`
			AllOf []struct {
				If struct {
					Properties struct {
						Route struct {
							Const string `json:"const"`
						} `json:"route"`
					} `json:"properties"`
				} `json:"if"`
				Then struct {
					Required []string `json:"required"`
				} `json:"then"`
			} `json:"allOf"`
			Dependencies map[string][]string `json:"dependencies"`
			Properties   struct {
				Registries struct {
					MaxProperties int `json:"maxProperties"`
					PropertyNames struct {
						Pattern string `json:"pattern"`
					} `json:"propertyNames"`
				} `json:"registries"`
			} `json:"properties"`
		} `json:"networkProfile"`
	} `json:"definitions"`
	Properties struct {
		EgressProxy struct {
			Properties struct {
				Registries struct {
					MaxProperties int `json:"maxProperties"`
					PropertyNames struct {
						Pattern string `json:"pattern"`
					} `json:"propertyNames"`
				} `json:"registries"`
			} `json:"properties"`
		} `json:"egressProxy"`
	} `json:"properties"`
}

func loadRegistrySchema(t *testing.T) registrySchema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s registrySchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// validRegistry is a registry Validate accepts, for one field to be varied.
func validRegistry() Registry {
	return Registry{Name: "art-npm", Ecosystem: EcosystemNPM, URL: "https://npm.reg.example/api/npm/",
		Auth: Auth{Type: AuthBearer}}
}

// TestChartRegistrySchemaMatchesValidator holds values.schema.json's
// registry and grant rules to Validate: the enums and numeric bounds are
// the package's constants, the patterns the package's own, and for the
// fields whose whole rule a schema can carry (names, CodeArtifact
// identifiers, internal CIDR shape, pod selector namespace) a corpus of
// fixed edge cases plus seeded random strings is admitted by the schema
// exactly when Validate accepts it. The rest (the deny list, CIDR overlaps,
// the CodeArtifact host, a pypi index's /simple/) is the render guards',
// pinned by TestDeniedHostMatchesChart and hack/chart-render-test.sh.
func TestChartRegistrySchemaMatchesValidator(t *testing.T) {
	s := loadRegistrySchema(t)
	reg := s.Definitions.Registry
	rp := reg.Properties
	ca := rp.Auth.Properties.CodeArtifact
	in := rp.Internal.Properties
	grant := s.Definitions.RegistryGrant
	np := s.Definitions.NetworkProfile
	regs := s.Properties.EgressProxy.Properties.Registries

	oneOf := make([][]string, 0, len(grant.OneOf))
	for _, o := range grant.OneOf {
		oneOf = append(oneOf, o.Required)
	}
	anyOf := make([][]string, 0, len(np.AnyOf))
	for _, o := range np.AnyOf {
		anyOf = append(anyOf, o.Required)
	}
	routeThen := map[string][]string{}
	for _, o := range np.AllOf {
		routeThen[o.If.Properties.Route.Const] = o.Then.Required
	}
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"registries maxProperties", regs.MaxProperties, MaxRegistries},
		{"registries propertyNames", regs.PropertyNames.Pattern, namePattern.String()},
		{"registry required", reg.Required, []string{"ecosystem", "url", "auth"}},
		{"ecosystem enum", rp.Ecosystem.Enum, Ecosystems},
		{"url pattern", rp.URL.Pattern, registryURLPattern.String()},
		{"auth.type enum", rp.Auth.Properties.Type.Enum, []string{AuthNone, AuthBearer, AuthBasic, AuthCodeArtifact}},
		{"codeartifact required", ca.Required, []string{"domain", "domainOwner", "region"}},
		{"codeartifact domain", ca.Properties.Domain.Pattern, codeArtifactDomain.String()},
		{"codeartifact domainOwner", ca.Properties.DomainOwner.Pattern, codeArtifactOwner.String()},
		{"codeartifact region", ca.Properties.Region.Pattern, awsRegion.String()},
		{"durationSeconds minimum", ca.Properties.DurationSeconds.Minimum, MinCodeArtifactDuration},
		{"durationSeconds maximum", ca.Properties.DurationSeconds.Maximum, DefaultCodeArtifactDuration},
		{"fileHosts maxItems", rp.FileHosts.MaxItems, MaxRegistryHosts},
		{"fileHosts pattern", rp.FileHosts.Items.Pattern, hostPattern.String()},
		{"redirectHosts maxItems", rp.RedirectHosts.MaxItems, MaxRegistryHosts},
		{"redirectHosts pattern", rp.RedirectHosts.Items.Pattern, `^(\*\.)?` + strings.TrimPrefix(hostPattern.String(), "^")},
		{"internal.cidrs minItems", in.CIDRs.MinItems, 1},
		{"podSelector namespace", in.PodSelector.Properties.Namespace.Pattern, dnsLabel.String()},
		{"podSelector matchLabels minProperties", in.PodSelector.Properties.MatchLabels.MinProperties, 1},
		{"podSelector matchLabels maxProperties", in.PodSelector.Properties.MatchLabels.MaxProperties, maxPodLabels},
		{"targetPort maximum", in.TargetPort.Maximum, MaxPort},
		{"privateModules maxItems", rp.PrivateModules.MaxItems, MaxModules},
		{"maxArtifactBytes minimum", rp.MaxArtifactBytes.Minimum, int64(0)},
		{"maxArtifactBytes maximum", rp.MaxArtifactBytes.Maximum, MaxArtifactBytesLimit},
		{"healthPackage maxLength", rp.HealthPackage.MaxLength, MaxHealthPackageLen},
		{"go if", reg.If.Properties.Ecosystem.Const, EcosystemGo},
		{"go then", reg.Then.Required, []string{"privateModules", "publicChecksumLookups"}},
		{"packages maxItems", grant.Properties.Packages.MaxItems, MaxGrantPackages},
		{"packagePolicy enum", grant.Properties.PackagePolicy.Enum, []string{PackagePolicyRegistry}},
		{"grant oneOf", oneOf, [][]string{{"packages"}, {"packagePolicy"}}},
		{"profile anyOf", anyOf, [][]string{{"route"}, {"registries"}}},
		{"profile route rules", routeThen, map[string][]string{RouteGo: {"modules"}, RouteTunnel: {"hosts"}}},
		{"profile dependencies", np.Dependencies, map[string][]string{
			"modules": {"route"}, "hosts": {"route"}, "clients": {"hosts"}, "upload": {"hosts"},
		}},
		{"profile registries maxProperties", np.Properties.Registries.MaxProperties, len(Ecosystems)},
		{"profile registries propertyNames", np.Properties.Registries.PropertyNames.Pattern, namePattern.String()},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("values.schema.json %s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if rp.PrivateModules.Items.Pattern != loadChartSchema(t).Definitions.NetworkProfile.Properties.Modules.Items.Pattern {
		t.Error("values.schema.json privateModules items pattern differs from the profile modules pattern")
	}

	fields := []struct {
		name     string
		schema   stringSchema
		validate func(string) bool
		fixed    []string
		gen      chartString
	}{
		{
			name:   "registry name",
			schema: stringSchema{Pattern: regs.PropertyNames.Pattern},
			validate: func(v string) bool {
				r := validRegistry()
				r.Name = v
				return r.Validate(RegistryContext{}) == nil
			},
			fixed: []string{"", "a", "art-npm", "Art", "1a", "a-", "-a", "a_b", "a.b", strings.Repeat("a", 32),
				strings.Repeat("a", 33)},
			gen: chartString{alphabet: "az09-_.A", maxLen: 36},
		},
		{
			name:     "codeartifact domain",
			schema:   ca.Properties.Domain,
			validate: func(v string) bool { return codeArtifactValid(v, "377946145366", "us-east-1") },
			fixed: []string{"", "a", "ab", "maul-gate", "Maul", "1ab", "ab-", strings.Repeat("a", 50),
				strings.Repeat("a", 51)},
			gen: chartString{alphabet: "az09-A", maxLen: 54},
		},
		{
			name:     "codeartifact domainOwner",
			schema:   ca.Properties.DomainOwner,
			validate: func(v string) bool { return codeArtifactValid("maul-gate", v, "us-east-1") },
			fixed:    []string{"", "377946145366", "37794614536", "3779461453667", "37794614536a"},
			gen:      chartString{alphabet: "0123456789a", maxLen: 14},
		},
		{
			name:     "codeartifact region",
			schema:   ca.Properties.Region,
			validate: func(v string) bool { return codeArtifactValid("maul-gate", "377946145366", v) },
			fixed:    []string{"", "us-east-1", "eu-west-2", "us-gov-west-1", "useast1", "us-east-", "US-east-1", "u-east-1"},
			gen:      chartString{alphabet: "use-1t2", maxLen: 14},
		},
		{
			name:   "internal cidr shape",
			schema: in.CIDRs.Items,
			validate: func(v string) bool {
				// The schema carries the shape (an IPv4 /24 to /32); the
				// octet range and masking are the render guard's and the
				// validator's, so only well-formed masked prefixes count.
				p, err := netip.ParsePrefix(v)
				return err == nil && p.Addr().Is4() && p.Bits() >= MinInternalPrefixLen
			},
			fixed: []string{"", "10.1.2.0/24", "10.1.2.3/32", "10.1.2.0/23", "10.1.2.0/33", "10.1.2/24",
				"::1/128", "10.1.2.0", "10.1.2.0/024"},
			gen: chartString{alphabet: "0123456789./", maxLen: 18},
		},
		{
			name:   "pod selector namespace",
			schema: in.PodSelector.Properties.Namespace,
			validate: func(v string) bool {
				return PodSelector{Namespace: v, MatchLabels: map[string]string{"app": "nexus"}}.validate() == nil
			},
			fixed: []string{"", "tools", "Tools", "-a", "a-", "a.b", strings.Repeat("a", 63), strings.Repeat("a", 64)},
			gen:   chartString{alphabet: "az09-.A", maxLen: 66},
		},
	}
	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			check := func(v string) bool {
				if f.name == "internal cidr shape" && !shapeOnlyCIDR(v) {
					return true // octets past 255 or a leading zero: the guard's, not the schema's
				}
				if got, want := f.schema.admits(t, v), f.validate(v); got != want {
					t.Errorf("%s %q: the chart schema admits it %v, the validator %v", f.name, v, got, want)
					return false
				}
				return true
			}
			for _, v := range f.fixed {
				check(v)
			}
			r := rand.New(rand.NewSource(seed))
			if err := quick.Check(func() bool { return check(f.gen.draw(r)) }, quickConfig(3000)); err != nil {
				t.Error(err)
			}
		})
	}
}

// shapeOnlyCIDR reports whether a string's octets, if it has four, are
// each 0-255 without a leading zero: the corpus the CIDR shape pattern is
// compared on.
func shapeOnlyCIDR(v string) bool {
	addr, _, _ := strings.Cut(v, "/")
	octets := strings.Split(addr, ".")
	if len(octets) != 4 {
		return true
	}
	for _, o := range octets {
		n, err := strconv.Atoi(o)
		if err != nil || n > 255 || (len(o) > 1 && o[0] == '0') {
			return false
		}
	}
	return true
}

// codeArtifactValid is Validate on a codeartifact registry whose URL host
// is the identifiers' own endpoint, so only the identifiers are judged.
func codeArtifactValid(domain, owner, region string) bool {
	r := Registry{Name: "ca", Ecosystem: EcosystemNPM,
		URL:  fmt.Sprintf("https://%s-%s.d.codeartifact.%s.amazonaws.com/npm/r/", domain, owner, region),
		Auth: Auth{Type: AuthCodeArtifact, CodeArtifact: &CodeArtifact{Domain: domain, DomainOwner: owner, Region: region}}}
	return r.Validate(RegistryContext{}) == nil
}

// chartHelper is the body of one define in _helpers.tpl.
func chartHelper(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(chartDir, "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	def := `(?s)\{\{- define "` + regexp.QuoteMeta(name) + `" -\}\}\n(.*?)\n\{\{- end \}\}`
	m := regexp.MustCompile(def).FindSubmatch(src)
	if m == nil {
		t.Fatalf("_helpers.tpl does not define %s", name)
	}
	return string(m[1])
}

// TestDeniedHostMatchesChart: the deny table the chart's guards spell out
// is this package's, list for list, and the chart's maul.egressDeniedHost
// and maul.egressDeniedPattern reach the same verdict as DeniedHost and
// DeniedPattern over a seeded corpus of hosts and patterns built from the
// table's own labels (run through helm on a scratch chart holding the real
// _helpers.tpl, so the template code itself is what is judged).
func TestDeniedHostMatchesChart(t *testing.T) {
	prefixes := make([]string, 0, len(AlwaysDeniedCIDRs))
	for _, p := range AlwaysDeniedCIDRs {
		prefixes = append(prefixes, p.String())
	}
	for _, c := range []struct {
		helper string
		want   []string
	}{
		{"maul.egressDeniedForgeHosts", ForgeHosts},
		{"maul.egressDeniedModelHosts", ModelAPIHosts},
		{"maul.egressDeniedClusterHosts", clusterHosts},
		{"maul.egressClusterSuffixes", clusterSuffixes},
		{"maul.egressTooWideSuffixes", TooWideSuffixes},
		{"maul.egressAlwaysDeniedCIDRs", prefixes},
		{"maul.egressMinPatternLabels", []string{strconv.Itoa(MinPatternLabels)}},
	} {
		if got := strings.Fields(chartHelper(t, c.helper)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %v, want %v", c.helper, got, c.want)
		}
	}

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	cases := deniedCorpus()
	verdicts := chartDenyVerdicts(t, cases)
	denied := 0
	for _, v := range verdicts {
		if v != "" {
			denied++
		}
	}
	t.Logf("%d cases, %d denied by the chart", len(cases), denied)
	for i, c := range cases {
		ctx := DenyContext{Hosts: denyCorpusExtra, Internal: c.Internal, InternalCIDRs: c.prefixes()}
		var err error
		if c.Pattern {
			err = DeniedPattern(c.Host, ctx)
		} else {
			err = DeniedHost(c.Host, ctx)
		}
		if chartAdmits := verdicts[i] == ""; chartAdmits != (err == nil) {
			t.Errorf("%q (pattern %v, internal %v): the chart says %q, the validator %v",
				c.Host, c.Pattern, c.Internal, verdicts[i], err)
		}
	}
}

// denyCase is one host or pattern judged by both sides.
type denyCase struct {
	Host     string   `json:"host"`
	Pattern  bool     `json:"pattern"`
	Internal bool     `json:"internal"`
	CIDRs    []string `json:"cidrs"`
}

func (c denyCase) prefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(c.CIDRs))
	for _, s := range c.CIDRs {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// denyCorpusExtra stands for the profiles' forge hosts and the broker's
// overrides.
var denyCorpusExtra = []string{"ghe.corp.example", "gw.corp.example"}

// deniedCorpus is the spec's examples plus seeded hosts and patterns over
// the table's labels, with in-cluster names, IP literals and both internal
// settings.
func deniedCorpus() []denyCase {
	fixed := []string{
		"github.com", "api.github.com", "objects.githubusercontent.com", "api.anthropic.com",
		"bedrock-runtime.us-east-1.amazonaws.com", "bedrock.us-east-1.vpce.amazonaws.com",
		"x.bedrock-runtime.us-east-1.vpce.amazonaws.com", "us-east5-aiplatform.googleapis.com",
		"x.cognitiveservices.azure.com", "generativelanguage.googleapis.com", "kubernetes.default.svc",
		"nexus.tools.svc.cluster.local", "acme.jfrog.io", "ghe.corp.example", "x.gw.corp.example",
		"maul-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com", "10.100.12.34", "10.100.12.35",
		"169.254.169.254", "127.0.0.1", "files.pythonhosted.org", "Acme.JFrog.io.",
	}
	patterns := []string{
		"*.amazonaws.com", "*.googleapis.com", "*.githubusercontent.com", "*.azure.com",
		"*.s3.us-east-1.amazonaws.com", "*.us-east-1.amazonaws.com", "*.acme.io", "*.cdn.acme.example",
		"*.corp.example", "*.default.svc.cluster.local", "*.vpce.amazonaws.com", "*.x.us-east-1.vpce.amazonaws.com",
		"*.bedrock.us-east-1.vpce.amazonaws.com", "*.blob.core.windows.net", "*.10.0.0.1",
	}
	out := make([]denyCase, 0, 2*(len(fixed)+len(patterns))+2500)
	for _, internal := range []bool{false, true} {
		cidrs := []string(nil)
		if internal {
			cidrs = []string{"10.100.12.34/32"}
		}
		for _, h := range fixed {
			out = append(out, denyCase{Host: h, Internal: internal, CIDRs: cidrs})
		}
		for _, p := range patterns {
			out = append(out, denyCase{Host: p, Pattern: true, Internal: internal, CIDRs: cidrs})
		}
	}
	labels := []string{
		"github", "com", "githubusercontent", "objects", "anthropic", "api", "bedrock-runtime", "bedrock",
		"us-east-1", "amazonaws", "vpce", "s3", "googleapis", "us-east5-aiplatform", "aiplatform",
		"generativelanguage", "azure", "openai", "services", "ai", "cognitiveservices", "kubernetes", "default",
		"svc", "cluster", "local", "acme", "jfrog", "io", "example", "cdn", "corp", "gw", "ghe", "storage",
		"cloudfront", "net", "windows", "blob", "core", "x",
	}
	r := rand.New(rand.NewSource(seed))
	for range 2500 {
		n := 1 + r.Intn(6)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = labels[r.Intn(len(labels))]
		}
		host := strings.Join(parts, ".")
		c := denyCase{Host: host, Pattern: r.Intn(3) == 0, Internal: r.Intn(2) == 0}
		if c.Pattern {
			c.Host = "*." + host
		}
		if r.Intn(10) == 0 {
			c.Host = fmt.Sprintf("%d.%d.%d.%d", r.Intn(256), r.Intn(256), r.Intn(256), r.Intn(256))
			c.Pattern = false
		}
		if c.Internal {
			c.CIDRs = []string{"10.100.12.0/24"}
		}
		out = append(out, c)
	}
	return out
}

// chartDenyVerdicts runs the cases through the chart's deny helpers: a
// scratch chart holding the real _helpers.tpl and one template that prints
// each case's verdict, rendered by helm once.
func chartDenyVerdicts(t *testing.T, cases []denyCase) []string {
	t.Helper()
	dir := t.TempDir()
	helpers, err := os.ReadFile(filepath.Join(chartDir, "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	probe := `apiVersion: v1
kind: ConfigMap
metadata:
    name: deny-probe
data:
    result: |
        {{- range $i, $c := .Values.cases }}
        {{- $ctx := dict "host" $c.host "internal" $c.internal "cidrs" ($c.cidrs | default list) }}
        {{- $_ := set $ctx "extra" $.Values.extra }}
        {{- $helper := ternary "maul.egressDeniedPattern" "maul.egressDeniedHost" (eq $c.pattern true) }}
        {{ $i }}|{{ include $helper $ctx }}
        {{- end }}
`
	values, err := json.Marshal(map[string]any{"cases": cases, "extra": denyCorpusExtra})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"Chart.yaml":                 []byte("apiVersion: v2\nname: deny-probe\nversion: 0.0.1\n"),
		"templates/_helpers.tpl":     helpers,
		"templates/probe.yaml":       []byte(probe),
		filepath.Join("values.json"): values,
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.CommandContext(t.Context(), "helm", "template", "probe", dir,
		"-f", filepath.Join(dir, "values.json")).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	verdicts := make([]string, len(cases))
	seen := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		idx, why, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		i, err := strconv.Atoi(idx)
		if err != nil || i < 0 || i >= len(cases) {
			continue
		}
		verdicts[i] = why
		seen++
	}
	if seen != len(cases) {
		t.Fatalf("the probe chart printed %d verdicts, want %d", seen, len(cases))
	}
	return verdicts
}
