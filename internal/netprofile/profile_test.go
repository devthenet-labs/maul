// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// specJSON is the shared profile JSON exactly as the design renders it
// (egress-proxy step-1 spec, section 3.4).
const specJSON = `[{"name":"go","route":"go","port":8090,"host":"github.com",
  "repositories":["devthenet-labs/maul-preview-demo"],
  "modules":["github.com/google/uuid","golang.org/x"],
  "bakedModCache":"/opt/go/pkg/mod","allowPseudoVersions":false}]`

// validProfile is a profile every rejection case below breaks in one way.
func validProfile() Profile {
	return Profile{
		Name: "go", Route: RouteGo, Port: 8090, Host: "github.com",
		Repositories:  []string{"devthenet-labs/maul-preview-demo"},
		Modules:       Prefixes{"github.com/google/uuid", "golang.org/x/"},
		BakedModCache: "/opt/go/pkg/mod",
	}
}

func TestParseSpecJSON(t *testing.T) {
	set, err := Parse([]byte(specJSON))
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 1 {
		t.Fatalf("Len = %d, want 1", set.Len())
	}
	p, ok := set.Get("go")
	if !ok {
		t.Fatal("profile go missing")
	}
	if p.Route != RouteGo || p.Port != 8090 || p.Host != "github.com" || p.BakedModCache != "/opt/go/pkg/mod" ||
		p.AllowPseudoVersions || len(p.Repositories) != 1 || p.Modules.String() != "github.com/google/uuid,golang.org/x" {
		t.Fatalf("parsed profile = %+v", p)
	}
	if _, ok := set.Get("npm"); ok {
		t.Fatal("Get found a profile that is not in the set")
	}
}

func TestParseEmptyAndDefaults(t *testing.T) {
	for _, in := range []string{"", "  ", "null", "[]"} {
		set, err := Parse([]byte(in))
		if err != nil || set.Len() != 0 {
			t.Errorf("Parse(%q) = %d profiles, %v; want an empty set", in, set.Len(), err)
		}
	}
	set, err := Parse([]byte(
		`[{"name":"go","route":"go","port":8090,"repositories":["a/b"],"modules":["example.com/m"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := set.Get("go")
	if p.Host != DefaultHost {
		t.Fatalf("host = %q, want the default %q", p.Host, DefaultHost)
	}
	if p.BakedModCache != "" || p.AllowPseudoVersions {
		t.Fatalf("unset bakedModCache/allowPseudoVersions = %q/%v, want \"\"/false (network only, tagged only)",
			p.BakedModCache, p.AllowPseudoVersions)
	}
}

func TestParseSortsByName(t *testing.T) {
	set, err := Parse([]byte(`[
	  {"name":"zeta","route":"go","port":8091,"repositories":["a/z"],"modules":["example.com/z"]},
	  {"name":"alpha","route":"go","port":8090,"repositories":["a/a"],"modules":["example.com/a"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	ps := set.Profiles()
	if len(ps) != 2 || ps[0].Name != "alpha" || ps[1].Name != "zeta" {
		t.Fatalf("profiles = %+v, want alpha then zeta", ps)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"unknown field", `[{"name":"go","route":"go","port":8090,"repositories":["a/b"],"modules":["example.com/m"],
			"extra":1}]`, "unknown field"},
		{"not an array", `{"name":"go"}`, "cannot unmarshal"},
		{"trailing data", `[] []`, "trailing data"},
		{"bad name", `[{"name":"Go","route":"go","port":8090,"repositories":["a/b"],"modules":["example.com/m"]}]`,
			"DNS label"},
		{"route npm", `[{"name":"go","route":"npm","port":8090,"repositories":["a/b"],"modules":["example.com/m"]}]`,
			"route"},
		{"duplicate port", `[
			{"name":"a","route":"go","port":8090,"repositories":["a/b"],"modules":["example.com/m"]},
			{"name":"b","route":"go","port":8090,"repositories":["a/c"],"modules":["example.com/m"]}]`, "port 8090"},
		{"duplicate name", `[
			{"name":"a","route":"go","port":8090,"repositories":["a/b"],"modules":["example.com/m"]},
			{"name":"a","route":"go","port":8091,"repositories":["a/c"],"modules":["example.com/m"]}]`, "named twice"},
		{"uppercase host", `[{"name":"go","route":"go","port":8090,"host":"GitHub.com","repositories":["a/b"],
			"modules":["example.com/m"]}]`, "host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.json))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse error = %v, want one naming %q", err, tt.want)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{"empty name", func(p *Profile) { p.Name = "" }, "DNS label"},
		{"name starts with a digit", func(p *Profile) { p.Name = "1go" }, "DNS label"},
		{"name ends with a dash", func(p *Profile) { p.Name = "go-" }, "DNS label"},
		{"name too long", func(p *Profile) { p.Name = "g" + strings.Repeat("o", MaxNameLen) }, "DNS label"},
		{"route empty", func(p *Profile) { p.Route = "" }, "route"},
		{"route npm", func(p *Profile) { p.Route = "npm" }, "route"},
		{"port 8081", func(p *Profile) { p.Port = HealthPort }, "health port"},
		{"port privileged", func(p *Profile) { p.Port = 443 }, "outside"},
		{"port too high", func(p *Profile) { p.Port = 65536 }, "outside"},
		{"uppercase host", func(p *Profile) { p.Host = "GitHub.com" }, "host"},
		{"host with a scheme", func(p *Profile) { p.Host = "https://github.com" }, "host"},
		{"empty host", func(p *Profile) { p.Host = "" }, "host"},
		{"no repositories", func(p *Profile) { p.Repositories = nil }, "repositories"},
		{"too many repositories", func(p *Profile) { p.Repositories = make([]string, MaxRepositories+1) },
			"repositories"},
		{"empty pattern", func(p *Profile) { p.Repositories = []string{""} }, "1-256 bytes"},
		{"long pattern", func(p *Profile) { p.Repositories = []string{strings.Repeat("a", MaxPatternLen+1)} },
			"1-256 bytes"},
		{"invalid RE2", func(p *Profile) { p.Repositories = []string{"devthenet-labs/(maul"} }, "missing closing )"},
		{"RE2 rejects backreferences", func(p *Profile) { p.Repositories = []string{`(a)\1`} }, "invalid escape"},
		{"group breakout", func(p *Profile) { p.Repositories = []string{"x)|(.*"} }, "unexpected )"},
		{"group breakout suffix", func(p *Profile) { p.Repositories = []string{"a)|(b"} }, "unexpected )"},
		{"no modules", func(p *Profile) { p.Modules = nil }, "modules"},
		{"too many modules", func(p *Profile) {
			p.Modules = make(Prefixes, MaxModules+1)
			for i := range p.Modules {
				p.Modules[i] = fmt.Sprintf("example.com/m%d", i)
			}
		}, "modules"},
		{"module star", func(p *Profile) { p.Modules = Prefixes{"*"} }, "module prefix"},
		{"module bare TLD", func(p *Profile) { p.Modules = Prefixes{"com"} }, "module prefix"},
		{"module golang.org", func(p *Profile) { p.Modules = Prefixes{"golang.org"} }, "toolchain"},
		{"module golang.org/", func(p *Profile) { p.Modules = Prefixes{"golang.org/"} }, "toolchain"},
		{"module golang.org/toolchain", func(p *Profile) { p.Modules = Prefixes{"golang.org/toolchain"} }, "toolchain"},
		{"module golang.org/toolchainx", func(p *Profile) { p.Modules = Prefixes{"golang.org/toolchainx"} },
			"toolchain"},
		{"module uppercase host", func(p *Profile) { p.Modules = Prefixes{"GitHub.com/"} }, "module prefix"},
		{"module empty", func(p *Profile) { p.Modules = Prefixes{""} }, "module prefix"},
		{"module double slash", func(p *Profile) { p.Modules = Prefixes{"github.com//x"} }, "module prefix"},
		{"baked relative", func(p *Profile) { p.BakedModCache = "opt/go/pkg/mod" }, "bakedModCache"},
		{"baked unclean dotdot", func(p *Profile) { p.BakedModCache = "/opt/../go" }, "bakedModCache"},
		{"baked unclean dot", func(p *Profile) { p.BakedModCache = "/opt/./go" }, "bakedModCache"},
		{"baked trailing slash", func(p *Profile) { p.BakedModCache = "/opt/go/" }, "bakedModCache"},
		{"baked root", func(p *Profile) { p.BakedModCache = "/" }, "bakedModCache"},
		{"baked with a space", func(p *Profile) { p.BakedModCache = "/opt/go mod" }, "bakedModCache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validProfile()
			tt.mutate(&p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want one naming %q", err, tt.want)
			}
			if _, err := NewSet([]Profile{p}); err == nil {
				t.Fatal("NewSet accepted a profile Validate refuses")
			}
		})
	}
}

// TestParseRejectsGroupBreakout: a repository pattern whose stray ")"
// would close the anchoring group and add an unanchored alternative is
// refused at Parse, rather than widening the profile to every repository on
// the host. Regression for a pattern that For once matched against
// evil/anything.
func TestParseRejectsGroupBreakout(t *testing.T) {
	for _, pattern := range []string{"devthenet-labs/maul)|(.*", "x)|(.*", "a)|(b"} {
		t.Run(pattern, func(t *testing.T) {
			data, err := json.Marshal([]map[string]any{{
				"name": "go", "route": "go", "port": 8090,
				"repositories": []string{pattern}, "modules": []string{"github.com/google/uuid"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			set, err := Parse(data)
			if err == nil {
				p, reason := set.For("https://github.com/evil/anything")
				t.Fatalf("Parse accepted %q; For(evil/anything) = %q, %q", pattern, p.Name, reason)
			}
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
	}{
		{"as given", func(*Profile) {}},
		{"max-length name", func(p *Profile) { p.Name = "g" + strings.Repeat("o", MaxNameLen-1) }},
		{"one-letter name", func(p *Profile) { p.Name = "g" }},
		{"lowest port", func(p *Profile) { p.Port = MinPort }},
		{"highest port", func(p *Profile) { p.Port = MaxPort }},
		{"enterprise host", func(p *Profile) { p.Host = "ghe.example.com" }},
		{"no baked cache", func(p *Profile) { p.BakedModCache = "" }},
		{"pseudo-versions", func(p *Profile) { p.AllowPseudoVersions = true }},
		{"golang.org/x", func(p *Profile) { p.Modules = Prefixes{"golang.org/x"} }},
		{"whole github (documented residue)", func(p *Profile) { p.Modules = Prefixes{"github.com/"} }},
		{"mixed-case path element", func(p *Profile) { p.Modules = Prefixes{"github.com/BurntSushi/toml"} }},
		{"underscore", func(p *Profile) { p.Modules = Prefixes{"github.com/prometheus/client_golang"} }},
		{"regex patterns", func(p *Profile) { p.Repositories = []string{"devthenet-labs/.*", "acme/(api|web)"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validProfile()
			tt.mutate(&p)
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNewSetLimits(t *testing.T) {
	ps := make([]Profile, 0, MaxProfiles+1)
	for i := range MaxProfiles + 1 {
		p := validProfile()
		p.Name = fmt.Sprintf("p%d", i)
		p.Port = 8090 + i
		ps = append(ps, p)
	}
	if _, err := NewSet(ps[:MaxProfiles]); err != nil {
		t.Fatalf("%d profiles: %v", MaxProfiles, err)
	}
	if _, err := NewSet(ps); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("%d profiles: error = %v, want a limit refusal", len(ps), err)
	}
}

// TestSetIsolatedFromCaller: neither the slice handed to NewSet nor a
// profile read back can change what the set matches or allows.
func TestSetIsolatedFromCaller(t *testing.T) {
	in := []Profile{validProfile()}
	set, err := NewSet(in)
	if err != nil {
		t.Fatal(err)
	}
	in[0].Modules[0] = "evil.example.com"
	in[0].Repositories[0] = ".*"
	got, _ := set.Get("go")
	got.Modules[0] = "evil.example.com"
	set.Profiles()[0].Modules[1] = "evil.example.com"
	p, _ := set.Get("go")
	if p.Modules.Allows("evil.example.com") || !p.Modules.Allows("github.com/google/uuid") {
		t.Fatalf("modules changed through a caller's slice: %v", p.Modules)
	}
	if _, r := set.For("https://github.com/other/repo"); r != ReasonNoMatch {
		t.Fatalf("repositories changed through a caller's slice: %v", r)
	}
}

func TestLabelKey(t *testing.T) {
	if got := LabelKey("go"); got != "maul.run/network-go" {
		t.Fatalf("LabelKey = %q", got)
	}
	// The longest name still gives a valid label key: a DNS prefix and a
	// name part of at most 63 characters.
	long := LabelKey("g" + strings.Repeat("o", MaxNameLen-1))
	_, part, _ := strings.Cut(long, "/")
	if len(part) > 63 {
		t.Fatalf("name part %q is %d characters, over 63", part, len(part))
	}
}
