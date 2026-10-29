// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The intent kinds' schema tests. They run inside TestSchemaValidation's
// single envtest boot (schema_envtest_test.go), each against its own object
// names in the default namespace.

var (
	schemaNow    = metav1.NewTime(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	schemaDigest = "sha256:" + strings.Repeat("d", 64)
	schemaSHA    = strings.Repeat("a", 40)
)

// sameJSON compares two values by their JSON encoding. metav1.Time encodes
// as RFC 3339 UTC, so a value read back from the API server (in local time)
// compares equal to the one written — and a field the generated schema
// pruned shows up as a difference.
func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	if string(g) != string(w) {
		t.Errorf("%s round-tripped as\n  %s\nwant\n  %s", what, g, w)
	}
}

func schemaProject(name string) *maulv1.Project {
	return &maulv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: maulv1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        maulv1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories: []maulv1.ProjectRepository{
				{Name: "shop", URL: "https://github.com/acme/shop"},
			},
		},
	}
}

// testProjectSchema exercises the Project schema: the defaults a minimal
// Project is completed with, every bound at and past its edge, the label
// rule, the name rule, the immutable intent repository, and a status
// round-trip.
func testProjectSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	testProjectDefaults(ctx, t, c)
	testProjectBounds(ctx, t, c)
	testProjectPreviews(ctx, t, c)
	testProjectUpdates(ctx, t, c)
	testProjectStatus(ctx, t, c)
}

// schemaPreview is a valid per-repository preview at `path` ("" omits it).
func schemaPreview(leaf, path string) *maulv1.ProjectRepositoryPreview {
	return &maulv1.ProjectRepositoryPreview{
		ProjectPreview: maulv1.ProjectPreview{
			ImageRepository: "registry.example/maul/previews/" + leaf, Port: 8080, ReadinessPath: "/healthz",
		},
		Path: path,
	}
}

// previewedRepos is n repositories, each previewed at /p<i> (the first at /).
func previewedRepos(n int) []maulv1.ProjectRepository {
	out := make([]maulv1.ProjectRepository, n)
	for i := range out {
		path := fmt.Sprintf("/p%d", i)
		if i == 0 {
			path = "/"
		}
		out[i] = maulv1.ProjectRepository{
			Name: fmt.Sprintf("app%d", i), URL: fmt.Sprintf("https://github.com/acme/app%d", i),
			Preview: schemaPreview(fmt.Sprintf("app%d", i), path),
		}
	}
	return out
}

// testProjectPreviews exercises the per-repository previews (slice 3): at
// most four, at distinct paths in the path grammar, an omitted path
// defaulted to "/", and the spec.preview shorthand only for a lone
// repository with no per-repository preview. Repository names are free: a
// mixed-case, punctuated one needs only a short key and a lowercase leaf.
func testProjectPreviews(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	shorthand := &maulv1.ProjectPreview{
		ImageRepository: "registry.example/maul/previews/shop", Port: 8080, ReadinessPath: "/healthz",
	}
	tests := []struct {
		name    string
		mutate  func(*maulv1.Project)
		wantErr bool
	}{
		{"the shorthand on one repository", func(p *maulv1.Project) { p.Spec.Preview = shorthand }, false},
		{"a freely named repository previewed by its key", func(p *maulv1.Project) {
			p.Spec.Repositories = []maulv1.ProjectRepository{{
				Name: "web", URL: "https://github.com/acme/Acme.Web_App", Preview: schemaPreview("acme-web", "/"),
			}}
		}, false},
		{"two previewed repositories and a library", func(p *maulv1.Project) {
			p.Spec.Repositories = append(previewedRepos(2),
				maulv1.ProjectRepository{Name: "lib", URL: "https://github.com/acme/lib"})
		}, false},
		{"four previewed repositories", func(p *maulv1.Project) { p.Spec.Repositories = previewedRepos(4) }, false},
		{"four previewed of eight", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(8)
			for i := 4; i < 8; i++ {
				p.Spec.Repositories[i].Preview = nil
			}
		}, false},
		{"five previewed repositories", func(p *maulv1.Project) { p.Spec.Repositories = previewedRepos(5) }, true},
		{"a nested path", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(2)
			p.Spec.Repositories[1].Preview.Path = "/api/v1"
		}, false},
		{"two repositories at one path", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(2)
			p.Spec.Repositories[1].Preview.Path = "/"
		}, true},
		// An omitted path is the default "/", so it collides with one.
		{"an omitted path beside /", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(2)
			p.Spec.Repositories[1].Preview.Path = ""
		}, true},
		{"an uppercase path", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(2)
			p.Spec.Repositories[1].Preview.Path = "/API"
		}, true},
		{"a relative path", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(2)
			p.Spec.Repositories[1].Preview.Path = "api"
		}, true},
		{"a trailing slash", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(2)
			p.Spec.Repositories[1].Preview.Path = "/api/"
		}, true},
		{"an empty segment", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(2)
			p.Spec.Repositories[1].Preview.Path = "/api//v1"
		}, true},
		{"a path with a query", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(2)
			p.Spec.Repositories[1].Preview.Path = "/api?x=1"
		}, true},
		// The image prefix is the operator's: the schema checks the shape
		// and the leaf, and the preview-controller and the slot admission
		// policy hold the repository to the configured prefix (an
		// agent-environment repository included). The leaf is a DNS label,
		// which the schema did not require before.
		{"a per-repository image under an operator's own prefix", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(1)
			p.Spec.Repositories[0].Preview.ImageRepository = "registry.example/acme/previews/app0"
		}, false},
		{"a per-repository image leaf ending in a hyphen", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(1)
			p.Spec.Repositories[0].Preview.ImageRepository = "registry.example/maul/previews/app-"
		}, true},
		{"a per-repository image leaf starting with a hyphen", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(1)
			p.Spec.Repositories[0].Preview.ImageRepository = "registry.example/maul/previews/-app"
		}, true},
		{"a per-repository image with no path segment", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(1)
			p.Spec.Repositories[0].Preview.ImageRepository = "registry.example/app0"
		}, true},
		{"a per-repository image with a tag", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(1)
			p.Spec.Repositories[0].Preview.ImageRepository = "registry.example/maul/previews/app0:latest"
		}, true},
		{"the shorthand with a leaf ending in a hyphen", func(p *maulv1.Project) {
			bad := *shorthand
			bad.ImageRepository = "registry.example/acme/previews/demo-"
			p.Spec.Preview = &bad
		}, true},
		{"the shorthand beside a per-repository preview", func(p *maulv1.Project) {
			p.Spec.Repositories = previewedRepos(1)
			p.Spec.Preview = shorthand
		}, true},
		{"the shorthand with two repositories", func(p *maulv1.Project) {
			p.Spec.Repositories = append(p.Spec.Repositories,
				maulv1.ProjectRepository{Name: "lib", URL: "https://github.com/acme/lib"})
			p.Spec.Preview = shorthand
		}, true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := schemaProject(fmt.Sprintf("proj-previews-%d", i))
			tt.mutate(p)
			err := c.Create(ctx, p)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(project: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}

	t.Run("an omitted path defaults to /", func(t *testing.T) {
		p := schemaProject("proj-preview-default")
		p.Spec.Repositories = previewedRepos(2)
		p.Spec.Repositories[0].Preview.Path = ""
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatalf("Get(project) = %v", err)
		}
		if path := got.Spec.Repositories[0].Preview.Path; path != "/" {
			t.Errorf("defaulted path = %q, want /", path)
		}
	})

	// A legacy Project cannot grow a second repository while it keeps the
	// shorthand: the operator moves it to repositories[0].preview first.
	t.Run("a shorthand project gains a repository", func(t *testing.T) {
		p := schemaProject("proj-preview-grow")
		p.Spec.Preview = shorthand
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatalf("Get(project) = %v", err)
		}
		got.Spec.Repositories = append(got.Spec.Repositories,
			maulv1.ProjectRepository{Name: "lib", URL: "https://github.com/acme/lib"})
		if err := c.Update(ctx, got); err == nil {
			t.Error("Update(shorthand with two repositories) = nil, want rejection")
		}
		got.Spec.Repositories[0].Preview = &maulv1.ProjectRepositoryPreview{ProjectPreview: *shorthand}
		got.Spec.Preview = nil
		if err := c.Update(ctx, got); err != nil {
			t.Errorf("Update(moved to repositories[0].preview) = %v, want nil", err)
		}
	})
}

// testProjectUpdates: the intent repository is immutable — an Intent is
// named <project>-<issue>, leaving the repository out, so a moved Project's
// new issues would collide with the old repository's Intents — while the
// rest of the spec stays editable.
func testProjectUpdates(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	p := schemaProject("proj-updates")
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("Create(project) = %v", err)
	}
	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.ProjectSpec)
		wantErr bool
	}{
		{"the intent repository moved", func(s *maulv1.ProjectSpec) {
			s.IntentRepository = "https://github.com/acme/shop-intents"
		}, true},
		// No normalisation: even a change of case is a change.
		{"the intent repository recased", func(s *maulv1.ProjectSpec) {
			s.IntentRepository = "https://github.com/Acme/Intents"
		}, true},
		{"the approvers", func(s *maulv1.ProjectSpec) {
			s.Approvers.Logins = []string{"octocat", "hubot"}
		}, false},
		{"the repositories", func(s *maulv1.ProjectSpec) {
			s.Repositories[0].URL = "https://github.com/acme/store"
		}, false},
		{"the limits", func(s *maulv1.ProjectSpec) { s.Limits.MaxActiveIntents = 5 }, false},
		{"suspend", func(s *maulv1.ProjectSpec) { s.Suspend = true }, false},
		// A Project may change harness: each run records its own.
		{"the harness", func(s *maulv1.ProjectSpec) { s.Harness = maulv1.ProjectHarnessCodex }, false},
	} {
		t.Run("update "+tt.name, func(t *testing.T) {
			got := &maulv1.Project{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
				t.Fatalf("Get(project) = %v", err)
			}
			tt.mutate(&got.Spec)
			if err := c.Update(ctx, got); (err != nil) != tt.wantErr {
				t.Errorf("Update(project: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
	got := &maulv1.Project{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
		t.Fatalf("Get(project) = %v", err)
	}
	if got.Spec.IntentRepository != p.Spec.IntentRepository {
		t.Errorf("intentRepository = %q after the refused updates, want %q",
			got.Spec.IntentRepository, p.Spec.IntentRepository)
	}
}

// testProjectDefaults: a minimal Project is completed with the design's
// defaults, and an explicit false survives defaulting.
func testProjectDefaults(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	t.Run("a minimal project is defaulted", func(t *testing.T) {
		if err := c.Create(ctx, schemaProject("proj-minimal")); err != nil {
			t.Fatalf("Create(minimal project) = %v, want nil", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKey{Name: "proj-minimal", Namespace: "default"}, got); err != nil {
			t.Fatalf("Get(project) = %v", err)
		}
		s := got.Spec
		if s.Labels.Approve != maulv1.DefaultApproveLabel || s.Labels.Trigger != "" {
			t.Errorf("labels = %+v, want approve %q and trigger left to the controller", s.Labels, maulv1.DefaultApproveLabel)
		}
		// The exported defaults and the schema's default markers must agree.
		revisions, checkFixes := maulv1.DefaultMaxRevisions, maulv1.DefaultMaxCheckFixes
		wantLimits := maulv1.ProjectLimits{
			MaxActiveIntents: maulv1.DefaultMaxActiveIntents,
			MaxRevisions:     &revisions,
			MaxCheckFixes:    &checkFixes,
			MaxCostMicroUSD:  maulv1.DefaultMaxCostMicroUSD,
		}
		sameJSON(t, "defaulted limits", s.Limits, wantLimits)
		if s.RequireRepositoryImage == nil || !*s.RequireRepositoryImage {
			t.Errorf("requireRepositoryImage = %v, want true", s.RequireRepositoryImage)
		}
		if s.Checks.Timeout == nil || s.Checks.Timeout.Duration != 30*time.Minute || len(s.Checks.Fix) != 0 {
			t.Errorf("checks = %+v, want timeout 30m and no auto-fixed checks", s.Checks)
		}
	})

	t.Run("explicit zero and false values survive defaulting", func(t *testing.T) {
		p := schemaProject("proj-zeroes")
		off, zero := false, int32(0)
		p.Spec.RequireRepositoryImage = &off
		p.Spec.Limits.MaxRevisions = &zero
		p.Spec.Limits.MaxCheckFixes = &zero
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v, want nil", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatalf("Get(project) = %v", err)
		}
		if got.Spec.RequireRepositoryImage == nil || *got.Spec.RequireRepositoryImage {
			t.Errorf("requireRepositoryImage = %v, want false", got.Spec.RequireRepositoryImage)
		}
		l := got.Spec.Limits
		if l.MaxRevisions == nil || *l.MaxRevisions != 0 || l.MaxCheckFixes == nil || *l.MaxCheckFixes != 0 {
			t.Errorf("limits = %+v, want maxRevisions and maxCheckFixes kept at 0", l)
		}
	})
}

// testProjectBounds creates one Project per case, each a valid Project with
// one field moved to (accepted) or just past (rejected) its bound.
func testProjectBounds(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	repos := func(n int) []maulv1.ProjectRepository {
		out := make([]maulv1.ProjectRepository, n)
		for i := range out {
			out[i] = maulv1.ProjectRepository{
				Name: fmt.Sprintf("app%d", i), URL: fmt.Sprintf("https://github.com/acme/app%d", i),
			}
		}
		return out
	}
	logins := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("user%d", i)
		}
		return out
	}
	tests := []struct {
		name    string
		mutate  func(*maulv1.Project)
		wantErr bool
	}{
		{"eight repositories", func(p *maulv1.Project) { p.Spec.Repositories = repos(8) }, false},
		{"nine repositories", func(p *maulv1.Project) { p.Spec.Repositories = repos(9) }, true},
		{"no repositories", func(p *maulv1.Project) { p.Spec.Repositories = nil }, true},
		{"duplicate repository key", func(p *maulv1.Project) {
			p.Spec.Repositories = append(repos(1),
				maulv1.ProjectRepository{Name: "app0", URL: "https://github.com/acme/other"})
		}, true},
		// One entry per repository: pull requests are keyed by URL, and
		// every repository shares the intent branch.
		{"two keys for one repository url", func(p *maulv1.Project) {
			p.Spec.Repositories = append(repos(1),
				maulv1.ProjectRepository{Name: "again", URL: "https://github.com/acme/app0"})
		}, true},
		{"two repository urls differing only in case", func(p *maulv1.Project) {
			p.Spec.Repositories = append(repos(1),
				maulv1.ProjectRepository{Name: "again", URL: "https://GitHub.com/Acme/App0"})
		}, true},
		{"a repository url and its .git form", func(p *maulv1.Project) {
			p.Spec.Repositories = append(repos(1),
				maulv1.ProjectRepository{Name: "again", URL: "https://github.com/acme/app0.git"})
		}, true},
		{"a repository whose name embeds .git", func(p *maulv1.Project) {
			p.Spec.Repositories = append(repos(1),
				maulv1.ProjectRepository{Name: "pages", URL: "https://github.com/acme/app0.github.io"})
		}, false},
		{"repository key not a DNS label", func(p *maulv1.Project) { p.Spec.Repositories[0].Name = "Shop" }, true},
		{"harness claude", func(p *maulv1.Project) { p.Spec.Harness = maulv1.ProjectHarnessClaude }, false},
		{"harness codex", func(p *maulv1.Project) { p.Spec.Harness = maulv1.ProjectHarnessCodex }, false},
		{"harness not in the enum", func(p *maulv1.Project) { p.Spec.Harness = "copilot" }, true},
		{"repository key at the name budget", func(p *maulv1.Project) {
			p.Spec.Repositories[0].Name = strings.Repeat("a", maulv1.MaxRepositoryKeyLength)
		}, false},
		{"repository key past the name budget", func(p *maulv1.Project) {
			p.Spec.Repositories[0].Name = strings.Repeat("a", maulv1.MaxRepositoryKeyLength+1)
		}, true},
		{"plain http repository", func(p *maulv1.Project) {
			p.Spec.Repositories[0].URL = "http://github.com/acme/shop"
		}, true},
		{"credentials in the repository url", func(p *maulv1.Project) {
			p.Spec.Repositories[0].URL = "https://x:token@github.com/acme/shop"
		}, true},
		{"repository url past owner/name", func(p *maulv1.Project) {
			p.Spec.Repositories[0].URL = "https://github.com/acme/shop/tree/main"
		}, true},
		{"intent repository not a url", func(p *maulv1.Project) { p.Spec.IntentRepository = "acme/intents" }, true},
		{"twenty approvers", func(p *maulv1.Project) { p.Spec.Approvers.Logins = logins(20) }, false},
		{"twenty-one approvers", func(p *maulv1.Project) { p.Spec.Approvers.Logins = logins(21) }, true},
		{"no approvers", func(p *maulv1.Project) { p.Spec.Approvers.Logins = nil }, true},
		{"duplicate approver", func(p *maulv1.Project) { p.Spec.Approvers.Logins = []string{"octocat", "octocat"} }, true},
		{"approver with a space", func(p *maulv1.Project) { p.Spec.Approvers.Logins = []string{"octo cat"} }, true},
		{"a bot approver", func(p *maulv1.Project) { p.Spec.Approvers.Logins = []string{"dependabot[bot]"} }, true},
		{"an EMU approver", func(p *maulv1.Project) { p.Spec.Approvers.Logins = []string{"octocat_acme"} }, false},
		{"trigger equals approve", func(p *maulv1.Project) {
			p.Spec.Labels = maulv1.ProjectLabels{Trigger: "maul:go", Approve: "maul:go"}
		}, true},
		{"trigger equals the defaulted approve label, case-insensitively", func(p *maulv1.Project) {
			p.Spec.Labels = maulv1.ProjectLabels{Trigger: "Maul:Approved"}
		}, true},
		// The derived trigger (maul:<name>) must not be the approve label.
		{"a project named approved with a derived trigger", func(p *maulv1.Project) { p.Name = "approved" }, true},
		{"a project named approved with its own trigger", func(p *maulv1.Project) {
			p.Name, p.Spec.Labels.Trigger = "approved", "maul:target"
		}, false},
		{"a derived trigger equal to a custom approve label, case-insensitively", func(p *maulv1.Project) {
			p.Name, p.Spec.Labels.Approve = "ship", "Maul:Ship"
		}, true},
		{"a derived trigger apart from a custom approve label", func(p *maulv1.Project) {
			p.Name, p.Spec.Labels.Approve = "ship", "maul:ok"
		}, false},
		{"a 50-character label", func(p *maulv1.Project) { p.Spec.Labels.Trigger = strings.Repeat("l", 50) }, false},
		{"a 51-character label", func(p *maulv1.Project) { p.Spec.Labels.Trigger = strings.Repeat("l", 51) }, true},
		{"cost ceiling at its cap", func(p *maulv1.Project) { p.Spec.Limits.MaxCostMicroUSD = 1000000000 }, false},
		{"cost ceiling past its cap", func(p *maulv1.Project) { p.Spec.Limits.MaxCostMicroUSD = 1000000001 }, true},
		{"negative cost ceiling", func(p *maulv1.Project) { p.Spec.Limits.MaxCostMicroUSD = -1 }, true},
		{"twenty revisions", func(p *maulv1.Project) { n := int32(20); p.Spec.Limits.MaxRevisions = &n }, false},
		{"twenty-one revisions", func(p *maulv1.Project) { n := int32(21); p.Spec.Limits.MaxRevisions = &n }, true},
		{"negative check fixes", func(p *maulv1.Project) { n := int32(-1); p.Spec.Limits.MaxCheckFixes = &n }, true},
		{"twenty-one active intents", func(p *maulv1.Project) { p.Spec.Limits.MaxActiveIntents = 21 }, true},
		{"stage turns past the bound", func(p *maulv1.Project) { p.Spec.Limits.Build.MaxTurns = 1001 }, true},
		{"stage tokens past the bound", func(p *maulv1.Project) { p.Spec.Limits.Revise.TokenBudget = 100000001 }, true},
		// A stage's cost budget fits under the per-intent ceiling, and its
		// timeout is between a minute and four hours (LF2).
		{"stage cost at the intent ceiling", func(p *maulv1.Project) {
			p.Spec.Limits.Build.MaxCostMicroUSD = maulv1.DefaultMaxCostMicroUSD
		}, false},
		{"stage cost past the intent ceiling", func(p *maulv1.Project) {
			p.Spec.Limits.Revise.MaxCostMicroUSD = maulv1.DefaultMaxCostMicroUSD + 1
		}, true},
		{"plan cost past the intent ceiling", func(p *maulv1.Project) {
			p.Spec.Limits.Plan.MaxCostMicroUSD = maulv1.DefaultMaxCostMicroUSD + 1
		}, true},
		{"stage cost under a raised intent ceiling", func(p *maulv1.Project) {
			p.Spec.Limits.MaxCostMicroUSD, p.Spec.Limits.Build.MaxCostMicroUSD = 100000000, 60000000
		}, false},
		{"stage cost past the bound", func(p *maulv1.Project) {
			p.Spec.Limits.MaxCostMicroUSD, p.Spec.Limits.Build.MaxCostMicroUSD = 1000000000, 1000000001
		}, true},
		{"negative stage cost", func(p *maulv1.Project) { p.Spec.Limits.Build.MaxCostMicroUSD = -1 }, true},
		// The evaluate and check review stages are bounded like the others.
		{"evaluate limits", func(p *maulv1.Project) {
			p.Spec.Limits.Evaluate = maulv1.StageLimits{MaxTurns: 80, TokenBudget: 400000,
				MaxCostMicroUSD: maulv1.DefaultMaxCostMicroUSD, Timeout: &metav1.Duration{Duration: time.Hour}}
		}, false},
		{"check review limits", func(p *maulv1.Project) {
			p.Spec.Limits.CheckReview = maulv1.StageLimits{MaxTurns: 20, TokenBudget: 100000,
				MaxCostMicroUSD: maulv1.DefaultMaxCostMicroUSD, Timeout: &metav1.Duration{Duration: time.Minute}}
		}, false},
		{"evaluate cost past the intent ceiling", func(p *maulv1.Project) {
			p.Spec.Limits.Evaluate.MaxCostMicroUSD = maulv1.DefaultMaxCostMicroUSD + 1
		}, true},
		{"check review cost past the intent ceiling", func(p *maulv1.Project) {
			p.Spec.Limits.CheckReview.MaxCostMicroUSD = maulv1.DefaultMaxCostMicroUSD + 1
		}, true},
		{"evaluate turns past the bound", func(p *maulv1.Project) { p.Spec.Limits.Evaluate.MaxTurns = 1001 }, true},
		{"check review timeout over four hours", func(p *maulv1.Project) {
			p.Spec.Limits.CheckReview.Timeout = &metav1.Duration{Duration: 4*time.Hour + time.Second}
		}, true},
		{"stage timeout of a minute", func(p *maulv1.Project) {
			p.Spec.Limits.Plan.Timeout = &metav1.Duration{Duration: time.Minute}
		}, false},
		{"stage timeout of four hours", func(p *maulv1.Project) {
			p.Spec.Limits.Build.Timeout = &metav1.Duration{Duration: 4 * time.Hour}
		}, false},
		{"stage timeout under a minute", func(p *maulv1.Project) {
			p.Spec.Limits.Build.Timeout = &metav1.Duration{Duration: 59 * time.Second}
		}, true},
		{"stage timeout over four hours", func(p *maulv1.Project) {
			p.Spec.Limits.Revise.Timeout = &metav1.Duration{Duration: 4*time.Hour + time.Second}
		}, true},
		{"thirty-three fixed checks", func(p *maulv1.Project) {
			p.Spec.Checks.Fix = logins(33)
		}, true},
		{"check timeout under a minute", func(p *maulv1.Project) {
			p.Spec.Checks.Timeout = &metav1.Duration{Duration: 30 * time.Second}
		}, true},
		{"check timeout over six hours", func(p *maulv1.Project) {
			p.Spec.Checks.Timeout = &metav1.Duration{Duration: 7 * time.Hour}
		}, true},
		{"check timeout of one hour", func(p *maulv1.Project) {
			p.Spec.Checks.Timeout = &metav1.Duration{Duration: time.Hour}
		}, false},
		{"re-running failed checks", func(p *maulv1.Project) {
			p.Spec.Checks.Fix, p.Spec.Checks.RerunFailed = []string{"test"}, true
		}, false},
		// A repository's resource class names one of the operator's
		// classes: a DNS label, as resourceclass.ValidName holds it.
		{"a resource class", func(p *maulv1.Project) { p.Spec.Repositories[0].AgentResourceClass = "large" }, false},
		{"a resource class at its length", func(p *maulv1.Project) {
			p.Spec.Repositories[0].AgentResourceClass = strings.Repeat("c", 32)
		}, false},
		{"a resource class past its length", func(p *maulv1.Project) {
			p.Spec.Repositories[0].AgentResourceClass = strings.Repeat("c", 33)
		}, true},
		{"a resource class not a DNS label", func(p *maulv1.Project) {
			p.Spec.Repositories[0].AgentResourceClass = "Large"
		}, true},
		{"a resource class with a dot", func(p *maulv1.Project) {
			p.Spec.Repositories[0].AgentResourceClass = "large.2"
		}, true},
		// The name budget: every Intent and IntentRun name derived from the
		// Project's must fit in a label value.
		{"a name at the name budget", func(p *maulv1.Project) {
			p.Name = strings.Repeat("p", maulv1.MaxProjectNameLength)
		}, false},
		{"a name past the name budget", func(p *maulv1.Project) {
			p.Name = strings.Repeat("p", maulv1.MaxProjectNameLength+1)
		}, true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := schemaProject(fmt.Sprintf("proj-bounds-%d", i))
			tt.mutate(p)
			err := c.Create(ctx, p)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(project: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
	t.Run("a repository's resource class round-trips", func(t *testing.T) {
		p := schemaProject("proj-resource-class")
		p.Spec.Repositories = append(p.Spec.Repositories,
			maulv1.ProjectRepository{Name: "web", URL: "https://github.com/acme/web", AgentResourceClass: "large"})
		if err := c.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		var got maulv1.Project
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "repositories", got.Spec.Repositories, p.Spec.Repositories)
	})
}

// testProjectStatus writes a populated status through the subresource and
// reads it back, so a field the generated schema prunes fails here.
func testProjectStatus(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	t.Run("status round-trips and is bounded", func(t *testing.T) {
		p := schemaProject("proj-status")
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v", err)
		}
		want := maulv1.ProjectStatus{
			Conditions: []metav1.Condition{{
				Type: maulv1.ConditionReady, Status: metav1.ConditionFalse, ObservedGeneration: 1,
				LastTransitionTime: schemaNow, Reason: maulv1.ReasonAmbiguousIntentRepository,
				Message: "project other shares https://github.com/acme/intents with the same trigger",
			}},
			ObservedGeneration: 1,
			ActiveIntents:      2,
			LastPolledAt:       schemaNow.DeepCopy(),
		}
		p.Status = *want.DeepCopy()
		if err := c.Status().Update(ctx, p); err != nil {
			t.Fatalf("Status().Update(project) = %v, want nil", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatalf("Get(project) = %v", err)
		}
		sameJSON(t, "project status", got.Status, want)
		got.Status.ActiveIntents = -1
		if err := c.Status().Update(ctx, got); err == nil {
			t.Error("Status().Update(activeIntents=-1) = nil, want minimum rejection")
		}
	})
}

// schemaIntent is a valid Intent for issue `issue` of Project target, named
// as the schema requires (<project>-<issue>).
func schemaIntent(issue int64) *maulv1.Intent {
	return &maulv1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: maulv1.IntentName("target", issue), Namespace: "default"},
		Spec: maulv1.IntentSpec{
			Project: "target",
			Issue: maulv1.IntentIssue{
				Repository: "https://github.com/acme/intents",
				Number:     issue,
				URL:        fmt.Sprintf("https://github.com/acme/intents/issues/%d", issue),
			},
			RequestedBy: maulv1.IntentRequest{Login: "octocat", At: schemaNow, EventID: 42},
		},
	}
}

// fullIntentStatus populates every Intent status field at a legal value.
func fullIntentStatus() maulv1.IntentStatus {
	return maulv1.IntentStatus{
		Phase: maulv1.IntentInReview,
		PhaseTimes: []maulv1.IntentPhaseTime{
			{Phase: maulv1.IntentPending, At: schemaNow},
			{Phase: maulv1.IntentInReview, At: schemaNow},
		},
		Conditions: []metav1.Condition{{
			Type: maulv1.ConditionRevisionLimitReached, Status: metav1.ConditionFalse,
			LastTransitionTime: schemaNow, Reason: "WithinLimit", Message: "1 of 3 revisions",
		}},
		ObservedGeneration: 1,
		Input: &maulv1.IntentInput{Revision: 1, Digest: schemaDigest, ConfigMap: "target-1-input-r1",
			ContextDigest: schemaDigest},
		Plan: &maulv1.IntentPlan{
			Revision: 1, Digest: schemaDigest, ConfigMap: "target-1-plan-r1",
			CommentID: 1001, CommentDigest: schemaDigest, PostedAt: schemaNow.DeepCopy(),
			Summary: "Add GET /version", Repositories: []string{"https://github.com/acme/shop"},
		},
		Approval: &maulv1.IntentApproval{
			By: "octocat", Source: maulv1.IntentActionLabel, EventID: 43, At: schemaNow, PlanRevision: 1,
			PlanDigest: schemaDigest, InputDigest: schemaDigest,
		},
		LastTrigger: &maulv1.IntentAction{
			Source: maulv1.IntentActionCommand, EventID: 44, Login: "octocat", At: schemaNow,
		},
		Commands: &maulv1.IntentCommands{
			Seen:          &maulv1.IntentCommentRef{ID: 45, At: schemaNow},
			RefusedActors: []int64{583231, 7},
		},
		Branch: "maul-intent/target-1",
		PullRequests: []maulv1.IntentPullRequest{{
			Repository: "https://github.com/acme/shop", Number: 7,
			URL: "https://github.com/acme/shop/pull/7", NodeID: "PR_kwDOAbCdEf", HeadSHA: schemaSHA,
			State: "merged", MergedAt: schemaNow.DeepCopy(), MergeCommitSHA: schemaSHA,
			ChecksObservedHeadSHA: schemaSHA, ChecksObservedProjectGeneration: 3,
			ChecksRerun: &maulv1.IntentChecksRerun{HeadSHA: schemaSHA, CheckRunIDs: []int64{71, 72},
				Checks: []string{"lint", "test"}, WorkflowRunIDs: []int64{81}, RequestedAt: schemaNow},
			PreviewCommentID: 1002, PreviewDigest: schemaDigest,
			AcceptanceStatusID: 1003, AcceptanceHead: schemaSHA, AcceptanceDigest: schemaDigest,
			VerifyDigest: schemaDigest,
		}},
		PreviewBases: []maulv1.IntentPreviewBase{
			{Repository: "https://github.com/acme/web", SHA: strings.Repeat("b", 40)},
		},
		Rounds:              3,
		RoundNoticesThrough: 2,
		Revisions:           1,
		CheckFixes:          1,
		VerifyFixes:         1,
		Acceptance: &maulv1.IntentAcceptance{
			PlanRevision: 1, Digest: schemaDigest, Criteria: schemaCriteria(),
			Accepted: []maulv1.AcceptanceRecord{{ID: "sounds-good", By: "octocat", ByID: 583231, At: schemaNow,
				CommentID: 46, HeadSetDigest: schemaDigest, Heads: []string{"aaaaaaa"}}},
		},
		Verification: &maulv1.IntentVerification{
			State: maulv1.VerificationFailing, Round: 2, Attempt: 1,
			Ref:           &maulv1.ObjectReference{Name: "target-1-v2-a1", UID: "u-pv"},
			HeadSetDigest: schemaDigest, Heads: schemaHeads(),
			Passed: 1, Failed: 1, Quarantined: 1, ManualOpen: 1, ManualAccepted: 1, NotRun: 1,
			Failing: []maulv1.CriterionState{{ID: "battle-leaderboard", Outcome: maulv1.VerdictFail,
				Fingerprint: "maul-verify-fingerprint/1:" + strings.Repeat("f", 64)}},
			QuarantinedIDs: []string{"kick-onsets"},
			AutoFix: maulv1.AutoFix{State: maulv1.AutoFixHalted, Reason: maulv1.AutoFixHumanReview,
				Since: schemaNow.DeepCopy(),
				Rearm: &maulv1.Rearm{By: "octocat", ByID: 583231, At: schemaNow, Round: 2}},
			CancelRun: "target-1-rev4-shop-a1",
			Requested: &maulv1.VerifyRequest{By: "octocat", ByID: 583231, At: schemaNow, CommentID: 47, Fix: true,
				Reset: true},
			Reset:     maulv1.ResetState{Generation: 1, By: "octocat", ByID: 583231, At: schemaNow.DeepCopy()},
			Automatic: 3, LastAutomaticAt: schemaNow.DeepCopy(), InfraRetries: 1,
		},
		Usage: maulv1.IntentUsage{
			CostMicroUSD: 1234567, InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 40,
		},
		Tracking:    &maulv1.IntentTracking{StatusCommentID: 1000, StatusDigest: schemaDigest},
		ActiveRun:   &maulv1.ObjectReference{Name: "target-1-rev1-shop-a1", UID: "u-1"},
		CompletedAt: schemaNow.DeepCopy(),
	}
}

// testIntentSchema exercises the Intent schema: spec immutability except
// suspend (every other field refused one at a time), the spec bounds, and a
// full status round-trip plus each status bound.
func testIntentSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	key := client.ObjectKey{Name: "target-1", Namespace: "default"}
	if err := c.Create(ctx, schemaIntent(1)); err != nil {
		t.Fatalf("Create(intent) = %v, want nil", err)
	}
	fresh := func(t *testing.T) *maulv1.Intent {
		t.Helper()
		i := &maulv1.Intent{}
		if err := c.Get(ctx, key, i); err != nil {
			t.Fatalf("Get(intent) = %v", err)
		}
		return i
	}

	t.Run("suspend is mutable", func(t *testing.T) {
		i := fresh(t)
		i.Spec.Suspend = true
		if err := c.Update(ctx, i); err != nil {
			t.Fatalf("Update(spec.suspend=true) = %v, want nil", err)
		}
		if !fresh(t).Spec.Suspend {
			t.Error("spec.suspend = false after update, want true")
		}
	})

	for _, tt := range []struct {
		name   string
		mutate func(*maulv1.IntentSpec)
	}{
		{"project", func(s *maulv1.IntentSpec) { s.Project = "other" }},
		{"issue number", func(s *maulv1.IntentSpec) { s.Issue.Number = 2 }},
		{"issue repository", func(s *maulv1.IntentSpec) { s.Issue.Repository = "https://github.com/acme/other" }},
		{"issue url", func(s *maulv1.IntentSpec) { s.Issue.URL = "https://github.com/acme/intents/issues/2" }},
		{"issue url removed", func(s *maulv1.IntentSpec) { s.Issue.URL = "" }},
		{"requester login", func(s *maulv1.IntentSpec) { s.RequestedBy.Login = "mallory" }},
		{"requester event", func(s *maulv1.IntentSpec) { s.RequestedBy.EventID = 99 }},
		{"requester time", func(s *maulv1.IntentSpec) { s.RequestedBy.At = metav1.NewTime(schemaNow.Add(time.Hour)) }},
	} {
		t.Run("spec "+tt.name+" is immutable", func(t *testing.T) {
			i := fresh(t)
			tt.mutate(&i.Spec)
			if err := c.Update(ctx, i); err == nil {
				t.Errorf("Update(spec %s) = nil, want immutability rejection", tt.name)
			}
		})
	}

	// Each case mutates a valid spec and is then named from it as the
	// schema requires, unless it names itself.
	for n, tt := range []struct {
		name    string
		mutate  func(*maulv1.IntentSpec)
		rename  string
		wantErr bool
	}{
		{"a bot requester", func(s *maulv1.IntentSpec) { s.RequestedBy.Login = "github-actions[bot]" }, "", false},
		{"a requester with a space", func(s *maulv1.IntentSpec) { s.RequestedBy.Login = "octo cat" }, "", true},
		{"issue number zero", func(s *maulv1.IntentSpec) { s.Issue.Number = 0 }, "", true},
		{"requester event zero", func(s *maulv1.IntentSpec) { s.RequestedBy.EventID = 0 }, "", true},
		{"issue repository not a url", func(s *maulv1.IntentSpec) { s.Issue.Repository = "acme/intents" }, "", true},
		{"issue url not https", func(s *maulv1.IntentSpec) { s.Issue.URL = "javascript:alert(1)" }, "", true},
		// The name budget: the Intent name is at most 33 characters.
		{"the longest project and issue", func(s *maulv1.IntentSpec) {
			s.Project, s.Issue.Number = strings.Repeat("p", maulv1.MaxProjectNameLength), maulv1.MaxIntentIssueNumber
		}, "", false},
		{"a project past the name budget", func(s *maulv1.IntentSpec) {
			s.Project = strings.Repeat("p", maulv1.MaxProjectNameLength+1)
		}, "", true},
		{"an issue number past seven digits", func(s *maulv1.IntentSpec) {
			s.Issue.Number = maulv1.MaxIntentIssueNumber + 1
		}, "", true},
		{"a name other than <project>-<issue>", func(*maulv1.IntentSpec) {}, "target-other", true},
		{"a name for another issue", func(*maulv1.IntentSpec) {}, "target-2", true},
		{"a name for another project", func(*maulv1.IntentSpec) {}, "targets-1", true},
		{"a name with no issue", func(*maulv1.IntentSpec) {}, "target-", true},
		{"a name padding the issue with a zero", func(s *maulv1.IntentSpec) { s.Issue.Number = 7 }, "target-07", true},
		{"a name carrying a suffix", func(s *maulv1.IntentSpec) { s.Issue.Number = 8 }, "target-8-x", true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			i := schemaIntent(int64(100 + n))
			tt.mutate(&i.Spec)
			i.Name = maulv1.IntentName(i.Spec.Project, i.Spec.Issue.Number)
			if tt.rename != "" {
				i.Name = tt.rename
			}
			if err := c.Create(ctx, i); (err != nil) != tt.wantErr {
				t.Errorf("Create(intent: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}

	t.Run("status round-trips", func(t *testing.T) {
		i := fresh(t)
		i.Status = fullIntentStatus()
		if err := c.Status().Update(ctx, i); err != nil {
			t.Fatalf("Status().Update(full intent status) = %v, want nil", err)
		}
		sameJSON(t, "intent status", fresh(t).Status, fullIntentStatus())
	})

	phaseTimes := func(n int) []maulv1.IntentPhaseTime {
		out := make([]maulv1.IntentPhaseTime, n)
		for k := range out {
			out[k] = maulv1.IntentPhaseTime{Phase: maulv1.IntentPlanning, At: schemaNow}
		}
		return out
	}
	prs := func(n int) []maulv1.IntentPullRequest {
		out := make([]maulv1.IntentPullRequest, n)
		for k := range out {
			out[k] = maulv1.IntentPullRequest{Repository: fmt.Sprintf("https://github.com/acme/app%d", k), Number: 1}
		}
		return out
	}
	actors := func(n int) []int64 {
		out := make([]int64, n)
		for k := range out {
			out[k] = int64(k + 1)
		}
		return out
	}
	bases := func(n int) []maulv1.IntentPreviewBase {
		out := make([]maulv1.IntentPreviewBase, n)
		for k := range out {
			out[k] = maulv1.IntentPreviewBase{Repository: fmt.Sprintf("https://github.com/acme/app%d", k), SHA: schemaSHA}
		}
		return out
	}
	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.IntentStatus)
		wantErr bool
	}{
		{"an unknown phase", func(s *maulv1.IntentStatus) { s.Phase = "Bogus" }, true},
		{"a finding phase", func(s *maulv1.IntentStatus) { s.Phase = "Remediated" }, true},
		{"64 phase times", func(s *maulv1.IntentStatus) { s.PhaseTimes = phaseTimes(maulv1.MaxIntentPhaseTimes) }, false},
		{"65 phase times", func(s *maulv1.IntentStatus) {
			s.PhaseTimes = phaseTimes(maulv1.MaxIntentPhaseTimes + 1)
		}, true},
		{"8 pull requests", func(s *maulv1.IntentStatus) { s.PullRequests = prs(8) }, false},
		{"9 pull requests", func(s *maulv1.IntentStatus) { s.PullRequests = prs(9) }, true},
		{"two pull requests in one repository", func(s *maulv1.IntentStatus) {
			s.PullRequests = append(prs(1), maulv1.IntentPullRequest{Repository: "https://github.com/acme/app0", Number: 2})
		}, true},
		{"a pull request state outside the enum", func(s *maulv1.IntentStatus) { s.PullRequests[0].State = "draft" }, true},
		{"a short head sha", func(s *maulv1.IntentStatus) { s.PullRequests[0].HeadSHA = "abc123" }, true},
		{"a sha-256 head", func(s *maulv1.IntentStatus) { s.PullRequests[0].HeadSHA = strings.Repeat("b", 64) }, false},
		{"a finding branch", func(s *maulv1.IntentStatus) { s.Branch = "maul/finding-abc" }, true},
		{"a branch outside the intent prefix", func(s *maulv1.IntentStatus) { s.Branch = "main" }, true},
		{"a bare-hex digest", func(s *maulv1.IntentStatus) { s.Input.Digest = strings.Repeat("d", 64) }, true},
		{"a bot approval", func(s *maulv1.IntentStatus) { s.Approval.By = "renovate[bot]" }, true},
		// An event id is meaningless without the id space it is from.
		{"an approval without its source", func(s *maulv1.IntentStatus) { s.Approval.Source = "" }, true},
		{"an approval from an unknown source", func(s *maulv1.IntentStatus) { s.Approval.Source = "review" }, true},
		{"an approval by command", func(s *maulv1.IntentStatus) {
			s.Approval.Source = maulv1.IntentActionCommand
		}, false},
		{"a trigger without its source", func(s *maulv1.IntentStatus) { s.LastTrigger.Source = "" }, true},
		{"a trigger without its event", func(s *maulv1.IntentStatus) { s.LastTrigger.EventID = 0 }, true},
		{"a refused bot trigger", func(s *maulv1.IntentStatus) { s.LastTrigger.Login = "dependabot[bot]" }, false},
		{"a trigger by a malformed login", func(s *maulv1.IntentStatus) { s.LastTrigger.Login = "octo cat" }, true},
		{"a 201-character plan summary", func(s *maulv1.IntentStatus) { s.Plan.Summary = strings.Repeat("s", 201) }, true},
		{"nine planned repositories", func(s *maulv1.IntentStatus) {
			s.Plan.Repositories = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
		}, true},
		{"a negative cost", func(s *maulv1.IntentStatus) { s.Usage.CostMicroUSD = -1 }, true},
		{"a plan revision of zero", func(s *maulv1.IntentStatus) { s.Plan.Revision = 0 }, true},
		// Rounds and revisions name runs, so they stay inside the name budget.
		{"rounds at the bound", func(s *maulv1.IntentStatus) { s.Rounds = maulv1.MaxIntentRound }, false},
		{"rounds past the bound", func(s *maulv1.IntentStatus) { s.Rounds = maulv1.MaxIntentRound + 1 }, true},
		{"negative notice cursor", func(s *maulv1.IntentStatus) { s.RoundNoticesThrough = -1 }, true},
		{"notice cursor past the bound", func(s *maulv1.IntentStatus) {
			s.RoundNoticesThrough = maulv1.MaxIntentRound + 1
		}, true},
		{"an input revision past the bound", func(s *maulv1.IntentStatus) {
			s.Input.Revision = maulv1.MaxIntentRound + 1
		}, true},
		{"a plan revision past the bound", func(s *maulv1.IntentStatus) {
			s.Plan.Revision = maulv1.MaxIntentRound + 1
		}, true},
		{"an approved revision past the bound", func(s *maulv1.IntentStatus) {
			s.Approval.PlanRevision = maulv1.MaxIntentRound + 1
		}, true},
		{"refused actors at the bound", func(s *maulv1.IntentStatus) {
			s.Commands.RefusedActors = actors(maulv1.MaxIntentRefusedActors)
		}, false},
		{"refused actors past the bound", func(s *maulv1.IntentStatus) {
			s.Commands.RefusedActors = actors(maulv1.MaxIntentRefusedActors + 1)
		}, true},
		{"a refused actor twice", func(s *maulv1.IntentStatus) { s.Commands.RefusedActors = []int64{7, 7} }, true},
		{"applied commands at the bound", func(s *maulv1.IntentStatus) {
			s.Commands.Applied = actors(maulv1.MaxIntentAppliedCommands)
		}, false},
		{"applied commands past the bound", func(s *maulv1.IntentStatus) {
			s.Commands.Applied = actors(maulv1.MaxIntentAppliedCommands + 1)
		}, true},
		{"a command applied twice", func(s *maulv1.IntentStatus) { s.Commands.Applied = []int64{7, 7} }, true},
		// Slice 3: per-PR check observation and the recorded preview bases.
		{"a malformed per-PR checks head", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].ChecksObservedHeadSHA = "HEAD"
		}, true},
		{"a negative per-PR checks generation", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].ChecksObservedProjectGeneration = -1
		}, true},
		// The one re-run of failed checks on a pull request's head.
		{"a malformed re-run head", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].ChecksRerun.HeadSHA = "HEAD"
		}, true},
		{"a re-run of no check run", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].ChecksRerun.CheckRunIDs = nil
		}, true},
		{"a re-run of no Actions run", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].ChecksRerun.WorkflowRunIDs = nil
		}, true},
		{"a re-run of 32 check runs", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].ChecksRerun.CheckRunIDs = actors(32)
		}, false},
		{"a re-run of 33 check runs", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].ChecksRerun.CheckRunIDs = actors(33)
		}, true},
		{"a re-run of 33 Actions runs", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].ChecksRerun.WorkflowRunIDs = actors(33)
		}, true},
		{"a re-run naming a check twice", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].ChecksRerun.Checks = []string{"test", "test"}
		}, true},
		// The sticky preview comment's record on its pull request.
		{"a negative preview comment id", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].PreviewCommentID = -1
		}, true},
		{"a bare-hex preview digest", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].PreviewDigest = strings.Repeat("d", 64)
		}, true},
		{"8 preview bases", func(s *maulv1.IntentStatus) { s.PreviewBases = bases(8) }, false},
		{"9 preview bases", func(s *maulv1.IntentStatus) { s.PreviewBases = bases(9) }, true},
		{"two preview bases for one repository", func(s *maulv1.IntentStatus) {
			s.PreviewBases = append(bases(1), maulv1.IntentPreviewBase{Repository: "https://github.com/acme/app0",
				SHA: strings.Repeat("c", 40)})
		}, true},
		// A preview image is tagged sha-<40 hex>: a base is never anything else.
		{"a short preview base", func(s *maulv1.IntentStatus) { s.PreviewBases[0].SHA = "abc123" }, true},
		{"a sha-256 preview base", func(s *maulv1.IntentStatus) { s.PreviewBases[0].SHA = strings.Repeat("b", 64) }, true},
		{"a preview base without its repository", func(s *maulv1.IntentStatus) {
			s.PreviewBases[0].Repository = ""
		}, true},
		{"a preview base for a credentialed url", func(s *maulv1.IntentStatus) {
			s.PreviewBases[0].Repository = "https://x:token@github.com/acme/web"
		}, true},
		{"a seen comment without its id", func(s *maulv1.IntentStatus) { s.Commands.Seen.ID = 0 }, true},
		// Acceptance verification: a fixed vocabulary and bounded records.
		{"an unknown verification state", func(s *maulv1.IntentStatus) { s.Verification.State = "Green" }, true},
		{"an unknown auto-fix reason", func(s *maulv1.IntentStatus) {
			s.Verification.AutoFix.Reason = "Because"
		}, true},
		{"an error verdict", func(s *maulv1.IntentStatus) { s.Verification.Failing[0].Outcome = "error" }, true},
		{"a fingerprint of a value", func(s *maulv1.IntentStatus) {
			s.Verification.Failing[0].Fingerprint = "maul-verify-fingerprint/1:leaderboard"
		}, true},
		{"a fourth verification attempt", func(s *maulv1.IntentStatus) { s.Verification.Attempt = 4 }, true},
		{"seventeen approved criteria", func(s *maulv1.IntentStatus) {
			for len(s.Acceptance.Criteria) < 17 {
				s.Acceptance.Criteria = append(s.Acceptance.Criteria, s.Acceptance.Criteria[2])
			}
		}, true},
		{"33 acceptance records", func(s *maulv1.IntentStatus) {
			for len(s.Acceptance.Accepted) < 33 {
				s.Acceptance.Accepted = append(s.Acceptance.Accepted, s.Acceptance.Accepted[0])
			}
		}, true},
		{"an acceptance by a bot", func(s *maulv1.IntentStatus) {
			s.Acceptance.Accepted[0].By = "maul[bot]"
		}, true},
		{"an acceptance without the approver's id", func(s *maulv1.IntentStatus) {
			s.Acceptance.Accepted[0].ByID = 0
		}, true},
		{"a bare-hex acceptance digest", func(s *maulv1.IntentStatus) {
			s.PullRequests[0].AcceptanceDigest = strings.Repeat("d", 64)
		}, true},
		{"a negative verify-fix count", func(s *maulv1.IntentStatus) { s.VerifyFixes = -1 }, true},
	} {
		t.Run("status with "+tt.name, func(t *testing.T) {
			i := fresh(t)
			i.Status = fullIntentStatus()
			tt.mutate(&i.Status)
			if err := c.Status().Update(ctx, i); (err != nil) != tt.wantErr {
				t.Errorf("Status().Update(intent: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}

// schemaIntentRun is a valid run of the given stage, each at round 1: a plan
// run at input revision 1, a build run of approved plan revision 1, a revise
// run pinning that plan and taking its image from the build round, and an
// evaluate or check review run pinning that plan.
func schemaIntentRun(name string, stage maulv1.IntentStage) *maulv1.IntentRun {
	r := &maulv1.IntentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: maulv1.IntentRunSpec{
			IntentRef: maulv1.ObjectReference{Name: "target-1", UID: "u-intent"},
			Stage:     stage,
			Repository: maulv1.IntentRunRepository{
				URL:           "https://github.com/acme/shop",
				RepositoryRef: maulv1.LocalObjectReference{Name: name + "-src"},
			},
			Round:   1,
			Attempt: 1,
			Inputs:  maulv1.IntentRunInputs{ConfigMap: name + "-input", InputDigest: schemaDigest},
			Grant:   maulv1.IntentRunGrant{MaxTurns: 40, TokenBudget: 200000, TimeoutMilliseconds: 1200000},
		},
	}
	switch stage {
	case maulv1.IntentStageBuild:
		r.Spec.Inputs.PlanRevision, r.Spec.Inputs.PlanDigest = 1, schemaDigest
	case maulv1.IntentStageRevise:
		r.Spec.Inputs.PlanRevision, r.Spec.Inputs.PlanDigest = 1, schemaDigest
		r.Spec.Trigger = maulv1.IntentRunTriggerReview
		r.Spec.Inputs.ReviewIDs = []int64{5001}
		r.Spec.ImageFrom = &maulv1.ObjectReference{Name: "target-1-bld-r1-shop-a1", UID: "u-r0"}
	case maulv1.IntentStageEvaluate:
		r.Spec.Inputs.PlanRevision, r.Spec.Inputs.PlanDigest = 1, schemaDigest
		r.Spec.Evaluation = schemaRunEvaluation()
	case maulv1.IntentStageCheckReview:
		r.Spec.Inputs.PlanRevision, r.Spec.Inputs.PlanDigest = 1, schemaDigest
	}
	return r
}

// schemaRunEvaluation is an evaluate run's record of the verification it
// judges.
func schemaRunEvaluation() *maulv1.IntentRunEvaluation {
	return &maulv1.IntentRunEvaluation{Verification: "target-1-v1-a1", UID: "u-pv", ToolIP: "10.42.0.17",
		Rung: maulv1.RungFull}
}

// testIntentRunSchema exercises the IntentRun schema: spec immutability, the
// per-stage invariants (round, plan pin, trigger, imageFrom), the slice 1b
// consumption records (each trigger's own, required, and only on its kind of
// round) and their bounds, the name backstop, and a full status round-trip.
func testIntentRunSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	for _, stage := range maulv1.IntentStages() {
		if err := c.Create(ctx, schemaIntentRun("target-1-"+string(stage), stage)); err != nil {
			t.Fatalf("Create(valid %s run) = %v, want nil", stage, err)
		}
	}
	key := client.ObjectKey{Name: "target-1-revise", Namespace: "default"}
	fresh := func(t *testing.T) *maulv1.IntentRun {
		t.Helper()
		r := &maulv1.IntentRun{}
		if err := c.Get(ctx, key, r); err != nil {
			t.Fatalf("Get(intent run) = %v", err)
		}
		return r
	}

	for _, tt := range []struct {
		name   string
		mutate func(*maulv1.IntentRunSpec)
	}{
		{"round", func(s *maulv1.IntentRunSpec) { s.Round = 2 }},
		{"consumed reviews", func(s *maulv1.IntentRunSpec) { s.Inputs.ReviewIDs = append(s.Inputs.ReviewIDs, 5002) }},
		{"image source", func(s *maulv1.IntentRunSpec) { s.ImageFrom.Name = "target-1-rev1-shop-a1-src" }},
		{"grant", func(s *maulv1.IntentRunSpec) { s.Grant.MaxTurns = 1000 }},
		{"harness", func(s *maulv1.IntentRunSpec) { s.Harness = maulv1.ProjectHarnessCodex }},
		{"previous attempt added", func(s *maulv1.IntentRunSpec) {
			s.PreviousAttempt = &maulv1.PreviousAttempt{Name: "x", Attempt: 1, Outcome: "timeout"}
		}},
	} {
		t.Run("spec "+tt.name+" is immutable", func(t *testing.T) {
			r := fresh(t)
			tt.mutate(&r.Spec)
			if err := c.Update(ctx, r); err == nil {
				t.Errorf("Update(intent run spec %s) = nil, want immutability rejection", tt.name)
			}
		})
	}

	ids := func(n int) []int64 {
		out := make([]int64, n)
		for k := range out {
			out[k] = int64(k + 1)
		}
		return out
	}
	for n, tt := range []struct {
		name    string
		stage   maulv1.IntentStage
		mutate  func(*maulv1.IntentRunSpec)
		wantErr bool
	}{
		{"an unknown stage", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) { s.Stage = "deploy" }, true},
		{"an unknown trigger", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) { s.Trigger = "manual" }, true},
		// Each trigger records what it consumed, and each record belongs
		// to its own kind of round: the exactly-once guarantee.
		{"a command round consuming the reviews since the last round", maulv1.IntentStageRevise,
			func(s *maulv1.IntentRunSpec) {
				s.Trigger, s.Inputs.CommandID = maulv1.IntentRunTriggerCommand, 9001
			}, false},
		{"a command round with no reviews", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.CommandID, s.Inputs.ReviewIDs = maulv1.IntentRunTriggerCommand, 9001, nil
		}, false},
		{"a checks trigger", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.CheckRunIDs = maulv1.IntentRunTriggerChecks, nil, ids(32)
		}, false},
		// A check CI reports as a commit status is consumed by its status
		// id, in its own list: status and check-run ids are separate id
		// spaces.
		{"a checks round consuming commit statuses only", maulv1.IntentStageRevise,
			func(s *maulv1.IntentRunSpec) {
				s.Trigger, s.Inputs.ReviewIDs, s.Inputs.StatusIDs = maulv1.IntentRunTriggerChecks, nil, ids(32)
			}, false},
		{"a checks round consuming check runs and commit statuses", maulv1.IntentStageRevise,
			func(s *maulv1.IntentRunSpec) {
				s.Trigger, s.Inputs.ReviewIDs = maulv1.IntentRunTriggerChecks, nil
				s.Inputs.CheckRunIDs, s.Inputs.StatusIDs = ids(1), ids(1)
			}, false},
		{"a review round without its reviews", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = nil
		}, true},
		{"a checks round without its check runs or commit statuses", maulv1.IntentStageRevise,
			func(s *maulv1.IntentRunSpec) {
				s.Trigger, s.Inputs.ReviewIDs = maulv1.IntentRunTriggerChecks, nil
			}, true},
		{"a command round without its command", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Trigger = maulv1.IntentRunTriggerCommand
		}, true},
		{"a checks round consuming reviews", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.CheckRunIDs = maulv1.IntentRunTriggerChecks, ids(1)
		}, true},
		{"a review round consuming check runs", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.CheckRunIDs = ids(1)
		}, true},
		{"a review round consuming commit statuses", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.StatusIDs = ids(1)
		}, true},
		{"a command round consuming commit statuses", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.CommandID, s.Inputs.StatusIDs = maulv1.IntentRunTriggerCommand, 9001, ids(1)
		}, true},
		{"a review round consuming a command", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.CommandID = 9001
		}, true},
		{"a plan run consuming reviews", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = ids(1)
		}, true},
		{"a build run consuming check runs", maulv1.IntentStageBuild, func(s *maulv1.IntentRunSpec) {
			s.Inputs.CheckRunIDs = ids(1)
		}, true},
		{"a build run consuming a command", maulv1.IntentStageBuild, func(s *maulv1.IntentRunSpec) {
			s.Inputs.CommandID = 9001
		}, true},
		{"a build run consuming commit statuses", maulv1.IntentStageBuild, func(s *maulv1.IntentRunSpec) {
			s.Inputs.StatusIDs = ids(1)
		}, true},
		// The UID is what an adoption on AlreadyExists compares, and what
		// pins a revise run's image source: neither may be left out.
		{"an intent ref without its uid", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) {
			s.IntentRef.UID = ""
		}, true},
		{"a revise run whose image source has no uid", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.ImageFrom.UID = ""
		}, true},
		{"a plan run at round 0", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) { s.Round = 0 }, true},
		{"a build run at round 0", maulv1.IntentStageBuild, func(s *maulv1.IntentRunSpec) {
			s.Round, s.Inputs.PlanRevision = 0, 0
		}, true},
		// A build's round is its plan revision, so a build after a revival
		// and a new approval is a new round, never the first build's name.
		{"a build run of a later plan revision", maulv1.IntentStageBuild, func(s *maulv1.IntentRunSpec) {
			s.Round, s.Inputs.PlanRevision = 3, 3
		}, false},
		{"a build run at a round other than its plan revision", maulv1.IntentStageBuild,
			func(s *maulv1.IntentRunSpec) { s.Round = 2 }, true},
		{"a revise run at round 0", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) { s.Round = 0 }, true},
		{"a round at the bound", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) {
			s.Round = maulv1.MaxIntentRound
		}, false},
		{"a round past the bound", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Round = maulv1.MaxIntentRound + 1
		}, true},
		{"a plan revision past the bound", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.PlanRevision = maulv1.MaxIntentRound + 1
		}, true},
		{"a build run without the plan digest", maulv1.IntentStageBuild, func(s *maulv1.IntentRunSpec) {
			s.Inputs.PlanDigest = ""
		}, true},
		{"a build run without the plan revision", maulv1.IntentStageBuild, func(s *maulv1.IntentRunSpec) {
			s.Inputs.PlanRevision = 0
		}, true},
		{"a revise run without the plan", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.PlanRevision, s.Inputs.PlanDigest = 0, ""
		}, true},
		{"a revise run without a trigger", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Trigger = ""
		}, true},
		{"a revise run without the build image", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.ImageFrom = nil
		}, true},
		{"a build run with a trigger", maulv1.IntentStageBuild, func(s *maulv1.IntentRunSpec) {
			s.Trigger = maulv1.IntentRunTriggerReview
		}, true},
		{"a plan run with an image source", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) {
			s.ImageFrom = &maulv1.ObjectReference{Name: "r0"}
		}, true},
		{"attempt zero", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) { s.Attempt = 0 }, true},
		{"attempt seventeen", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) { s.Attempt = 17 }, true},
		{"32 consumed reviews", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = ids(32)
		}, false},
		{"33 consumed reviews", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = ids(33)
		}, true},
		{"33 consumed check runs", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.CheckRunIDs = maulv1.IntentRunTriggerChecks, nil, ids(33)
		}, true},
		{"33 consumed commit statuses", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.StatusIDs = maulv1.IntentRunTriggerChecks, nil, ids(33)
		}, true},
		{"a commit status consumed twice", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.StatusIDs = maulv1.IntentRunTriggerChecks, nil, []int64{7, 7}
		}, true},
		{"a review consumed twice", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.ReviewIDs = []int64{7, 7}
		}, true},
		{"a timeout past a day", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) {
			s.Grant.TimeoutMilliseconds = 86400001
		}, true},
		{"a bare-hex input digest", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) {
			s.Inputs.InputDigest = strings.Repeat("d", 64)
		}, true},
		// A replan's or revival's plan run pins its context file; nothing
		// but a plan run is handed one.
		{"a plan run pinning its context", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) {
			s.Inputs.ContextDigest = schemaDigest
		}, false},
		{"a bare-hex context digest", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) {
			s.Inputs.ContextDigest = strings.Repeat("d", 64)
		}, true},
		{"a build run pinning a context", maulv1.IntentStageBuild, func(s *maulv1.IntentRunSpec) {
			s.Inputs.ContextDigest = schemaDigest
		}, true},
		{"a revise run pinning a context", maulv1.IntentStageRevise, func(s *maulv1.IntentRunSpec) {
			s.Inputs.ContextDigest = schemaDigest
		}, true},
		{"credentials in the repository url", maulv1.IntentStagePlan, func(s *maulv1.IntentRunSpec) {
			s.Repository.URL = "https://x:token@github.com/acme/shop"
		}, true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			r := schemaIntentRun(fmt.Sprintf("target-1-case-%d", n), tt.stage)
			tt.mutate(&r.Spec)
			if err := c.Create(ctx, r); (err != nil) != tt.wantErr {
				t.Errorf("Create(intent run: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}

	// A typed client omits an empty uid, which the cases above cover; an
	// explicitly empty one passes has(), so the rules refuse it by size.
	// The first case, which blanks nothing, is the control: the same
	// unstructured path accepts a valid run.
	for _, tt := range []struct {
		field   string
		wantErr bool
	}{{"", false}, {"intentRef", true}, {"imageFrom", true}} {
		name := "create unstructured with every uid set"
		if tt.field != "" {
			name = fmt.Sprintf("create unstructured with spec.%s.uid blanked", tt.field)
		}
		t.Run(name, func(t *testing.T) {
			r := schemaIntentRun("target-1-uid-"+cmp.Or(strings.ToLower(tt.field), "control"), maulv1.IntentStageRevise)
			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(r)
			if err != nil {
				t.Fatalf("ToUnstructured(intent run) = %v", err)
			}
			u := &unstructured.Unstructured{Object: obj}
			u.SetGroupVersionKind(maulv1.GroupVersion.WithKind("IntentRun"))
			if tt.field != "" {
				if err := unstructured.SetNestedField(u.Object, "", "spec", tt.field, "uid"); err != nil {
					t.Fatalf("set spec.%s.uid: %v", tt.field, err)
				}
			}
			if err := c.Create(ctx, u); (err != nil) != tt.wantErr {
				t.Errorf("Create(intent run, spec.%s.uid blanked) = %v, wantErr %v", tt.field, err, tt.wantErr)
			}
		})
	}

	// The backstop behind the name budget: a run name is a label value.
	for _, tt := range []struct {
		size    int
		wantErr bool
	}{{63, false}, {64, true}} {
		t.Run(fmt.Sprintf("create with a %d-character name", tt.size), func(t *testing.T) {
			name := "target-1-plan-r1-a1-" + strings.Repeat("n", tt.size-len("target-1-plan-r1-a1-"))
			if err := c.Create(ctx, schemaIntentRun(name, maulv1.IntentStagePlan)); (err != nil) != tt.wantErr {
				t.Errorf("Create(intent run named %d characters) = %v, wantErr %v", tt.size, err, tt.wantErr)
			}
		})
	}

	fullStatus := func() maulv1.IntentRunStatus {
		return maulv1.IntentRunStatus{
			Phase:  maulv1.RunComplete,
			JobRef: &maulv1.JobReference{Namespace: "maul-agents", Name: "maul-abc-int-a1", UID: "u-job"},
			RunnerImage: &maulv1.RunnerImageRef{
				Image:    "ghcr.io/acme/go-agent-env@sha256:" + strings.Repeat("a", 64),
				Source:   maulv1.RunnerImageSourceRepository,
				Manifest: ".maul/agent.yaml",
			},
			BaseSHA:       schemaSHA,
			PushedCommit:  strings.Repeat("c", 40),
			Outcome:       "ok",
			Report:        "## Changes\n",
			Detail:        "pushed",
			Usage:         maulv1.UsageSummary{InputTokens: 1, OutputTokens: 2, CostUSD: "0.123456"},
			NumTurns:      34,
			FirstEditTurn: 9,
			Transcript:    &maulv1.TranscriptRef{Name: "target-1-revise-transcript", Turns: 12},
			StartedAt:     schemaNow.DeepCopy(),
			FinishedAt:    schemaNow.DeepCopy(),
			Conditions: []metav1.Condition{{
				Type: maulv1.ConditionComplete, Status: metav1.ConditionTrue,
				LastTransitionTime: schemaNow, Reason: "ok", Message: "run complete",
			}},
			ObservedGeneration: 1,
		}
	}
	t.Run("status round-trips", func(t *testing.T) {
		r := fresh(t)
		r.Status = fullStatus()
		if err := c.Status().Update(ctx, r); err != nil {
			t.Fatalf("Status().Update(full intent run status) = %v, want nil", err)
		}
		sameJSON(t, "intent run status", fresh(t).Status, fullStatus())
	})
	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.IntentRunStatus)
		wantErr bool
	}{
		{"a 64 KiB report", func(s *maulv1.IntentRunStatus) { s.Report = strings.Repeat("r", 65536) }, false},
		{"a report past 64 KiB", func(s *maulv1.IntentRunStatus) { s.Report = strings.Repeat("r", 65537) }, true},
		{"a detail past 4 KiB", func(s *maulv1.IntentRunStatus) { s.Detail = strings.Repeat("d", 4097) }, true},
		{"an outcome past 64", func(s *maulv1.IntentRunStatus) { s.Outcome = strings.Repeat("o", 65) }, true},
		{"a malformed pushed commit", func(s *maulv1.IntentRunStatus) { s.PushedCommit = "HEAD" }, true},
		{"an unknown run phase", func(s *maulv1.IntentRunStatus) { s.Phase = "Bogus" }, true},
		{"an unknown image source", func(s *maulv1.IntentRunStatus) { s.RunnerImage.Source = "bogus" }, true},
		{"100000 turns", func(s *maulv1.IntentRunStatus) { s.NumTurns = 100000 }, false},
		{"turns past 100000", func(s *maulv1.IntentRunStatus) { s.NumTurns = 100001 }, true},
		{"negative turns", func(s *maulv1.IntentRunStatus) { s.NumTurns = -1 }, true},
		{"a first edit on turn 100000", func(s *maulv1.IntentRunStatus) { s.FirstEditTurn = 100000 }, false},
		{"a first edit past turn 100000", func(s *maulv1.IntentRunStatus) { s.FirstEditTurn = 100001 }, true},
		{"a negative first edit turn", func(s *maulv1.IntentRunStatus) { s.FirstEditTurn = -1 }, true},
	} {
		t.Run("status with "+tt.name, func(t *testing.T) {
			r := fresh(t)
			r.Status = fullStatus()
			tt.mutate(&r.Status)
			if err := c.Status().Update(ctx, r); (err != nil) != tt.wantErr {
				t.Errorf("Status().Update(intent run: %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}
