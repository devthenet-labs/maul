// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// The tunnel routes the fixtures describe (step 3): a tunnel-only Python
// profile, the Go route with an npm tunnel, and a profile with every route
// kind at once.
func tunnelRoutes() NetworkRoutes {
	return NetworkRoutes{TunnelHosts: []string{"files.pythonhosted.org", "pypi.org"}}
}

func goTunnelRoutes() NetworkRoutes {
	return NetworkRoutes{PublicGo: true, TunnelHosts: []string{"registry.npmjs.org"}}
}

func allRoutes() NetworkRoutes {
	return NetworkRoutes{PublicGo: true, Grants: []netprofile.PodGrant{routeNPM()},
		TunnelHosts: []string{"files.pythonhosted.org", "pypi.org"}}
}

// TestNetworkTunnelPromptGoldens pins the remediate and build prompts of a
// run whose profile has tunnel hosts (§4.6 of the step-3 spec): tunnel
// only, the Go route beside a tunnel, and grants beside both.
func TestNetworkTunnelPromptGoldens(t *testing.T) {
	for _, tt := range []struct {
		golden  string
		render  func(string, NetworkRoutes) (string, error)
		profile string
		routes  NetworkRoutes
	}{
		{"prompt_build_network_tunnel.md", renderBuildRoutes, "py", tunnelRoutes()},
		{"prompt_remediate_network_tunnel.md", renderRemediateRoutes, "py", tunnelRoutes()},
		{"prompt_build_network_go_tunnel.md", renderBuildRoutes, "go", goTunnelRoutes()},
		{"prompt_remediate_network_go_tunnel.md", renderRemediateRoutes, "go", goTunnelRoutes()},
		{"prompt_build_network_all_routes.md", renderBuildRoutes, "web", allRoutes()},
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

// TestNetworkTunnelPrompt: a tunnel's prompt names every host, the probe
// that reads the proxy's verdict and the report it goes in, and the
// settings not to touch; a tunnel-only profile is never described as a Go
// route nor as offline; the Go route beside a tunnel keeps its Go text;
// and a profile without tunnel hosts renders exactly as before.
func TestNetworkTunnelPrompt(t *testing.T) {
	for _, render := range []func(string, NetworkRoutes) (string, error){renderBuildRoutes, renderRemediateRoutes} {
		got, err := render("py", tunnelRoutes())
		if err != nil {
			t.Fatal(err)
		}
		flat := strings.Join(strings.Fields(got), " ")
		for _, want := range []string{
			"`files.pythonhosted.org` and `pypi.org`",
			"`403 egress-proxy: …`",
			"Do not change `HTTPS_PROXY`, `NO_PROXY`",
			"never disable TLS verification",
			"Never publish or upload a package",
			"`curl -sSv -x \"$HTTPS_PROXY\" https://<host>/ 2>&1 | grep -i egress-proxy`",
			"set_tunnel(\"<host>\", 443)",
			"put the host and the proxy's exact line in your final message and in your report",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("tunnel prompt lacks %q:\n%s", want, got)
			}
		}
		for _, never := range []string{"GOPROXY", "**Go**", "for Go", "`410`", "no network access",
			"no **network access**"} {
			if strings.Contains(flat, never) {
				t.Errorf("tunnel-only prompt says %q:\n%s", never, got)
			}
		}
		if n := strings.Count(flat, "needs is refused"); n != 1 {
			t.Errorf("tunnel-only prompt says what to do with a refused dependency %d times, want once:\n%s", n, got)
		}

		// A grant-only profile without the Go route has no Go answer to
		// describe either.
		grants, err := render("web", webRoutes())
		if err != nil {
			t.Fatal(err)
		}
		if flat := strings.Join(strings.Fields(grants), " "); strings.Contains(flat, "for Go") ||
			strings.Contains(flat, "`410`") {
			t.Errorf("npm+pypi prompt describes the Go refusal:\n%s", grants)
		}

		withGo, err := render("go", goTunnelRoutes())
		if err != nil {
			t.Fatal(err)
		}
		flat = strings.Join(strings.Fields(withGo), " ")
		for _, want := range []string{"**Go**", "Do not change `GOPROXY`", "`registry.npmjs.org`", "egress-proxy",
			"(`410` for Go)"} {
			if !strings.Contains(flat, want) {
				t.Errorf("go+tunnel prompt lacks %q:\n%s", want, withGo)
			}
		}

		step1, err := render("go", NetworkRoutes{})
		if err != nil {
			t.Fatal(err)
		}
		if same, err := render("go", NetworkRoutes{PublicGo: true}); err != nil || same != step1 {
			t.Errorf("routes without grants or hosts changed the step-1 prompt (err %v)", err)
		}
		if strings.Contains(step1, "egress-proxy") {
			t.Errorf("the step-1 prompt mentions the tunnel:\n%s", step1)
		}
	}
}

// TestNetworkTunnelPRBodyAndPlan: the pull request line and the plan
// prompt of a build with a tunnel name its hosts, and a tunnel-only
// profile is not called a Go route in either.
func TestNetworkTunnelPRBodyAndPlan(t *testing.T) {
	body, err := PRBody(123, "remediation report here", false, nil, "py", tunnelRoutes())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "pr_body_network_tunnel.md", body)
	if want := "network profile `py` (HTTPS downloads from `files.pythonhosted.org` and `pypi.org`)"; !strings.Contains(
		body, want) || strings.Contains(body, "Go modules") {
		t.Errorf("PR body lacks %q or calls the tunnel Go:\n%s", want, body)
	}

	routes := tunnelRoutes()
	plan, err := RenderPlanPrompt(PlanPrompt{
		IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md",
		Intent: testPlanRequest, BuildMaxTurns: 150, BuildTokenBudget: 800000, Limits: testPlanLimits,
		BuildProfiles: []BuildProfile{{URL: testAPIURL, Profile: "py", Routes: &routes}},
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "prompt_plan_network_tunnel.md", plan)
	flat := strings.Join(strings.Fields(plan), " ")
	want := "can fetch any package downloaded over HTTPS from `files.pythonhosted.org` and `pypi.org`"
	if !strings.Contains(flat, want) || strings.Contains(flat, "Go module") {
		t.Errorf("plan prompt lacks %q or calls the tunnel Go:\n%s", want, plan)
	}

	pc, _ := planProfileComments()
	pc.NetworkProfiles = []string{"py"}
	pc.NetworkRoutes = []NetworkRoutes{tunnelRoutes()}
	advice := mustRenderPlan(t, pc)
	golden(t, "intent_plan_network_tunnel.md", advice)
	if want := "any package downloaded over HTTPS from `files.pythonhosted.org` and `pypi.org`"; !strings.Contains(
		strings.Join(strings.Fields(advice), " "), want) {
		t.Errorf("plan comment lacks %q:\n%s", want, advice)
	}
}

// TestBoundRoutesTunnel: a plan prompt keeps a build's tunnel hosts only
// as a controller writes them (sorted valid host names); anything else is
// described as step 1 would.
func TestBoundRoutesTunnel(t *testing.T) {
	good := tunnelRoutes()
	if got := boundRoutes(&good); got == nil || strings.Join(got.TunnelHosts, ",") != "files.pythonhosted.org,pypi.org" {
		t.Errorf("boundRoutes(%+v) = %+v", good, got)
	}
	for _, hosts := range [][]string{{"pypi.org", "files.pythonhosted.org"}, {"PyPI.org"}, {"pypi.org`"},
		{"10.0.0.1"}, {"pypi.org", "pypi.org"}} {
		r := NetworkRoutes{TunnelHosts: hosts}
		if got := boundRoutes(&r); got != nil {
			t.Errorf("boundRoutes kept %v: %+v", hosts, got)
		}
	}
}

// TestRoutesOfAndWithTunnel: the controller's view of a tunnel profile and
// the pod's view of the same Job agree on what the texts say.
func TestRoutesOfAndWithTunnel(t *testing.T) {
	for _, tt := range []struct {
		profile netprofile.Profile
		pod     netprofile.PodTunnel
	}{
		{netprofile.Profile{Name: "py", Route: netprofile.RouteTunnel,
			Hosts: netprofile.Hosts{"pypi.org", "files.pythonhosted.org"}},
			netprofile.PodTunnel{Hosts: []string{"files.pythonhosted.org", "pypi.org"}}},
		{netprofile.Profile{Name: "go", Route: netprofile.RouteGo, Hosts: netprofile.Hosts{"registry.npmjs.org"}},
			netprofile.PodTunnel{GoRoute: true, Hosts: []string{"registry.npmjs.org"}}},
	} {
		ctrl := RoutesOf(tt.profile)
		pod := PodRoutes(nil, nil).WithTunnel(tt.pod, nil)
		if ctrl.routeList() != pod.routeList() || ctrl.ecosystems() != pod.ecosystems() ||
			ctrl.PublicGo != pod.PublicGo {
			t.Errorf("profile %s: controller %+v and pod %+v disagree", tt.profile.Name, ctrl, pod)
		}
	}
	if r := (NetworkRoutes{PublicGo: true}).WithTunnel(netprofile.PodTunnel{}, nil); !r.PublicGo || r.BeyondGo() {
		t.Errorf("no tunnel changed the routes: %+v", r)
	}
}
