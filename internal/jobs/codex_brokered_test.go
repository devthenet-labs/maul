// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/harness"
	"github.com/bitwise-media-group/patchy/internal/provider"
)

// brokeredCodexConfig is brokeredConfig with the codex runner brokered as
// runnercfg builds it: no Secret, the broker's openai route as its only
// gateway env, plus a credential channel a bug upstream might put there.
func brokeredCodexConfig() Config {
	cfg := brokeredConfig()
	env := provider.CodexEnv("http://" + brokerHost + ":8080")
	env["OPENAI_API_KEY"] = "must-never-pass-through"
	cfg.Runners["codex"] = Runner{
		Image:    "ghcr.io/bitwise-media-group/patchy/codex-agent-runner:1",
		Brokered: true,
		Env:      env,
	}
	return cfg
}

// codexRemediationSpec is a remediation on the codex runner.
func codexRemediationSpec() Spec {
	spec := testSpec()
	spec.Phase, spec.Kind = "remediate", "remediation"
	spec.Harness, spec.Model = "codex", "openai/gpt-5.3-codex"
	spec.Owner = "finding-abc123def0-1-rem-1"
	spec.InvestigationMarkdown = "# Investigation\n"
	spec.MaxTurns, spec.TokenBudget = 120, 600000
	return spec
}

// TestGoldenBrokeredCodexJob pins the brokered codex Job: the claude broker
// posture (the projected caller token in the agent container only, the
// broker route as the gateway env), no Secret of any kind, and no
// placeholder in the pod env (agent-runner sets codex's per stage).
func TestGoldenBrokeredCodexJob(t *testing.T) {
	goldenJob(t, "job_brokered_codex", buildJobForTest(t, brokeredCodexConfig(), codexRemediationSpec()))
}

// TestGoldenBrokeredCodexDNSNoneJob: a brokered codex Job needs no
// resolver, so it runs under DNSNone with the broker, read off
// PATCHY_CODEX_BASE_URL, pinned in its hosts file beside the artifact
// server; clearing the three DNS fields gives back the cluster-DNS Job.
func TestGoldenBrokeredCodexDNSNoneJob(t *testing.T) {
	cfg := brokeredCodexConfig()
	cfg.DNS, cfg.Resolver = DNSNone, clusterResolver()
	spec := codexRemediationSpec()
	job, err := New(fake.NewClientset(), cfg, nil).build(context.Background(),
		NameFor(spec.Finding, spec.Kind, int32(spec.Attempt)), spec)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	goldenJob(t, "job_brokered_codex_dns_none", job)

	pod := &job.Spec.Template.Spec
	var hosts []string
	for _, a := range pod.HostAliases {
		hosts = append(hosts, a.Hostnames...)
	}
	if !slices.Contains(hosts, brokerHost) || !slices.Contains(hosts, artifactHost) {
		t.Errorf("hosts file = %v, want the broker and the artifact server", hosts)
	}
	pod.DNSPolicy, pod.DNSConfig, pod.HostAliases = "", nil, nil
	if got, want := marshal(t, job), marshal(t, buildJobForTest(t, brokeredCodexConfig(), spec)); got != want {
		t.Errorf("DNSNone changed more than the pod's name resolution:\n--- want\n%s\n--- got\n%s", want, got)
	}
}

// TestBrokeredCodexJobShape: a brokered codex pod holds no model credential
// in any form, the caller token is mounted in the agent container alone,
// and the pod needs no resolver.
func TestBrokeredCodexJobShape(t *testing.T) {
	cfg := brokeredCodexConfig()
	if NeedsResolver(cfg.Runners["codex"]) {
		t.Error("a brokered codex runner needs a resolver")
	}
	pod := createJob(t, cfg, codexRemediationSpec()).Spec.Template.Spec
	if brokerVolume(pod) == nil {
		t.Fatal("no projected broker-token volume")
	}
	agent, prepare := pod.Containers[0], pod.InitContainers[0]
	envs := envMap(agent)
	if got := envs["PATCHY_BROKER_TOKEN_FILE"].Value; got != brokerTokenPath {
		t.Errorf("PATCHY_BROKER_TOKEN_FILE = %q", got)
	}
	if got := envs[provider.CodexBaseURLEnv].Value; got != "http://"+brokerHost+":8080/openai/v1" {
		t.Errorf("%s = %q, want the broker's openai route", provider.CodexBaseURLEnv, got)
	}
	for _, c := range []corev1.Container{agent, prepare} {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				t.Errorf("%s env %s references a Secret", c.Name, e.Name)
			}
		}
	}
	for _, k := range harness.NewCodex().EnvKeys() {
		if _, ok := envs[k]; ok {
			t.Errorf("codex credential channel %s is in the pod env", k)
		}
	}
	for _, k := range []string{provider.PlaceholderAuthEnv, provider.CodexTokenEnv, provider.CodexPlaceholderKeyEnv} {
		if _, ok := envs[k]; ok {
			t.Errorf("%s is in the pod env; agent-runner sets codex's per stage", k)
		}
	}
	for _, m := range prepare.VolumeMounts {
		if m.Name == volBrokerToken {
			t.Error("the init container mounts the broker token")
		}
	}
}

// TestCodexEvaluationRefused: no evaluation Job runs on codex until evolve
// can be brokered, whatever the runner fleet holds.
func TestCodexEvaluationRefused(t *testing.T) {
	c := New(fake.NewClientset(), brokeredCodexConfig(), nil)
	_, err := c.CreateEval(context.Background(), EvalSpec{
		Evaluation: "eval-1", Unit: "eval-1-u0", Harness: "codex", UnitJSON: []byte(`{}`),
		ArtifactURL: "http://blobs/x", ArtifactDigest: strings.Repeat("a", 64),
	})
	if !errors.Is(err, ErrCodexEvaluation) {
		t.Fatalf("CreateEval = %v, want ErrCodexEvaluation", err)
	}
}
