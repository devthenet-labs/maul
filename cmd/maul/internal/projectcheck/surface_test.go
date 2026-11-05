// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
)

const (
	profilesOn = "MAUL_NETWORK_PROFILES_ENABLED"
	profilesAt = "MAUL_NETWORK_PROFILES"

	profilesJSON = `[{"name":"acme-go","host":"github.com","port":9801,"repositories":["acme/shop-lib"],` +
		`"route":"go","modules":["github.com/acme/"]}]`
	twoProfiles = `[{"name":"a","host":"github.com","port":9801,"repositories":["acme/.*"],"route":"go",` +
		`"modules":["github.com/acme/"]},{"name":"b","host":"github.com","port":9802,` +
		`"repositories":["acme/shop-.*"],"route":"go","modules":["github.com/acme/"]}]`
)

// agentsWorld is a Project with no preview and a cli surface on lib.
func agentsWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	for i := range w.project.Spec.Repositories {
		w.project.Spec.Repositories[i].Preview = nil
	}
	w.project.Spec.Surfaces = &v1alpha1.ProjectSurfaces{CLI: &v1alpha1.CLISurface{
		Repository: "lib", Build: []string{"go", "build", "-o", "/work/bin/tool", "./cmd/tool"},
		Binary: "/work/bin/tool",
	}}
	return w
}

// TestSurfaceAbsentAddsNothing: a Project without cli or library adds no
// surface line, so S1 reports are unchanged.
func TestSurfaceAbsentAddsNothing(t *testing.T) {
	for _, c := range newWorld(t).run().Checks {
		if strings.HasPrefix(c.Name, "surface") {
			t.Errorf("unexpected %s line", c.Name)
		}
	}
}

func TestSurfaceCLIPasses(t *testing.T) {
	w := agentsWorld(t)
	w.setting("intent-controller", profilesOn, "true")
	w.setting("intent-controller", profilesAt, profilesJSON)
	r := w.run()
	expect(t, r, CheckSurface, "lib", checkreport.Pass, "cli", "lib")
	expect(t, r, CheckSurfaceBuild, "lib", checkreport.Pass, "/work/bin/tool", "10m")
	expect(t, r, CheckSurfaceProfile, "lib", checkreport.Pass, "acme-go")
	expect(t, r, CheckAgentImage, "lib", checkreport.Pass)
}

func TestSurfaceShape(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*world)
		key    string
		status checkreport.Status
		want   []string
	}{
		{"library", func(w *world) {
			w.project.Spec.Surfaces = &v1alpha1.ProjectSurfaces{Library: &v1alpha1.LibrarySurface{
				Repository: "lib", Language: v1alpha1.LibraryLanguageGo}}
		}, "lib", checkreport.Pass, []string{"library", "go"}},
		{"library language", func(w *world) {
			w.project.Spec.Surfaces = &v1alpha1.ProjectSurfaces{Library: &v1alpha1.LibrarySurface{
				Repository: "lib", Language: "python"}}
		}, "", checkreport.Fail, []string{"python", "go"}},
		{"both", func(w *world) {
			w.project.Spec.Surfaces.Library = &v1alpha1.LibrarySurface{Language: v1alpha1.LibraryLanguageGo}
		}, "", checkreport.Fail, []string{"at most one"}},
		{"beside browser", func(w *world) {
			w.project.Spec.Surfaces.Browser = &v1alpha1.BrowserSurface{Entry: "/"}
		}, "", checkreport.Fail, []string{v1alpha1.ReasonSurfaceCombinationUnsupported, "browser"}},
		{"beside preview", func(w *world) {
			w.repo("web").Preview = &v1alpha1.ProjectRepositoryPreview{Path: "/"}
		}, "", checkreport.Fail, []string{v1alpha1.ReasonSurfaceCombinationUnsupported, "preview"}},
		{"unknown repository", func(w *world) { w.project.Spec.Surfaces.CLI.Repository = "nope" },
			"", checkreport.Fail, []string{`"nope"`, "web, api, lib"}},
		{"no repository, many", func(w *world) { w.project.Spec.Surfaces.CLI.Repository = "" },
			"", checkreport.Fail, []string{"names no repository", "3"}},
		{"no repository, one", func(w *world) {
			w.project.Spec.Surfaces.CLI.Repository = ""
			w.project.Spec.Repositories = w.project.Spec.Repositories[2:]
		}, "lib", checkreport.Pass, []string{"lib"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := agentsWorld(t)
			tc.mutate(w)
			expect(t, w.run(), CheckSurface, tc.key, tc.status, tc.want...)
		})
	}
}

func TestSurfaceBuild(t *testing.T) {
	long := strings.Repeat("x", 257)
	cases := []struct {
		name   string
		mutate func(*v1alpha1.CLISurface)
		status checkreport.Status
		want   []string
	}{
		{"no write under bin", func(c *v1alpha1.CLISurface) { c.Build = []string{"go", "build", "./..."} },
			checkreport.Fail, []string{"/work/bin"}},
		{"empty", func(c *v1alpha1.CLISurface) { c.Build = nil }, checkreport.Fail, []string{"1 to 16"}},
		{"too many", func(c *v1alpha1.CLISurface) {
			c.Build = append(make([]string, 16), "-o", "/work/bin/x")
		}, checkreport.Fail, []string{"1 to 16"}},
		{"long arg", func(c *v1alpha1.CLISurface) { c.Build = []string{long, "/work/bin/x"} },
			checkreport.Fail, []string{"256"}},
		{"control char", func(c *v1alpha1.CLISurface) { c.Build = []string{"a\nb", "/work/bin/x"} },
			checkreport.Fail, []string{"control"}},
		{"binary elsewhere", func(c *v1alpha1.CLISurface) { c.Binary = "/usr/bin/tool" },
			checkreport.Fail, []string{"binary", "/work/bin"}},
		{"binary too deep", func(c *v1alpha1.CLISurface) { c.Binary = "/work/bin/a/b/c" },
			checkreport.Fail, []string{"binary"}},
		{"timeout low", func(c *v1alpha1.CLISurface) { c.BuildTimeout = &metav1.Duration{Duration: time.Second} },
			checkreport.Fail, []string{"1m", "20m"}},
		{"timeout high", func(c *v1alpha1.CLISurface) { c.BuildTimeout = &metav1.Duration{Duration: time.Hour} },
			checkreport.Fail, []string{"1m", "20m"}},
		{"timeout set", func(c *v1alpha1.CLISurface) { c.BuildTimeout = &metav1.Duration{Duration: 15 * time.Minute} },
			checkreport.Pass, []string{"15m"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := agentsWorld(t)
			tc.mutate(w.project.Spec.Surfaces.CLI)
			expect(t, w.run(), CheckSurfaceBuild, "lib", tc.status, tc.want...)
		})
	}
}

// TestSurfaceBuildLibrary: a library surface has no build line.
func TestSurfaceBuildLibrary(t *testing.T) {
	w := agentsWorld(t)
	w.project.Spec.Surfaces = &v1alpha1.ProjectSurfaces{Library: &v1alpha1.LibrarySurface{
		Repository: "lib", Language: v1alpha1.LibraryLanguageGo}}
	for _, c := range w.run().Checks {
		if c.Name == CheckSurfaceBuild {
			t.Errorf("library has a build line: %s", c.Reason)
		}
	}
}

// TestSurfaceImage: the surface's repository needs an accepted image even
// when the Project does not require one elsewhere.
func TestSurfaceImage(t *testing.T) {
	no := false
	w := agentsWorld(t)
	w.project.Spec.RequireRepositoryImage = &no
	delete(w.github.files[w.repo("lib").URL], ".maul/agent.yaml")
	delete(w.github.files[w.repo("api").URL], ".maul/agent.yaml")
	r := w.run()
	expect(t, r, CheckAgentImage, "lib", checkreport.Fail, "declares no agent image", "cli surface",
		v1alpha1.ReasonRepositoryImageRequired)
	// The other repositories keep the Project's own rule.
	expect(t, r, CheckAgentImage, "api", checkreport.Skip, "default runner image")
}

// TestSurfaceImageConsequence: a FAIL the surface alone causes names what
// it holds (the evaluation), never a build block requireRepositoryImage:
// false does not impose; a Project that requires one keeps S1's words and
// adds the surface's.
func TestSurfaceImageConsequence(t *testing.T) {
	const blocks = "every build and revise run blocks with ImageRequired"
	no := false
	cases := []struct {
		name    string
		require *bool
		mutate  func(*world)
		want    []string
		notWant string
	}{
		{"undeclared, surface only", &no, func(w *world) {
			delete(w.github.files[w.repo("lib").URL], ".maul/agent.yaml")
		}, []string{"cli surface", v1alpha1.ReasonRepositoryImageRequired, "default runner image"}, blocks},
		{"source off, surface only", &no, func(w *world) {
			w.setting("source-controller", keyRepositoryImages, "false")
		}, []string{"cli surface", v1alpha1.ReasonRepositoryImageRequired, "default runner image"}, blocks},
		{"undeclared, both", nil, func(w *world) {
			delete(w.github.files[w.repo("lib").URL], ".maul/agent.yaml")
		}, []string{"requireRepositoryImage", blocks, "cli surface", v1alpha1.ReasonRepositoryImageRequired}, ""},
		{"source off, both", nil, func(w *world) {
			w.setting("source-controller", keyRepositoryImages, "false")
		}, []string{blocks, "cli surface", v1alpha1.ReasonRepositoryImageRequired}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := agentsWorld(t)
			w.project.Spec.RequireRepositoryImage = tc.require
			tc.mutate(w)
			r := w.run()
			expect(t, r, CheckAgentImage, "lib", checkreport.Fail, tc.want...)
			if c := line(t, r, CheckAgentImage, "lib"); tc.notWant != "" && strings.Contains(c.Reason, tc.notWant) {
				t.Errorf("reason claims %q: %s", tc.notWant, c.Reason)
			}
		})
	}
}

func TestSurfaceProfile(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*world)
		status checkreport.Status
		want   []string
	}{
		{"matches", func(w *world) {
			w.setting("intent-controller", profilesOn, "true")
			w.setting("intent-controller", profilesAt, profilesJSON)
		}, checkreport.Pass, []string{"acme-go", "go", "tool pod"}},
		{"disabled", func(w *world) {}, checkreport.Pass, []string{"no network profile", "tool pod"}},
		{"no match", func(w *world) {
			w.setting("intent-controller", profilesOn, "true")
			w.setting("intent-controller", profilesAt, strings.ReplaceAll(profilesJSON, "shop-lib", "other"))
		}, checkreport.Pass, []string{"no network profile", "no-match"}},
		{"ambiguous", func(w *world) {
			w.setting("intent-controller", profilesOn, "true")
			w.setting("intent-controller", profilesAt, twoProfiles)
		}, checkreport.Fail, []string{"ambiguous"}},
		{"unparsable", func(w *world) {
			w.setting("intent-controller", profilesOn, "true")
			w.setting("intent-controller", profilesAt, "{")
		}, checkreport.Fail, []string{profilesAt}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := agentsWorld(t)
			tc.mutate(w)
			expect(t, w.run(), CheckSurfaceProfile, "lib", tc.status, tc.want...)
		})
	}
}

func TestSurfaceProfileNoIntentSettings(t *testing.T) {
	w := agentsWorld(t)
	w.drop(func(o client.Object) bool { return o.GetLabels()[labelName] == "intent-controller" })
	expect(t, w.run(), CheckSurfaceProfile, "lib", checkreport.Skip, "intent-controller")
}
