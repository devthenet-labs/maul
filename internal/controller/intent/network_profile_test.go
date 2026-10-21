// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/netprofile"
)

// withProfiles turns network profiles on in the guard, the run scheduler's
// and the intent reconciler's alike (one guard in production), with one
// profile, go, for profileRepos.
func withProfiles(t *testing.T, e *env, profileRepos string) {
	t.Helper()
	set, err := netprofile.Parse([]byte(`[{"name":"go","route":"go","port":8090,"repositories":["` +
		profileRepos + `"],"modules":["github.com/google/uuid"],"bakedModCache":"/opt/go/pkg/mod"}]`))
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("http://maul-egress-proxy.maul.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	e.runs.Images.ProfilesEnabled, e.runs.Images.Profiles, e.runs.Images.ProxyBase = true, set, base
	e.intent.Images.ProfilesEnabled, e.intent.Images.Profiles, e.intent.Images.ProxyBase = true, set, base
}

// specsOf are the launched Job specs of phase, in no particular order.
func (e *env) specsOf(phase string) []jobs.Spec {
	e.jobs.mu.Lock()
	defer e.jobs.mu.Unlock()
	var out []jobs.Spec
	for _, s := range e.jobs.specs {
		if s.Phase == phase {
			out = append(out, s)
		}
	}
	return out
}

// TestBuildAndReviseRoundsGetTheNetworkProfile: with a profile for the
// intent's repository, the build and every revise round (review feedback
// and a check-fix) ask for it on the repository's image, and each run
// records what Create read back; the plan never asks.
func TestBuildAndReviseRoundsGetTheNetworkProfile(t *testing.T) {
	project := testProject()
	project.Spec.Checks.Fix = []string{"test"}
	e := newEnv(t, project)
	withProfiles(t, e, "acme/app")
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.gh.mu.Lock()
	body := e.gh.prs[number].body
	e.gh.mu.Unlock()
	if !strings.Contains(body, "network profile `go`") {
		t.Errorf("the pull request body does not say the build had the profile:\n%s", body)
	}

	// A review asks for changes: a review-triggered revise round.
	e.gh.reviews[number] = []ghclient.Review{{ID: 901, NodeID: "review-901", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)

	// Then a check fails on the new head: a check-fix round.
	head := in.Status.PullRequests[0].HeadSHA
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 71, Name: "test", HeadSHA: head,
		Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
		DetailsURL: "https://github.com/acme/app/actions/runs/81/jobs/91",
		Output:     ghclient.CheckOutput{Title: "Go failed", Summary: "failed"}}}
	e.gh.workflowJobs[81] = []ghclient.WorkflowJob{{ID: 91, CheckRunID: 71, HeadSHA: head,
		Name: "go test", Conclusion: "failure"}}
	e.gh.jobLogs[91] = "go test ./...\nFAIL version test"
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)

	for _, s := range e.specsOf("plan") {
		if s.NetworkProfile != nil {
			t.Errorf("plan spec asked for network profile %+v", s.NetworkProfile)
		}
	}
	builds := e.specsOf("build")
	if len(builds) != 3 {
		t.Fatalf("build-phase Jobs = %d, want the build and two revise rounds", len(builds))
	}
	want := jobs.NetworkProfile{Name: "go", Route: "go",
		ProxyURL: "http://maul-egress-proxy.maul.svc.cluster.local:8090", BakedModCache: "/opt/go/pkg/mod"}
	for _, s := range builds {
		if s.NetworkProfile == nil || !reflect.DeepEqual(*s.NetworkProfile, want) || s.RunnerImage != repoImage {
			t.Errorf("build-phase spec %s: profile %+v image %q, want %+v on the repository's image",
				s.Finding, s.NetworkProfile, s.RunnerImage, want)
		}
	}
	triggers := map[v1alpha1.IntentRunTrigger]bool{}
	for _, stage := range []v1alpha1.IntentStage{v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise} {
		for _, run := range e.runsOf(name, stage) {
			triggers[run.Spec.Trigger] = true
			if run.Status.RunnerImage == nil || run.Status.RunnerImage.NetworkProfile != "go" {
				t.Errorf("run %s recorded %+v, want network profile go", run.Name, run.Status.RunnerImage)
			}
		}
	}
	if !triggers[v1alpha1.IntentRunTriggerReview] || !triggers[v1alpha1.IntentRunTriggerChecks] {
		t.Errorf("revise triggers seen = %v, want review and checks", triggers)
	}
	for _, run := range e.runsOf(name, v1alpha1.IntentStagePlan) {
		if run.Status.RunnerImage != nil && run.Status.RunnerImage.NetworkProfile != "" {
			t.Errorf("plan run %s recorded network profile %q", run.Name, run.Status.RunnerImage.NetworkProfile)
		}
	}
}

// TestBuildWithoutAPinGetsNoNetworkProfile: a Project that does not require
// the repository's image builds on the default image when none is pinned,
// and such a build never asks for a profile; nor does a build for a
// repository no profile matches.
func TestBuildWithoutAPinGetsNoNetworkProfile(t *testing.T) {
	for _, tt := range []struct {
		name, repos, image string
		require            bool
	}{
		{"no pin, not required", "acme/app", "", false},
		{"no profile matches", "acme/application", repoImage, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := testProject()
			p.Spec.RequireRepositoryImage = new(tt.require)
			e := newEnv(t, p)
			withProfiles(t, e, tt.repos)
			name := e.awaiting()
			e.gh.label(1, "maul:approved", approver)
			e.drive(name, v1alpha1.IntentInReview, tt.image)
			builds := e.specsOf("build")
			if len(builds) != 1 || builds[0].NetworkProfile != nil {
				t.Fatalf("build specs = %+v, want one without a network profile", builds)
			}
		})
	}
}
