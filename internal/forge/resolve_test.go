// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package forge

import (
	"errors"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

func mkForge(name, baseURL string, orgs, repos []string) v1alpha1.Forge {
	return v1alpha1.Forge{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ForgeSpec{
			Provider:     v1alpha1.ForgeProviderGitHub,
			BaseURL:      baseURL,
			SecretRef:    v1alpha1.LocalSecretReference{Name: "s"},
			Orgs:         orgs,
			Repositories: repos,
		},
	}
}

func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantHost  string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{"plain https", "https://github.com/acme/orders", "github.com", "acme", "orders", false},
		{"git suffix stripped", "https://github.com/acme/orders.git", "github.com", "acme", "orders", false},
		{"trailing path ignored", "https://github.com/acme/orders/pull/7", "github.com", "acme", "orders", false},
		{"host case folded", "https://GitHub.COM/acme/orders", "github.com", "acme", "orders", false},
		{"ghes host", "https://git.corp.example/acme/orders", "git.corp.example", "acme", "orders", false},
		{"whitespace trimmed", "  https://github.com/acme/orders  ", "github.com", "acme", "orders", false},
		{"ssh scheme rejected", "git@github.com:acme/orders.git", "", "", "", true},
		{"no owner", "https://github.com/orders", "", "", "", true},
		{"empty name after suffix", "https://github.com/acme/.git", "", "", "", true},
		{"empty string", "", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, repo, err := ParseRepoURL(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("ParseRepoURL(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
			}
			if err != nil {
				return
			}
			if host != c.wantHost || repo.Owner != c.wantOwner || repo.Name != c.wantName {
				t.Errorf("ParseRepoURL(%q) = %q %q/%q, want %q %q/%q",
					c.in, host, repo.Owner, repo.Name, c.wantHost, c.wantOwner, c.wantName)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	catchAll := mkForge("catch-all", "", nil, nil)
	acme := mkForge("acme", "", []string{"acme"}, nil)
	acmeServices := mkForge("acme-services", "", []string{"acme"}, []string{`^acme/svc-`})
	ghes := mkForge("corp", "https://git.corp.example", nil, nil)
	suspended := mkForge("paused", "", []string{"acme"}, nil)
	suspended.Spec.Suspend = true

	cases := []struct {
		name    string
		forges  []v1alpha1.Forge
		url     string
		want    string // winning forge name
		wantErr error
	}{
		{"single catch-all", []v1alpha1.Forge{catchAll}, "https://github.com/acme/orders", "catch-all", nil},
		{"org filter beats catch-all", []v1alpha1.Forge{catchAll, acme},
			"https://github.com/acme/orders", "acme", nil},
		{"org+repo beats org", []v1alpha1.Forge{catchAll, acme, acmeServices},
			"https://github.com/acme/svc-pay", "acme-services", nil},
		{"repo regex non-match falls back", []v1alpha1.Forge{catchAll, acmeServices},
			"https://github.com/acme/orders", "catch-all", nil},
		{"org filter is case-insensitive", []v1alpha1.Forge{acme}, "https://github.com/ACME/orders", "acme", nil},
		{"host mismatch no match", []v1alpha1.Forge{ghes}, "https://github.com/acme/orders", "", ErrNoMatch},
		{"ghes host matches", []v1alpha1.Forge{catchAll, ghes}, "https://git.corp.example/acme/orders", "corp", nil},
		{"suspended forge ignored", []v1alpha1.Forge{suspended}, "https://github.com/acme/orders", "", ErrNoMatch},
		{"equal specificity is ambiguous", []v1alpha1.Forge{acme, mkForge("acme2", "", []string{"acme"}, nil)},
			"https://github.com/acme/orders", "", ErrAmbiguous},
		{"no forges", nil, "https://github.com/acme/orders", "", ErrNoMatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(c.forges, c.url)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("Resolve() error = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.Forge.Name != c.want {
				t.Errorf("Resolve() forge = %q, want %q", got.Forge.Name, c.want)
			}
		})
	}
}

func TestResolveInvalidRegexNeverMatches(t *testing.T) {
	bad := mkForge("bad-regex", "", nil, []string{`^(unclosed`})
	if _, err := Resolve([]v1alpha1.Forge{bad}, "https://github.com/acme/orders"); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("Resolve(bad regex) error = %v, want ErrNoMatch", err)
	}
}

// TestWebHosts: a Forge's host, plus the web host above an api. host
// (GitHub Enterprise Cloud with data residency); a GHES base URL is its web
// host already.
func TestWebHosts(t *testing.T) {
	for _, c := range []struct {
		name, base string
		want       []string
	}{
		{"empty", "", []string{"github.com"}},
		{"ghes url", "https://ghe.corp/api/v3", []string{"ghe.corp"}},
		{"ghes bare", "ghe.corp", []string{"ghe.corp"}},
		{"data residency", "https://api.octo.ghe.com", []string{"api.octo.ghe.com", "octo.ghe.com"}},
		{"uppercase", "https://API.Octo.GHE.com", []string{"api.octo.ghe.com", "octo.ghe.com"}},
		{"trailing slash", "https://api.octo.ghe.com/", []string{"api.octo.ghe.com", "octo.ghe.com"}},
		{"api.github.com", "https://api.github.com", []string{"api.github.com", "github.com"}},
		{"single label below api", "https://api.corp", []string{"api.corp"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := mkForge("f", c.base, nil, nil)
			if got := WebHosts(&f); !slices.Equal(got, c.want) {
				t.Errorf("WebHosts(baseURL=%q) = %v, want %v", c.base, got, c.want)
			}
		})
	}
}

func TestHost(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{"default github", "", "github.com"},
		{"bare host", "git.corp.example", "git.corp.example"},
		{"url form", "https://git.corp.example", "git.corp.example"},
		{"url with path", "https://git.corp.example/api/v3", "git.corp.example"},
		{"case folded", "Git.CORP.example", "git.corp.example"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := mkForge("f", c.base, nil, nil)
			if got := Host(&f); got != c.want {
				t.Errorf("Host(baseURL=%q) = %q, want %q", c.base, got, c.want)
			}
		})
	}
}

func FuzzParseRepoURL(f *testing.F) {
	f.Add("https://github.com/acme/orders")
	f.Add("https://github.com/acme/orders.git")
	f.Add("http://a/b/c")
	f.Add("git@github.com:acme/orders.git")
	f.Add("https://github.com//orders")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		host, repo, err := ParseRepoURL(s) // must not panic on any input
		if err != nil {
			return
		}
		if host == "" || repo.Owner == "" || repo.Name == "" {
			t.Errorf("ParseRepoURL(%q) accepted empty components: host=%q owner=%q name=%q",
				s, host, repo.Owner, repo.Name)
		}
	})
}
