// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/quick"
)

// registryProfileJSON is a step-2 profile set: a registry-only profile
// (no route) granting npm and pypi, and a go profile that also holds a go
// grant.
const registryProfileJSON = `[
 {"name":"web","port":8091,"repositories":["devthenet-labs/patchy-polyglot-demo"],
  "modules":[],"bakedModCache":"","allowPseudoVersions":false,
  "registries":[
   {"name":"ca-npm","ecosystem":"npm",
    "hosts":["patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com"],
    "packagePolicy":"registry"},
   {"name":"ca-pypi","ecosystem":"pypi",
    "hosts":["patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com","files.pythonhosted.org"],
    "packages":["six","requests","charset-normalizer"],"sdists":true}]},
 {"name":"go","route":"go","port":8090,"repositories":["devthenet-labs/patchy-preview-demo"],
  "modules":["github.com/","golang.org/x"],"bakedModCache":"/opt/go/pkg/mod","allowPseudoVersions":false,
  "registries":[
   {"name":"art-go","ecosystem":"go","hosts":["acme.jfrog.io"],"packages":["acme.example/"],
    "privateModules":["acme.example/"],"allowPseudoVersions":true}]}]`

func TestParseRegistryProfiles(t *testing.T) {
	set, err := Parse([]byte(registryProfileJSON))
	if err != nil {
		t.Fatal(err)
	}
	web, ok := set.Get("web")
	if !ok || web.Route != "" || len(web.Modules) != 0 || len(web.Registries) != 2 {
		t.Fatalf("web = %+v", web)
	}
	npm, ok := web.GrantFor(EcosystemNPM)
	if !ok || npm.Name != "ca-npm" || npm.PackagePolicy != PackagePolicyRegistry {
		t.Fatalf("npm grant = %+v", npm)
	}
	pip, _ := web.Grant("ca-pypi")
	if !pip.PipSdists || !pip.Allows("Charset_Normalizer") || pip.Allows("flask") {
		t.Fatalf("pypi grant = %+v", pip)
	}
	if _, ok := web.GrantFor(EcosystemGo); ok {
		t.Fatal("web has no go grant")
	}
	goP, _ := set.Get("go")
	g, _ := goP.Grant("art-go")
	if !g.AllowPseudoVersions || !g.PrivateModulePrefixes().Allows("acme.example/x") {
		t.Fatalf("go grant = %+v", g)
	}
	// The overlap of public modules and private modules is legal: the
	// more specific private prefix wins at the proxy.
	if !goP.Modules.Allows("github.com/acme/x") {
		t.Fatal("public modules changed")
	}
	if got := set.Hosts(); len(got) != 1 || got[0] != "github.com" {
		t.Fatalf("Hosts = %v", got)
	}
	// Grants read back cannot change the set.
	pip.Packages[0] = "flask"
	web.Registries[0].Packages = []string{"evil"}
	again, _ := set.Get("web")
	if again.Registries[1].Packages[0] != "six" || again.Registries[0].Packages != nil {
		t.Fatal("a grant read back shares state with the set")
	}
}

// TestStepOneProfileReencodes: a step-1 profile carries no registries key
// when encoded again (the chart renders the key only for a profile with
// grants).
func TestStepOneProfileReencodes(t *testing.T) {
	set, err := Parse([]byte(specJSON))
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(set.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "registries") {
		t.Fatalf("a step-1 profile encodes a registries key: %s", out)
	}
	p, _ := set.Get("go")
	if p.Registries != nil {
		t.Fatalf("a step-1 profile reads back registries %#v, want nil", p.Registries)
	}
}

func validGrant() Grant {
	return Grant{Name: "ca-npm", Ecosystem: EcosystemNPM, Hosts: []string{"acme.jfrog.io"},
		Packages: []string{"left-pad", "@acme/*"}}
}

func registryOnlyProfile() Profile {
	return Profile{Name: "web", Port: 8091, Host: "github.com", Repositories: []string{"a/b"},
		Registries: []Grant{validGrant()}}
}

// TestGrantGuards mirrors the chart's render guards 10, 11 and 17 (the
// grant half), plus the profile rules a grant changes.
func TestGrantGuards(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{"no route and no grants", func(p *Profile) { p.Registries = nil }, "must hold registry grants"},
		{"route npm", func(p *Profile) { p.Route = "npm" }, "not supported"},
		{"modules without route", func(p *Profile) { p.Modules = Prefixes{"example.com/m"} }, "set route: go"},
		{"10 two npm grants", func(p *Profile) {
			g := validGrant()
			g.Name = "other"
			p.Registries = append(p.Registries, g)
		}, "at most one grant per ecosystem"},
		{"10 four grants", func(p *Profile) {
			p.Registries = []Grant{validGrant(), validGrant(), validGrant(), validGrant()}
		}, "at most one grant per ecosystem"},
		{"11 neither", func(p *Profile) { p.Registries[0].Packages = nil }, "no default"},
		{"11 both", func(p *Profile) { p.Registries[0].PackagePolicy = PackagePolicyRegistry }, "not both"},
		{"11 unknown policy", func(p *Profile) {
			p.Registries[0].Packages, p.Registries[0].PackagePolicy = nil, "any"
		}, "packagePolicy"},
		{"17 scripts on pypi", func(p *Profile) {
			p.Registries[0] = Grant{Name: "pip", Ecosystem: EcosystemPyPI, Packages: []string{"six"}, NPMScripts: true}
		}, "npm grants only"},
		{"17 allowPrerelease on go", func(p *Profile) {
			p.Registries[0] = Grant{Name: "g", Ecosystem: EcosystemGo, Packages: []string{"a.example/"},
				AllowPrerelease: true}
		}, "npm grants only"},
		{"17 sdists on npm", func(p *Profile) { p.Registries[0].PipSdists = true }, "pypi grants only"},
		{"17 allowPseudoVersions on npm", func(p *Profile) { p.Registries[0].AllowPseudoVersions = true },
			"go grants only"},
		{"privateModules on npm", func(p *Profile) { p.Registries[0].PrivateModules = []string{"a.example/"} },
			"go grants only"},
		{"bad grant name", func(p *Profile) { p.Registries[0].Name = "Bad" }, "DNS label"},
		{"bad ecosystem", func(p *Profile) { p.Registries[0].Ecosystem = "cargo" }, "ecosystem"},
		{"bad npm package", func(p *Profile) { p.Registries[0].Packages = []string{"../x"} }, "packages"},
		{"bad npm wildcard", func(p *Profile) { p.Registries[0].Packages = []string{"@Acme/*"} }, "packages"},
		{"npm bare wildcard", func(p *Profile) { p.Registries[0].Packages = []string{"*"} }, "packages"},
		{"bad pip package", func(p *Profile) {
			p.Registries[0] = Grant{Name: "pip", Ecosystem: EcosystemPyPI, Packages: []string{"six/x"}}
		}, "packages"},
		{"bad go package", func(p *Profile) {
			p.Registries[0] = Grant{Name: "g", Ecosystem: EcosystemGo, Packages: []string{"golang.org"}}
		}, "toolchain"},
		{"too many packages", func(p *Profile) {
			p.Registries[0].Packages = make([]string, MaxGrantPackages+1)
			for i := range p.Registries[0].Packages {
				p.Registries[0].Packages[i] = "p" + strings.Repeat("x", i%200)
			}
		}, "at most"},
		{"denied host", func(p *Profile) { p.Registries[0].Hosts = []string{"api.github.com"} }, "deny list"},
		{"wide pattern host", func(p *Profile) { p.Registries[0].Hosts = []string{"*.amazonaws.com"} }, "hosts"},
		{"bad host", func(p *Profile) { p.Registries[0].Hosts = []string{"https://x"} }, "host name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := registryOnlyProfile()
			tt.mutate(&p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want one naming %q", err, tt.want)
			}
			if _, err := NewSet([]Profile{p}); err == nil {
				t.Fatal("NewSet accepted a profile Validate refuses")
			}
		})
	}
	// Accepted shapes.
	for name, mutate := range map[string]func(*Profile){
		"registry-only":   func(*Profile) {},
		"registry policy": func(p *Profile) { p.Registries[0].Packages, p.Registries[0].PackagePolicy = nil, "registry" },
		"route and grant": func(p *Profile) { p.Route, p.Modules = RouteGo, Prefixes{"golang.org/x"} },
		"three ecosystems": func(p *Profile) {
			p.Registries = append(p.Registries,
				Grant{Name: "pip", Ecosystem: EcosystemPyPI, Packages: []string{"six"}, PipSdists: true},
				Grant{Name: "g", Ecosystem: EcosystemGo, PackagePolicy: "registry", PrivateModules: []string{"a.example/"},
					AllowPseudoVersions: true})
		},
		"internal ip host": func(p *Profile) { p.Registries[0].Hosts = []string{"10.20.30.40", "nexus.tools.svc"} },
		"pattern host":     func(p *Profile) { p.Registries[0].Hosts = []string{"a.b.c", "*.s3.us-east-1.amazonaws.com"} },
		"npm scripts and prerelease": func(p *Profile) {
			p.Registries[0].NPMScripts, p.Registries[0].AllowPrerelease = true, true
		},
		"baked cache without route": func(p *Profile) { p.BakedModCache = "/opt/go/pkg/mod" },
	} {
		p := registryOnlyProfile()
		mutate(&p)
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestGrantDeniedHostsForges(t *testing.T) {
	g := Grant{Name: "r", Ecosystem: EcosystemNPM, PackagePolicy: "registry",
		Hosts: []string{"npm.acme.example", "*.cdn.acme.example"}}
	if err := g.DeniedHosts(DenyContext{}); err != nil {
		t.Fatal(err)
	}
	for _, forge := range []string{"npm.acme.example", "acme.example", "x.cdn.acme.example", "cdn.acme.example"} {
		if err := g.DeniedHosts(DenyContext{Hosts: []string{forge}}); err == nil {
			t.Errorf("a grant reaching the forge %s passed the Forge check", forge)
		}
	}
	if err := g.DeniedHosts(DenyContext{Hosts: []string{"ghe.other.example"}}); err != nil {
		t.Errorf("an unrelated forge refused the grant: %v", err)
	}
}

func TestGrantAllows(t *testing.T) {
	npm := Grant{Ecosystem: EcosystemNPM, Packages: []string{"left-pad", "@acme/*", "@types/node"}}
	for pkg, want := range map[string]bool{
		"left-pad": true, "left-pad2": false, "Left-Pad": false, "@acme/x": true, "@acme/": true,
		"@acmex/y": false, "@types/node": true, "@types/react": false, "": false, "acme/x": false,
	} {
		if got := npm.Allows(pkg); got != want {
			t.Errorf("npm Allows(%q) = %v, want %v", pkg, got, want)
		}
	}
	pip := Grant{Ecosystem: EcosystemPyPI, Packages: []string{"zope.interface", "Six"}}
	for pkg, want := range map[string]bool{
		"zope-interface": true, "Zope_Interface": true, "six": true, "sixx": false, "zope": false,
	} {
		if got := pip.Allows(pkg); got != want {
			t.Errorf("pip Allows(%q) = %v, want %v", pkg, got, want)
		}
	}
	goG := Grant{Ecosystem: EcosystemGo, Packages: []string{"acme.example/lib"}}
	if !goG.Allows("acme.example/lib/v2") || goG.Allows("acme.example/libx") {
		t.Error("go Allows does not follow the module prefix matcher")
	}
	reg := Grant{Ecosystem: EcosystemNPM, PackagePolicy: PackagePolicyRegistry}
	if !reg.Allows("anything") || reg.Allows("") {
		t.Error("packagePolicy registry must admit every name and refuse the empty one")
	}
}

// TestPropertyNPMScopeWildcard: "@s/*" admits exactly the names in scope
// s.
func TestPropertyNPMScopeWildcard(t *testing.T) {
	prop := func(allowed, p npmPkg) bool {
		if allowed.scope == "" {
			allowed.scope = "acme"
		}
		g := Grant{Ecosystem: EcosystemNPM, Packages: []string{"@" + allowed.scope + "/*"}}
		if g.Validate() != nil && validPackage(EcosystemNPM, g.Packages[0]) != nil {
			return false
		}
		in := NPMFullName(allowed.scope, p.name)
		out := NPMFullName(p.scope, p.name)
		return g.Allows(in) && g.Allows(out) == (p.scope == allowed.scope)
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyPrivateModulesArePrefixes: step 1's prefix boundary holds on
// a grant's privateModules and on a go grant's packages.
func TestPropertyPrivateModulesArePrefixes(t *testing.T) {
	prop := func(d derived) bool {
		want := d.m == d.base || strings.HasPrefix(d.m, d.base+"/")
		g := Grant{Ecosystem: EcosystemGo, Packages: []string{d.base}, PrivateModules: []string{d.base}}
		return g.PrivateModulePrefixes().Allows(d.m) == want && g.Allows(d.m) == want
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

// TestCheckGrants mirrors guard 9 and the grant/registry agreement the
// proxy checks at startup.
func TestCheckGrants(t *testing.T) {
	regs, err := ParseRegistries([]byte(`[
	 {"name":"ca-npm","ecosystem":"npm",
	  "url":"https://patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com/npm/npm-store/",
	  "auth":{"type":"codeartifact",
	   "codeartifact":{"domain":"patchy-gate","domainOwner":"377946145366","region":"us-east-1"}}},
	 {"name":"ca-pypi","ecosystem":"pypi",
	  "url":"https://patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com/pypi/pypi-store/simple/",
	  "auth":{"type":"codeartifact",
	   "codeartifact":{"domain":"patchy-gate","domainOwner":"377946145366","region":"us-east-1"}},
	  "fileHosts":["files.pythonhosted.org"]},
	 {"name":"art-go","ecosystem":"go","url":"https://acme.jfrog.io/go/","auth":{"type":"basic"},
	  "privateModules":["acme.example/"],"publicChecksumLookups":true}]`), RegistryContext{})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := Parse([]byte(registryProfileJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckGrants(profiles, regs); err != nil {
		t.Fatalf("CheckGrants of the matching documents: %v", err)
	}
	for name, edit := range map[string]func(string) string{
		"9 unknown registry": func(s string) string { return strings.Replace(s, `"name":"ca-npm"`, `"name":"nope"`, 1) },
		"ecosystem disagrees": func(s string) string {
			return strings.Replace(s, `{"name":"art-go","ecosystem":"go","hosts":["acme.jfrog.io"],"packages":["acme.example/"],
    "privateModules":["acme.example/"],"allowPseudoVersions":true}`,
				`{"name":"art-go","ecosystem":"npm","hosts":["acme.jfrog.io"],"packages":["left-pad"]}`, 1)
		},
		"private modules differ": func(s string) string {
			return strings.Replace(s, `"privateModules":["acme.example/"]`, `"privateModules":["other.example/"]`, 1)
		},
		"hosts differ": func(s string) string {
			return strings.Replace(s, `"hosts":["acme.jfrog.io"]`, `"hosts":["other.jfrog.io"]`, 1)
		},
	} {
		edited := edit(registryProfileJSON)
		if edited == registryProfileJSON {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		set, err := Parse([]byte(edited))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := CheckGrants(set, regs); err == nil {
			t.Errorf("%s: CheckGrants passed", name)
		}
	}
}
