// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// grantSubagents makes s an intent build granted subagents when v is set,
// the one Job a grant may ride on, and leaves it as it is otherwise.
func grantSubagents(s *Spec, v string) {
	if v == "" {
		return
	}
	s.Phase, s.Kind, s.BuildSubagents = "build", "intent", true
}

// subagentsEnv is the agent container's MAUL_BUILD_SUBAGENTS entry of the
// Job spec creates, or nil.
func subagentsEnv(t *testing.T, cfg Config, spec Spec) *corev1.EnvVar {
	t.Helper()
	job := createJob(t, cfg, spec)
	env := job.Spec.Template.Spec.Containers[0].Env
	for i := range env {
		if env[i].Name == "MAUL_BUILD_SUBAGENTS" {
			return &env[i]
		}
	}
	return nil
}

// TestCreateCarriesBuildSubagents: an intent build granted subagents
// carries MAUL_BUILD_SUBAGENTS=true; a Job without the grant has no such
// entry on the default image, and on a repository image an explicit empty
// one, so the image's ENV can never grant it.
func TestCreateCarriesBuildSubagents(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cfg     func() Config
		spec    func() Spec
		granted bool
		want    *string
	}{
		{"granted", testConfig, testSpec, true, new("true")},
		{"not granted", testConfig, testSpec, false, nil},
		{"granted on a repository image", injectedConfig, injectedSpec, true, new("true")},
		{"not granted on a repository image", injectedConfig, injectedSpec, false, new("")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := tt.spec()
			spec.Phase, spec.Kind = "build", "intent"
			spec.BuildSubagents = tt.granted
			got := subagentsEnv(t, tt.cfg(), spec)
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("MAUL_BUILD_SUBAGENTS = %+v, want absent", got)
			case tt.want != nil && (got == nil || got.Value != *tt.want || got.ValueFrom != nil):
				t.Errorf("MAUL_BUILD_SUBAGENTS = %+v, want the literal %q", got, *tt.want)
			}
		})
	}
}

// TestCreateRefusesSubagentsOffTheBuild: the grant rides only on an intent
// build on claude. Anywhere else it is a contradiction in what the
// controller asked for, refused before anything is created, never dropped.
func TestCreateRefusesSubagentsOffTheBuild(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		phase, kind, harness string
	}{
		{"investigate", "investigate", "investigation", "claude"},
		{"remediate", "remediate", "remediation", "claude"},
		{"plan", "plan", "intent", "claude"},
		{"build of another kind", "build", "remediation", "claude"},
		{"build on codex", "build", "intent", "codex"},
		{"build on copilot", "build", "intent", "copilot"},
		{"build on the fake harness", "build", "intent", "fake"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := testSpec()
			spec.Phase, spec.Kind, spec.Harness = tt.phase, tt.kind, tt.harness
			spec.BuildSubagents = true
			cs := fake.NewClientset()
			if _, _, err := New(cs, testConfig(), nil).Create(context.Background(), spec); !errors.Is(err,
				ErrSubagentsRefused) {
				t.Fatalf("Create = %v, want ErrSubagentsRefused", err)
			}
			jobs, err := cs.BatchV1().Jobs("maul-agents").List(context.Background(), metav1.ListOptions{})
			if err != nil || len(jobs.Items) != 0 {
				t.Errorf("Jobs listed %v (%v), want none created", jobs, err)
			}
		})
	}
}
