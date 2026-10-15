// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf8"
)

// The chart renders agent.networkProfiles into the JSON Parse reads, and its
// values.schema.json and render guards judge each profile before any binary
// sees it. These tests keep the chart's copies of the rules in step with
// this package's, so a profile the chart admits is one the controllers and
// the proxy start with.

const chartDir = "../../charts/patchy"

// chartSchema is the part of values.schema.json the profile rules live in.
type chartSchema struct {
	Definitions struct {
		NetworkProfile struct {
			Required   []string `json:"required"`
			Properties struct {
				Route struct {
					Enum []string `json:"enum"`
				} `json:"route"`
				Port struct {
					Minimum int `json:"minimum"`
					Maximum int `json:"maximum"`
					Not     struct {
						Const int `json:"const"`
					} `json:"not"`
				} `json:"port"`
				Host         stringSchema `json:"host"`
				Repositories struct {
					MinItems int          `json:"minItems"`
					MaxItems int          `json:"maxItems"`
					Items    stringSchema `json:"items"`
				} `json:"repositories"`
				Modules struct {
					MinItems int          `json:"minItems"`
					MaxItems int          `json:"maxItems"`
					Items    stringSchema `json:"items"`
				} `json:"modules"`
				BakedModCache       stringSchema `json:"bakedModCache"`
				AllowPseudoVersions struct {
					Default bool `json:"default"`
				} `json:"allowPseudoVersions"`
			} `json:"properties"`
		} `json:"networkProfile"`
	} `json:"definitions"`
	Properties struct {
		Agent struct {
			Properties struct {
				NetworkProfiles struct {
					MaxProperties int `json:"maxProperties"`
					PropertyNames struct {
						Pattern string `json:"pattern"`
					} `json:"propertyNames"`
				} `json:"networkProfiles"`
			} `json:"properties"`
		} `json:"agent"`
	} `json:"properties"`
}

// stringSchema is a JSON-schema string: a pattern and length bounds.
type stringSchema struct {
	Pattern   string `json:"pattern"`
	MinLength int    `json:"minLength"`
	MaxLength int    `json:"maxLength"`
	Default   string `json:"default"`
}

// admits judges s the way the chart's schema validator does: the pattern
// (unanchored by JSON-schema rules; every profile pattern anchors itself)
// and the length bounds in characters.
func (s stringSchema) admits(t *testing.T, v string) bool {
	t.Helper()
	if s.Pattern != "" && !regexp.MustCompile(s.Pattern).MatchString(v) {
		return false
	}
	n := utf8.RuneCountInString(v)
	return n >= s.MinLength && (s.MaxLength == 0 || n <= s.MaxLength)
}

func loadChartSchema(t *testing.T) chartSchema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s chartSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// chartString is a string drawn mostly from one field's alphabet, so the
// generated corpus sits on the grammar's edges rather than far outside it.
type chartString struct {
	alphabet string
	maxLen   int
}

func (c chartString) draw(r *rand.Rand) string {
	n := r.Intn(c.maxLen + 1)
	var b strings.Builder
	for range n {
		b.WriteByte(c.alphabet[r.Intn(len(c.alphabet))])
	}
	return b.String()
}

// TestChartProfileSchemaMatchesValidator holds values.schema.json to
// Validate: the numeric bounds are the package's constants, and for each
// string field a corpus (fixed edge cases plus seeded random strings over
// the field's alphabet) is admitted by the schema exactly when Validate
// accepts it. The one rule the schema cannot carry, the module prefixes that
// would admit the Go toolchain, is the chart's render guard
// (patchy.egressProxyGuard, exercised by hack/chart-render-test.sh), so for
// modules the schema plus that rule must equal ValidPrefix.
func TestChartProfileSchemaMatchesValidator(t *testing.T) {
	s := loadChartSchema(t)
	np := s.Definitions.NetworkProfile.Properties
	profiles := s.Properties.Agent.Properties.NetworkProfiles

	// The bounds.
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"route enum", np.Route.Enum, []string{RouteGo}},
		{"port minimum", np.Port.Minimum, MinPort},
		{"port maximum", np.Port.Maximum, MaxPort},
		{"port not", np.Port.Not.Const, HealthPort},
		{"repositories minItems", np.Repositories.MinItems, 1},
		{"repositories maxItems", np.Repositories.MaxItems, MaxRepositories},
		{"repository pattern minLength", np.Repositories.Items.MinLength, 1},
		{"repository pattern maxLength", np.Repositories.Items.MaxLength, MaxPatternLen},
		{"modules minItems", np.Modules.MinItems, 1},
		{"modules maxItems", np.Modules.MaxItems, MaxModules},
		{"host maxLength", np.Host.MaxLength, maxHostLen},
		{"host default", np.Host.Default, DefaultHost},
		{"bakedModCache maxLength", np.BakedModCache.MaxLength, maxBakedCacheLen},
		{"allowPseudoVersions default", np.AllowPseudoVersions.Default, false},
		{"networkProfiles maxProperties", profiles.MaxProperties, MaxProfiles},
		{"required", s.Definitions.NetworkProfile.Required, []string{"route", "port", "repositories", "modules"}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("values.schema.json %s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if !validBakedModCacheDefault(np.BakedModCache.Default) {
		t.Errorf("values.schema.json bakedModCache default %q is not a valid bakedModCache", np.BakedModCache.Default)
	}

	nameSchema := stringSchema{Pattern: profiles.PropertyNames.Pattern}
	fields := []struct {
		name     string
		schema   func(string) bool
		validate func(string) bool
		fixed    []string
		gen      chartString
	}{
		{
			name:   "name",
			schema: func(v string) bool { return nameSchema.admits(t, v) },
			validate: func(v string) bool {
				p := validProfile()
				p.Name = v
				return p.Validate() == nil
			},
			fixed: []string{"", "go", "g", "Go", "1go", "go-", "-go", "go-1", "go_1", "go.x",
				strings.Repeat("a", 32), strings.Repeat("a", 33), "a" + strings.Repeat("-", 30) + "b"},
			gen: chartString{alphabet: "abz019-_.A", maxLen: 36},
		},
		{
			name:   "host",
			schema: func(v string) bool { return np.Host.admits(t, v) },
			validate: func(v string) bool {
				p := validProfile()
				p.Host = v
				return p.Validate() == nil
			},
			fixed: []string{"", "github.com", "GitHub.com", "github.com.", ".github.com", "a", "a-", "x..y",
				"git_hub.com", strings.Repeat("a", 253), strings.Repeat("a", 254)},
			gen: chartString{alphabet: "ab09.-_A", maxLen: 12},
		},
		{
			name:   "module prefix",
			schema: func(v string) bool { return np.Modules.Items.admits(t, v) && !toolchainGuard(v) },
			validate: func(v string) bool {
				return ValidPrefix(v) == nil
			},
			fixed: []string{"", "*", "com", "golang.org", "golang.org/", "golang.org/x", "golang.org/x/",
				"golang.org/toolchain", "golang.org/toolchainx/y", "GitHub.com/", "github.com/BurntSushi/toml",
				"github.com/prometheus/client_golang", "gopkg.in/yaml.v3", "example.com//x", "example.com/x//",
				"-a.com", "a-.com", "a.com/~x+y", "a.com/x y"},
			gen: chartString{alphabet: "ago.lnr/tcAB_~+-x", maxLen: 24},
		},
		{
			name:   "bakedModCache",
			schema: func(v string) bool { return np.BakedModCache.admits(t, v) },
			validate: func(v string) bool {
				p := validProfile()
				p.BakedModCache = v
				return p.Validate() == nil
			},
			fixed: []string{"", "/", "/opt/go/pkg/mod", "opt/go", "/opt/../x", "/opt/./x", "/opt/x/", "/opt//x",
				"/.", "/..", "/...", "/.x", "/..x", "/x.", "/x..", "/a b", "/a+b_c-d", "/" + strings.Repeat("a", 4095),
				"/" + strings.Repeat("a", 4096)},
			gen: chartString{alphabet: "/./a+_-", maxLen: 10},
		},
	}
	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			check := func(v string) bool {
				if got, want := f.schema(v), f.validate(v); got != want {
					t.Errorf("%s %q: the chart schema admits it %v, the validator %v", f.name, v, got, want)
					return false
				}
				return true
			}
			for _, v := range f.fixed {
				check(v)
			}
			r := rand.New(rand.NewSource(seed))
			if err := quick.Check(func() bool { return check(f.gen.draw(r)) }, quickConfig(5000)); err != nil {
				t.Error(err)
			}
		})
	}
}

// toolchainGuard is the chart's render guard 6 (patchy.egressProxyGuard):
// a module prefix that is golang.org itself or starts golang.org/toolchain.
func toolchainGuard(v string) bool {
	t := strings.TrimSuffix(v, "/")
	return t == "golang.org" || strings.HasPrefix(t, "golang.org/toolchain")
}

func validBakedModCacheDefault(v string) bool { return v != "" && validBakedModCache(v) == nil }

// TestChartLabelPrefix: the chart spells the profile label prefix once, in
// the patchy.networkProfileLabelPrefix helper, and it is LabelPrefix. Every
// template that selects or guards the label (the proxy's ingress, the
// agent-side policies, the admission policy and the NOTES) goes through the
// helper, and no template writes the prefix out by hand.
func TestChartLabelPrefix(t *testing.T) {
	templates := filepath.Join(chartDir, "templates")
	helpers, err := os.ReadFile(filepath.Join(templates, "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	def := regexp.MustCompile(`(?s)\{\{- define "patchy\.networkProfileLabelPrefix" -\}\}\n(.*?)\n\{\{- end \}\}`).
		FindSubmatch(helpers)
	if def == nil {
		t.Fatal("_helpers.tpl does not define patchy.networkProfileLabelPrefix")
	}
	if got := string(def[1]); got != LabelPrefix {
		t.Errorf("patchy.networkProfileLabelPrefix = %q, want netprofile.LabelPrefix %q", got, LabelPrefix)
	}
	files, err := filepath.Glob(filepath.Join(templates, "*"))
	if err != nil {
		t.Fatal(err)
	}
	spelled := strings.TrimSuffix(LabelPrefix, "-")
	for _, name := range files {
		if st, err := os.Stat(name); err != nil || st.IsDir() {
			continue // crds/: generated, and never the label
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Base(name)
		n := strings.Count(string(src), spelled)
		if base == "_helpers.tpl" {
			n-- // the helper's own definition
		}
		if n != 0 {
			t.Errorf("%s spells the profile label prefix out %d time(s); use patchy.networkProfileLabelPrefix", base, n)
		}
	}
	for _, base := range []string{
		"egress-proxy.yaml", "networkpolicy-profiles.yaml", "admission-policy.yaml", "NOTES.txt",
	} {
		src, err := os.ReadFile(filepath.Join(templates, base))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), `include "patchy.networkProfileLabelPrefix"`) {
			t.Errorf("%s does not take the profile label prefix from patchy.networkProfileLabelPrefix", base)
		}
	}
}
