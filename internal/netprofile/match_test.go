// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bitwise-media-group/patchy/internal/forge"
)

func mustSet(t *testing.T, ps ...Profile) Set {
	t.Helper()
	set, err := NewSet(ps)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func profileFor(name string, port int, host string, repos ...string) Profile {
	return Profile{Name: name, Route: RouteGo, Port: port, Host: host, Repositories: repos,
		Modules: Prefixes{"github.com/google/uuid"}}
}

func TestForExamples(t *testing.T) {
	set := mustSet(t,
		profileFor("go", 8090, "github.com", "devthenet-labs/patchy", "devthenet-labs/patchy-preview-demo"),
		profileFor("ghe", 8091, "ghe.example.com", "acme/.*"),
	)
	tests := []struct {
		name   string
		url    string
		want   string
		reason Reason
	}{
		{"exact", "https://github.com/devthenet-labs/patchy", "go", ""},
		{"anchored: no prefix match", "https://github.com/devthenet-labs/patchy-target", "", ReasonNoMatch},
		{"anchored: no suffix match", "https://github.com/xdevthenet-labs/patchy", "", ReasonNoMatch},
		{"second pattern", "https://github.com/devthenet-labs/patchy-preview-demo", "go", ""},
		{"case-insensitive owner/name", "https://github.com/DevThenet-Labs/Patchy", "go", ""},
		{"case-insensitive host", "https://GitHub.com/devthenet-labs/patchy", "go", ""},
		{".git suffix", "https://github.com/devthenet-labs/patchy.git", "go", ""},
		{"trailing slash", "https://github.com/devthenet-labs/patchy/", "go", ""},
		{"deeper path", "https://github.com/devthenet-labs/patchy/tree/main", "go", ""},
		{"host mismatch", "https://gitlab.com/devthenet-labs/patchy", "", ReasonNoMatch},
		{"host with a port is that host", "https://github.com:443/devthenet-labs/patchy", "go", ""},
		{"other host's profile", "https://ghe.example.com/acme/api", "ghe", ""},
		{"pattern does not span the slash", "https://ghe.example.com/acme-corp/api", "", ReasonNoMatch},
		{"not a URL", "devthenet-labs/patchy", "", ReasonBadURL},
		{"ssh scheme", "ssh://github.com/devthenet-labs/patchy", "", ReasonBadURL},
		{"owner only", "https://github.com/devthenet-labs", "", ReasonBadURL},
		{"empty", "", "", ReasonBadURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, reason := set.For(tt.url)
			if p.Name != tt.want || reason != tt.reason {
				t.Fatalf("For(%q) = %q, %q; want %q, %q", tt.url, p.Name, reason, tt.want, tt.reason)
			}
		})
	}
}

// TestForAmbiguous: a repository two profiles match gets neither.
func TestForAmbiguous(t *testing.T) {
	set := mustSet(t,
		profileFor("a", 8090, "github.com", "devthenet-labs/.*"),
		profileFor("b", 8091, "github.com", "devthenet-labs/patchy-target"),
	)
	if p, reason := set.For("https://github.com/devthenet-labs/patchy-target"); p.Name != "" || reason != ReasonAmbiguous {
		t.Fatalf("For = %q, %q; want no profile, %q", p.Name, reason, ReasonAmbiguous)
	}
	if p, reason := set.For("https://github.com/devthenet-labs/other"); p.Name != "a" || reason != "" {
		t.Fatalf("For = %q, %q; want a", p.Name, reason)
	}
}

func TestForEmptySet(t *testing.T) {
	var set Set
	if p, reason := set.For("https://github.com/a/b"); p.Name != "" || reason != ReasonNoMatch {
		t.Fatalf("zero Set: For = %q, %q", p.Name, reason)
	}
}

// TestParseRepoURLAgreesWithForge pins parseRepoURL to forge.ParseRepoURL,
// which it copies so this package can stay stdlib-only.
func TestParseRepoURLAgreesWithForge(t *testing.T) {
	corpus := []string{
		"https://github.com/devthenet-labs/patchy",
		"https://github.com/devthenet-labs/patchy.git",
		"https://github.com/devthenet-labs/patchy/",
		"https://github.com/devthenet-labs/patchy/tree/main",
		"https://GitHub.COM/DevThenet-Labs/Patchy",
		"http://github.com/a/b",
		"https://github.com:8443/a/b",
		"https://user:pass@github.com/a/b",
		"https://github.com/a/.git",
		"https://github.com/a/b.git.git",
		"https://github.com//b",
		"https://github.com/a",
		"https://github.com/",
		"https://github.com",
		"https:///a/b",
		"ssh://git@github.com/a/b",
		"git@github.com:a/b.git",
		"github.com/a/b",
		"",
		"  https://github.com/a/b  ",
		"https://github.com/a/b?x=1#frag",
		"https://github.com/a%2Fb/c",
		"https://[::1]/a/b",
		"https://ghe.example.com/org/repo",
		"%zz",
	}
	for _, raw := range corpus {
		host, owner, name, err := parseRepoURL(raw)
		fHost, fRepo, fErr := forge.ParseRepoURL(raw)
		if (err == nil) != (fErr == nil) {
			t.Errorf("%q: error = %v, forge error = %v", raw, err, fErr)
			continue
		}
		if err != nil {
			continue
		}
		if host != fHost || owner != fRepo.Owner || name != fRepo.Name {
			t.Errorf("%q: %s %s/%s, forge %s %s/%s", raw, host, owner, name, fHost, fRepo.Owner, fRepo.Name)
		}
	}
}

// patternPool draws literals and small regexes whose matches overlap, plus
// ones that can never match a lowercased subject.
var patternPool = []string{
	"o1/r1", "o1/r2", "o1/.*", "o2/r[0-9]", "o2/r1", "(o1|o2)/r1", "O1/R1", "o1/r", "o1/r1-x", ".*/r2", "o3/.+",
}

var ownerPool = []string{"o1", "O1", "o2", "o3", "o1x"}
var namePool = []string{"r1", "R1", "r2", "r", "r1-x", "r1.git", "r9"}
var hostPool = []string{"github.com", "ghe.example.com"}

// genSet is a set of 1-4 profiles over the pools.
type genSet struct{ profiles []Profile }

func (genSet) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(4)
	ps := make([]Profile, n)
	for i := range ps {
		repos := make([]string, 1+r.Intn(3))
		for j := range repos {
			repos[j] = patternPool[r.Intn(len(patternPool))]
		}
		ps[i] = profileFor(fmt.Sprintf("p%d", i), 8090+i, hostPool[r.Intn(len(hostPool))], repos...)
	}
	return reflect.ValueOf(genSet{profiles: ps})
}

// genURL is a repository URL over the pools, with case, scheme and suffix
// variants.
type genURL string

func (genURL) Generate(r *rand.Rand, _ int) reflect.Value {
	host := hostPool[r.Intn(len(hostPool))]
	if r.Intn(4) == 0 {
		host = strings.ToUpper(host)
	}
	scheme := []string{"https", "http"}[r.Intn(2)]
	suffix := []string{"", "/", "/tree/main"}[r.Intn(3)]
	u := fmt.Sprintf("%s://%s/%s/%s%s", scheme, host, ownerPool[r.Intn(len(ownerPool))],
		namePool[r.Intn(len(namePool))], suffix)
	return reflect.ValueOf(genURL(u))
}

// TestPropertyForIsSubset: For returns a profile only if that profile's host
// and one of its anchored patterns match, and only if no other profile
// does; when exactly one matches, it is returned.
func TestPropertyForIsSubset(t *testing.T) {
	prop := func(g genSet, u genURL) bool {
		set, err := NewSet(g.profiles)
		if err != nil {
			t.Logf("generated an invalid set: %v", err)
			return false
		}
		// The oracle, independent of matches(): the URL split by hand.
		rest := strings.SplitN(strings.SplitN(string(u), "://", 2)[1], "/", 4)
		host := strings.ToLower(rest[0])
		subject := strings.ToLower(rest[1] + "/" + strings.TrimSuffix(rest[2], ".git"))
		var matching []string
		for _, p := range g.profiles {
			if p.Host != host {
				continue
			}
			for _, pat := range p.Repositories {
				if regexp.MustCompile(`^(?:` + pat + `)$`).MatchString(subject) {
					matching = append(matching, p.Name)
					break
				}
			}
		}
		got, reason := set.For(string(u))
		switch len(matching) {
		case 0:
			return got.Name == "" && reason == ReasonNoMatch
		case 1:
			return got.Name == matching[0] && reason == ""
		default:
			return got.Name == "" && reason == ReasonAmbiguous
		}
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}
