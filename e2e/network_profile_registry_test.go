// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/e2e/fakegithub"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// The registry profile tests run the job controllers as the chart would
// with a step-2 profile, web, on port 8091: a registry-only profile granted
// an npm registry under the registry's own policy and a pypi registry with
// an allowlist. As with step 1's tests, envtest enforces no NetworkPolicy
// and runs no proxy, so these prove what the Jobs carry and what the
// controllers refuse.
const registryLabel = "patchy.bitwisemedia.uk/network-web"

// registryProfilesFor is the profile set's JSON, its one profile matching
// owner/name on 127.0.0.1, with the hosts the chart renders into each
// grant.
func registryProfilesFor(ownerName string) string {
	return fmt.Sprintf(`[{"name":"web","port":8091,"host":"127.0.0.1","repositories":[%q],"registries":[`+
		`{"name":"art-npm","ecosystem":"npm","hosts":["acme.jfrog.io"],"packagePolicy":"registry"},`+
		`{"name":"art-pypi","ecosystem":"pypi","hosts":["acme.jfrog.io"],"packages":["six","requests"]}]}]`,
		ownerName)
}

// registryProfileArgs are the job controllers' profile flags for ownerName.
func registryProfileArgs(ownerName string) []string {
	return []string{"--network-profiles-enabled", "--network-profiles", registryProfilesFor(ownerName),
		"--egress-proxy-url", proxyURL, "--agent-dns", "none"}
}

// wantRegistryEnv is the npm and pip environment a web profile Job's agent
// carries, verbatim (§5.2, §5.3 of the step-2 spec), with the variables
// agent-runner reads to describe the routes.
var wantRegistryEnv = map[string]string{
	"NPM_CONFIG_REGISTRY":              "http://localhost:8091/r/art-npm/",
	"NPM_CONFIG_REPLACE_REGISTRY_HOST": "always",
	"NPM_CONFIG_AUDIT":                 "false",
	"NPM_CONFIG_FUND":                  "false",
	"NPM_CONFIG_UPDATE_NOTIFIER":       "false",
	"NPM_CONFIG_IGNORE_SCRIPTS":        "true",
	"NPM_CONFIG_CACHE":                 "/tmp/npm-cache",
	"NPM_CONFIG_STRICT_SSL":            "true",
	"PIP_CONFIG_FILE":                  "/dev/null",
	"PIP_INDEX_URL":                    "http://localhost:8091/r/art-pypi/simple/",
	"PIP_EXTRA_INDEX_URL":              " ",
	"PIP_FIND_LINKS":                   " ",
	"PIP_TRUSTED_HOST":                 "localhost:8091",
	"PIP_ONLY_BINARY":                  ":all:",
	"PIP_NO_INPUT":                     "1",
	"PIP_DISABLE_PIP_VERSION_CHECK":    "1",
	"PIP_CACHE_DIR":                    "/tmp/pip-cache",
	"PATCHY_NETWORK_PROFILE":           "web",
	"PATCHY_NETWORK_ECOSYSTEMS":        "npm,pypi",
	"PATCHY_NETWORK_GRANTS": `[{"registry":"art-npm","ecosystem":"npm","anyPackage":true},` +
		`{"registry":"art-pypi","ecosystem":"pypi","packages":["six","requests"]}]`,
}

// checkRegistryJob asserts that job carries the web profile (want) or
// none: the label on the Job and its pod, the npm and pip environment
// verbatim, no go variable a registry-only profile does not own, and the
// proxy's host pinned in the hosts file.
func checkRegistryJob(t *testing.T, job batchv1.Job, want bool) {
	t.Helper()
	for what, labels := range map[string]map[string]string{"job": job.Labels, "pod": job.Spec.Template.Labels} {
		if _, ok := labels[registryLabel]; ok != want {
			t.Errorf("%s %s label %s present = %v, want %v", job.Name, what, registryLabel, ok, want)
		}
	}
	env := agentEnvOf(job)
	for name, value := range wantRegistryEnv {
		got, ok := env[name]
		if want && got != value {
			t.Errorf("%s %s = %q, want %q", job.Name, name, got, value)
		}
		if !want && ok {
			t.Errorf("%s carries %s=%q without a profile", job.Name, name, got)
		}
	}
	if _, ok := env["GOPROXY"]; ok && want {
		t.Errorf("%s carries GOPROXY=%q from a profile with no go route", job.Name, env["GOPROXY"])
	}
	if want {
		pinned := false
		for _, a := range job.Spec.Template.Spec.HostAliases {
			for _, h := range a.Hostnames {
				pinned = pinned || h == "localhost"
			}
		}
		if !pinned {
			t.Errorf("%s hostAliases = %+v, want the proxy's host pinned", job.Name, job.Spec.Template.Spec.HostAliases)
		}
	}
	for _, v := range env {
		if strings.Contains(v, "jfrog") {
			t.Errorf("%s: a registry host reached the pod: %q", job.Name, v)
		}
	}
}

// TestIntentRegistryProfile: intent-controller started with a registry
// profile for the application repository. The plan Job carries none of it;
// the build Job carries the npm and pip environment verbatim, with the proxy pinned. Its
// changeset, which points requirements.txt at the egress proxy, is refused
// naming the file before any GitHub write: no ref is created.
func TestIntentRegistryProfile(t *testing.T) {
	e := startIntents(t, append([]string{"--broker-url", "http://localhost:8080"},
		registryProfileArgs(intentOwner+"/"+appRepo)...)...)
	e.kubelet.setAgentEnv("PATCHY_FAKE_BUILD_EXTRA_PATH", "requirements.txt")
	e.kubelet.setAgentEnv("PATCHY_FAKE_BUILD_EXTRA_CONTENT", "--index-url http://localhost:8091/r/art-pypi/simple/")

	number, name := e.fileIntent(t)
	planJob := e.kubelet.waitRun(t, "the plan job to run", phaseIs("plan"))
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	checkRegistryJob(t, planJob.Job, false)

	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	buildRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, appRepo, 1)
	job := e.kubelet.waitRun(t, "the build job to run", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(buildRun, intentRunKind, 1)
	})
	checkRegistryJob(t, job.Job, true)

	var run *v1alpha1.IntentRun
	eventually(t, "the build run to settle", func() bool {
		run = e.run(t, buildRun)
		return run.Status.Outcome != ""
	})
	if run.Status.Outcome != string(envelope.OutcomeChangesetRejected) ||
		!strings.Contains(run.Status.Detail, "requirements.txt") || !strings.Contains(run.Status.Detail, "egress proxy") {
		t.Errorf("build run settled %q: %q, want changeset_rejected naming requirements.txt", run.Status.Outcome,
			run.Status.Detail)
	}
	if run.Status.RunnerImage == nil || run.Status.RunnerImage.NetworkProfile != "web" {
		t.Errorf("build run recorded %+v, want the web profile", run.Status.RunnerImage)
	}
	if w := e.gh.RefWrites(); len(w) != 0 {
		t.Errorf("ref writes = %+v, want none: the changeset was refused before any GitHub write", w)
	}
}

// TestRemediationRegistryProfile: remediation-controller started with a
// registry profile for one repository launches its remediation on the
// repository's accepted image; the remediate Job carries the npm and pip
// environment verbatim with the proxy pinned, and the Remediation records
// the profile.
func TestRemediationRegistryProfile(t *testing.T) {
	cl := startCluster(t)
	gh := fakegithub.New()
	t.Cleanup(gh.Close)
	cl.githubCredentials(t, gh.URL)
	ctx := context.Background()
	now := metav1.Now()
	const name, repoURL = "finding-eeeeeeeeee-1", "https://127.0.0.1/acme/web"
	fnd := fabricateFinding(t, cl, name, v1alpha1.LevelHigh, repoURL, func(st *v1alpha1.FindingStatus) {
		st.Phase = v1alpha1.PhaseQueued
		st.PhaseTimes = []v1alpha1.PhaseTime{{Phase: v1alpha1.PhaseQueued, At: now}}
		st.Attempts = v1alpha1.AttemptCounts{Investigation: 1, Remediation: 1}
		st.Investigation = &v1alpha1.InvestigationSummary{
			Name: name + "-inv-1", Attempt: 1, Outcome: "ok",
			Recommendation: v1alpha1.RecommendationRemediate, Confidence: "0.9",
			Exploitability: v1alpha1.RatingHigh, Likelihood: v1alpha1.RatingHigh, Impact: v1alpha1.RatingHigh,
			CompletedAt: &now,
		}
	})
	for _, obj := range []client.Object{
		&v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: name + "-src", Namespace: namespace},
			Spec: v1alpha1.RepositorySpec{URL: repoURL}},
	} {
		if err := cl.client.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	inv := &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-inv-1", Namespace: namespace},
		Spec: v1alpha1.InvestigationSpec{FindingRef: v1alpha1.ObjectReference{Name: fnd.Name, UID: fnd.UID},
			Attempt: 1, RepositoryRef: &v1alpha1.LocalObjectReference{Name: name + "-src"}},
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
		ObjectMeta: metav1.ObjectMeta{Name: name + "-rem-1", Namespace: namespace},
		Spec: v1alpha1.RemediationSpec{
			FindingRef:       v1alpha1.ObjectReference{Name: fnd.Name, UID: fnd.UID},
			InvestigationRef: v1alpha1.ObjectReference{Name: name + "-inv-1"},
			RepositoryRef:    v1alpha1.LocalObjectReference{Name: name + "-src"},
			Attempt:          1, Priority: 50,
			Parameters: v1alpha1.AgentParameters{Model: "anthropic/claude-sonnet-5", Harness: "claude"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	cl.controller(t, "source-controller",
		"--artifact-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"--artifact-base-url", "http://127.0.0.1:1",
		"--artifact-dir", t.TempDir())
	key := types.NamespacedName{Namespace: namespace, Name: name + "-src"}
	var repo v1alpha1.Repository
	eventually(t, "repository artifact", func() bool {
		return cl.client.Get(ctx, key, &repo) == nil && repo.Status.Artifact != nil
	})
	repo.Status.RunnerImage = &v1alpha1.RunnerImage{Declared: "registry.example/acme/web-env:1",
		Manifest: ".patchy/agent.yaml", Verified: true, SearchPath: "/usr/local/bin:/usr/bin:/bin",
		Image: "registry.example/acme/web-env@sha256:" + strings.Repeat("cd", 32)}
	if err := cl.client.Status().Update(ctx, &repo); err != nil {
		t.Fatal(err)
	}

	cl.controller(t, "remediation-controller", append([]string{
		"--repository-images", "--agent-ephemeral-storage", "1Gi",
		"--claude-agent-image", "patchy/claude-agent-runner:e2e", "--broker-url", "http://localhost:8080"},
		registryProfileArgs("acme/web")...)...)

	var rem v1alpha1.Remediation
	remKey := types.NamespacedName{Namespace: namespace, Name: name + "-rem-1"}
	eventually(t, "the remediation to launch", func() bool {
		return cl.client.Get(ctx, remKey, &rem) == nil && rem.Status.JobRef != nil && rem.Status.RunnerImage != nil
	})
	if rem.Status.RunnerImage.Source != v1alpha1.RunnerImageSourceRepository ||
		rem.Status.RunnerImage.NetworkProfile != "web" {
		t.Errorf("recorded %+v, want the repository's image with profile web", rem.Status.RunnerImage)
	}
	var job batchv1.Job
	if err := cl.client.Get(ctx, client.ObjectKey{Namespace: agentsNS, Name: rem.Status.JobRef.Name}, &job); err != nil {
		t.Fatal(err)
	}
	checkRegistryJob(t, job, true)
}
