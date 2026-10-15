// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"net/url"
	"strings"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// webProfiles is a profile set with one registry-only profile, web, for
// profileRepos: an npm grant under the registry's policy and a pypi grant
// with an allowlist.
const webProfiles = `[{"name":"web","port":8091,"repositories":["%s"],"registries":[` +
	`{"name":"art-npm","ecosystem":"npm","hosts":["acme.jfrog.io"],"packagePolicy":"registry"},` +
	`{"name":"art-pypi","ecosystem":"pypi","hosts":["acme.jfrog.io"],"packages":["six"]}]}]`

// withRegistryProfiles is withProfiles for webProfiles.
func withRegistryProfiles(t *testing.T, e *env, profileRepos string) {
	t.Helper()
	set, err := netprofile.Parse([]byte(strings.Replace(webProfiles, "%s", profileRepos, 1)))
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("http://patchy-egress-proxy.patchy.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	e.runs.Images.ProfilesEnabled, e.runs.Images.Profiles, e.runs.Images.ProxyBase = true, set, base
	e.intent.Images.ProfilesEnabled, e.intent.Images.Profiles, e.intent.Images.ProxyBase = true, set, base
}

// TestRegistryProfileReachesPlanBuildAndPR: with a registry profile for the
// intent's repository, the plan comment says which ecosystems the build
// fetches and through which registry, the build asks for the profile, and
// the pull request body names its routes.
func TestRegistryProfileReachesPlanBuildAndPR(t *testing.T) {
	e := newEnv(t, testProject())
	withRegistryProfiles(t, e, "acme/app")
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
	if len(builds) != 1 || builds[0].NetworkProfile == nil || builds[0].NetworkProfile.Name != "web" {
		t.Fatalf("build specs = %+v, want one with the web profile", builds)
	}
	number := in.Status.PullRequests[0].Number
	e.gh.mu.Lock()
	body := e.gh.prs[number].body
	e.gh.mu.Unlock()
	if want := "network profile `web` (npm via `art-npm`, PyPI via `art-pypi`)"; !strings.Contains(body, want) {
		t.Errorf("the pull request body lacks %q:\n%s", want, body)
	}
}
