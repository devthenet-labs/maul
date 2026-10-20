// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"context"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/runnerguard"
)

// profileGuard is a guard with repository images and network profiles on
// and one profile, go, for acme/orders (the fixtures' repository).
func profileGuard(t *testing.T) runnerguard.Guard {
	t.Helper()
	set, err := netprofile.Parse([]byte(`[{"name":"go","route":"go","port":8090,` +
		`"repositories":["acme/orders"],"modules":["github.com/google/uuid"],"bakedModCache":"/opt/go/pkg/mod"}]`))
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("http://maul-egress-proxy.maul.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	return runnerguard.Guard{Enabled: true, ProfilesEnabled: true, Profiles: set, ProxyBase: base}
}

// TestRemediationLaunchAppliesNetworkProfile: a remediate launch on the
// repository's accepted image, for a repository the operator's profile
// matches, creates a Job carrying the profile label and records the
// profile from Create's read-back; a revived finding runs the default image
// and gets none. The Jobs are built by the real jobs client.
func TestRemediationLaunchAppliesNetworkProfile(t *testing.T) {
	investigated := []v1alpha1.Phase{v1alpha1.PhaseOpened, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating}
	for _, tt := range []struct {
		name        string
		history     []v1alpha1.Phase
		wantProfile string
	}{
		{"accepted image", append(investigated, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating), "go"},
		{"revived out of HandedOff", append(investigated, v1alpha1.PhaseHandedOff, v1alpha1.PhaseQueued,
			v1alpha1.PhaseRemediating), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objs := withImage(acceptedImage(), "", tt.history...)
			rem := objs[1].(*v1alpha1.Remediation)
			rem.Status.JobRef = nil
			if tt.wantProfile == "" {
				// Attempt 1 was held and approved; this is attempt 2, on the
				// default image.
				held := rem.DeepCopy()
				held.Status = v1alpha1.RemediationStatus{Phase: v1alpha1.RunFailed,
					Conditions: []metav1.Condition{runnerguard.HeldCondition(1, runnerguard.SkipRevived)}}
				rem.Name, rem.Spec.Attempt = "finding-aa-1-rem-2", 2
				objs = append(objs, held)
			}
			r, c := newRemediation(t, &fakeCRRunner{}, &fakeForge{}, objs...)
			cs := k8sfake.NewClientset()
			r.Runner = jobs.New(cs, jobs.Config{
				Namespace: "maul-agents", AllowRepositoryImages: true, NetworkProfiles: true,
				EphemeralStorage: "8Gi",
				Runners: map[string]jobs.Runner{"claude": {Image: "claude-agent-runner:1", Brokered: true,
					Inject: []string{"agent-runner", "claude"}}},
			}, nil)
			r.Images = profileGuard(t)
			remOnce(t, r, rem.Name)

			list, err := cs.BatchV1().Jobs("maul-agents").List(t.Context(), metav1.ListOptions{})
			if err != nil || len(list.Items) != 1 {
				t.Fatalf("jobs = %v, %v; want one", list, err)
			}
			job := list.Items[0]
			_, labelled := job.Spec.Template.Labels[netprofile.LabelKey("go")]
			if labelled != (tt.wantProfile != "") {
				t.Errorf("pod labels %v, want the profile label %v", job.Spec.Template.Labels, tt.wantProfile != "")
			}
			var got v1alpha1.Remediation
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(rem), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status.RunnerImage == nil || got.Status.RunnerImage.NetworkProfile != tt.wantProfile {
				t.Errorf("recorded runner image %+v, want network profile %q", got.Status.RunnerImage, tt.wantProfile)
			}
		})
	}
}

// TestRemediationPRBodySaysNetworkProfile: the pull request of a run whose
// Job carried a network profile, as the Remediation recorded it at launch,
// says so; one without says nothing about it.
func TestRemediationPRBodySaysNetworkProfile(t *testing.T) {
	for _, profile := range []string{"go", ""} {
		t.Run("profile="+profile, func(t *testing.T) {
			objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceRepository)
			objs[1].(*v1alpha1.Remediation).Status.RunnerImage.NetworkProfile = profile
			fw := &fakeForge{}
			r, _ := newRemediation(t, &fakeCRRunner{done: true, events: crdRemediationEvent(true)}, fw, objs...)
			remReconcile(t, r)
			if len(fw.bodies) != 1 {
				t.Fatalf("PR bodies = %d, want 1", len(fw.bodies))
			}
			if got := strings.Contains(fw.bodies[0], "network profile `go`"); got != (profile != "") {
				t.Errorf("profile line present = %v, want %v:\n%s", got, profile != "", fw.bodies[0])
			}
		})
	}
}

// registryProfileGuard is profileGuard with a second profile, web, holding
// an npm registry grant, for acme/orders too (so the go profile matches
// nothing once the test points the guard at web alone).
func registryProfileGuard(t *testing.T) runnerguard.Guard {
	t.Helper()
	g := profileGuard(t)
	set, err := netprofile.Parse([]byte(`[{"name":"web","port":8091,"repositories":["acme/orders"],` +
		`"registries":[{"name":"art-npm","ecosystem":"npm","hosts":["acme.jfrog.io"],"packagePolicy":"registry"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	g.Profiles = set
	return g
}

// TestRemediationPRBodyNamesRegistryRoutes: the pull request of a run whose
// recorded profile holds registry grants in the controller's profile set
// names each ecosystem and its registry; a recorded profile the set no
// longer holds keeps step 1's line.
func TestRemediationPRBodyNamesRegistryRoutes(t *testing.T) {
	for profile, want := range map[string]string{
		"web": "network profile `web` (npm via `art-npm`), so it could fetch packages",
		"go":  "network profile `go`, so it could fetch Go modules",
	} {
		t.Run(profile, func(t *testing.T) {
			objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceRepository)
			objs[1].(*v1alpha1.Remediation).Status.RunnerImage.NetworkProfile = profile
			fw := &fakeForge{}
			r, _ := newRemediation(t, &fakeCRRunner{done: true, events: crdRemediationEvent(true)}, fw, objs...)
			r.Images = registryProfileGuard(t)
			remReconcile(t, r)
			if len(fw.bodies) != 1 || !strings.Contains(fw.bodies[0], want) {
				t.Errorf("PR bodies %q, want one saying %q", fw.bodies, want)
			}
		})
	}
}

// TestRemediationChangesetNamingTheProxyIsRefused: a run whose Job carried
// a network profile (as recorded at launch) and whose changeset points a
// lockfile at the egress proxy is refused before any forge call, naming
// the file; the same changeset from a run without a profile is pushed.
func TestRemediationChangesetNamingTheProxyIsRefused(t *testing.T) {
	for _, profile := range []string{"go", ""} {
		t.Run("profile="+profile, func(t *testing.T) {
			events := crdRemediationEvent(true)
			events[0].Remediation.Changeset.Upserts = append(events[0].Remediation.Changeset.Upserts,
				envelope.FileChange{Path: "package-lock.json", Mode: "100644", ContentB64: base64.StdEncoding.EncodeToString(
					[]byte(`"resolved": "http://maul-egress-proxy.maul.svc.cluster.local:8091/r/ca-npm/x/-/x-1.0.0.tgz"`))})
			objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceRepository)
			objs[1].(*v1alpha1.Remediation).Status.RunnerImage.NetworkProfile = profile
			fw := &fakeForge{}
			r, c := newRemediation(t, &fakeCRRunner{done: true, events: events}, fw, objs...)
			r.Images = profileGuard(t)
			remReconcile(t, r)
			if profile == "" {
				if len(fw.pushed) != 1 {
					t.Errorf("pushed = %v, want the changeset pushed", fw.pushed)
				}
				return
			}
			rem := getRem(t, c)
			if len(fw.pushed) != 0 || fw.prCalls != 0 || rem.Status.Stage == nil ||
				rem.Status.Stage.Outcome != string(envelope.OutcomeChangesetRejected) ||
				!strings.Contains(rem.Status.Stage.Detail, "package-lock.json") {
				t.Errorf("pushed %v, stage %+v; want no forge call and changeset_rejected naming the file",
					fw.pushed, rem.Status.Stage)
			}
		})
	}
}

// forgeList is a fixed Forge host list (runnerguard.ForgeHosts).
type forgeList []string

func (f forgeList) WebHosts(context.Context, string) ([]string, error) { return f, nil }

// TestRemediationGrantNamingAForgeFailsTheLaunch: a remediate launch whose
// profile's registry grant names a Forge's host fails the run as
// launch_refused, naming the host, with no Job created; with no Forge
// matching it launches with the grants on the spec.
func TestRemediationGrantNamingAForgeFailsTheLaunch(t *testing.T) {
	for _, tt := range []struct {
		name   string
		forges forgeList
		refuse bool
	}{
		{"a Forge on the registry's domain", forgeList{"github.com", "jfrog.io"}, true},
		{"no Forge matches", forgeList{"github.com", "ghe.corp.example"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objs := withImage(acceptedImage(), "", v1alpha1.PhaseOpened, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating)
			objs[1].(*v1alpha1.Remediation).Status.JobRef = nil
			runner := &fakeCRRunner{}
			r, c := newRemediation(t, runner, &fakeForge{}, objs...)
			r.Images = registryProfileGuard(t)
			r.Images.Forges = tt.forges
			remReconcile(t, r)
			rem := getRem(t, c)
			if !tt.refuse {
				if len(runner.created) != 1 || runner.created[0].NetworkProfile == nil ||
					len(runner.created[0].NetworkProfile.Registries) != 1 {
					t.Fatalf("created %+v, want one Job with the web profile's grant", runner.created)
				}
				return
			}
			if len(runner.created) != 0 || rem.Status.Stage == nil || rem.Status.Stage.Outcome != outcomeLaunchRefused ||
				!strings.Contains(rem.Status.Stage.Detail, "acme.jfrog.io") {
				t.Errorf("created %d Jobs, stage %+v; want none and launch_refused naming the host",
					len(runner.created), rem.Status.Stage)
			}
		})
	}
}

// tunnelProfileGuard is profileGuard with one tunnel-only profile for
// acme/orders: the Python index and its file host, with the pip preset.
func tunnelProfileGuard(t *testing.T) runnerguard.Guard {
	t.Helper()
	g := profileGuard(t)
	set, err := netprofile.Parse([]byte(`[{"name":"py","route":"tunnel","port":8092,"repositories":["acme/orders"],` +
		`"modules":[],"bakedModCache":"","hosts":["pypi.org","files.pythonhosted.org"],"clients":["pip"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	g.Profiles = set
	return g
}

// TestRemediationTunnelNamingAForgeFailsTheLaunch: a remediate launch whose
// profile's tunnel hosts include a Forge's web host fails the run as
// launch_refused, naming the host, with no Job created; with no Forge
// matching it launches with the tunnel hosts and presets on the spec; and
// the pull request of such a run names the hosts, never Go modules.
func TestRemediationTunnelNamingAForgeFailsTheLaunch(t *testing.T) {
	for _, tt := range []struct {
		name   string
		forges forgeList
		refuse bool
	}{
		{"a Forge above a tunnel host", forgeList{"github.com", "pythonhosted.org"}, true},
		{"no Forge matches", forgeList{"github.com", "ghe.corp.example"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objs := withImage(acceptedImage(), "", v1alpha1.PhaseOpened, v1alpha1.PhaseQueued, v1alpha1.PhaseRemediating)
			objs[1].(*v1alpha1.Remediation).Status.JobRef = nil
			runner := &fakeCRRunner{}
			r, c := newRemediation(t, runner, &fakeForge{}, objs...)
			r.Images = tunnelProfileGuard(t)
			r.Images.Forges = tt.forges
			remReconcile(t, r)
			rem := getRem(t, c)
			if !tt.refuse {
				if len(runner.created) != 1 || runner.created[0].NetworkProfile == nil ||
					strings.Join(runner.created[0].NetworkProfile.Hosts, ",") != "pypi.org,files.pythonhosted.org" ||
					strings.Join(runner.created[0].NetworkProfile.Clients, ",") != "pip" {
					t.Fatalf("created %+v, want one Job with the py profile's hosts and preset", runner.created)
				}
				return
			}
			if len(runner.created) != 0 || rem.Status.Stage == nil || rem.Status.Stage.Outcome != outcomeLaunchRefused ||
				!strings.Contains(rem.Status.Stage.Detail, "files.pythonhosted.org") {
				t.Errorf("created %d Jobs, stage %+v; want none and launch_refused naming the host",
					len(runner.created), rem.Status.Stage)
			}
		})
	}

	objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceRepository)
	objs[1].(*v1alpha1.Remediation).Status.RunnerImage.NetworkProfile = "py"
	fw := &fakeForge{}
	r, _ := newRemediation(t, &fakeCRRunner{done: true, events: crdRemediationEvent(true)}, fw, objs...)
	r.Images = tunnelProfileGuard(t)
	remReconcile(t, r)
	want := "network profile `py` (HTTPS downloads from `files.pythonhosted.org` and `pypi.org`)"
	if len(fw.bodies) != 1 || !strings.Contains(fw.bodies[0], want) || strings.Contains(fw.bodies[0], "Go modules") {
		t.Errorf("PR bodies %q, want one saying %q and no Go modules", fw.bodies, want)
	}
}
