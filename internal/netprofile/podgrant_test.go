// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func podNPM() PodGrant {
	return PodGrant{Registry: "ca-npm", Ecosystem: EcosystemNPM, AnyPackage: true}
}
func podPyPI() PodGrant {
	return PodGrant{Registry: "ca-pypi", Ecosystem: EcosystemPyPI, Packages: []string{"six"}}
}
func podGo() PodGrant {
	return PodGrant{Registry: "art-go", Ecosystem: EcosystemGo, Packages: []string{"acme.example/"},
		PrivateModules: []string{"acme.example/"}}
}

// TestPodGrantOf: the agent is told the grant's policy and switches, the
// first MaxPodPackages allowlist entries and a count of the rest, and
// nothing about the registry's hosts.
func TestPodGrantOf(t *testing.T) {
	g := Grant{Name: "ca-pypi", Ecosystem: EcosystemPyPI, Hosts: []string{"x.example"}, PipSdists: true}
	for i := range MaxPodPackages + 5 {
		g.Packages = append(g.Packages, fmt.Sprintf("p%d", i))
	}
	pg := PodGrantOf(g)
	if len(pg.Packages) != MaxPodPackages || pg.MorePackages != 5 || !pg.Sdists || pg.AnyPackage {
		t.Errorf("PodGrantOf = %+v", pg)
	}
	if err := pg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	pg = PodGrantOf(Grant{Name: "ca-npm", Ecosystem: EcosystemNPM, PackagePolicy: PackagePolicyRegistry})
	if !pg.AnyPackage || pg.Packages != nil || pg.MorePackages != 0 {
		t.Errorf("registry policy PodGrantOf = %+v", pg)
	}
}

// TestPodGrantsRoundTrip: encoding sorts by ecosystem, decoding gives the
// same grants back, and the ecosystems list agrees.
func TestPodGrantsRoundTrip(t *testing.T) {
	enc, err := EncodePodGrants([]PodGrant{podPyPI(), podNPM(), podGo()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePodGrants(enc)
	if err != nil {
		t.Fatal(err)
	}
	want := []PodGrant{podGo(), podNPM(), podPyPI()}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if l := EcosystemList(false, got); l != "go,npm,pypi" {
		t.Errorf("EcosystemList = %q", l)
	}
	if l := EcosystemList(true, []PodGrant{podNPM()}); l != "go,npm" {
		t.Errorf("EcosystemList(public go, npm) = %q", l)
	}
	if g, err := DecodePodGrants(""); err != nil || g != nil {
		t.Errorf("DecodePodGrants(\"\") = %v, %v", g, err)
	}
}

// TestDecodePodGrantsRefuses: anything the controller could not have
// written is refused, so a value an image smuggled past the reservation
// never reaches the prompt as a fact.
func TestDecodePodGrantsRefuses(t *testing.T) {
	for _, s := range []string{
		`{}`,
		`[{"registry":"ca-npm","ecosystem":"npm","anyPackage":true,"extra":1}]`,
		`[{"registry":"ca-npm","ecosystem":"npm","anyPackage":true}] []`,
		`[{"registry":"ca-npm","ecosystem":"npm"}]`,
		`[{"registry":"ca-npm","ecosystem":"npm","anyPackage":true,"packages":["x"]}]`,
		`[{"registry":"ca-npm","ecosystem":"npm","anyPackage":true,"morePackages":3}]`,
		`[{"registry":"ca-npm","ecosystem":"npm","packages":["x"],"morePackages":3}]`,
		`[{"registry":"ca-npm","ecosystem":"maven","anyPackage":true}]`,
		`[{"registry":"Ca","ecosystem":"npm","anyPackage":true}]`,
		`[{"registry":"ca-npm","ecosystem":"npm","packages":["a\nb"]}]`,
		`[{"registry":"ca-npm","ecosystem":"npm","anyPackage":true,"sdists":true}]`,
		`[{"registry":"a","ecosystem":"npm","anyPackage":true},{"registry":"b","ecosystem":"npm","anyPackage":true}]`,
		`[{"registry":"p","ecosystem":"pypi","anyPackage":true},{"registry":"n","ecosystem":"npm","anyPackage":true}]`,
		strings.Repeat(" ", maxPodGrantsBytes) + `[]x`,
	} {
		if _, err := DecodePodGrants(s); err == nil {
			t.Errorf("DecodePodGrants(%.80q) = nil error", s)
		}
	}
}

func TestParseEcosystems(t *testing.T) {
	for in, ok := range map[string]bool{
		"": true, "go": true, "npm,pypi": true, "go,npm,pypi": true,
		"npm,go": false, "go,go": false, "maven": false, "go,": false, " go": false,
	} {
		if _, err := ParseEcosystems(in); (err == nil) != ok {
			t.Errorf("ParseEcosystems(%q) = %v, want ok %v", in, err, ok)
		}
	}
	if err := CheckPodRoutes([]string{"go", "npm"}, []PodGrant{podNPM()}); err != nil {
		t.Errorf("public go plus npm: %v", err)
	}
	if err := CheckPodRoutes([]string{"npm", "pypi"}, []PodGrant{podNPM()}); err == nil {
		t.Error("pypi listed without a grant was admitted")
	}
	if err := CheckPodRoutes([]string{"go"}, []PodGrant{podNPM()}); err == nil {
		t.Error("an npm grant not listed was admitted")
	}
	if err := CheckPodRoutes([]string{"go"}, nil); err == nil {
		t.Error("ecosystems without grants was admitted")
	}
}

// TestPropertyPodGrantsRoundTrip: for generated grant sets (any subset of
// the ecosystems, either policy, every switch its ecosystem allows, up to
// past MaxPodPackages entries), PodGrantOf validates, the encoding decodes
// to the same grants in ecosystem order, and the ecosystems list holds
// exactly the granted ecosystems (plus go for the public route) and
// agrees with them. Seeded so the gate is deterministic.
func TestPropertyPodGrantsRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20261015))
	for i := range 1000 {
		grants := drawGrants(rng)
		pod := make([]PodGrant, 0, len(grants))
		for _, g := range grants {
			pod = append(pod, PodGrantOf(g))
		}
		enc, err := EncodePodGrants(pod)
		if err != nil {
			t.Fatalf("iteration %d: encode %+v: %v", i, pod, err)
		}
		dec, err := DecodePodGrants(enc)
		if err != nil {
			t.Fatalf("iteration %d: decode %s: %v", i, enc, err)
		}
		if msg := sameGrants(dec, pod, grants); msg != "" {
			t.Fatalf("iteration %d: %s", i, msg)
		}
		eco, err := ParseEcosystems(EcosystemList(rng.Intn(2) == 0, dec))
		if err != nil || len(dec) > 0 && CheckPodRoutes(eco, dec) != nil {
			t.Fatalf("iteration %d: ecosystems %v (%v) disagree with %+v", i, eco, err, dec)
		}
	}
}

// drawGrants is a generated grant set: any subset of the ecosystems in any
// order, either policy, every switch its ecosystem allows, and allowlists
// up to past MaxPodPackages entries.
func drawGrants(rng *rand.Rand) []Grant {
	coin := func() bool { return rng.Intn(2) == 0 }
	pkg := map[string]func(int) string{
		EcosystemGo:   func(i int) string { return fmt.Sprintf("m%d.example/", i) },
		EcosystemNPM:  func(i int) string { return fmt.Sprintf("p%d", i) },
		EcosystemPyPI: func(i int) string { return fmt.Sprintf("p-%d", i) },
	}
	var grants []Grant
	for _, eco := range Ecosystems {
		if !coin() {
			continue
		}
		g := Grant{Name: "r-" + eco, Ecosystem: eco}
		if coin() {
			g.PackagePolicy = PackagePolicyRegistry
		} else {
			for n := range 1 + rng.Intn(MaxPodPackages+8) {
				g.Packages = append(g.Packages, pkg[eco](n))
			}
		}
		switch eco {
		case EcosystemGo:
			g.AllowPseudoVersions = coin()
			if coin() {
				g.PrivateModules = []string{"corp.example/"}
			}
		case EcosystemNPM:
			g.NPMScripts, g.AllowPrerelease = coin(), coin()
		case EcosystemPyPI:
			g.PipSdists = coin()
		}
		grants = append(grants, g)
	}
	rng.Shuffle(len(grants), func(a, b int) { grants[a], grants[b] = grants[b], grants[a] })
	return grants
}

// sameGrants judges a decoded set against what was encoded and the grants
// it came from, returning what is wrong.
func sameGrants(dec, pod []PodGrant, grants []Grant) string {
	if len(dec) != len(pod) {
		return fmt.Sprintf("%d grants back, want %d", len(dec), len(pod))
	}
	for _, d := range dec {
		var orig PodGrant
		for _, p := range pod {
			if p.Ecosystem == d.Ecosystem {
				orig = p
			}
		}
		if !reflect.DeepEqual(d, orig) {
			return fmt.Sprintf("%+v came back as %+v", orig, d)
		}
		if len(d.Packages)+d.MorePackages != len(grantNamed(grants, d.Registry).Packages) {
			return d.Registry + " lost count of its packages"
		}
	}
	return ""
}

func grantNamed(gs []Grant, name string) Grant {
	for _, g := range gs {
		if g.Name == name {
			return g
		}
	}
	return Grant{}
}
