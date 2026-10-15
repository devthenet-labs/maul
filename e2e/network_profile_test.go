// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"fmt"
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/e2e/fakegithub"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// The network profile tests run the job controllers as the chart would
// with a profile, go, on port 8090, for one repository on the fake forge's
// host. envtest enforces no NetworkPolicy, so these prove only what the
// Jobs carry; that the policies admit exactly the labelled pods is the live
// gate's to prove.
const (
	networkLabel = "patchy.bitwisemedia.uk/network-go"
	// proxyURL is the egress proxy the controllers are pointed at: a name
	// this machine resolves, so --agent-dns none can pin it.
	proxyURL = "http://localhost"
)

// networkProfilesFor is the profile set's JSON, its one profile matching
// owner/name on 127.0.0.1.
func networkProfilesFor(ownerName string) string {
	return fmt.Sprintf(`[{"name":"go","route":"go","port":8090,"host":"127.0.0.1","repositories":[%q],`+
		`"modules":["github.com/google/uuid"],"bakedModCache":"/opt/go/pkg/mod","allowPseudoVersions":false}]`,
		ownerName)
}

// networkProfileArgs are the job controllers' profile flags for ownerName.
func networkProfileArgs(ownerName string) []string {
	return []string{"--network-profiles-enabled", "--network-profiles", networkProfilesFor(ownerName),
		"--egress-proxy-url", proxyURL, "--agent-dns", "none"}
}

// wantProfileEnv is the go environment a profile Job's agent carries,
// verbatim.
var wantProfileEnv = map[string]string{
	"GOENV":                  "off",
	"GOPROXY":                "http://localhost:8090/go,file:///opt/go/pkg/mod/cache/download",
	"GOSUMDB":                "sum.golang.org",
	"GONOSUMDB":              "patchy.invalid",
	"GONOPROXY":              "patchy.invalid",
	"GOPRIVATE":              "patchy.invalid",
	"GOINSECURE":             "patchy.invalid",
	"GOAUTH":                 "off",
	"GOTOOLCHAIN":            "local",
	"GOPATH":                 "/tmp/go",
	"GOMODCACHE":             "/tmp/go/pkg/mod",
	"PATCHY_NETWORK_PROFILE": "go",
}

// agentEnvOf is the literal environment of job's agent container.
func agentEnvOf(job batchv1.Job) map[string]string {
	env := map[string]string{}
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name != "agent" {
			continue
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				env[e.Name] = e.Value
			}
		}
	}
	return env
}

// checkProfileJob asserts that job carries the go profile (want) or none:
// the label on the Job and its pod, the go environment verbatim, and,
// under --agent-dns none, the proxy's host pinned in the hosts file.
func checkProfileJob(t *testing.T, job batchv1.Job, want bool) {
	t.Helper()
	for what, labels := range map[string]map[string]string{
		"job": job.Labels, "pod": job.Spec.Template.Labels} {
		if _, ok := labels[networkLabel]; ok != want {
			t.Errorf("%s %s label %s present = %v, want %v", job.Name, what, networkLabel, ok, want)
		}
	}
	env := agentEnvOf(job)
	for name, value := range wantProfileEnv {
		got, ok := env[name]
		if want && got != value {
			t.Errorf("%s %s = %q, want %q", job.Name, name, got, value)
		}
		if !want && ok {
			t.Errorf("%s carries %s=%q without a profile", job.Name, name, got)
		}
	}
	pinned := slices.ContainsFunc(job.Spec.Template.Spec.HostAliases, func(a corev1.HostAlias) bool {
		return slices.Contains(a.Hostnames, "localhost")
	})
	if want && !pinned {
		t.Errorf("%s hostAliases = %+v, want the proxy's host pinned", job.Name, job.Spec.Template.Spec.HostAliases)
	}
}

// TestIntentNetworkProfile: intent-controller started with a profile for
// one of a multi-repository Project's two repositories. The plan Job reads
// both trees on the default image and carries no profile; the matched
// repository's build Job carries the label, the go environment verbatim and
// the proxy's host in its hosts file, and its run records the profile; the
// other repository's build Job carries none of it.
func TestIntentNetworkProfile(t *testing.T) {
	e := startMultiRepo(t, []v1alpha1.ProjectRepository{
		{Name: "web", URL: webRepoURL},
		{Name: "api", URL: apiRepoURL},
	}, nil, false, append([]string{"--intent-multi-repo", "--intent-max-concurrent-runs", "2",
		"--broker-url", "http://localhost:8080"}, networkProfileArgs(intentOwner+"/"+apiRepo)...)...)

	number, name := e.fileIntent(t)
	planJob := e.kubelet.waitRun(t, "the plan job to run", phaseIs("plan"))
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	checkProfileJob(t, planJob.Job, false)
	if _, ok := planJob.Env["GOPROXY"]; ok {
		t.Errorf("the plan job carries GOPROXY=%q", planJob.Env["GOPROXY"])
	}

	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	for _, c := range []struct {
		key  string
		want bool
	}{{"web", false}, {"api", true}} {
		buildRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, c.key, 1)
		job := e.kubelet.waitRun(t, "the "+c.key+" build job to run", func(r agentRun) bool {
			return r.Job.Name == jobs.NameFor(buildRun, intentRunKind, 1)
		})
		checkProfileJob(t, job.Job, c.want)
		run := e.run(t, buildRun)
		wantProfile := map[bool]string{true: "go"}[c.want]
		if run.Status.RunnerImage == nil || run.Status.RunnerImage.Source != v1alpha1.RunnerImageSourceRepository ||
			run.Status.RunnerImage.NetworkProfile != wantProfile {
			t.Errorf("%s build run recorded %+v, want the repository's image with profile %q", c.key,
				run.Status.RunnerImage, wantProfile)
		}
	}
	e.waitPhase(t, name, v1alpha1.IntentInReview)
}

// TestRemediationNetworkProfile: remediation-controller started with a
// profile for one repository launches two remediations, each on its
// repository's accepted image (written onto the Repository status as
// source-controller would; envtest runs no pod, so no registry is needed).
// The matched repository's remediate Job carries the label, the go
// environment verbatim and the proxy's host in its hosts file, and the
// Remediation records the profile; the other repository's carries none.
func TestRemediationNetworkProfile(t *testing.T) {
	cl := startCluster(t)
	gh := fakegithub.New()
	t.Cleanup(gh.Close)
	cl.githubCredentials(t, gh.URL)
	ctx := context.Background()
	now := metav1.Now()
	image := "registry.example/acme/go-env@sha256:" +
		"abababababababababababababababababababababababababababababababab"

	findings := []struct {
		name, repoURL string
		want          bool
	}{
		{"finding-cccccccccc-1", "https://127.0.0.1/acme/shop", true},
		{"finding-dddddddddd-1", "https://127.0.0.1/acme/other", false},
	}
	for _, f := range findings {
		fnd := fabricateFinding(t, cl, f.name, v1alpha1.LevelHigh, f.repoURL, func(st *v1alpha1.FindingStatus) {
			st.Phase = v1alpha1.PhaseQueued
			st.PhaseTimes = []v1alpha1.PhaseTime{{Phase: v1alpha1.PhaseQueued, At: now}}
			st.Attempts = v1alpha1.AttemptCounts{Investigation: 1, Remediation: 1}
			st.Investigation = &v1alpha1.InvestigationSummary{
				Name: f.name + "-inv-1", Attempt: 1, Outcome: "ok",
				Recommendation: v1alpha1.RecommendationRemediate, Confidence: "0.9",
				Exploitability: v1alpha1.RatingHigh, Likelihood: v1alpha1.RatingHigh, Impact: v1alpha1.RatingHigh,
				CompletedAt: &now,
			}
		})
		if err := cl.client.Create(ctx, &v1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: f.name + "-src", Namespace: namespace},
			Spec:       v1alpha1.RepositorySpec{URL: f.repoURL},
		}); err != nil {
			t.Fatal(err)
		}
		inv := &v1alpha1.Investigation{
			ObjectMeta: metav1.ObjectMeta{Name: f.name + "-inv-1", Namespace: namespace},
			Spec: v1alpha1.InvestigationSpec{
				FindingRef:    v1alpha1.ObjectReference{Name: fnd.Name, UID: fnd.UID},
				Attempt:       1,
				RepositoryRef: &v1alpha1.LocalObjectReference{Name: f.name + "-src"},
			},
		}
		if err := cl.client.Create(ctx, inv); err != nil {
			t.Fatal(err)
		}
		inv.Status.Phase = v1alpha1.RunComplete
		inv.Status.Report = "---\nanalysis---\nbump the dependency\n"
		if err := cl.client.Status().Update(ctx, inv); err != nil {
			t.Fatal(err)
		}
		if err := cl.client.Create(ctx, &v1alpha1.Remediation{
			ObjectMeta: metav1.ObjectMeta{Name: f.name + "-rem-1", Namespace: namespace},
			Spec: v1alpha1.RemediationSpec{
				FindingRef:       v1alpha1.ObjectReference{Name: fnd.Name, UID: fnd.UID},
				InvestigationRef: v1alpha1.ObjectReference{Name: f.name + "-inv-1"},
				RepositoryRef:    v1alpha1.LocalObjectReference{Name: f.name + "-src"},
				Attempt:          1,
				Priority:         50,
				Parameters:       v1alpha1.AgentParameters{Model: "anthropic/claude-sonnet-5", Harness: "claude"},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	cl.controller(t, "source-controller",
		"--artifact-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"--artifact-base-url", "http://127.0.0.1:1",
		"--artifact-dir", t.TempDir())
	for _, f := range findings {
		key := types.NamespacedName{Namespace: namespace, Name: f.name + "-src"}
		var repo v1alpha1.Repository
		eventually(t, "repository artifact for "+f.name, func() bool {
			return cl.client.Get(ctx, key, &repo) == nil && repo.Status.Artifact != nil
		})
		// The accepted, digest-pinned image source-controller would have
		// resolved from the tree's declaration; this one runs without
		// --repository-images, so it never writes or clears the field.
		repo.Status.RunnerImage = &v1alpha1.RunnerImage{Declared: "registry.example/acme/go-env:1",
			Manifest: ".patchy/agent.yaml", Image: image, SearchPath: "/usr/local/go/bin:/usr/bin:/bin",
			Verified: true}
		if err := cl.client.Status().Update(ctx, &repo); err != nil {
			t.Fatal(err)
		}
	}

	// Brokered claude alone: under --agent-dns none no runner may dial its
	// model API by name.
	cl.controller(t, "remediation-controller", append([]string{
		"--max-concurrent-remediations", "2", "--repository-images", "--agent-ephemeral-storage", "1Gi",
		"--claude-agent-image", "patchy/claude-agent-runner:e2e", "--broker-url", "http://localhost:8080"},
		networkProfileArgs("acme/shop")...)...)

	for _, f := range findings {
		var rem v1alpha1.Remediation
		key := types.NamespacedName{Namespace: namespace, Name: f.name + "-rem-1"}
		eventually(t, "the remediation of "+f.name+" to launch", func() bool {
			return cl.client.Get(ctx, key, &rem) == nil && rem.Status.JobRef != nil && rem.Status.RunnerImage != nil
		})
		wantProfile := map[bool]string{true: "go"}[f.want]
		if rem.Status.RunnerImage.Source != v1alpha1.RunnerImageSourceRepository ||
			rem.Status.RunnerImage.NetworkProfile != wantProfile {
			t.Errorf("%s recorded %+v, want the repository's image with profile %q", f.name,
				rem.Status.RunnerImage, wantProfile)
		}
		var job batchv1.Job
		if err := cl.client.Get(ctx, client.ObjectKey{Namespace: agentsNS, Name: rem.Status.JobRef.Name},
			&job); err != nil {
			t.Fatal(err)
		}
		checkProfileJob(t, job, f.want)
	}
}
