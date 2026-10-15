// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// The tunnel profile test runs intent-controller as the chart would with a
// step-3 profile, py, on port 8092: a tunnel-only profile listing the
// Python index and its file host, the pip preset, and two names that are a
// Forge's web hosts only once the test creates that Forge. As with the
// other profile tests, envtest enforces no NetworkPolicy and runs no
// proxy, so this proves what the Jobs carry and what the controller
// refuses (§8.8 of the step-3 spec).
const (
	tunnelLabel = "patchy.bitwisemedia.uk/network-py"
	// tunnelBroker is the broker given as an IP literal, which NO_PROXY
	// must still name (critique #10), or the CLI's model traffic would go
	// to the proxy.
	tunnelBroker = "http://10.0.0.5:8080"
	// ghesHost is a GHES Forge's host, listed as a tunnel host; octoHost
	// the web host of a data-residency Forge configured by its API host.
	ghesHost = "ghe.corp.dev"
	octoHost = "octo.ghe.com"
)

// tunnelProfilesFor is the profile set's JSON, its one profile matching
// owner/name on 127.0.0.1.
func tunnelProfilesFor(ownerName string) string {
	return fmt.Sprintf(`[{"name":"py","route":"tunnel","port":8092,"host":"127.0.0.1","repositories":[%q],`+
		`"modules":[],"bakedModCache":"","allowPseudoVersions":false,`+
		`"hosts":["pypi.org","files.pythonhosted.org",%q,%q],"clients":["pip"]}]`, ownerName, ghesHost, octoHost)
}

// wantTunnelEnv is the tunnel environment a py profile Job's agent
// carries, verbatim (§4.2): the proxy for https, NO_PROXY naming the
// artifact server (127.0.0.1), the broker's IP literal and the proxy
// (localhost) with loopback and the cluster suffixes, the routes and hosts
// agent-runner reads, and the pip preset. The plain-http and ALL_PROXY
// names stay blank.
var wantTunnelEnv = map[string]string{
	"PATCHY_NETWORK_PROFILE":        "py",
	"HTTPS_PROXY":                   "http://localhost:8092",
	"https_proxy":                   "http://localhost:8092",
	"NO_PROXY":                      ".cluster.local,.svc,10.0.0.5,127.0.0.1,::1,localhost",
	"no_proxy":                      ".cluster.local,.svc,10.0.0.5,127.0.0.1,::1,localhost",
	"PATCHY_NETWORK_ROUTES":         "tunnel",
	"PATCHY_NETWORK_HOSTS":          "files.pythonhosted.org," + ghesHost + "," + octoHost + ",pypi.org",
	"PIP_PROXY":                     "http://localhost:8092",
	"PIP_DISABLE_PIP_VERSION_CHECK": "1",
	"PIP_NO_INPUT":                  "1",
	"HTTP_PROXY":                    "",
	"http_proxy":                    "",
	"ALL_PROXY":                     "",
	"all_proxy":                     "",
}

// checkTunnelJob asserts that job carries the py profile (want) or none:
// the label on the Job and its pod, the tunnel environment verbatim, no Go
// route, and the proxy's host pinned in the hosts file under DNS none.
func checkTunnelJob(t *testing.T, job batchv1.Job, want bool) {
	t.Helper()
	for what, labels := range map[string]map[string]string{"job": job.Labels, "pod": job.Spec.Template.Labels} {
		if _, ok := labels[tunnelLabel]; ok != want {
			t.Errorf("%s %s label %s present = %v, want %v", job.Name, what, tunnelLabel, ok, want)
		}
	}
	env := agentEnvOf(job)
	for name, value := range wantTunnelEnv {
		got, ok := env[name]
		if want && (!ok || got != value) {
			t.Errorf("%s %s = %q (set %v), want %q", job.Name, name, got, ok, value)
		}
		if !want && value != "" && got == value {
			t.Errorf("%s carries %s=%q without a profile", job.Name, name, got)
		}
	}
	if strings.Contains(env["GOPROXY"], "8092") {
		t.Errorf("%s carries GOPROXY=%q from a tunnel-only profile", job.Name, env["GOPROXY"])
	}
	pinned := slices.ContainsFunc(job.Spec.Template.Spec.HostAliases, func(a corev1.HostAlias) bool {
		return slices.Contains(a.Hostnames, "localhost")
	})
	if want && (!pinned || job.Spec.Template.Spec.DNSPolicy != corev1.DNSNone) {
		t.Errorf("%s dnsPolicy %s hostAliases %+v, want DNS none with the proxy's host pinned", job.Name,
			job.Spec.Template.Spec.DNSPolicy, job.Spec.Template.Spec.HostAliases)
	}
}

// TestIntentTunnelProfile: intent-controller started with a tunnel profile
// for the application repository. The plan Job carries none of it; the
// build Job carries the label and the tunnel environment verbatim (NO_PROXY
// naming the broker's IP literal and the artifact host) with the proxy's
// host pinned under DNS none, the run records the profile and its report
// is read back from status, and the pull request names the hosts. Then a
// Forge whose host is a listed tunnel host (GHES), and one configured by
// its API host whose web host is listed (data residency), each refuse the
// next intent's build before any Job exists, naming the host.
func TestIntentTunnelProfile(t *testing.T) {
	// Three intents are filed, and the first stays in review while the
	// others run, so the Project admits three at once.
	cl, gh, registry := startSourceStack(t, appRepo)
	e := withIntentsFor(t, cl, gh, v1alpha1.ProjectSpec{
		IntentRepository: intentRepoURL,
		Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login, reader.Login}},
		Repositories:     []v1alpha1.ProjectRepository{{Name: appRepo, URL: appRepoURL}},
		Limits:           v1alpha1.ProjectLimits{MaxActiveIntents: 3},
	}, "--broker-url", tunnelBroker, "--network-profiles-enabled",
		"--network-profiles", tunnelProfilesFor(intentOwner+"/"+appRepo),
		"--egress-proxy-url", proxyURL, "--agent-dns", "none")
	e.registry = registry

	number, name := e.fileIntent(t)
	planJob := e.kubelet.waitRun(t, "the plan job to run", phaseIs("plan"))
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	checkTunnelJob(t, planJob.Job, false)

	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	buildRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, appRepo, 1)
	job := e.kubelet.waitRun(t, "the build job to run", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(buildRun, intentRunKind, 1)
	})
	checkTunnelJob(t, job.Job, true)
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	run := e.run(t, buildRun)
	if run.Status.RunnerImage == nil || run.Status.RunnerImage.NetworkProfile != "py" {
		t.Errorf("build run recorded %+v, want the py profile", run.Status.RunnerImage)
	}
	// The agent's report is the run's durable record of what the proxy
	// refused (§2.8 point 1): it is on the run's status, readable with
	// kubectl alone.
	if strings.TrimSpace(run.Status.Report) == "" {
		t.Errorf("build run %s has no report on its status", buildRun)
	}
	pr, ok := e.gh.Pull(int(prOf(t, in, appRepoURL).Number))
	if want := "network profile `py` (HTTPS downloads from `files.pythonhosted.org`"; !ok ||
		!strings.Contains(pr.Body, want) || strings.Contains(pr.Body, "Go modules") {
		t.Errorf("pull request body (found %v) lacks %q or names Go modules:\n%s", ok, want, pr.Body)
	}

	for _, f := range []struct{ name, baseURL, host string }{
		{"ghes", "https://" + ghesHost + "/api/v3", ghesHost},
		{"octo", "https://api." + octoHost, octoHost},
	} {
		t.Run(f.name, func(t *testing.T) {
			ctx := context.Background()
			forge := &v1alpha1.Forge{
				ObjectMeta: metav1.ObjectMeta{Name: f.name, Namespace: namespace},
				Spec: v1alpha1.ForgeSpec{Provider: v1alpha1.ForgeProviderGitHub, BaseURL: f.baseURL,
					SecretRef: v1alpha1.LocalSecretReference{Name: "patchy-forge"}},
			}
			if err := e.cl.client.Create(ctx, forge); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.cl.client.Delete(ctx, forge) })

			number, name := e.fileIntent(t)
			in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
			afterSecond(in.Status.Plan.PostedAt.Time)
			e.gh.LabelIssue(number, approveLabel, approver)
			buildRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, appRepo, 1)
			var run *v1alpha1.IntentRun
			eventually(t, "the build run to be refused", func() bool {
				runs := e.runs(t, name)
				r, ok := runs[buildRun]
				run = &r
				return ok && r.Status.Outcome != ""
			})
			if run.Status.Outcome != "launch_refused" || !strings.Contains(run.Status.Detail, f.host) ||
				!strings.Contains(run.Status.Detail, "Forge") {
				t.Errorf("build run settled %q: %q, want launch_refused naming %s as a Forge's host",
					run.Status.Outcome, run.Status.Detail, f.host)
			}
			if run.Status.JobRef != nil {
				t.Errorf("build run %s has Job %+v; a refused profile launches none", buildRun, run.Status.JobRef)
			}
			// The next case's refusal must name its own Forge's host
			// alone, so this Forge is gone before it starts.
			if err := e.cl.client.Delete(ctx, forge); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the Forge to be deleted", func() bool {
				return kerrors.IsNotFound(e.cl.client.Get(ctx, client.ObjectKeyFromObject(forge), &v1alpha1.Forge{}))
			})
		})
	}
}
