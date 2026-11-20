// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsetup

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata")

// valid is an Input every rule admits; each case below changes one field.
func valid() Input {
	return Input{
		Name:       "overdub",
		Namespace:  "maul",
		IntentRepo: "overdubstudio/intents",
		Repos:      []string{"overdubstudio/app"},
		Approvers:  []string{"alice"},
	}
}

// TestPlanRefuses holds every rule the Project CRD enforces, each refused
// before anything is written, with the flag to fix named in the error.
func TestPlanRefuses(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	for _, tt := range []struct {
		name   string
		change func(*Input)
		want   string
	}{
		{"name too long", func(in *Input) { in.Name = long(26) }, "at most 25"},
		{"name not a DNS label", func(in *Input) { in.Name = "Over_dub" }, "project name"},
		{"name empty", func(in *Input) { in.Name = "" }, "project name"},
		{"namespace not a DNS label", func(in *Input) { in.Namespace = "Maul" }, "-n"},
		{"intent repository missing", func(in *Input) { in.IntentRepo = "" }, "--intent-repo"},
		{"intent repository one segment", func(in *Input) { in.IntentRepo = "intents" }, "--intent-repo"},
		{"intent repository three segments", func(in *Input) { in.IntentRepo = "a/b/c" }, "--intent-repo"},
		{"intent repository http", func(in *Input) { in.IntentRepo = "http://github.com/a/b" }, "--intent-repo"},
		{"intent repository with credentials", func(in *Input) {
			in.IntentRepo = "https://x@github.com/a/b"
		}, "--intent-repo"},
		{"intent repository deeper path", func(in *Input) {
			in.IntentRepo = "https://github.com/a/b/tree"
		}, "--intent-repo"},
		{"intent repository too long", func(in *Input) {
			in.IntentRepo = "https://github.com/a/" + long(240)
		}, "at most 256"},
		{"no repository", func(in *Input) { in.Repos = nil }, "--repo"},
		{"nine repositories", func(in *Input) {
			in.Repos = nil
			for _, r := range "abcdefghi" {
				in.Repos = append(in.Repos, "o/"+string(r))
			}
		}, "at most 8"},
		{"repository not a URL", func(in *Input) { in.Repos = []string{"app"} }, "--repo"},
		{"repository with a query", func(in *Input) { in.Repos = []string{"https://github.com/o/a?x"} }, "--repo"},
		{"default key too long", func(in *Input) {
			in.Repos = []string{"o/a-very-long-repository-name"}
		}, "write --repo <short>=owner/repo"},
		{"default key with a dot", func(in *Input) {
			in.Repos = []string{"o/my.app"}
		}, "write --repo <short>=owner/repo"},
		{"given key invalid", func(in *Input) { in.Repos = []string{"App=o/app"} }, "--repo"},
		{"given key too long", func(in *Input) { in.Repos = []string{long(17) + "=o/app"} }, "--repo"},
		{"duplicate key", func(in *Input) { in.Repos = []string{"o/app", "app=p/other"} }, "twice"},
		{"duplicate URL", func(in *Input) {
			in.Repos = []string{"o/app", "web=https://github.com/O/App.git"}
		}, "same repository"},
		{"no approver", func(in *Input) { in.Approvers = nil }, "--approver"},
		{"approver bot", func(in *Input) { in.Approvers = []string{"maul[bot]"} }, "--approver"},
		{"approver leading hyphen", func(in *Input) { in.Approvers = []string{"-alice"} }, "--approver"},
		{"approver too long", func(in *Input) { in.Approvers = []string{long(65)} }, "--approver"},
		{"approver twice", func(in *Input) { in.Approvers = []string{"alice", "Alice"} }, "twice"},
		{"21 approvers", func(in *Input) {
			in.Approvers = nil
			for i := range 21 {
				in.Approvers = append(in.Approvers, "u"+long(i+1))
			}
		}, "at most 20"},
		{"trigger label too long", func(in *Input) { in.TriggerLabel = long(51) }, "--trigger-label"},
		{"approve label too long", func(in *Input) { in.ApproveLabel = long(51) }, "--approve-label"},
		{"labels equal", func(in *Input) {
			in.TriggerLabel, in.ApproveLabel = "go", "Go"
		}, "differ"},
		{"trigger equals the default approve label", func(in *Input) {
			in.TriggerLabel = "maul:Approved"
		}, "differ"},
		{"derived trigger equals the approve label", func(in *Input) {
			in.ApproveLabel = "maul:overdub"
		}, "differ"},
		{"a Project named approved", func(in *Input) { in.Name = "approved" }, "--trigger-label"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := valid()
			tt.change(&in)
			_, err := Plan(in)
			if err == nil {
				t.Fatalf("Plan(%+v) = nil, want an error containing %q", in, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Plan(%+v) = %v, want it to contain %q", in, err, tt.want)
			}
		})
	}
}

// TestPlanExpands pins the shorthand: owner/repo is a github.com URL, a
// full https URL is kept, and a key defaults to the lowercased repository
// name.
func TestPlanExpands(t *testing.T) {
	for _, tt := range []struct {
		name  string
		repo  string
		want  Repository
		owner Account
	}{
		{"shorthand", "overdubstudio/App", Repository{Key: "app", URL: "https://github.com/overdubstudio/App"},
			Account{Host: "github.com", Owner: "overdubstudio"}},
		{"full URL", "https://ghe.example.com/Team/web",
			Repository{Key: "web", URL: "https://ghe.example.com/Team/web"},
			Account{Host: "ghe.example.com", Owner: "Team"}},
		{"key given", "ui=overdubstudio/frontend-web-application",
			Repository{Key: "ui", URL: "https://github.com/overdubstudio/frontend-web-application"},
			Account{Host: "github.com", Owner: "overdubstudio"}},
		{"key given on a URL", "api=https://github.com/overdubstudio/backend",
			Repository{Key: "api", URL: "https://github.com/overdubstudio/backend"},
			Account{Host: "github.com", Owner: "overdubstudio"}},
		{"git suffix", "overdubstudio/app.git", Repository{Key: "app", URL: "https://github.com/overdubstudio/app.git"},
			Account{Host: "github.com", Owner: "overdubstudio"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := valid()
			in.Repos = []string{tt.repo}
			p, err := Plan(in)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.Repositories[0]; got.Key != tt.want.Key || got.URL != tt.want.URL ||
				got.account() != tt.owner {
				t.Errorf("repository = %+v, want %+v of %+v", got, tt.want, tt.owner)
			}
		})
	}
}

// TestLabels: the effective labels are the ones given, else the trigger
// maul:<name> and the approve label the CRD defaults.
func TestLabels(t *testing.T) {
	in := valid()
	p, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Trigger() != "maul:overdub" || p.Approve() != "maul:approved" {
		t.Errorf("labels = %q, %q", p.Trigger(), p.Approve())
	}
	in.Name, in.TriggerLabel, in.ApproveLabel = "approved", "go", "ok"
	if p, err = Plan(in); err != nil {
		t.Fatal(err)
	}
	if p.Trigger() != "go" || p.Approve() != "ok" {
		t.Errorf("labels = %q, %q", p.Trigger(), p.Approve())
	}
	// GitHub's 50 is characters, as the schema's maxLength is: 50 of them
	// in 100 bytes are admitted.
	in.TriggerLabel = strings.Repeat("ö", 50)
	if _, err := Plan(in); err != nil {
		t.Errorf("a 50-character label of 100 bytes: %v", err)
	}
}

// TestOwners lists every account the App must be installed on once, the
// intent repository's first, compared case-insensitively.
func TestOwners(t *testing.T) {
	in := valid()
	in.IntentRepo = "Overdubstudio/intents"
	in.Repos = []string{"overdubstudio/app", "web=https://ghe.example.com/team/web", "lib=acme/lib",
		"ui=OVERDUBSTUDIO/ui"}
	p, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []Account{{"github.com", "Overdubstudio"}, {"ghe.example.com", "team"}, {"github.com", "acme"}}
	if got := p.Owners(); !reflect.DeepEqual(got, want) {
		t.Errorf("Owners() = %+v, want %+v", got, want)
	}
}

// TestRenderGolden pins the manifest byte for byte: only apiVersion, kind,
// metadata and spec, labels only when given.
func TestRenderGolden(t *testing.T) {
	multi := valid()
	multi.Name = "shop"
	multi.IntentRepo = "https://github.com/acme/intents"
	multi.Repos = []string{"acme/shop", "api=acme/backend-api", "https://github.com/acme/web"}
	multi.Approvers = []string{"alice", "bob"}
	multi.TriggerLabel, multi.ApproveLabel = "shop", "ship-it"
	for _, tt := range []struct {
		name string
		in   Input
	}{
		{"project-one", valid()},
		{"project-multi", multi},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Plan(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Render(p)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", tt.name+".yaml")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(got) != string(want) {
				t.Errorf("%s drifted:\n%s\nwant\n%s", path, got, want)
			}
			for _, absent := range []string{"status", "creationTimestamp"} {
				if strings.Contains(string(got), absent) {
					t.Errorf("the manifest holds %s", absent)
				}
			}
		})
	}
}
