// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/previewtools"
)

// realJobs points h's reconciler at a real jobs builder over a fake
// clientset, as intent-controller configures it with the agents host on.
func realJobs(h *harness) *k8sfake.Clientset {
	cs := k8sfake.NewClientset()
	cfg := jobs.Config{
		Namespace: agentsNS, ServiceAccount: "maul-agent", Deadline: time.Hour, TTL: 2 * time.Hour,
		Runners: map[string]jobs.Runner{"claude": {Image: "ghcr.io/devthenet-labs/maul/claude-agent-runner:1",
			Brokered: true, Inject: []string{"agent-runner", "claude"},
			Env: map[string]string{"ANTHROPIC_BASE_URL": "http://maul-egress-broker.maul.svc:8080/anthropic"}}},
		CPURequest: "500m", MemoryRequest: "1Gi", AllowRepositoryImages: true, EphemeralStorage: "8Gi",
		NetworkProfiles: true, Resolver: resolver{}, SandboxProbeDNS: "10.43.0.10:53", DNS: jobs.DNSNone,
	}
	h.r.Tools = Kube{CS: cs, Reader: readerOf{cs}, Jobs: jobs.New(cs, cfg, nil), Namespace: agentsNS}
	return cs
}

// TestRealBuilderProfile: under a network profile the build gets, the tool
// pod gets the same one and the relay's programs its environment, the
// image's module cache never overriding the profile's; a workspace that
// recorded none while the build gets one is stale.
func TestRealBuilderProfile(t *testing.T) {
	set, err := netprofile.Parse([]byte(`[{"name":"go","route":"go","port":8091,"repositories":["octo/render"],` +
		`"modules":["github.com/google/uuid"],"bakedModCache":"/opt/go/pkg/mod"}]`))
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("http://maul-egress-proxy.maul.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	w.pv.Spec.Workspace.NetworkProfile = "go"
	h := newHarness(t, w)
	h.r.Images.ProfilesEnabled, h.r.Images.Profiles, h.r.Images.ProxyBase = true, set, base
	cs := realJobs(h)
	h.reconcile(t)
	if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationRunning {
		t.Fatalf("status = %+v, want Running", pv.Status)
	}
	job, err := cs.BatchV1().Jobs(agentsNS).Get(context.Background(), pvName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Labels[netprofile.LabelKey("go")] != netprofile.LabelValue {
		t.Errorf("labels %v: the tool pod does not carry the build's profile", job.Labels)
	}
	secret, err := cs.CoreV1().Secrets(agentsNS).Get(context.Background(), pvName+jobs.ToolsSecretSuffix,
		metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := previewtools.ParseConfig(secret.Data[previewtools.ConfigFile])
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range c.Workspace.Env {
		env[e.Name] = e.Value
	}
	if !strings.HasPrefix(env["GOPROXY"], "http://") || !strings.Contains(env["GOPROXY"], ":8091/") ||
		env["GOMODCACHE"] != "/tmp/go/pkg/mod" {
		t.Errorf("relay env %v, want the profile's", env)
	}

	h2 := newHarness(t, newWorld(t))
	h2.r.Images = h.r.Images
	realJobs(h2)
	h2.reconcile(t)
	if pv := h2.pv(t); pv.Status.Phase != v1alpha1.VerificationStale {
		t.Errorf("status = %+v, want Stale: the workspace's profile is not the build's", pv.Status)
	}
}
