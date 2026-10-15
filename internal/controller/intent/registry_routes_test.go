// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// webProfiles is a profile set with one registry-only profile, web, for
// a repository: an npm grant under the registry's policy and a pypi grant
// with an allowlist.
const webProfiles = `[{"name":"web","port":8091,"repositories":["%s"],"registries":[` +
	`{"name":"art-npm","ecosystem":"npm","hosts":["acme.jfrog.io"],"packagePolicy":"registry"},` +
	`{"name":"art-pypi","ecosystem":"pypi","hosts":["acme.jfrog.io"],"packages":["six"]}]}]`

// withRegistryProfiles is withProfiles for webProfiles, for acme/app.
func withRegistryProfiles(t *testing.T, e *env) {
	t.Helper()
	set, err := netprofile.Parse([]byte(strings.Replace(webProfiles, "%s", "acme/app", 1)))
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("http://patchy-egress-proxy.patchy.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	e.runs.Images.ProfilesEnabled, e.runs.Images.Profiles, e.runs.Images.ProxyBase = true, set, base
	e.intent.Images.ProfilesEnabled, e.intent.Images.Profiles, e.intent.Images.ProxyBase = true, set, base
	e.runs.Images.Forges = forgeHosts{"github.com"}
	e.intent.Images.Forges = forgeHosts{"github.com"}
}

// forgeHosts is a fixed Forge host list (runnerguard.ForgeHosts).
type forgeHosts []string

func (f forgeHosts) WebHosts(context.Context, string) ([]string, error) { return f, nil }

// TestRegistryProfileReachesPlanBuildAndPR: with a registry profile for the
// intent's repository, the plan comment says which ecosystems the build
// fetches and through which registry, the build spec carries the grants,
// and the pull request body names its routes.
func TestRegistryProfileReachesPlanBuildAndPR(t *testing.T) {
	e := newEnv(t, testProject())
	withRegistryProfiles(t, e)
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return planOutput(defaultOutput(spec).Events[0].Plan.Stage, planWithDependencies)
		}
		return defaultOutput(spec)
	}
	name := e.awaiting()
	plans := e.gh.withMarker("patchy:plan")
	if len(plans) != 1 {
		t.Fatalf("plan comments = %d, want 1", len(plans))
	}
	for _, want := range []string{"network profile `web`", "any npm package `art-npm` serves",
		"the PyPI projects the operator allows from `art-pypi`"} {
		if !strings.Contains(plans[0].Body, want) {
			t.Errorf("plan comment lacks %q:\n%s", want, plans[0].Body)
		}
	}

	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	builds := e.specsOf("build")
	if len(builds) != 1 || builds[0].NetworkProfile == nil || builds[0].NetworkProfile.Name != "web" ||
		len(builds[0].NetworkProfile.Registries) != 2 || builds[0].NetworkProfile.Route != "" {
		t.Fatalf("build specs = %+v, want one with the web profile's two grants", builds)
	}
	number := in.Status.PullRequests[0].Number
	e.gh.mu.Lock()
	body := e.gh.prs[number].body
	e.gh.mu.Unlock()
	if want := "network profile `web` (npm via `art-npm`, PyPI via `art-pypi`)"; !strings.Contains(body, want) {
		t.Errorf("the pull request body lacks %q:\n%s", want, body)
	}
}

// TestRegistryGrantNamingAForgeIsRefused: a registry grant whose host is a
// Forge CR's host (here a GitHub Enterprise host only the Forge list
// knows) settles the build launch-refused, naming the host, and no build
// Job is created.
func TestRegistryGrantNamingAForgeIsRefused(t *testing.T) {
	e := newEnv(t, testProject())
	withRegistryProfiles(t, e)
	e.runs.Images.Forges = forgeHosts{"github.com", "jfrog.io"}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentFailed, repoImage)
	if builds := e.specsOf("build"); len(builds) != 0 {
		t.Errorf("build Jobs created = %d, want none", len(builds))
	}
	runs := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(runs) == 0 {
		t.Fatal("no build run")
	}
	for _, run := range runs {
		if run.Status.Outcome != OutcomeLaunchRefused || !strings.Contains(run.Status.Detail, "acme.jfrog.io") {
			t.Errorf("build run %s settled %q: %q, want launch_refused naming acme.jfrog.io", run.Name,
				run.Status.Outcome, run.Status.Detail)
		}
	}
}

// TestProfileBuildNamingTheProxyIsRefused: a build that carried a network
// profile and returns a requirements.txt pointing pip at the egress proxy
// is refused before any GitHub write, naming the file; the same changeset
// from a build without a profile is pushed as before.
func TestProfileBuildNamingTheProxyIsRefused(t *testing.T) {
	for _, profile := range []bool{true, false} {
		t.Run(map[bool]string{true: "profile", false: "no profile"}[profile], func(t *testing.T) {
			e := newEnv(t, testProject())
			if profile {
				withRegistryProfiles(t, e)
			}
			e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
				out := defaultOutput(spec)
				if spec.Phase == "build" {
					out.Events[0].Remediation.Changeset.Upserts = append(out.Events[0].Remediation.Changeset.Upserts,
						envelope.FileChange{Path: "requirements.txt", Mode: "100644",
							ContentB64: base64.StdEncoding.EncodeToString([]byte("--index-url " +
								"http://patchy-egress-proxy.patchy.svc.cluster.local:8091/r/art-pypi/simple/\nsix\n"))})
				}
				return out
			}
			name := e.awaiting()
			e.gh.label(1, "patchy:approved", approver)
			if !profile {
				e.drive(name, v1alpha1.IntentInReview, repoImage)
				return
			}
			e.drive(name, v1alpha1.IntentFailed, repoImage)
			e.gh.mu.Lock()
			commits := len(e.gh.commits)
			e.gh.mu.Unlock()
			if commits != 0 {
				t.Errorf("GitHub commits = %d, want none", commits)
			}
			for _, run := range e.runsOf(name, v1alpha1.IntentStageBuild) {
				if run.Status.Outcome != string(envelope.OutcomeChangesetRejected) ||
					!strings.Contains(run.Status.Detail, "requirements.txt") ||
					!strings.Contains(run.Status.Detail, "egress proxy") {
					t.Errorf("build run %s settled %q: %q, want changeset_rejected naming requirements.txt",
						run.Name, run.Status.Outcome, run.Status.Detail)
				}
			}
		})
	}
}

// TestPlanJobIsToldItsBuildsRoutes: a plan Job whose build gets a registry
// profile is told the profile's routes beside its name, and nothing of the
// proxy; it asks for no profile itself.
func TestPlanJobIsToldItsBuildsRoutes(t *testing.T) {
	e := newEnv(t, testProject())
	withRegistryProfiles(t, e)
	e.awaiting()
	e.jobs.mu.Lock()
	defer e.jobs.mu.Unlock()
	plans := 0
	for name, spec := range e.jobs.specs {
		if spec.Phase != "plan" {
			continue
		}
		plans++
		if spec.NetworkProfile != nil {
			t.Errorf("plan Job %s asked for network profile %+v", name, spec.NetworkProfile)
		}
		got, err := templates.DecodeBuildProfiles(e.jobs.envs[name][envBuildProfiles])
		if err != nil || len(got) != 1 || got[0].Profile != "web" || got[0].Routes == nil ||
			len(got[0].Routes.Grants) != 2 || got[0].Routes.Grants[0].Registry != "art-npm" {
			t.Errorf("plan Job %s was told %+v (%v), want the web profile with its two grants", name, got, err)
		}
	}
	if plans == 0 {
		t.Fatal("no plan Job launched")
	}
}

// TestPlanJobIsToldItsBuildsTunnel: a plan Job whose build gets a
// tunnel-only profile is told the profile's tunnel hosts beside its name,
// and no Go route; it asks for no profile itself.
func TestPlanJobIsToldItsBuildsTunnel(t *testing.T) {
	e := newEnv(t, testProject())
	set, err := netprofile.Parse([]byte(`[{"name":"py","route":"tunnel","port":8092,"repositories":["acme/app"],` +
		`"modules":[],"bakedModCache":"","hosts":["pypi.org","files.pythonhosted.org"],"clients":["pip"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("http://patchy-egress-proxy.patchy.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	e.runs.Images.ProfilesEnabled, e.runs.Images.Profiles, e.runs.Images.ProxyBase = true, set, base
	e.intent.Images.ProfilesEnabled, e.intent.Images.Profiles, e.intent.Images.ProxyBase = true, set, base
	e.runs.Images.Forges = forgeHosts{"github.com"}
	e.intent.Images.Forges = forgeHosts{"github.com"}
	e.awaiting()
	e.jobs.mu.Lock()
	defer e.jobs.mu.Unlock()
	plans := 0
	for name, spec := range e.jobs.specs {
		if spec.Phase != "plan" {
			continue
		}
		plans++
		if spec.NetworkProfile != nil {
			t.Errorf("plan Job %s asked for network profile %+v", name, spec.NetworkProfile)
		}
		got, err := templates.DecodeBuildProfiles(e.jobs.envs[name][envBuildProfiles])
		if err != nil || len(got) != 1 || got[0].Profile != "py" || got[0].Routes == nil || got[0].Routes.PublicGo ||
			strings.Join(got[0].Routes.TunnelHosts, ",") != "files.pythonhosted.org,pypi.org" {
			t.Errorf("plan Job %s was told %+v (%v), want the py profile with its tunnel hosts", name, got, err)
		}
	}
	if plans == 0 {
		t.Fatal("no plan Job launched")
	}
}
