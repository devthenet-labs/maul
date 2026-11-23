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

// TestOutcomeFingerprintGoldens pins fingerprint/2 to its documented
// canonical bytes, sha256 taken outside Go: {"o":[{"id":…,"d":…},…]},
// sorted by id, duplicates dropped.
func TestOutcomeFingerprintGoldens(t *testing.T) {
	elo := OutcomeRef{ID: "elo", Digest: "sha256:aa", Manual: true}
	rank := OutcomeRef{ID: "rank", Digest: "sha256:bb", Keep: true}
	for _, tt := range []struct {
		name string
		got  string
		want string
	}{
		{"two outcomes", OutcomeFingerprint([]OutcomeRef{rank, elo, rank}),
			"maul-evaluation-fingerprint/2:b8a4f4e6bbfd94c05ed122d523a98542fde8fccc8da91209ad70dc7cf55c0bc1"},
		{"one outcome", OutcomeFingerprint([]OutcomeRef{elo}),
			"maul-evaluation-fingerprint/2:da5ba5a2a329079309a6378b26abaa38247254cf7b7f710e5fb83c6d4be3372b"},
		{"none", OutcomeFingerprint(nil),
			"maul-evaluation-fingerprint/2:af4f11051ab9c47a5ff25fad91f666380c23ddf997fa8aa71e30a14ee6458d01"},
		{"piece", OutcomeFingerprintPiece(elo),
			"o2:elo maul-evaluation-fingerprint/2:da5ba5a2a329079309a6378b26abaa38247254cf7b7f710e5fb83c6d4be3372b"},
	} {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// TestOutcomeFingerprint: fingerprint/2 names the failing outcomes by id and
// digest in any order, and is never equal to a fingerprint/1 of the same
// failure, nor a v2 piece to a v1 one.
func TestOutcomeFingerprint(t *testing.T) {
	a, b := OutcomeRef{ID: "elo", Digest: "sha256:aa"}, OutcomeRef{ID: "rank", Digest: "sha256:bb"}
	base := OutcomeFingerprint([]OutcomeRef{a, b})
	if !strings.HasPrefix(base, OutcomeFingerprintVersion+":") ||
		OutcomeFingerprintVersion == EvaluationFingerprintVersion {
		t.Fatalf("fingerprint %q is not apart from %s", base, EvaluationFingerprintVersion)
	}
	for _, tt := range []struct {
		name string
		refs []OutcomeRef
	}{
		{"order", []OutcomeRef{b, a}},
		{"duplicates", []OutcomeRef{a, b, a}},
		{"keep and manual flags", []OutcomeRef{{ID: "elo", Digest: "sha256:aa", Keep: true, Manual: true}, b}},
	} {
		if got := OutcomeFingerprint(tt.refs); got != base {
			t.Errorf("%s changed the fingerprint", tt.name)
		}
	}
	for _, tt := range []struct {
		name string
		refs []OutcomeRef
	}{
		{"another outcome set", []OutcomeRef{a}},
		{"a changed digest", []OutcomeRef{a, {ID: "rank", Digest: "sha256:cc"}}},
		{"digests swapped between outcomes",
			[]OutcomeRef{{ID: "elo", Digest: "sha256:bb"}, {ID: "rank", Digest: "sha256:aa"}}},
		{"an id running into its digest", []OutcomeRef{{ID: "elo sha256:aa", Digest: ""}, b}},
	} {
		if got := OutcomeFingerprint(tt.refs); got == base {
			t.Errorf("%s left the fingerprint unchanged", tt.name)
		}
	}
	for _, refs := range [][]OutcomeRef{nil, {a}, {a, b}} {
		ids, digests := []string{}, []string{}
		for _, r := range refs {
			ids, digests = append(ids, r.ID), append(digests, r.Digest)
		}
		for _, v1 := range []string{EvaluationFingerprint(ids, digests, nil), EvaluationFingerprint(ids, nil, nil)} {
			if OutcomeFingerprint(refs) == v1 {
				t.Errorf("v2 equals v1 for %v", refs)
			}
		}
	}
	v1Piece := "o:" + a.ID + " " + EvaluationFingerprint([]string{a.ID}, []string{a.Digest}, nil)
	if p := OutcomeFingerprintPiece(a); p == v1Piece || !strings.HasPrefix(p, OutcomePiecePrefix+a.ID+" ") {
		t.Errorf("piece %q is not a v2 piece apart from v1's %q", p, v1Piece)
	}
}
