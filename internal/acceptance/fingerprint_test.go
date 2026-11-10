// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

var evaluationFingerprintForm = regexp.MustCompile(`^maul-evaluation-fingerprint/1:[0-9a-f]{64}$`)

// TestEvaluationFingerprint: the evaluator's fix fingerprint names the
// failing outcomes, the failing check digests and the failing parts, in
// any order, and nothing that changes from run to run.
func TestEvaluationFingerprint(t *testing.T) {
	crash := []httpstep.PartFact{{Name: "api", Role: httpstep.RoleComponent, Waiting: "CrashLoopBackOff", Restarts: 3,
		LogTail: "panic: a"}}
	crashAgain := []httpstep.PartFact{{Name: "api", Role: httpstep.RoleComponent, Waiting: "CrashLoopBackOff", Restarts: 9,
		LogTail: "panic: b"}}
	healthy := []httpstep.PartFact{{Name: "web", Role: httpstep.RoleComponent, Ready: true}}
	base := EvaluationFingerprint([]string{"elo", "rank"}, []string{"sha256:a", "sha256:b"}, crash)
	if !evaluationFingerprintForm.MatchString(base) {
		t.Fatalf("fingerprint %q has the wrong form", base)
	}
	type args struct {
		name     string
		outcomes []string
		checks   []string
		env      []httpstep.PartFact
	}
	same := []args{
		{"order of outcomes and checks", []string{"rank", "elo"}, []string{"sha256:b", "sha256:a"}, crash},
		{"duplicates", []string{"elo", "rank", "elo"}, []string{"sha256:a", "sha256:b", "sha256:a"}, crash},
		{"restart counts and log tails", []string{"elo", "rank"}, []string{"sha256:a", "sha256:b"}, crashAgain},
		{"healthy parts", []string{"elo", "rank"}, []string{"sha256:a", "sha256:b"}, append(slices.Clone(crash), healthy...)},
	}
	for _, tt := range same {
		if got := EvaluationFingerprint(tt.outcomes, tt.checks, tt.env); got != base {
			t.Errorf("%s changed the fingerprint", tt.name)
		}
	}
	differ := []args{
		{"another outcome", []string{"elo"}, []string{"sha256:a", "sha256:b"}, crash},
		{"another check", []string{"elo", "rank"}, []string{"sha256:a"}, crash},
		{"a healthy environment", []string{"elo", "rank"}, []string{"sha256:a", "sha256:b"}, healthy},
		{"an outcome named like a check", []string{"elo", "rank", "sha256:a"}, []string{"sha256:b"}, crash},
	}
	for _, tt := range differ {
		if got := EvaluationFingerprint(tt.outcomes, tt.checks, tt.env); got == base {
			t.Errorf("%s left the fingerprint unchanged", tt.name)
		}
	}
	if !strings.HasPrefix(EvaluationFingerprint(nil, nil, nil), "maul-evaluation-fingerprint/1:") {
		t.Error("the evaluation fingerprint is not apart from slice 1's maul-verify-fingerprint/1")
	}
}

// TestEvaluationFingerprintParts: a different failing part, waiting reason,
// termination reason or exit code is a different failure, in any order of
// the parts (the parts half of slice 1's fingerprint tests, which
// EvaluationFingerprint shares).
func TestEvaluationFingerprintParts(t *testing.T) {
	envs := [][]httpstep.PartFact{
		nil,
		{{Name: "api", Role: httpstep.RoleComponent, Waiting: "CrashLoopBackOff"}},
		{{Name: "web", Role: httpstep.RoleComponent, Waiting: "CrashLoopBackOff"}},
		{{Name: "api", Role: httpstep.RoleComponent, Waiting: "ImagePullBackOff"}},
		{{Name: "api", Role: httpstep.RoleComponent, LastTermination: &httpstep.Termination{Reason: "Error", ExitCode: 1}}},
		{{Name: "api", Role: httpstep.RoleComponent, LastTermination: &httpstep.Termination{Reason: "Error", ExitCode: 2}}},
		{{Name: "api", Role: httpstep.RoleComponent,
			LastTermination: &httpstep.Termination{Reason: "OOMKilled", ExitCode: 137}}},
		{{Name: "web", Role: httpstep.RoleComponent, Evicted: true}},
	}
	seen := map[string]int{}
	for i, env := range envs {
		f := EvaluationFingerprint([]string{"elo"}, []string{"sha256:a"}, env)
		if j, ok := seen[f]; ok {
			t.Errorf("environments %d and %d share a fingerprint", j, i)
		}
		seen[f] = i
	}
	env := []httpstep.PartFact{envs[1][0], envs[7][0]}
	rev := []httpstep.PartFact{env[1], env[0]}
	if EvaluationFingerprint(nil, nil, env) != EvaluationFingerprint(nil, nil, rev) {
		t.Error("the fingerprint depends on part order")
	}
}
