// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerguard

import (
	"context"
	"math/rand"
	"net/url"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// profilesJSON is two profiles: go for patchy-target and patchy-preview-demo,
// and wide for every devthenet-labs repository named patchy-preview-*, so
// patchy-preview-demo matches both.
const profilesJSON = `[
 {"name":"go","route":"go","port":8090,"repositories":["devthenet-labs/patchy-target",
  "devthenet-labs/patchy-preview-demo"],"modules":["github.com/google/uuid"],"bakedModCache":"/opt/go/pkg/mod"},
 {"name":"wide","route":"go","port":8091,"repositories":["devthenet-labs/patchy-preview-.*"],
  "modules":["golang.org/x"],"bakedModCache":""}]`

const (
	targetURL    = "https://github.com/devthenet-labs/patchy-target"
	ambiguousURL = "https://github.com/devthenet-labs/patchy-preview-demo"
	unmatchedURL = "https://github.com/devthenet-labs/patchy"
	wrongHostURL = "https://gitlab.com/devthenet-labs/patchy-target"
)

func testProfiles(t testing.TB) netprofile.Set {
	t.Helper()
	set, err := netprofile.Parse([]byte(profilesJSON))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func proxyBase(t testing.TB) *url.URL {
	t.Helper()
	u, err := url.Parse("http://patchy-egress-proxy.patchy.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func remediateSpec(image string) jobs.Spec {
	return jobs.Spec{Phase: "remediate", Kind: "remediation", RunnerImage: image}
}

func TestProfile(t *testing.T) {
	on := Guard{Enabled: true, ProfilesEnabled: true, Profiles: testProfiles(t), ProxyBase: proxyBase(t)}
	off := on
	off.ProfilesEnabled = false
	tripped := on
	tripped.Breaker = trippedBreaker()
	tests := []struct {
		name        string
		guard       Guard
		spec        jobs.Spec
		repoURL     string
		wantApplied string
		wantSkip    string
	}{
		{"one match", on, remediateSpec(pinned), targetURL, "go", ""},
		{"case and .git", on, remediateSpec(pinned), "https://GitHub.com/DevTheNet-Labs/Patchy-Target.git", "go", ""},
		{"build of kind intent", on, jobs.Spec{Phase: "build", Kind: "intent", RunnerImage: pinned}, targetURL, "go", ""},
		{"no image", on, remediateSpec(""), targetURL, "", ""},
		{"investigate", on, jobs.Spec{Phase: "investigate", Kind: "investigation", RunnerImage: pinned}, targetURL,
			"", ""},
		{"plan", on, jobs.Spec{Phase: "plan", Kind: "intent", RunnerImage: pinned}, targetURL, "", ""},
		{"disabled", off, remediateSpec(pinned), targetURL, "", SkipProfilesDisabled},
		{"disabled with no profiles", Guard{Enabled: true}, remediateSpec(pinned), targetURL, "", ""},
		{"breaker", tripped, remediateSpec(pinned), targetURL, "", SkipBreaker},
		{"no match (anchored)", on, remediateSpec(pinned), unmatchedURL, "", ""},
		{"wrong host", on, remediateSpec(pinned), wrongHostURL, "", ""},
		{"ambiguous", on, remediateSpec(pinned), ambiguousURL, "", SkipProfileAmbiguous},
		{"unparseable", on, remediateSpec(pinned), "not a url", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := tt.spec
			applied, skipped := tt.guard.Profile(&spec, tt.repoURL)
			if applied != tt.wantApplied || skipped != tt.wantSkip {
				t.Fatalf("Profile = (%q, %q), want (%q, %q)", applied, skipped, tt.wantApplied, tt.wantSkip)
			}
			if tt.wantApplied == "" {
				if spec.NetworkProfile != nil {
					t.Errorf("spec.NetworkProfile = %+v, want nil", spec.NetworkProfile)
				}
				return
			}
			want := jobs.NetworkProfile{Name: "go", Route: "go",
				ProxyURL:      "http://patchy-egress-proxy.patchy.svc.cluster.local:8090",
				BakedModCache: "/opt/go/pkg/mod"}
			if spec.NetworkProfile == nil || *spec.NetworkProfile != want {
				t.Errorf("spec.NetworkProfile = %+v, want %+v", spec.NetworkProfile, want)
			}
		})
	}
}

// stageSpec is how the controllers shape a stage's Job: remediation
// launches remediate Jobs of kind remediation, and the intent controller
// every stage of kind intent, a revise round (review feedback or a
// check-fix) in the build phase (its jobPhase table).
var stageSpec = map[string]jobs.Spec{
	"investigate": {Phase: "investigate", Kind: "investigation"},
	"remediate":   {Phase: "remediate", Kind: "remediation"},
	"plan":        {Phase: "plan", Kind: "intent"},
	"build":       {Phase: "build", Kind: "intent"},
	"revise":      {Phase: "build", Kind: "intent"},
	"check-fix":   {Phase: "build", Kind: "intent"},
}

// TestPropertyProfileOnlyWhereAllowed: over stage × image (none, an
// accepted pin, a revived finding's) × repository URL (one match, none,
// two, a wrong host, case and .git variants) × the profile kill switch ×
// the breaker, a launch that pins as the controllers pin (Pin for a
// Finding's stages, PinFor for an intent's) and then asks Profile ends with
// spec.NetworkProfile set iff the stage is remediate, build, revise or
// check-fix, an accepted image is pinned, exactly one profile matches,
// profiles are on and the breaker is closed. Fed to jobs.Create, the
// result is either a Job carrying exactly that profile or a Job carrying
// none, never a refusal. Seeded so the gate is deterministic.
func TestPropertyProfileOnlyWhereAllowed(t *testing.T) {
	rng := rand.New(rand.NewSource(20261014))
	stages := []string{"investigate", "remediate", "plan", "build", "revise", "check-fix"}
	urls := []struct {
		url   string
		match bool
	}{
		{targetURL, true},
		{"https://github.com/devthenet-labs/patchy-target.git", true},
		{"https://GITHUB.com/DevTheNet-Labs/patchy-target", true},
		{unmatchedURL, false},
		{ambiguousURL, false},
		{wrongHostURL, false},
	}
	accepted := &v1alpha1.RunnerImage{Manifest: ".patchy/agent.yaml", Image: pinned, SearchPath: "/usr/bin:/bin"}
	client := jobs.New(fake.NewClientset(), jobs.Config{
		Namespace: "patchy-agents", AllowRepositoryImages: true, NetworkProfiles: true, EphemeralStorage: "8Gi",
		Runners: map[string]jobs.Runner{"claude": {Image: "runner:1", Brokered: true,
			Inject: []string{"agent-runner", "claude"}}},
	}, nil)
	for i := range 3000 {
		stage := stages[rng.Intn(len(stages))]
		image := rng.Intn(3) // 0 none, 1 accepted, 2 revived finding
		u := urls[rng.Intn(len(urls))]
		g := Guard{Enabled: true, ProfilesEnabled: rng.Intn(2) == 0, Profiles: testProfiles(t),
			ProxyBase: proxyBase(t)}
		breaker := rng.Intn(4) == 0
		if breaker {
			g.Breaker = trippedBreaker()
		}
		repo := repoWith(nil)
		if image > 0 {
			repo = repoWith(accepted)
		}
		fnd := findingThrough(v1alpha1.PhaseInvestigating)
		if image == 2 {
			fnd = findingThrough(v1alpha1.PhaseInvestigating, v1alpha1.PhaseHandedOff, v1alpha1.PhaseQueued)
		}
		spec := stageSpec[stage]
		spec.Harness, spec.Finding, spec.Attempt = "claude", "f-1", 1
		if spec.Kind == "intent" {
			g.PinFor(&spec, repo)
		} else {
			g.Pin(&spec, repo, fnd)
		}
		applied, _ := g.Profile(&spec, u.url)

		allowed := stage == "remediate" || stage == "build" || stage == "revise" || stage == "check-fix"
		pinnedImage := image == 1 || image == 2 && spec.Kind == "intent"
		want := allowed && pinnedImage && u.match && g.ProfilesEnabled && !breaker
		if got := spec.NetworkProfile != nil; got != want || (applied != "") != want {
			t.Fatalf("iteration %d: stage %s image %d url %s enabled %v breaker %v: profile %+v (%q), want %v",
				i, stage, image, u.url, g.ProfilesEnabled, breaker, spec.NetworkProfile, applied, want)
		}

		_, ref, err := client.Create(context.Background(), spec)
		if err != nil {
			t.Fatalf("iteration %d: Create: %v", i, err)
		}
		if wantRef := map[bool]string{true: "go"}[want]; ref.NetworkProfile != wantRef {
			t.Fatalf("iteration %d: Create recorded profile %q, want %q", i, ref.NetworkProfile, wantRef)
		}
		if err := client.Delete(context.Background(), jobs.NameFor(spec.Finding, spec.Kind, 1)); err != nil {
			t.Fatalf("iteration %d: Delete: %v", i, err)
		}
	}
}
