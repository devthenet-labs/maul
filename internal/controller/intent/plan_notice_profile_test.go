// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/runnerguard"
)

// planWithDependencies is validPlan adding two dependencies, so the plan
// comment carries its dependency advice.
var planWithDependencies = strings.Replace(validPlan, "new_dependencies: []",
	"new_dependencies:\n  - \"github.com/google/uuid v1.6.0\"\n  - \"libvips 8.15\"", 1)

// TestPlanNoticeAgreesWithBuildProfile: for the same guard and repository,
// the profile the plan comment's dependency advice names is exactly the one
// a build of that repository on its accepted image asks for, across every
// input the build's decision reads: the repository-images and profiles
// kill switches, the proxy, the sandbox breaker, and which profiles match.
func TestPlanNoticeAgreesWithBuildProfile(t *testing.T) {
	one, err := netprofile.Parse([]byte(`[{"name":"go","route":"go","port":8090,"repositories":["acme/app"],` +
		`"modules":["github.com/google/uuid"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	two, err := netprofile.Parse([]byte(`[{"name":"go","route":"go","port":8090,"repositories":["acme/app"],` +
		`"modules":["github.com/google/uuid"]},{"name":"go2","route":"go","port":8091,"repositories":["acme/.*"],` +
		`"modules":["github.com/google/uuid"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("http://maul-egress-proxy.maul.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	on := func() runnerguard.Guard {
		return runnerguard.Guard{Enabled: true, Breaker: runnerguard.NewBreaker("intent-test", nil),
			ProfilesEnabled: true, Profiles: one, ProxyBase: base}
	}
	for _, tt := range []struct {
		name  string
		guard func() runnerguard.Guard
		url   string
		want  string
	}{
		{"matching profile", on, appRepoURL, "go"},
		{"no profile matches", on, "https://github.com/acme/other", ""},
		{"profiles disabled", func() runnerguard.Guard { g := on(); g.ProfilesEnabled = false; return g }, appRepoURL, ""},
		{"no proxy", func() runnerguard.Guard { g := on(); g.ProxyBase = nil; return g }, appRepoURL, ""},
		{"no profiles", func() runnerguard.Guard { g := on(); g.Profiles = netprofile.Set{}; return g }, appRepoURL, ""},
		{"repository images disabled", func() runnerguard.Guard { g := on(); g.Enabled = false; return g }, appRepoURL, ""},
		{"breaker tripped", func() runnerguard.Guard {
			g := on()
			g.Breaker.Trip(context.Background(), "a-job", "a-run")
			return g
		}, appRepoURL, ""},
		{"two profiles match", func() runnerguard.Guard { g := on(); g.Profiles = two; return g }, appRepoURL, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := tt.guard()
			notice := buildProfile(g, tt.url)
			built := launchedBuildProfile(t, g, tt.url)
			if notice != built || notice != tt.want {
				t.Errorf("plan notice names %q, the build asks for %q, want both %q", notice, built, tt.want)
			}
		})
	}
}

// launchedBuildProfile is the profile the run scheduler's build launch asks
// for on a build of repoURL whose Repository pinned an accepted image, or ""
// for none (or no launch).
func launchedBuildProfile(t *testing.T, g runnerguard.Guard, repoURL string) string {
	t.Helper()
	r := &RunReconciler{Images: g, BuildModel: "anthropic/claude-opus-5"}
	run := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{Name: "t-bld-r1-a1", Namespace: testNS}}
	run.Spec.Stage = v1alpha1.IntentStageBuild
	run.Spec.Repository.URL = repoURL
	run.Spec.Inputs.PlanDigest = digest([]byte(validPlan))
	cm := &corev1.ConfigMap{Data: map[string]string{keyInvestigation: validPlan}}
	repo := &v1alpha1.Repository{Spec: v1alpha1.RepositorySpec{URL: repoURL},
		Status: v1alpha1.RepositoryStatus{RunnerImage: &v1alpha1.RunnerImage{Image: repoImage}}}
	spec := jobs.Spec{Phase: jobPhase[v1alpha1.IntentStageBuild], Kind: KindIntent}
	if _, refusal := r.stageSpec(run, &spec, cm, testProject(), repo); refusal != nil {
		return ""
	}
	if spec.NetworkProfile == nil {
		return ""
	}
	return spec.NetworkProfile.Name
}

// TestPlanCommentSaysWhatTheBuildFetches: the plan comment of a plan adding
// dependencies tells the approver the build fetches Go modules through the
// egress proxy exactly when the build that follows runs with the profile,
// and that it runs offline otherwise.
func TestPlanCommentSaysWhatTheBuildFetches(t *testing.T) {
	for _, tt := range []struct {
		name, repos string
		profile     bool
	}{
		{"profile", "acme/app", true},
		{"no profile matches", "acme/application", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			withProfiles(t, e, tt.repos)
			e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
				if spec.Phase == "plan" {
					return planOutput(defaultOutput(spec).Events[0].Plan.Stage, planWithDependencies)
				}
				return defaultOutput(spec)
			}
			name := e.awaiting()
			plans := e.gh.withMarker("maul:plan")
			if len(plans) != 1 {
				t.Fatalf("plan comments = %d, want 1", len(plans))
			}
			body := plans[0].Body
			proxied := strings.Contains(body, "network profile `go`") && strings.Contains(body, "egress proxy")
			offline := strings.Contains(body, "The build runs offline")
			if proxied != tt.profile || offline == tt.profile {
				t.Errorf("plan comment: proxy advice %v, offline advice %v, want the build's profile %v:\n%s",
					proxied, offline, tt.profile, body)
			}
			e.gh.label(1, "maul:approved", approver)
			e.drive(name, v1alpha1.IntentInReview, repoImage)
			builds := e.specsOf("build")
			if len(builds) != 1 || (builds[0].NetworkProfile != nil) != tt.profile {
				t.Fatalf("build specs = %+v, want one with a profile %v", builds, tt.profile)
			}
		})
	}
}

// TestPlanRecordRetryAfterProfileChange: a retry of the plan's record that
// finds the plan comment it posted, after a restart changed the profile
// decision its dependency advice reads (here the profiles switched off),
// takes that comment as posted: no second plan comment, and the approve
// label an approver added since still approves.
func TestPlanRecordRetryAfterProfileChange(t *testing.T) {
	e := newEnv(t, testProject())
	withProfiles(t, e, "acme/app")
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return planOutput(defaultOutput(spec).Events[0].Plan.Stage, planWithDependencies)
		}
		return defaultOutput(spec)
	}
	name := e.newIntent(approver)
	e.failStatusIf = func(in *v1alpha1.Intent) bool {
		return in.Status.Plan != nil && in.Status.Plan.CommentID != 0
	}
	for range 30 {
		if e.failed > 0 {
			break
		}
		_ = e.reconcileIntent(name)
		e.readyRepositories("")
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if e.failed == 0 {
		t.Fatal("the plan comment was never recorded")
	}
	if plans := e.gh.withMarker("maul:plan"); len(plans) != 1 || !strings.Contains(plans[0].Body, "egress proxy") {
		t.Fatalf("plan comments = %+v, want one with the proxy advice", plans)
	}
	e.clock.Advance(time.Second)
	id := e.gh.label(1, "maul:approved", approver)
	// A restarted controller whose profiles are off would word the advice
	// offline.
	images := e.intent.Images
	images.ProfilesEnabled = false
	e.intent = &IntentReconciler{Client: e.c, APIReader: e.c, GitHub: e.gh, Settings: testSettings(),
		Images: images, Nudger: e.nudger, Now: e.clock.Now}
	in := e.drive(name, v1alpha1.IntentBuilding, repoImage)
	if ap := in.Status.Approval; ap == nil || ap.EventID != id || ap.Source != v1alpha1.IntentActionLabel {
		t.Fatalf("approval = %+v, want the label %d", ap, id)
	}
	if n := len(e.gh.withMarker("maul:plan")); n != 1 {
		t.Errorf("plan comments = %d, want exactly one", n)
	}
}
