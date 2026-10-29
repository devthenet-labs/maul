// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/sandboxprobe"
)

// The role fixtures: the PreviewVerification's UID, the tool pod's address
// from its status, and the controller's own resolver.
const (
	pvUID       = "6f1c2d3e-4b5a-4c7d-8e9f-0a1b2c3d4e5f"
	toolPodIP   = "10.42.3.17"
	clusterDNS  = "10.96.0.10:53"
	evalRunName = "intent-x-ev-r1-a1"
)

// roleConfig is brokeredConfig on the cluster resolver (the operator's
// agent.dns default) with the controller's resolver known for the dns probe.
func roleConfig() Config {
	cfg := brokeredConfig()
	cfg.Resolver = clusterResolver()
	cfg.SandboxProbeDNS = clusterDNS
	return cfg
}

// evaluatorSpec is an evaluate IntentRun's Job: no tree, no artifact, the
// tool pod pinned by address.
func evaluatorSpec() Spec {
	return Spec{
		Repo:                  "octo/repo",
		Attempt:               1,
		Phase:                 "evaluate",
		Harness:               "claude",
		Model:                 "anthropic/claude-sonnet-5",
		IssueMarkdown:         "# Intent\n",
		InvestigationMarkdown: "# Evaluation handoff\n",
		Kind:                  "intent",
		Owner:                 evalRunName,
		Finding:               evalRunName,
		Role:                  v1alpha1.RoleEvaluator,
		Evaluation:            pvUID,
		ToolHost:              &HostAlias{Name: ToolsHostName, IP: toolPodIP},
	}
}

// checkReviewSpec is a checkreview IntentRun's Job: no tree, no tool pod.
func checkReviewSpec() Spec {
	spec := evaluatorSpec()
	spec.Phase = "checkreview"
	spec.Owner, spec.Finding = "intent-x-cr-r1-a1", "intent-x-cr-r1-a1"
	spec.Role = v1alpha1.RoleCheckReview
	spec.ToolHost = nil
	return spec
}

// buildRoleJob runs build (shape plus name resolution) without creating
// anything.
func buildRoleJob(t *testing.T, cfg Config, spec Spec) *batchv1.Job {
	t.Helper()
	job, err := New(fake.NewClientset(), cfg, nil).build(context.Background(),
		NameFor(spec.Finding, spec.Kind, int32(spec.Attempt)), spec)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return job
}

// TestGoldenEvaluatorJob pins the evaluator's whole Job: role labels, no
// fetch, the strict probe with the dns target, DNS none with the broker and
// maul-tools pinned, MAUL_TOOLS_URL.
func TestGoldenEvaluatorJob(t *testing.T) {
	goldenJob(t, "job_evaluator", buildRoleJob(t, roleConfig(), evaluatorSpec()))
}

// TestGoldenCheckReviewJob pins the check reviewer's: role labels, no
// fetch, the wait probe, DNS none with only the broker pinned.
func TestGoldenCheckReviewJob(t *testing.T) {
	goldenJob(t, "job_checkreview", buildRoleJob(t, roleConfig(), checkReviewSpec()))
}

// TestRoleForcesDNSNone: both roles get dnsPolicy None whatever Config.DNS
// says, the broker pinned, and the artifact server never asked for (they
// fetch nothing). Only the evaluator gets maul-tools, at the tool pod's
// address, which is never resolved.
func TestRoleForcesDNSNone(t *testing.T) {
	for _, mode := range []DNSMode{"", DNSCluster, DNSNone} {
		for _, spec := range []Spec{evaluatorSpec(), checkReviewSpec()} {
			t.Run(string(mode)+"/"+spec.Role, func(t *testing.T) {
				r := clusterResolver()
				cfg := roleConfig()
				cfg.DNS, cfg.Resolver = mode, r
				pod := createJob(t, cfg, spec).Spec.Template.Spec
				if pod.DNSPolicy != corev1.DNSNone || pod.DNSConfig == nil ||
					!slices.Equal(pod.DNSConfig.Nameservers, []string{deadNameserver}) {
					t.Fatalf("dnsPolicy %q, dnsConfig %+v: want None on the dead nameserver", pod.DNSPolicy, pod.DNSConfig)
				}
				if !slices.Equal(r.asked, []string{brokerHost}) {
					t.Errorf("resolver asked %v, want only the broker", r.asked)
				}
				// Sorted by address: the tool pod's 10.42 before the broker's 10.100.
				want := []corev1.HostAlias{{IP: "10.100.40.2", Hostnames: []string{brokerHost}}}
				if spec.Role == v1alpha1.RoleEvaluator {
					want = append([]corev1.HostAlias{{IP: toolPodIP, Hostnames: []string{ToolsHostName}}}, want...)
				}
				if got := marshal(t, pod.HostAliases); got != marshal(t, want) {
					t.Errorf("hostAliases:\n%s\nwant:\n%s", got, marshal(t, want))
				}
			})
		}
	}
}

// TestRoleLabels: the role (and, with one, the verification's UID) is on the
// Job and its pod template alike, the selectors the role's egress policy and
// the tool pod's ingress policy match; the per-Job Secret carries neither.
func TestRoleLabels(t *testing.T) {
	noEval := checkReviewSpec()
	noEval.Evaluation = ""
	tests := []struct {
		name     string
		spec     Spec
		wantRole string
		wantPV   string
	}{
		{"evaluator", evaluatorSpec(), v1alpha1.RoleEvaluator, pvUID},
		{"checkreview", checkReviewSpec(), v1alpha1.RoleCheckReview, pvUID},
		{"checkreview without a verification", noEval, v1alpha1.RoleCheckReview, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewClientset()
			name, _, err := New(cs, roleConfig(), nil).Create(context.Background(), tt.spec)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			job, err := cs.BatchV1().Jobs("maul-agents").Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for where, lbls := range map[string]map[string]string{
				"job": job.Labels, "pod template": job.Spec.Template.Labels,
			} {
				if got := lbls[v1alpha1.LabelRole]; got != tt.wantRole {
					t.Errorf("%s %s = %q, want %q", where, v1alpha1.LabelRole, got, tt.wantRole)
				}
				got, ok := lbls[v1alpha1.LabelPreviewVerification]
				if got != tt.wantPV || ok != (tt.wantPV != "") {
					t.Errorf("%s %s = %q (present %v), want %q", where, v1alpha1.LabelPreviewVerification, got, ok, tt.wantPV)
				}
			}
			secret, err := cs.CoreV1().Secrets("maul-agents").Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{v1alpha1.LabelRole, v1alpha1.LabelPreviewVerification} {
				if _, ok := secret.Labels[k]; ok {
					t.Errorf("secret carries %s", k)
				}
			}
		})
	}
}

// TestRolePrepare: neither role fetches, extracts or commits anything; both
// stage the handoff under /work/input and make /work/scratch. The evaluator
// ends with the strict probe over every target, dns included, and is handed
// the controller's resolver; the check reviewer ends with the wait probe.
func TestRolePrepare(t *testing.T) {
	strict := toolsBinDir + "/" + agentRunnerBin + " " + sandboxprobe.Command +
		" --mode=strict --targets=internet,k8s,imds,dns\n"
	for _, tt := range []struct {
		spec    Spec
		tail    string
		wantDNS bool
	}{
		{evaluatorSpec(), strict, true},
		{checkReviewSpec(), waitProbeScript, false},
	} {
		t.Run(tt.spec.Role, func(t *testing.T) {
			job := buildRoleJob(t, roleConfig(), tt.spec)
			init := job.Spec.Template.Spec.InitContainers[0]
			script := init.Command[2]
			if !strings.HasSuffix(script, tt.tail) {
				t.Errorf("prepare script does not end with %q:\n%s", tt.tail, script)
			}
			for _, banned := range []string{"curl", "tar ", "git ", "MAUL_ARTIFACT", "/workspace"} {
				if strings.Contains(script, banned) {
					t.Errorf("prepare script contains %q:\n%s", banned, script)
				}
			}
			for _, want := range []string{"mkdir -p /work/input /work/scratch", "/work/input/issue.md"} {
				if !strings.Contains(script, want) {
					t.Errorf("prepare script lacks %q:\n%s", want, script)
				}
			}
			var dns string
			for _, e := range init.Env {
				if strings.HasPrefix(e.Name, "MAUL_ARTIFACT") || e.Name == "MAUL_BASE_SHA" {
					t.Errorf("init env carries %s", e.Name)
				}
				if e.Name == sandboxprobe.DNSEnv {
					dns = e.Value
				}
			}
			if (dns == clusterDNS) != tt.wantDNS || (!tt.wantDNS && dns != "") {
				t.Errorf("init %s = %q, want set %v", sandboxprobe.DNSEnv, dns, tt.wantDNS)
			}
		})
	}
}

// TestRoleWorkspace: a role pod works in /work (HOME too), with nothing at
// /workspace, so the posture's /work/scratch is the only directory its file
// tools see.
func TestRoleWorkspace(t *testing.T) {
	for _, spec := range []Spec{evaluatorSpec(), checkReviewSpec()} {
		job := buildRoleJob(t, roleConfig(), spec)
		pod := job.Spec.Template.Spec
		for _, ct := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
			for _, m := range ct.VolumeMounts {
				if m.Name == volWorkspace && m.MountPath != roleWorkDir {
					t.Errorf("%s/%s mounts the workspace at %s, want %s", spec.Role, ct.Name, m.MountPath, roleWorkDir)
				}
			}
			if v := envValue(ct.Env, "HOME"); v != roleWorkDir {
				t.Errorf("%s/%s HOME = %q, want %s", spec.Role, ct.Name, v, roleWorkDir)
			}
		}
		if v := envValue(pod.Containers[0].Env, "MAUL_WORKSPACE"); v != roleWorkDir {
			t.Errorf("%s MAUL_WORKSPACE = %q, want %s", spec.Role, v, roleWorkDir)
		}
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Errorf("%s automountServiceAccountToken = %v, want false", spec.Role, pod.AutomountServiceAccountToken)
		}
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// TestEvaluatorToolsURL: only the evaluator is told where its tool pod is,
// by the reserved name, which Config.Env cannot set or shadow.
func TestEvaluatorToolsURL(t *testing.T) {
	if !reservedEnv[ToolsURLEnv] || !slices.Contains(PerJobEnvNames(), ToolsURLEnv) {
		t.Fatalf("%s is not a reserved per-Job name", ToolsURLEnv)
	}
	cfg := roleConfig()
	cfg.Env[ToolsURLEnv] = "http://attacker.example:8090"
	ev := buildRoleJob(t, cfg, evaluatorSpec()).Spec.Template.Spec.Containers[0].Env
	var got []string
	for _, e := range ev {
		if e.Name == ToolsURLEnv {
			got = append(got, e.Value)
		}
	}
	if !slices.Equal(got, []string{"http://maul-tools:8090"}) {
		t.Errorf("evaluator %s = %v, want exactly http://maul-tools:8090", ToolsURLEnv, got)
	}
	cr := buildRoleJob(t, cfg, checkReviewSpec()).Spec.Template.Spec.Containers[0].Env
	if v := envValue(cr, ToolsURLEnv); v != "" {
		t.Errorf("check reviewer %s = %q, want unset", ToolsURLEnv, v)
	}
}

// TestRoleRefusals: every contradiction is refused before anything is
// created, the evaluator without its tool host among them.
func TestRoleRefusals(t *testing.T) {
	tests := []struct {
		name   string
		cfg    func(*Config)
		mutate func(*Spec)
	}{
		{"evaluator without ToolHost", nil, func(s *Spec) { s.ToolHost = nil }},
		{"evaluator without a verification", nil, func(s *Spec) { s.Evaluation = "" }},
		{"tool host without an IP", nil, func(s *Spec) { s.ToolHost.IP = "" }},
		{"tool host IP not an address", nil, func(s *Spec) { s.ToolHost.IP = "maul-tools.example" }},
		{"tool host name not a label", nil, func(s *Spec) { s.ToolHost.Name = "Maul_Tools" }},
		{"tool host on the check reviewer", nil, func(s *Spec) {
			*s = checkReviewSpec()
			s.ToolHost = &HostAlias{Name: ToolsHostName, IP: toolPodIP}
		}},
		{"tool host without a role", nil, func(s *Spec) { s.Role = "" }},
		{"verification label without a role", nil, func(s *Spec) { s.Role, s.ToolHost = "", nil }},
		{"unknown role", nil, func(s *Spec) { s.Role = "tools" }},
		{"role on a finding Job", nil, func(s *Spec) { s.Kind = "investigation" }},
		{"verification UID not a label value", nil, func(s *Spec) { s.Evaluation = "not a uid/" }},
		{"role with an artifact", nil, func(s *Spec) { s.ArtifactURL = testSpec().ArtifactURL }},
		{"role with a repository image", nil, func(s *Spec) { s.RunnerImage = injectedSpec().RunnerImage }},
		{"role with trees", nil, func(s *Spec) { s.Trees = []Tree{{Key: "web"}} }},
		{"role with a network profile", nil, func(s *Spec) { s.NetworkProfile = goProfile() }},
		{"evaluator without the controller's resolver", func(c *Config) { c.SandboxProbeDNS = "" }, nil},
		{"controller's resolver not ip:53", func(c *Config) { c.SandboxProbeDNS = "kube-dns:53" }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, spec := roleConfig(), evaluatorSpec()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			if tt.mutate != nil {
				tt.mutate(&spec)
			}
			cs := fake.NewClientset()
			_, _, err := New(cs, cfg, nil).Create(context.Background(), spec)
			if !errors.Is(err, ErrRoleRefused) {
				t.Fatalf("Create = %v, want ErrRoleRefused", err)
			}
			if s, _ := cs.CoreV1().Secrets("maul-agents").List(context.Background(), metav1.ListOptions{}); len(s.Items) > 0 {
				t.Errorf("a refused Job left %d Secrets", len(s.Items))
			}
		})
	}
}

// TestNoRoleLeavesTheJobAlone: a Job without a role carries no role label,
// no MAUL_TOOLS_URL and no dns probe target, and the controller's resolver
// being known changes nothing: every existing golden still holds.
func TestNoRoleLeavesTheJobAlone(t *testing.T) {
	cfg := brokeredConfig()
	want := marshal(t, buildJobForTest(t, cfg, testSpec()))
	cfg.SandboxProbeDNS = clusterDNS
	if got := marshal(t, buildJobForTest(t, cfg, testSpec())); got != want {
		t.Errorf("SandboxProbeDNS changed a Job without a role:\n--- want\n%s\n--- got\n%s", want, got)
	}
	if strings.Contains(want, v1alpha1.LabelRole) || strings.Contains(want, ToolsURLEnv) ||
		strings.Contains(want, sandboxprobe.DNSEnv) {
		t.Errorf("a Job without a role carries role material:\n%s", want)
	}
}
