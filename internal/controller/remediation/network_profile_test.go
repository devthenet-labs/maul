// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"net/url"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/runnerguard"
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
	base, err := url.Parse("http://patchy-egress-proxy.patchy.svc.cluster.local")
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
				Namespace: "patchy-agents", AllowRepositoryImages: true, NetworkProfiles: true,
				EphemeralStorage: "8Gi",
				Runners: map[string]jobs.Runner{"claude": {Image: "claude-agent-runner:1", Brokered: true,
					Inject: []string{"agent-runner", "claude"}}},
			}, nil)
			r.Images = profileGuard(t)
			remOnce(t, r, rem.Name)

			list, err := cs.BatchV1().Jobs("patchy-agents").List(t.Context(), metav1.ListOptions{})
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
