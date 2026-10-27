// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"regexp"
	"strings"
	"testing"
	"testing/quick"
)

var fingerprintForm = regexp.MustCompile(`^maul-verify-fingerprint/1:[0-9a-f]{64}$`)

// failing judges g's criterion against an application that answers the
// last step with the given status and body, both tries.
func failing(g genScenario, status int32, body string, latency int32) Verdict {
	srv := &server{c: g.c}
	last := len(Normalize([]Criterion{g.c})[0].Steps) - 1
	do := func(call Call) Response {
		resp := srv.do(call)
		if call.Step == last {
			resp.Status, resp.Body, resp.LatencyMs = status, []byte(body), latency
		}
		return resp
	}
	return Judge(g.c, Observe(g.c, g.rt, do), g.rt)
}

// TestFingerprintIgnoresValues: what an application answers beyond the
// matched bits (bodies, latencies, run tokens, log tails, restart counts)
// never changes the fingerprint, so a repeated failure is recognised.
func TestFingerprintIgnoresValues(t *testing.T) {
	prop := func(g genScenario) bool {
		a := failing(g, 599, `{"error":"boom","at":"12:00:01"}`, 12)
		other := g
		other.rt.Run = "abcdef"
		b := failing(other, 599, `{"error":"bang","at":"12:09:59","trace":"`+strings.Repeat("x", 300)+`"}`, 900)
		envA := []PartFact{{Name: "api", Role: RoleComponent, Waiting: "CrashLoopBackOff", Restarts: 3, LogTail: "panic: a"}}
		envB := []PartFact{{Name: "api", Role: RoleComponent, Waiting: "CrashLoopBackOff", Restarts: 9, LogTail: "panic: b"}}
		fa, fb := Fingerprint([]Verdict{a}, envA), Fingerprint([]Verdict{b}, envB)
		return a.Outcome == OutcomeFail && fa == fb && fingerprintForm.MatchString(fa)
	}
	if err := quick.Check(prop, quickConfig(200)); err != nil {
		t.Error(err)
	}
}

// TestFingerprintSensitive: a different status, a different matched bit or
// a different failing part is a different failure.
func TestFingerprintSensitive(t *testing.T) {
	prop := func(g genScenario) bool {
		a := failing(g, 599, `{}`, 1)
		b := failing(g, 598, `{}`, 1)
		fa, fb := Fingerprint([]Verdict{a}, nil), Fingerprint([]Verdict{b}, nil)
		if fa == fb {
			return false
		}
		// A different failing part, waiting reason or exit code.
		envs := [][]PartFact{
			{{Name: "api", Role: RoleComponent, Waiting: "CrashLoopBackOff"}},
			{{Name: "web", Role: RoleComponent, Waiting: "CrashLoopBackOff"}},
			{{Name: "api", Role: RoleComponent, Waiting: "ImagePullBackOff"}},
			{{Name: "api", Role: RoleComponent, LastTermination: &Termination{Reason: "Error", ExitCode: 1}}},
			{{Name: "api", Role: RoleComponent, LastTermination: &Termination{Reason: "Error", ExitCode: 2}}},
			{{Name: "api", Role: RoleComponent, LastTermination: &Termination{Reason: "OOMKilled", ExitCode: 137}}},
		}
		seen := map[string]bool{fa: true}
		for _, env := range envs {
			f := Fingerprint([]Verdict{a}, env)
			if seen[f] {
				return false
			}
			seen[f] = true
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(100)); err != nil {
		t.Error(err)
	}
}

func TestFingerprintMatchedBits(t *testing.T) {
	verdict := func(bits ...bool) Verdict {
		return Verdict{ID: "a", Outcome: OutcomeFail, Steps: []StepVerdict{{Index: 0, Status: 200, Matched: bits}}}
	}
	base, flipped := verdict(true, true, false), verdict(true, false, true)
	if Fingerprint([]Verdict{base}, nil) == Fingerprint([]Verdict{flipped}, nil) {
		t.Error("matched bits do not reach the fingerprint")
	}
	transport := base
	transport.Steps = []StepVerdict{{Index: 0, Transport: TransportTimeout, Matched: []bool{true, true, false}}}
	if Fingerprint([]Verdict{base}, nil) == Fingerprint([]Verdict{transport}, nil) {
		t.Error("the transport class does not reach the fingerprint")
	}
}

func TestFingerprintSetSemantics(t *testing.T) {
	a := Verdict{ID: "a", Outcome: OutcomeFail, Steps: []StepVerdict{{Status: 500, Matched: []bool{false}}}}
	b := Verdict{ID: "b", Outcome: OutcomeFail, Steps: []StepVerdict{{Status: 404, Matched: []bool{false}}}}
	pass := Verdict{ID: "c", Outcome: OutcomePass, Steps: []StepVerdict{{Status: 200, Matched: []bool{true}}}}
	quarantined := Verdict{ID: "d", Outcome: OutcomeQuarantined,
		Steps: []StepVerdict{{Status: 200, Matched: []bool{true}}}}
	ab := Fingerprint([]Verdict{a, b}, nil)
	if ab != Fingerprint([]Verdict{b, a}, nil) {
		t.Error("fingerprint depends on verdict order")
	}
	if ab != Fingerprint([]Verdict{pass, b, quarantined, a}, nil) {
		t.Error("passing or quarantined criteria reach the fingerprint")
	}
	if ab == Fingerprint([]Verdict{a}, nil) {
		t.Error("a criterion that stopped failing does not change the fingerprint")
	}
	healthy := []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Restarts: 4}}
	if ab != Fingerprint([]Verdict{a, b}, healthy) {
		t.Error("a healthy part reaches the fingerprint")
	}
	env := []PartFact{
		{Name: "api", Role: RoleComponent, Waiting: "CrashLoopBackOff"},
		{Name: "web", Role: RoleComponent, Evicted: true},
	}
	rev := []PartFact{env[1], env[0]}
	if Fingerprint(nil, env) != Fingerprint(nil, rev) {
		t.Error("fingerprint depends on part order")
	}
	if !fingerprintForm.MatchString(Fingerprint(nil, nil)) {
		t.Error("empty fingerprint form")
	}
}
