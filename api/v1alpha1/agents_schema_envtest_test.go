// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The agents-namespace evaluator's schema (SPEC S2-01): a PreviewVerification
// placed in the agents namespace (no Preview, a workspace instead), its base
// runs and build entries, a Project's cli and library surfaces, and the
// commit-pinned Repository. A slot verification is admitted and refused
// exactly as before: testPreviewVerificationSchema and
// testOutcomeVerificationSchema keep every S1 rule, and the table here adds
// the placement edges. They run inside TestSchemaValidation's single
// envtest boot.

func testAgentsSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	testAgentsVerificationSchema(ctx, t, c)
	testAgentsStatusSchema(ctx, t, c)
	testAgentsProjectSchema(ctx, t, c)
	testCommitRepositorySchema(ctx, t, c)
}

// schemaWorkspace is a valid cli workspace.
func schemaWorkspace() *maulv1.VerificationWorkspace {
	return &maulv1.VerificationWorkspace{
		Repository:        "render",
		HeadSHA:           strings.Repeat("a", 40),
		BaseSHA:           strings.Repeat("b", 40),
		HeadRepositoryRef: maulv1.ObjectReference{Name: "target-1-ws-head", UID: "u-head"},
		BaseRepositoryRef: maulv1.ObjectReference{Name: "target-1-ws-base", UID: "u-base"},
		SandboxImage:      "registry.example/acme/render-agent@sha256:" + strings.Repeat("c", 64),
		SandboxSearchPath: "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
		NetworkProfile:    "go-modules",
		Surface: maulv1.WorkspaceSurface{CLI: &maulv1.WorkspaceCLI{
			Build:        []string{"go", "build", "-o", "/work/bin/bbrender", "./cmd/bbrender"},
			Binary:       "/work/bin/bbrender",
			BuildTimeout: metav1.Duration{Duration: 10 * time.Minute},
		}},
	}
}

// schemaAgentsVerification is a valid agents-placed outcome verification.
func schemaAgentsVerification(name string) *maulv1.PreviewVerification {
	v := schemaOutcomeVerification(name)
	v.Spec.Placement = maulv1.PlacementAgents
	v.Spec.PreviewRef, v.Spec.PreviewGeneration = maulv1.ObjectReference{}, 0
	v.Spec.Tools = &maulv1.ToolsSpec{Sandbox: true, MaxToolCalls: 300}
	v.Spec.Workspace = schemaWorkspace()
	v.Spec.Heads = []maulv1.VerificationHead{{Component: v.Spec.Workspace.Repository,
		Repository: "https://github.com/acme/render", Revision: v.Spec.Workspace.HeadSHA}}
	v.Spec.Timeout = metav1.Duration{Duration: 75 * time.Minute}
	return v
}

func testAgentsVerificationSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	const name = "target-1-v9-a1"
	t.Run("an agents verification round-trips", func(t *testing.T) {
		v := schemaAgentsVerification(name)
		want := v.Spec.DeepCopy()
		if err := c.Create(ctx, v); err != nil {
			t.Fatalf("Create(agents verification) = %v", err)
		}
		got := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(v), got); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "agents verification spec", got.Spec, *want)
	})
	t.Run("a slot verification stores no placement", func(t *testing.T) {
		got := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKey{Name: "target-1-v7-a1", Namespace: "default"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Placement != "" || got.Spec.IsAgents() || got.Spec.Workspace != nil {
			t.Errorf("slot verification placement %q, workspace %+v: want neither", got.Spec.Placement,
				got.Spec.Workspace)
		}
	})
	for _, tt := range []struct {
		name   string
		mutate func(*maulv1.PreviewVerificationSpec)
	}{
		{"placement", func(s *maulv1.PreviewVerificationSpec) { s.Placement = maulv1.PlacementSlot }},
		{"workspace base", func(s *maulv1.PreviewVerificationSpec) { s.Workspace.BaseSHA = strings.Repeat("d", 40) }},
		{"workspace build", func(s *maulv1.PreviewVerificationSpec) { s.Workspace.Surface.CLI.Build[0] = "gox" }},
	} {
		t.Run("agents spec "+tt.name+" is immutable", func(t *testing.T) {
			v := &maulv1.PreviewVerification{}
			if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, v); err != nil {
				t.Fatal(err)
			}
			tt.mutate(&v.Spec)
			if err := c.Update(ctx, v); err == nil {
				t.Errorf("Update(agents spec %s) = nil, want immutability rejection", tt.name)
			}
		})
	}
	t.Run("a slot verification cannot become an agents one", func(t *testing.T) {
		v := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKey{Name: "target-1-v7-a1", Namespace: "default"}, v); err != nil {
			t.Fatal(err)
		}
		v.Spec.Placement = maulv1.PlacementAgents
		if err := c.Update(ctx, v); err == nil {
			t.Error("Update(slot placement to agents) = nil, want immutability rejection")
		}
	})

	library := func(v *maulv1.PreviewVerification) {
		v.Spec.Workspace.Surface = maulv1.WorkspaceSurface{
			Library: &maulv1.WorkspaceLibrary{Language: maulv1.LibraryLanguageGo}}
	}
	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.PreviewVerification)
		wantErr bool
	}{
		// The slot edges: what S1 always required, it still requires.
		{"slot: no preview ref", func(v *maulv1.PreviewVerification) {
			v.Spec.Placement, v.Spec.Workspace = "", nil
			v.Spec.Tools = &maulv1.ToolsSpec{MaxToolCalls: 300}
			v.Spec.Timeout.Duration = time.Hour
		}, true},
		{"slot: explicit placement with a preview", func(v *maulv1.PreviewVerification) {
			*v = *schemaOutcomeVerification(v.Name)
			v.Spec.Placement = maulv1.PlacementSlot
		}, false},
		{"slot: preview generation 0", func(v *maulv1.PreviewVerification) {
			*v = *schemaOutcomeVerification(v.Name)
			v.Spec.PreviewGeneration = 0
		}, true},
		{"slot: a workspace", func(v *maulv1.PreviewVerification) {
			ws := v.Spec.Workspace
			*v = *schemaOutcomeVerification(v.Name)
			v.Spec.Workspace = ws
		}, true},
		{"slot: a timeout of 75m", func(v *maulv1.PreviewVerification) {
			*v = *schemaOutcomeVerification(v.Name)
			v.Spec.Timeout.Duration = 75 * time.Minute
		}, true},
		{"an unknown placement", func(v *maulv1.PreviewVerification) { v.Spec.Placement = "node" }, true},
		// The agents edges.
		{"agents: library", library, false},
		{"agents: no network profile", func(v *maulv1.PreviewVerification) { v.Spec.Workspace.NetworkProfile = "" }, false},
		{"agents: no workspace", func(v *maulv1.PreviewVerification) { v.Spec.Workspace = nil }, true},
		{"agents: a preview ref", func(v *maulv1.PreviewVerification) {
			v.Spec.PreviewRef = maulv1.ObjectReference{Name: "target-1", UID: "u-preview"}
		}, true},
		{"agents: a preview generation", func(v *maulv1.PreviewVerification) { v.Spec.PreviewGeneration = 3 }, true},
		{"agents: criteria", func(v *maulv1.PreviewVerification) {
			v.Spec.Outcomes, v.Spec.OutcomesDigest, v.Spec.Checks, v.Spec.ChecksDigest = nil, "", nil, ""
			v.Spec.Rung, v.Spec.Tools = "", nil
			v.Spec.Criteria, v.Spec.CriteriaDigest = schemaCriteria()[:1], schemaDigest
		}, true},
		{"agents: a browser", func(v *maulv1.PreviewVerification) { v.Spec.Tools.Browser = true }, true},
		{"agents: a look-around", func(v *maulv1.PreviewVerification) { v.Spec.Tools.LookAroundRoutes = 1 }, true},
		{"agents: a timeout over 75m", func(v *maulv1.PreviewVerification) {
			v.Spec.Timeout.Duration = 76 * time.Minute
		}, true},
		{"agents: a timeout under a minute", func(v *maulv1.PreviewVerification) {
			v.Spec.Timeout.Duration = time.Second
		}, true},
		{"agents: a short head", func(v *maulv1.PreviewVerification) { v.Spec.Workspace.HeadSHA = "abc" }, true},
		// The workspace builds one of the heads the verification is of.
		{"agents: another head beside the workspace's", func(v *maulv1.PreviewVerification) {
			v.Spec.Heads = append(v.Spec.Heads, maulv1.VerificationHead{Component: "api",
				Repository: "https://github.com/acme/api", Revision: strings.Repeat("e", 40)})
		}, false},
		{"agents: a workspace head that is not the repository's head", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.HeadSHA = strings.Repeat("e", 40)
		}, true},
		{"agents: a workspace repository with no head", func(v *maulv1.PreviewVerification) {
			v.Spec.Heads[0].Component = "api"
		}, true},
		{"agents: an upper-case base", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.BaseSHA = strings.Repeat("B", 40)
		}, true},
		{"agents: a head repository without its uid", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.HeadRepositoryRef.UID = ""
		}, true},
		{"agents: a base repository without its uid", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.BaseRepositoryRef.UID = ""
		}, true},
		{"agents: a repository key over 16", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.Repository = strings.Repeat("r", 17)
		}, true},
		{"agents: a sandbox image by tag", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.SandboxImage = "registry.example/acme/render-agent:latest"
		}, true},
		{"agents: a relative search path", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.SandboxSearchPath = "bin:/usr/bin"
		}, true},
		{"agents: a malformed network profile", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.NetworkProfile = "Go Modules"
		}, true},
		{"agents: no surface", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.Surface = maulv1.WorkspaceSurface{}
		}, true},
		{"agents: both surfaces", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.Surface.Library = &maulv1.WorkspaceLibrary{Language: maulv1.LibraryLanguageGo}
		}, true},
		{"agents: a node library", func(v *maulv1.PreviewVerification) {
			library(v)
			v.Spec.Workspace.Surface.Library.Language = "node"
		}, true},
		{"agents: a binary outside /work/bin", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.Surface.CLI.Binary = "/usr/bin/bbrender"
		}, true},
		{"agents: a build writing nothing under /work/bin", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.Surface.CLI.Build = []string{"go", "build", "./..."}
		}, true},
		{"agents: a build timeout over 20m", func(v *maulv1.PreviewVerification) {
			v.Spec.Workspace.Surface.CLI.BuildTimeout.Duration = 21 * time.Minute
		}, true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			v := schemaAgentsVerification("target-1-v10-" + shortHash(tt.name))
			tt.mutate(v)
			err := c.Create(ctx, v)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(verification with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}

// fullAgentsStatus populates every status field the agents placement adds.
func fullAgentsStatus() maulv1.PreviewVerificationStatus {
	s := fullEvaluatorStatus()
	sha := strings.Repeat("a", 64)
	check := "sha256:" + strings.Repeat("1", 64)
	s.Ledger.Entries = append(s.Ledger.Entries,
		maulv1.LedgerIndexEntry{Seq: 3, Kind: maulv1.LedgerKindRelay, Verb: maulv1.LedgerVerbBuild, ResultSHA256: sha},
		maulv1.LedgerIndexEntry{Seq: 4, Kind: maulv1.LedgerKindAgent, Verb: maulv1.LedgerVerbFiles, ResultSHA256: sha},
		maulv1.LedgerIndexEntry{Seq: 5, Kind: maulv1.LedgerKindAgent, Verb: maulv1.LedgerVerbRun, Exit: 1,
			ResultSHA256: sha})
	s.Ledger.Count = int32(len(s.Ledger.Entries))
	s.Ledger.Builds = []maulv1.LedgerBuild{
		{Side: maulv1.TreeHead, Result: maulv1.BuildBuilt, Seq: 3, DurationMs: 42000},
		{Side: maulv1.TreeBase, Result: maulv1.BuildFailed, Seq: 6, ExitCode: 1, DurationMs: 1200},
	}
	s.Replay[0].Tree = maulv1.TreeHead
	s.Base = &maulv1.BaseStatus{Required: true, Unbuildable: true, Runs: []maulv1.CheckRun{
		{CheckDigest: check, Result: maulv1.CheckRunFail, Token: "b-0a1b2c", Seq: 7, Tree: maulv1.TreeBase,
			AssertionStep: 1, AssertionID: "expect"},
	}}
	s.Environment = append(s.Environment, maulv1.PartFact{Name: "build-head", Role: maulv1.PreviewRoleBuild})
	return s
}

func testAgentsStatusSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	const name = "target-1-v9-a1"
	t.Run("the agents status round-trips", func(t *testing.T) {
		v := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, v); err != nil {
			t.Fatal(err)
		}
		v.Status = fullAgentsStatus()
		if err := c.Status().Update(ctx, v); err != nil {
			t.Fatalf("Status().Update(agents status) = %v", err)
		}
		read := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(v), read); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "agents status", read.Status, fullAgentsStatus())
	})
	t.Run("the agents reasons are admitted", func(t *testing.T) {
		for _, r := range []maulv1.PreviewVerificationReason{
			maulv1.VerificationReasonToolsBuildFailed, maulv1.VerificationReasonSandboxUnenforced,
			maulv1.VerificationReasonSandboxImageUnavailable, maulv1.VerificationReasonTreeUnavailable,
			maulv1.VerificationReasonToolPodsSaturated, maulv1.VerificationReasonToolsEvicted,
			maulv1.VerificationReasonToolsDeadline,
		} {
			v := &maulv1.PreviewVerification{}
			if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, v); err != nil {
				t.Fatal(err)
			}
			v.Status = maulv1.PreviewVerificationStatus{Phase: maulv1.VerificationInfra, Reason: r}
			if err := c.Status().Update(ctx, v); err != nil {
				t.Errorf("Status().Update(reason %s) = %v", r, err)
			}
		}
	})
	for _, tt := range []struct {
		name   string
		mutate func(*maulv1.PreviewVerificationStatus)
	}{
		{"an unknown tree", func(s *maulv1.PreviewVerificationStatus) { s.Base.Runs[0].Tree = "merge" }},
		// A base run read as head would let a base result stand for head.
		{"a base run with no tree", func(s *maulv1.PreviewVerificationStatus) { s.Base.Runs[0].Tree = "" }},
		{"a base run on the head tree", func(s *maulv1.PreviewVerificationStatus) {
			s.Base.Runs[0].Tree = maulv1.TreeHead
		}},
		{"an unknown build side", func(s *maulv1.PreviewVerificationStatus) { s.Ledger.Builds[0].Side = "both" }},
		{"an unknown build result", func(s *maulv1.PreviewVerificationStatus) { s.Ledger.Builds[0].Result = "ok" }},
		{"a build at seq 0", func(s *maulv1.PreviewVerificationStatus) { s.Ledger.Builds[0].Seq = 0 }},
		{"five builds", func(s *maulv1.PreviewVerificationStatus) {
			for len(s.Ledger.Builds) < 5 {
				s.Ledger.Builds = append(s.Ledger.Builds, s.Ledger.Builds[0])
			}
		}},
		{"81 base runs", func(s *maulv1.PreviewVerificationStatus) {
			for len(s.Base.Runs) < 81 {
				s.Base.Runs = append(s.Base.Runs, s.Base.Runs[0])
			}
		}},
		{"an unknown verb", func(s *maulv1.PreviewVerificationStatus) { s.Ledger.Entries[2].Verb = "shell" }},
	} {
		t.Run("agents status with "+tt.name, func(t *testing.T) {
			v := &maulv1.PreviewVerification{}
			if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "default"}, v); err != nil {
				t.Fatal(err)
			}
			v.Status = fullAgentsStatus()
			tt.mutate(&v.Status)
			if err := c.Status().Update(ctx, v); err == nil {
				t.Errorf("Status().Update(%s) = nil, want rejection", tt.name)
			}
		})
	}
}

// schemaCLI is a valid cli surface.
func schemaCLI() *maulv1.CLISurface {
	return &maulv1.CLISurface{
		Build:  []string{"go", "build", "-o", "/work/bin/bbrender", "./cmd/bbrender"},
		Binary: "/work/bin/bbrender",
	}
}

func testAgentsProjectSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	t.Run("a cli surface round-trips with nothing defaulted", func(t *testing.T) {
		p := schemaProject("proj-cli")
		p.Spec.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
		want := p.Spec.Surfaces.DeepCopy()
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project with cli) = %v", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "cli surface", got.Spec.Surfaces, want)
		if got.Spec.Surfaces.CLI.Timeout() != maulv1.DefaultCLIBuildTimeout {
			t.Errorf("unset build timeout reads as %v, want %v", got.Spec.Surfaces.CLI.Timeout(),
				maulv1.DefaultCLIBuildTimeout)
		}
	})
	twoRepos := func(s *maulv1.ProjectSpec) {
		s.Repositories = append(s.Repositories, maulv1.ProjectRepository{Name: "render",
			URL: "https://github.com/acme/render"})
	}
	preview := &maulv1.ProjectPreview{ImageRepository: "registry.example/maul/previews/shop", Port: 8080,
		ReadinessPath: "/healthz"}
	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.ProjectSpec)
		wantErr bool
	}{
		{"a library surface", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{Library: &maulv1.LibrarySurface{Language: maulv1.LibraryLanguageGo}}
		}, false},
		{"a node library", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{Library: &maulv1.LibrarySurface{Language: "node"}}
		}, true},
		{"a library without a language", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{Library: &maulv1.LibrarySurface{}}
		}, true},
		{"both cli and library", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI(),
				Library: &maulv1.LibrarySurface{Language: maulv1.LibraryLanguageGo}}
		}, true},
		{"cli beside spec.preview", func(s *maulv1.ProjectSpec) {
			s.Surfaces, s.Preview = &maulv1.ProjectSurfaces{CLI: schemaCLI()}, preview
		}, true},
		{"cli beside a repository preview", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Repositories[0].Preview = &maulv1.ProjectRepositoryPreview{ProjectPreview: *preview}
		}, true},
		{"library beside spec.preview", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{Library: &maulv1.LibrarySurface{Language: maulv1.LibraryLanguageGo}}
			s.Preview = preview
		}, true},
		{"cli beside a browser", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI(), Browser: &maulv1.BrowserSurface{Entry: "/"}}
		}, true},
		{"cli naming its repository", func(s *maulv1.ProjectSpec) {
			twoRepos(s)
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.Repository = "render"
		}, false},
		{"cli naming no repository of two", func(s *maulv1.ProjectSpec) {
			twoRepos(s)
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
		}, true},
		{"cli naming an unknown repository", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.Repository = "render"
		}, true},
		{"library naming no repository of two", func(s *maulv1.ProjectSpec) {
			twoRepos(s)
			s.Surfaces = &maulv1.ProjectSurfaces{Library: &maulv1.LibrarySurface{Language: maulv1.LibraryLanguageGo}}
		}, true},
		{"library naming its repository", func(s *maulv1.ProjectSpec) {
			twoRepos(s)
			s.Surfaces = &maulv1.ProjectSurfaces{Library: &maulv1.LibrarySurface{Language: maulv1.LibraryLanguageGo,
				Repository: "shop"}}
		}, false},
		{"cli with no build", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.Build = nil
		}, true},
		{"cli with 17 build arguments", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			for len(s.Surfaces.CLI.Build) < 17 {
				s.Surfaces.CLI.Build = append(s.Surfaces.CLI.Build, "-v")
			}
		}, true},
		{"cli with a build argument of 257 bytes", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.Build[2] = strings.Repeat("x", 257)
		}, true},
		{"cli with a build writing nothing under /work/bin", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.Build = []string{"go", "build", "./..."}
		}, true},
		{"cli with a relative binary", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.Binary = "bbrender"
		}, true},
		{"cli with a binary five segments deep", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.Binary = "/work/bin/a/b/c"
		}, true},
		{"cli with a binary escaping /work/bin", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.Binary = "/work/bin/../etc"
		}, true},
		{"cli with a 20m build timeout", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.BuildTimeout = &metav1.Duration{Duration: 20 * time.Minute}
		}, false},
		{"cli with a 21m build timeout", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.BuildTimeout = &metav1.Duration{Duration: 21 * time.Minute}
		}, true},
		{"cli with a 30s build timeout", func(s *maulv1.ProjectSpec) {
			s.Surfaces = &maulv1.ProjectSurfaces{CLI: schemaCLI()}
			s.Surfaces.CLI.BuildTimeout = &metav1.Duration{Duration: 30 * time.Second}
		}, true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			p := schemaProject("proj-s2-" + shortHash(tt.name))
			tt.mutate(&p.Spec)
			err := c.Create(ctx, p)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(project with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}

func testCommitRepositorySchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	repo := func(name string, ref maulv1.RepositoryRef) *maulv1.Repository {
		return &maulv1.Repository{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: maulv1.RepositorySpec{URL: "https://github.com/acme/render", Ref: ref}}
	}
	commit := strings.Repeat("a", 40)
	for _, tt := range []struct {
		name    string
		ref     maulv1.RepositoryRef
		wantErr bool
	}{
		{"a commit", maulv1.RepositoryRef{Commit: commit}, false},
		{"a branch", maulv1.RepositoryRef{Branch: "main"}, false},
		{"neither", maulv1.RepositoryRef{}, false},
		{"both", maulv1.RepositoryRef{Branch: "main", Commit: commit}, true},
		{"a short commit", maulv1.RepositoryRef{Commit: "abc1234"}, true},
		{"an upper-case commit", maulv1.RepositoryRef{Commit: strings.Repeat("A", 40)}, true},
	} {
		t.Run("create repository with "+tt.name, func(t *testing.T) {
			err := c.Create(ctx, repo("repo-s2-"+shortHash(tt.name), tt.ref))
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(repository with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
	for _, tt := range []struct {
		name string
		from maulv1.RepositoryRef
		to   maulv1.RepositoryRef
		ok   bool
	}{
		{"commit changed", maulv1.RepositoryRef{Commit: commit},
			maulv1.RepositoryRef{Commit: strings.Repeat("b", 40)}, false},
		{"commit removed", maulv1.RepositoryRef{Commit: commit}, maulv1.RepositoryRef{}, false},
		{"commit to branch", maulv1.RepositoryRef{Commit: commit}, maulv1.RepositoryRef{Branch: "main"}, false},
		{"commit added", maulv1.RepositoryRef{}, maulv1.RepositoryRef{Commit: commit}, false},
		{"commit added beside a branch", maulv1.RepositoryRef{Branch: "main"}, maulv1.RepositoryRef{Commit: commit}, false},
		{"branch changed", maulv1.RepositoryRef{Branch: "main"}, maulv1.RepositoryRef{Branch: "dev"}, true},
	} {
		t.Run("update repository "+tt.name, func(t *testing.T) {
			r := repo("repo-s2-u-"+shortHash(tt.name), tt.from)
			if err := c.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			r.Spec.Ref = tt.to
			err := c.Update(ctx, r)
			if (err == nil) != tt.ok {
				t.Errorf("Update(repository %s) = %v, want ok %v", tt.name, err, tt.ok)
			}
		})
	}
}
