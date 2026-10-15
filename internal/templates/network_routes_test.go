// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// The grants the route fixtures describe, as a pod reads them.
func routeNPM() netprofile.PodGrant {
	return netprofile.PodGrant{Registry: "ca-npm", Ecosystem: netprofile.EcosystemNPM, AnyPackage: true}
}

func routePyPI() netprofile.PodGrant {
	return netprofile.PodGrant{Registry: "ca-pypi", Ecosystem: netprofile.EcosystemPyPI,
		Packages: []string{"six", "requests", "urllib3", "idna", "certifi", "charset-normalizer"}}
}

func routeGo() netprofile.PodGrant {
	return netprofile.PodGrant{Registry: "art-go", Ecosystem: netprofile.EcosystemGo,
		Packages: []string{"acme.example/"}, PrivateModules: []string{"acme.example/", "corp.example/x/"}}
}

// webRoutes is a registry-only profile granting npm and pypi; goRegRoutes
// the public go route beside a go registry; switchRoutes every grant with
// its switches on and an allowlist longer than the pod is told.
func webRoutes() NetworkRoutes {
	return NetworkRoutes{Grants: []netprofile.PodGrant{routeNPM(), routePyPI()}}
}

func goRegRoutes() NetworkRoutes {
	return NetworkRoutes{PublicGo: true, Grants: []netprofile.PodGrant{routeGo()}}
}

func switchRoutes() NetworkRoutes {
	g, n, p := routeGo(), routeNPM(), routePyPI()
	g.AnyPackage, g.Packages, g.AllowPseudoVersions, g.PrivateModules = true, nil, true, nil
	n.AnyPackage, n.Packages, n.AllowPrerelease, n.Scripts = false, []string{"left-pad", "@acme/*"}, true, true
	p.Sdists = true
	for i := len(p.Packages); i < netprofile.MaxPodPackages; i++ {
		p.Packages = append(p.Packages, fmt.Sprintf("pkg-%d", i))
	}
	p.MorePackages = 7
	return NetworkRoutes{Grants: []netprofile.PodGrant{g, n, p}}
}

func renderRemediateRoutes(profile string, r NetworkRoutes) (string, error) {
	return RenderRemediatePrompt(RemediatePrompt{
		IssuePath: "/workspace/input/issue.md", InvestigationPath: "/workspace/input/investigation.md",
		ReportPath: "/workspace/reports/remediation.md", CommitScriptPath: "/workspace/commit.sh",
		NetworkProfile: profile, NetworkRoutes: r,
	})
}

func renderBuildRoutes(profile string, r NetworkRoutes) (string, error) {
	return RenderBuildPrompt(BuildPrompt{
		PlanPath: "/workspace/input/investigation.md", ReportPath: "/workspace/reports/build.md",
		CommitScriptPath: "/workspace/commit.sh", Limits: testBuildLimits,
		NetworkProfile: profile, NetworkRoutes: r,
	})
}

// TestNetworkRoutesPromptGoldens pins the remediate and build prompts of a
// run whose profile holds registry grants: npm and pip, a go registry
// beside the public route, and every switch on.
func TestNetworkRoutesPromptGoldens(t *testing.T) {
	for _, tt := range []struct {
		golden  string
		render  func(string, NetworkRoutes) (string, error)
		profile string
		routes  NetworkRoutes
	}{
		{"prompt_build_network_routes.md", renderBuildRoutes, "web", webRoutes()},
		{"prompt_remediate_network_routes.md", renderRemediateRoutes, "web", webRoutes()},
		{"prompt_remediate_network_routes_go.md", renderRemediateRoutes, "go", goRegRoutes()},
		{"prompt_build_network_routes_switches.md", renderBuildRoutes, "all", switchRoutes()},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			got, err := tt.render(tt.profile, tt.routes)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tt.golden, got)
		})
	}
}

// TestNetworkRoutesPrompt: a profile with grants is described per
// ecosystem, under the opening sentence and above the shared closing,
// never as offline and never with step 1's Go paragraph alone; a run told
// to stop over a refused dependency is told once. Routes without grants
// change nothing: the step-1 and no-profile prompts are byte-identical.
func TestNetworkRoutesPrompt(t *testing.T) {
	for _, render := range []func(string, NetworkRoutes) (string, error){renderBuildRoutes, renderRemediateRoutes} {
		got, err := render("web", webRoutes())
		if err != nil {
			t.Fatal(err)
		}
		flat := strings.Join(strings.Fields(got), " ")
		for _, want := range []string{
			"the operator's dependency route `web` for: npm and PyPI.",
			"**npm**: `npm install <pkg>@<version>` and `npm ci` work through the registry `ca-npm`",
			"which packages exist is the registry's decision", "Only release versions (x.y.z)",
			"Install scripts do not run", "`package-lock.json` keeps the registry's own URLs",
			"**pip**:", "`six`, `requests`", "wheels only (no source builds)", "`--no-emit-index-url`",
			"A refused package answers `403` (`410` for Go) with a reason",
			"A reviewer will check every new dependency",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("the routes prompt lacks %q:\n%s", want, got)
			}
		}
		for _, banned := range []string{"no network access", "**Go**", "Go modules can be fetched"} {
			if strings.Contains(flat, banned) {
				t.Errorf("the npm/pip routes prompt says %q:\n%s", banned, got)
			}
		}
		if n := strings.Count(flat, "stop and report it"); n != 1 {
			t.Errorf("the routes prompt says stop and report %d times, want once", n)
		}

		for _, profile := range []string{"go", ""} {
			step1, err := render(profile, NetworkRoutes{})
			if err != nil {
				t.Fatal(err)
			}
			if same, err := render(profile, NetworkRoutes{PublicGo: true}); err != nil || same != step1 {
				t.Errorf("routes without grants changed the %q prompt (err %v)", profile, err)
			}
		}
	}

	got, err := renderRemediateRoutes("go", goRegRoutes())
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(got), " ")
	for _, want := range []string{"for: Go.", "**Go**", "the operator's registry `art-go` first",
		"for modules under `acme.example/`", "Private modules under `acme.example/` and `corp.example/x/` are not",
		"`checksum_not_public`", "never disable checksum verification"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the go registry prompt lacks %q:\n%s", want, got)
		}
	}

	got, err = renderBuildRoutes("all", switchRoutes())
	if err != nil {
		t.Fatal(err)
	}
	flat = strings.Join(strings.Fields(got), " ")
	for _, want := range []string{"for: Go, npm and PyPI.", "which decides which modules exist",
		"pseudo-) versions of those modules can be fetched too", "Prerelease versions can be fetched",
		"Install scripts run, with no network", "`left-pad`, `@acme/*`", "and 7 more",
		"wheels and source builds", "the build backend (setuptools, hatchling, …) must itself be allowed"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the switches prompt lacks %q:\n%s", want, got)
		}
	}
}

// TestNetworkRoutesParagraphWraps: the paragraph is wrapped like the
// hand-wrapped templates around it, every line within promptWidth and
// indented as a bullet continuation.
func TestNetworkRoutesParagraphWraps(t *testing.T) {
	for _, r := range []NetworkRoutes{webRoutes(), goRegRoutes(), switchRoutes()} {
		for _, line := range strings.Split(networkRoutesPrompt("web", r, "plan"), "\n") {
			if len(line) > promptWidth || !strings.HasPrefix(line, "  ") {
				t.Errorf("line of %d columns: %q", len(line), line)
			}
		}
	}
}

// routePlanComments are plan comments whose builds hold registry grants: a
// one-repository plan's, and a two-repository plan's where the first
// repository's build has a step-1 profile, the second a registry profile.
func routePlanComments() (single, multi PlanComment) {
	single, multi = planProfileComments()
	single.NetworkProfiles = []string{"web"}
	single.NetworkRoutes = []NetworkRoutes{webRoutes()}
	multi.NetworkProfiles = []string{"go", "web"}
	multi.NetworkRoutes = []NetworkRoutes{{PublicGo: true}, webRoutes()}
	return single, multi
}

// TestPlanCommentNetworkRoutes pins the dependency advice of plans whose
// builds hold registry grants, and checks it names each build's routes and
// policy, keeps patchy's own advice prefixes (SamePlanComment), and that
// routes without grants leave step 1's advice byte-identical.
func TestPlanCommentNetworkRoutes(t *testing.T) {
	single, multi := routePlanComments()
	offline := multi
	offline.NetworkProfiles = []string{"", "web"}
	offline.NetworkRoutes = []NetworkRoutes{{}, webRoutes()}
	for name, pc := range map[string]PlanComment{
		"intent_plan_network_routes.md":                      single,
		"intent_plan_repositories_network_routes.md":         multi,
		"intent_plan_repositories_network_routes_offline.md": offline,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RenderPlanComment(pc)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name, got)
			for _, want := range []string{"network profile `web`", "any npm package `ca-npm` serves",
				"the PyPI projects the operator allows from `ca-pypi`", "before you approve"} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q:\n%s", want, got)
				}
			}
			if same, err := SamePlanComment(pc, got); err != nil || !same {
				t.Errorf("SamePlanComment of its own render = %v, %v", same, err)
			}
			// A retry that finds the step-1 advice posted is the same
			// comment, and the other way round.
			step1 := pc
			step1.NetworkRoutes = nil
			posted, err := RenderPlanComment(step1)
			if err != nil {
				t.Fatal(err)
			}
			if same, err := SamePlanComment(pc, posted); err != nil || !same {
				t.Errorf("SamePlanComment across the advice = %v, %v", same, err)
			}
		})
	}
	if !strings.Contains(mustRenderPlan(t, multi), "tagged releases of public Go module paths the operator allows") {
		t.Error("the step-1 profile's build is not described beside the registry one")
	}
	if !strings.Contains(mustRenderPlan(t, offline), "Every other build runs offline") {
		t.Error("the build without a profile is not called offline")
	}

	for _, pc := range []PlanComment{single, multi} {
		bare := pc
		bare.NetworkRoutes = nil
		noGrants := pc
		noGrants.NetworkRoutes = make([]NetworkRoutes, len(pc.NetworkProfiles))
		for i := range noGrants.NetworkRoutes {
			noGrants.NetworkRoutes[i].PublicGo = true
		}
		if a, b := mustRenderPlan(t, bare), mustRenderPlan(t, noGrants); a != b {
			t.Errorf("routes without grants changed the plan comment:\n--- without\n%s\n--- with\n%s", a, b)
		}
	}
}

func mustRenderPlan(t *testing.T, pc PlanComment) string {
	t.Helper()
	got, err := RenderPlanComment(pc)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestNetworkRoutesPRBodies pins the pull request line of a run whose
// profile holds grants: it names each ecosystem with its registry; without
// grants the line is step 1's.
func TestNetworkRoutesPRBodies(t *testing.T) {
	routes := NetworkRoutes{PublicGo: true, Grants: []netprofile.PodGrant{routeNPM(), routePyPI()}}
	finding, err := PRBody(123, "remediation report here", false, nil, "web", routes)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "pr_body_network_routes.md", finding)
	intent, err := RenderIntentPRBody(IntentPRBody{
		IntentRepository: "devthenet-labs/intents", IssueNumber: 1,
		Summary:      "Add GET /version returning {sha, built} as JSON",
		PlanRevision: 2, PlanDigest: PlanDigest([]byte(testPlan)), ApprovedBy: "peter",
		NetworkProfile: "goreg", NetworkRoutes: goRegRoutes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "intent_pr_body_network_routes.md", intent)
	for body, want := range map[string]string{
		finding: "network profile `web` (Go, npm via `ca-npm`, PyPI via `ca-pypi`)",
		intent:  "network profile `goreg` (Go via `art-go` and the public route)",
	} {
		if !strings.Contains(body, want) || !strings.Contains(body, "lockfiles") {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}

	step1, err := PRBody(123, "remediation report here", false, nil, "go", NetworkRoutes{})
	if err != nil {
		t.Fatal(err)
	}
	if same, err := PRBody(123, "remediation report here", false, nil, "go",
		NetworkRoutes{PublicGo: true}); err != nil || same != step1 {
		t.Errorf("routes without grants changed the step-1 line (err %v):\n%s", err, same)
	}
	if none, err := PRBody(123, "remediation report here", false, nil, "", routes); err != nil ||
		strings.Contains(none, "network profile") {
		t.Errorf("routes without a profile name added a line (err %v):\n%s", err, none)
	}
}

// TestRoutesOfAndPodRoutes: the controller's view of a profile and the
// pod's view of the same Job agree on what the texts say.
func TestRoutesOfAndPodRoutes(t *testing.T) {
	p := netprofile.Profile{Name: "web", Route: netprofile.RouteGo, Registries: []netprofile.Grant{
		{Name: "ca-npm", Ecosystem: netprofile.EcosystemNPM, PackagePolicy: netprofile.PackagePolicyRegistry},
	}}
	ctrl := RoutesOf(p)
	pod := PodRoutes([]string{netprofile.EcosystemGo, netprofile.EcosystemNPM}, ctrl.Grants)
	if ctrl.routeList() != pod.routeList() || ctrl.ecosystems() != pod.ecosystems() {
		t.Errorf("controller %+v and pod %+v disagree", ctrl, pod)
	}
	if r := PodRoutes([]string{netprofile.EcosystemGo}, []netprofile.PodGrant{routeGo()}); r.PublicGo {
		t.Errorf("a listed go with a go grant read as the public route: %+v", r)
	}
}

// TestPlanPromptBuildRoutes pins what a plan prompt says when a build of
// the plan gets a profile with registry grants: what each build can fetch,
// per ecosystem and registry, instead of step 1's Go sentence; a set of
// step-1 profiles renders the #200 prompt byte for byte.
func TestPlanPromptBuildRoutes(t *testing.T) {
	web := webRoutes()
	one := func(b []BuildProfile) (string, error) {
		return RenderPlanPrompt(PlanPrompt{
			IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md",
			Intent: testPlanRequest, BuildMaxTurns: 150, BuildTokenBudget: 800000, Limits: testPlanLimits,
			BuildProfiles: b,
		})
	}
	for _, tt := range []struct {
		golden, want string
		render       func([]BuildProfile) (string, error)
		profiles     []BuildProfile
	}{
		{"prompt_plan_network_routes.md", "This repository's build runs with the operator's network profile `web`, " +
			"so while it builds it can fetch any npm package `ca-npm` serves and the PyPI projects the operator " +
			"allows from `ca-pypi`", one, []BuildProfile{{URL: testAPIURL, Profile: "web", Routes: &web}}},
		{"prompt_plan_trees_network_routes.md", "`" + testWebURL + "` (profile `go`: tagged releases of Go module " +
			"paths the operator allows), `" + testAPIURL + "` (profile `web`: any npm package `ca-npm` serves; the " +
			"PyPI projects the operator allows from `ca-pypi`)", renderTestTreesPlanPromptWith,
			[]BuildProfile{{URL: testWebURL, Profile: "go"}, {URL: testAPIURL, Profile: "web", Routes: &web}}},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			got, err := tt.render(tt.profiles)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tt.golden, got)
			if flat := strings.Join(strings.Fields(got), " "); !strings.Contains(flat, tt.want) ||
				!strings.Contains(flat, "must already be in the image") {
				t.Errorf("prompt lacks %q:\n%s", tt.want, got)
			}
		})
	}

	// Routes without grants, or grants that do not validate, are not what a
	// controller writes: the build is described as step 1 would.
	bad := NetworkRoutes{Grants: []netprofile.PodGrant{{Registry: "ca-npm", Ecosystem: "npm"}}}
	step1, err := one([]BuildProfile{{URL: testAPIURL, Profile: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*NetworkRoutes{{PublicGo: true}, &bad} {
		if got, err := one([]BuildProfile{{URL: testAPIURL, Profile: "go", Routes: r}}); err != nil || got != step1 {
			t.Errorf("routes %+v changed the step-1 prompt (err %v)", r, err)
		}
	}
	got, err := DecodeBuildProfiles(EncodeBuildProfiles([]BuildProfile{{URL: testAPIURL, Profile: "web", Routes: &web}}))
	if err != nil || len(got) != 1 || got[0].Routes == nil || fmt.Sprint(*got[0].Routes) != fmt.Sprint(web) {
		t.Errorf("round trip = %+v, %v; want the routes back", got, err)
	}
	if enc := EncodeBuildProfiles([]BuildProfile{{URL: testAPIURL, Profile: "go"}}); strings.Contains(enc, "routes") {
		t.Errorf("a step-1 profile's JSON grew a routes key: %s", enc)
	}
}
