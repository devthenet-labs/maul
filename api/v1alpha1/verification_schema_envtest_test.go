// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The acceptance-verification schema tests: the criteria mirror stored on
// Intents and PreviewVerifications, the Preview environment's set-once
// rules, the verify trigger's exactly-once record, the Project's
// verification settings and the PreviewVerification kind. They run inside
// TestSchemaValidation's single envtest boot.

func rawJSON(s string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(s)} }

func rawJSONPtr(s string) *apiextensionsv1.JSON {
	j := rawJSON(s)
	return &j
}

// schemaCriteria is one criterion of each kind, every field set, with the
// matcher shapes stored as raw JSON: what an approved plan's status copy
// holds.
func schemaCriteria() []maulv1.AcceptanceCriterion {
	exit := int32(0)
	return []maulv1.AcceptanceCriterion{
		{
			ID: "battle-leaderboard", Kind: maulv1.AcceptanceHTTP,
			Statement: "a battle's leaderboard ranks its patterns by votes", Target: "api",
			Steps: []maulv1.AcceptanceStep{
				{
					Name: "battle",
					Request: maulv1.AcceptanceRequest{Method: "POST", Path: "/api/battles",
						Headers: map[string]string{"X-Trace": "maul"}, JSON: rawJSONPtr(`{"name":"maul-{{run}}"}`)},
					Expect:  maulv1.AcceptanceExpect{Status: rawJSONPtr(`201`)},
					Capture: map[string]string{"battle": "/id"},
				},
				{
					Request: maulv1.AcceptanceRequest{Method: "GET", Path: "/api/battles/{{capture.battle}}/leaderboard"},
					Expect: maulv1.AcceptanceExpect{
						Status: rawJSONPtr(`[200,203]`),
						JSON: map[string]apiextensionsv1.JSON{
							"": rawJSON(`{"$by":"/id","$order":["a","b"]}`), "/n": rawJSON(`2`),
						},
						Headers:      map[string]apiextensionsv1.JSON{"Content-Type": rawJSON(`{"$contains":"json"}`)},
						BodyContains: "leaderboard",
					},
				},
			},
		},
		{
			ID: "kick-onsets", Kind: maulv1.AcceptanceCommand,
			Statement: "a four-on-the-floor kick renders onsets at 0/500/1000/1500 ms",
			In:        &maulv1.AcceptanceWhere{Component: "render"},
			Argv:      []string{"/render", "onsets", "--bpm", "120"},
			Expect: &maulv1.AcceptanceExpect{Exit: &exit, StdoutContains: "kick",
				StdoutJSON: map[string]apiextensionsv1.JSON{"/kick": rawJSON(`{"$approx":[0,500,1000,1500],"$tolerance":5}`)}},
			Timeout: "2m",
		},
		{
			ID: "sounds-good", Kind: maulv1.AcceptanceManual, Statement: "it sounds good",
			Reason: "only a human can judge how it sounds",
		},
	}
}

// schemaHeads is a one-component combined change.
func schemaHeads() []maulv1.VerificationHead {
	return []maulv1.VerificationHead{{Component: "api", Repository: "https://github.com/acme/api", Revision: schemaSHA}}
}

func testVerificationSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	testProjectVerificationSchema(ctx, t, c)
	testPreviewEnvironmentSchema(ctx, t, c)
	testVerifyTriggerSchema(ctx, t, c)
	testPreviewVerificationSchema(ctx, t, c)
	testEvaluatorSchema(ctx, t, c)
}

// testProjectVerificationSchema: the verification settings are optional and
// never defaulted into a stored Project, their limits are bounded, and maul's
// own status cannot be named as a check to fix.
func testProjectVerificationSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	t.Run("verification settings are not defaulted", func(t *testing.T) {
		p := schemaProject("proj-verify-unset")
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatal(err)
		}
		l := got.Spec.Limits
		if got.Spec.Verification != nil || l.MaxVerifyFixes != nil || l.MaxVerifications != nil {
			t.Errorf("verification %+v, limits %+v: want nothing defaulted", got.Spec.Verification, l)
		}
		if l.MaxVerifyFixesLimit() != maulv1.DefaultMaxVerifyFixes ||
			l.MaxVerificationsLimit() != maulv1.DefaultMaxVerifications ||
			got.Spec.VerificationEnabled() || got.Spec.VerificationTimeout() != maulv1.DefaultVerificationTimeout {
			t.Error("unset verification settings do not read as their defaults")
		}
	})
	t.Run("verification settings round-trip", func(t *testing.T) {
		p := schemaProject("proj-verify-set")
		fixes, verifications := int32(0), int32(100)
		p.Spec.Limits.MaxVerifyFixes, p.Spec.Limits.MaxVerifications = &fixes, &verifications
		p.Spec.Verification = &maulv1.ProjectVerification{Enabled: true, ManualGatesStatus: true, PostLogs: true,
			Timeout: &metav1.Duration{Duration: time.Hour}}
		want := p.Spec.DeepCopy()
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("Create(project) = %v", err)
		}
		got := &maulv1.Project{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "verification", got.Spec.Verification, want.Verification)
		if got.Spec.Limits.MaxVerifyFixesLimit() != 0 || got.Spec.Limits.MaxVerificationsLimit() != 100 {
			t.Errorf("limits = %+v, want maxVerifyFixes 0 and maxVerifications 100 kept", got.Spec.Limits)
		}
	})
	n := func(v int32) *int32 { return &v }
	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.ProjectSpec)
		wantErr bool
	}{
		{"maxVerifyFixes past its bound", func(s *maulv1.ProjectSpec) { s.Limits.MaxVerifyFixes = n(11) }, true},
		{"a negative maxVerifyFixes", func(s *maulv1.ProjectSpec) { s.Limits.MaxVerifyFixes = n(-1) }, true},
		{"maxVerifyFixes at its bound", func(s *maulv1.ProjectSpec) { s.Limits.MaxVerifyFixes = n(10) }, false},
		{"zero maxVerifications", func(s *maulv1.ProjectSpec) { s.Limits.MaxVerifications = n(0) }, true},
		{"maxVerifications past its bound", func(s *maulv1.ProjectSpec) { s.Limits.MaxVerifications = n(101) }, true},
		{"a verification timeout under a minute", func(s *maulv1.ProjectSpec) {
			s.Verification = &maulv1.ProjectVerification{Timeout: &metav1.Duration{Duration: 59 * time.Second}}
		}, true},
		{"a verification timeout over an hour", func(s *maulv1.ProjectSpec) {
			s.Verification = &maulv1.ProjectVerification{Timeout: &metav1.Duration{Duration: 61 * time.Minute}}
		}, true},
		// maul never fixes its own status as if it were CI's.
		{"maul/acceptance as a check to fix", func(s *maulv1.ProjectSpec) {
			s.Checks.Fix = []string{"test", "maul/acceptance"}
		}, true},
		{"maul/acceptance in another case", func(s *maulv1.ProjectSpec) {
			s.Checks.Fix = []string{"Maul/Acceptance"}
		}, true},
		{"another maul-looking check", func(s *maulv1.ProjectSpec) { s.Checks.Fix = []string{"maul/acceptance-e2e"} }, false},
		// The tester's part name is reserved while verification is on.
		{"a repository keyed tester under verification", func(s *maulv1.ProjectSpec) {
			s.Repositories[0].Name = "tester"
			s.Verification = &maulv1.ProjectVerification{Enabled: true}
		}, true},
		{"a repository keyed tester without verification", func(s *maulv1.ProjectSpec) {
			s.Repositories[0].Name = "tester"
			s.Verification = &maulv1.ProjectVerification{}
		}, false},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			p := schemaProject("pv-" + shortHash(tt.name))
			tt.mutate(&p.Spec)
			err := c.Create(ctx, p)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(project with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}

// shortHash names one test object per case inside the 25-character Project
// name budget.
func shortHash(s string) string {
	var h uint32 = 2166136261
	for i := range len(s) {
		h ^= uint32(s[i])
		h *= 16777619
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 8)
	for i := range out {
		out[i] = hex[h>>(28-4*i)&0xf]
	}
	return string(out)
}

// testPreviewEnvironmentSchema: an environment is set at creation only and
// never changes, nothing connects to the tester, and resetGeneration only
// grows.
func testPreviewEnvironmentSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	preview := func(name string, env *maulv1.PreviewEnvironment) *maulv1.Preview {
		return &maulv1.Preview{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: maulv1.PreviewSpec{
				IntentRef: maulv1.ObjectReference{Name: name, UID: types.UID("uid-1")},
				HostLabel: name,
				Components: []maulv1.PreviewComponent{{
					Name: "api", ImageRepository: "registry.example/maul/previews/api",
					Revision: schemaSHA, Port: 8080, ReadinessPath: "/health",
				}},
				TTL:         metav1.Duration{Duration: 24 * time.Hour},
				Environment: env,
			}}
	}
	env := func(edges ...maulv1.PreviewEdge) *maulv1.PreviewEnvironment {
		return &maulv1.PreviewEnvironment{Verification: true, Edges: edges}
	}
	toAPI := maulv1.PreviewEdge{From: maulv1.PreviewPartTester, To: "api", Port: 8080}

	for _, tt := range []struct {
		name    string
		env     *maulv1.PreviewEnvironment
		wantErr bool
	}{
		{"the tester's edge to a component", env(toAPI), false},
		{"no edges: a verification-capable Preview has a component", env(), true},
		{"an edge to the tester", env(maulv1.PreviewEdge{From: "api", To: maulv1.PreviewPartTester, Port: 8080}), true},
		{"an edge to itself", env(maulv1.PreviewEdge{From: "api", To: "api", Port: 8080}), true},
		{"a repeated edge", env(toAPI, toAPI), true},
		{"an edge on port 0", env(maulv1.PreviewEdge{From: maulv1.PreviewPartTester, To: "api"}), true},
		{"an edge to a malformed part", env(maulv1.PreviewEdge{From: maulv1.PreviewPartTester, To: "API", Port: 1}), true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			err := c.Create(ctx, preview("penv-"+shortHash(tt.name), tt.env))
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(preview with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}

	withEnv := preview("penv-with", env(toAPI))
	if err := c.Create(ctx, withEnv); err != nil {
		t.Fatalf("Create(preview with environment) = %v", err)
	}
	without := preview("penv-without", nil)
	if err := c.Create(ctx, without); err != nil {
		t.Fatalf("Create(preview without environment) = %v", err)
	}
	got := &maulv1.Preview{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(withEnv), got); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, "environment", got.Spec.Environment, env(toAPI))

	for _, tt := range []struct {
		name    string
		target  *maulv1.Preview
		mutate  func(*maulv1.PreviewSpec)
		wantErr bool
	}{
		{"an environment added", without, func(s *maulv1.PreviewSpec) { s.Environment = env(toAPI) }, true},
		{"the environment removed", withEnv, func(s *maulv1.PreviewSpec) { s.Environment = nil }, true},
		{"an edge changed", withEnv, func(s *maulv1.PreviewSpec) { s.Environment.Edges[0].Port = 9090 }, true},
		{"verification turned off", withEnv, func(s *maulv1.PreviewSpec) { s.Environment.Verification = false }, true},
		{"a PR head moved", withEnv, func(s *maulv1.PreviewSpec) {
			s.Components[0].Revision = strings.Repeat("b", 40)
		}, false},
		{"a reset", withEnv, func(s *maulv1.PreviewSpec) { s.ResetGeneration = 2 }, false},
		{"a reset undone", withEnv, func(s *maulv1.PreviewSpec) { s.ResetGeneration = 1 }, true},
		{"a reset cleared", withEnv, func(s *maulv1.PreviewSpec) { s.ResetGeneration = 0 }, true},
		{"a first reset without an environment", without, func(s *maulv1.PreviewSpec) { s.ResetGeneration = 1 }, false},
		{"a negative reset generation", without, func(s *maulv1.PreviewSpec) { s.ResetGeneration = -1 }, true},
	} {
		t.Run("update with "+tt.name, func(t *testing.T) {
			p := &maulv1.Preview{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(tt.target), p); err != nil {
				t.Fatal(err)
			}
			tt.mutate(&p.Spec)
			err := c.Update(ctx, p)
			if (err != nil) != tt.wantErr {
				t.Errorf("Update(preview with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}

	t.Run("observedResetGeneration round-trips", func(t *testing.T) {
		p := &maulv1.Preview{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(withEnv), p); err != nil {
			t.Fatal(err)
		}
		p.Status.Phase, p.Status.ObservedResetGeneration = maulv1.PreviewReady, 2
		if err := c.Status().Update(ctx, p); err != nil {
			t.Fatalf("Status().Update = %v", err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(withEnv), p); err != nil {
			t.Fatal(err)
		}
		if p.Status.ObservedResetGeneration != 2 {
			t.Errorf("observedResetGeneration = %d, want 2", p.Status.ObservedResetGeneration)
		}
	})
}

// testVerifyTriggerSchema: a verify round records its PreviewVerification by
// UID, and only a verify round records one; reviews are consumed by review
// and command rounds only.
func testVerifyTriggerSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	verifyRef := &maulv1.ObjectReference{Name: "target-1-v1-a1", UID: "u-pv"}
	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.IntentRunSpec)
		wantErr bool
	}{
		{"a verify round", func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.VerificationRef = maulv1.IntentRunTriggerVerify, nil, verifyRef
		}, false},
		{"a verify round without its verification", func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs = maulv1.IntentRunTriggerVerify, nil
		}, true},
		{"a verify round whose verification has no uid", func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs = maulv1.IntentRunTriggerVerify, nil
			s.Inputs.VerificationRef = &maulv1.ObjectReference{Name: "target-1-v1-a1"}
		}, true},
		{"a verify round consuming reviews", func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.VerificationRef = maulv1.IntentRunTriggerVerify, verifyRef
		}, true},
		{"a verify round consuming a command", func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.VerificationRef = maulv1.IntentRunTriggerVerify, nil, verifyRef
			s.Inputs.CommandID = 9001
		}, true},
		{"a verify round consuming check runs", func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.VerificationRef = maulv1.IntentRunTriggerVerify, nil, verifyRef
			s.Inputs.CheckRunIDs = []int64{1}
		}, true},
		{"a review round recording a verification", func(s *maulv1.IntentRunSpec) {
			s.Inputs.VerificationRef = verifyRef
		}, true},
		{"a command round recording a verification", func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.CommandID = maulv1.IntentRunTriggerCommand, nil, 9001
			s.Inputs.VerificationRef = verifyRef
		}, true},
		{"a checks round recording a verification", func(s *maulv1.IntentRunSpec) {
			s.Trigger, s.Inputs.ReviewIDs, s.Inputs.CheckRunIDs = maulv1.IntentRunTriggerChecks, nil, []int64{1}
			s.Inputs.VerificationRef = verifyRef
		}, true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			r := schemaIntentRun("target-1-vt-"+shortHash(tt.name), maulv1.IntentStageRevise)
			tt.mutate(&r.Spec)
			err := c.Create(ctx, r)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(intent run with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
	t.Run("a build run cannot record a verification", func(t *testing.T) {
		r := schemaIntentRun("target-1-vt-build", maulv1.IntentStageBuild)
		r.Spec.Inputs.VerificationRef = verifyRef
		if err := c.Create(ctx, r); err == nil {
			t.Error("Create(build run with a verificationRef) = nil, want rejection")
		}
	})
	t.Run("a verify round's record is immutable", func(t *testing.T) {
		r := schemaIntentRun("target-1-vt-immutable", maulv1.IntentStageRevise)
		r.Spec.Trigger, r.Spec.Inputs.ReviewIDs, r.Spec.Inputs.VerificationRef =
			maulv1.IntentRunTriggerVerify, nil, verifyRef.DeepCopy()
		if err := c.Create(ctx, r); err != nil {
			t.Fatalf("Create(verify run) = %v", err)
		}
		r.Spec.Inputs.VerificationRef.UID = "u-other"
		if err := c.Update(ctx, r); err == nil {
			t.Error("Update(verify run's verificationRef) = nil, want immutability rejection")
		}
	})
}

// schemaVerification is a valid PreviewVerification of Intent target-1.
func schemaVerification(name string) *maulv1.PreviewVerification {
	return &maulv1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: maulv1.PreviewVerificationSpec{
			IntentRef:         maulv1.ObjectReference{Name: "target-1", UID: "u-intent"},
			PreviewRef:        maulv1.ObjectReference{Name: "target-1", UID: "u-preview"},
			PreviewGeneration: 3,
			HeadSetDigest:     schemaDigest,
			Heads:             schemaHeads(),
			Round:             1,
			Attempt:           1,
			Run:               "0a1b2c",
			Criteria:          schemaCriteria()[:1],
			CriteriaDigest:    schemaDigest,
			Timeout:           metav1.Duration{Duration: 10 * time.Minute},
		},
	}
}

// fullVerificationStatus populates every PreviewVerification status field.
func fullVerificationStatus() maulv1.PreviewVerificationStatus {
	return maulv1.PreviewVerificationStatus{
		Phase: maulv1.VerificationObserved, Reason: maulv1.VerificationReasonOK,
		StartedAt: schemaNow.DeepCopy(), CompletedAt: schemaNow.DeepCopy(),
		TesterImageID: "registry.example/maul/preview-tester@sha256:" + strings.Repeat("1", 64),
		Observations: []maulv1.CriterionObservation{{
			ID: "battle-leaderboard",
			Tries: []maulv1.TryObservation{
				{Run: "v1a1t1-0a1b2c", Steps: []maulv1.StepObservation{{
					Method: "POST", Path: "/api/battles", Status: 201, TransportError: maulv1.TransportNone,
					JSONValid: true, BodyBytes: 42, LatencyMs: 7,
					Pointers:   map[string]string{"/id": `"b-1"`},
					Captures:   map[string]string{"battle": "b-1"},
					Headers:    map[string]string{"Content-Type": "application/json"},
					BodyPrefix: "eyJpZCI6ImItMSJ9",
				}}},
				{Run: "v1a1t2-0a1b2c", Steps: []maulv1.StepObservation{{
					Method: "POST", Path: "/api/battles", TransportError: maulv1.TransportRefused,
				}}},
			},
		}, {ID: "battle-history", TooLarge: true}},
		Environment: []maulv1.PartFact{
			{Name: "api", Role: maulv1.PreviewRoleComponent, Ready: false, Restarts: 3, Waiting: "CrashLoopBackOff",
				LastTermination: &maulv1.Termination{Reason: "OOMKilled", ExitCode: 137}, Evicted: true,
				Unschedulable: true, LogTail: "panic: out of memory\n"},
			{Name: maulv1.PreviewPartTester, Role: maulv1.PreviewRoleTester, Ready: true},
		},
		SummarySHA256: strings.Repeat("e", 64),
	}
}

// testPreviewVerificationSchema: the kind's spec is immutable and pins its
// Intent and Preview by UID; its status is a bounded observation in a fixed
// vocabulary.
func testPreviewVerificationSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	t.Run("a verification with every criterion kind round-trips", func(t *testing.T) {
		v := schemaVerification("target-1-v1-a1")
		v.Spec.Criteria = schemaCriteria()
		want := v.Spec.DeepCopy()
		if err := c.Create(ctx, v); err != nil {
			t.Fatalf("Create(verification) = %v", err)
		}
		got := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(v), got); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "verification spec", got.Spec, *want)

		got.Status = fullVerificationStatus()
		if err := c.Status().Update(ctx, got); err != nil {
			t.Fatalf("Status().Update(full status) = %v", err)
		}
		read := &maulv1.PreviewVerification{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(v), read); err != nil {
			t.Fatal(err)
		}
		sameJSON(t, "verification status", read.Status, fullVerificationStatus())
	})

	for _, tt := range []struct {
		name   string
		mutate func(*maulv1.PreviewVerificationSpec)
	}{
		{"heads", func(s *maulv1.PreviewVerificationSpec) { s.Heads[0].Revision = strings.Repeat("b", 40) }},
		{"criteria", func(s *maulv1.PreviewVerificationSpec) { s.Criteria[0].Statement = "something else" }},
		{"run", func(s *maulv1.PreviewVerificationSpec) { s.Run = "ffffff" }},
		{"timeout", func(s *maulv1.PreviewVerificationSpec) { s.Timeout.Duration = 20 * time.Minute }},
	} {
		t.Run("spec "+tt.name+" is immutable", func(t *testing.T) {
			v := &maulv1.PreviewVerification{}
			if err := c.Get(ctx, client.ObjectKey{Name: "target-1-v1-a1", Namespace: "default"}, v); err != nil {
				t.Fatal(err)
			}
			tt.mutate(&v.Spec)
			if err := c.Update(ctx, v); err == nil {
				t.Errorf("Update(verification spec %s) = nil, want immutability rejection", tt.name)
			}
		})
	}

	for _, tt := range []struct {
		name    string
		mutate  func(*maulv1.PreviewVerification)
		wantErr bool
	}{
		{"an intent ref without its uid", func(v *maulv1.PreviewVerification) { v.Spec.IntentRef.UID = "" }, true},
		{"a preview ref without its uid", func(v *maulv1.PreviewVerification) { v.Spec.PreviewRef.UID = "" }, true},
		{"a fourth attempt", func(v *maulv1.PreviewVerification) { v.Spec.Attempt = 4 }, true},
		{"the third attempt", func(v *maulv1.PreviewVerification) { v.Spec.Attempt = 3 }, false},
		{"round 0", func(v *maulv1.PreviewVerification) { v.Spec.Round = 0 }, true},
		{"a malformed run token", func(v *maulv1.PreviewVerification) { v.Spec.Run = "0A1B2C" }, true},
		{"no criteria", func(v *maulv1.PreviewVerification) { v.Spec.Criteria = nil }, true},
		{"seventeen criteria", func(v *maulv1.PreviewVerification) {
			for len(v.Spec.Criteria) < 17 {
				v.Spec.Criteria = append(v.Spec.Criteria, v.Spec.Criteria[0])
			}
		}, true},
		{"a short head", func(v *maulv1.PreviewVerification) { v.Spec.Heads[0].Revision = "abc" }, true},
		{"no heads", func(v *maulv1.PreviewVerification) { v.Spec.Heads = nil }, true},
		{"a bare head-set digest", func(v *maulv1.PreviewVerification) {
			v.Spec.HeadSetDigest = strings.Repeat("d", 64)
		}, true},
		{"a timeout over an hour", func(v *maulv1.PreviewVerification) { v.Spec.Timeout.Duration = 61 * time.Minute }, true},
		{"a 64-character name", func(v *maulv1.PreviewVerification) { v.Name = strings.Repeat("v", 64) }, true},
		{"a 63-character name", func(v *maulv1.PreviewVerification) { v.Name = strings.Repeat("w", 63) }, false},
		// The criteria grammar's backstop: the schema refuses what no
		// approved plan could hold.
		{"a reserved criterion kind", func(v *maulv1.PreviewVerification) { v.Spec.Criteria[0].Kind = "browser" }, true},
		{"a malformed criterion id", func(v *maulv1.PreviewVerification) { v.Spec.Criteria[0].ID = "Leaderboard" }, true},
		{"a two-line statement", func(v *maulv1.PreviewVerification) { v.Spec.Criteria[0].Statement = "a\nb" }, true},
		{"an http criterion without steps", func(v *maulv1.PreviewVerification) { v.Spec.Criteria[0].Steps = nil }, true},
		{"nine steps", func(v *maulv1.PreviewVerification) {
			for len(v.Spec.Criteria[0].Steps) < 9 {
				v.Spec.Criteria[0].Steps = append(v.Spec.Criteria[0].Steps, v.Spec.Criteria[0].Steps[0])
			}
		}, true},
		{"an unknown method", func(v *maulv1.PreviewVerification) {
			v.Spec.Criteria[0].Steps[0].Request.Method = "TRACE"
		}, true},
		{"a path with two leading slashes", func(v *maulv1.PreviewVerification) {
			v.Spec.Criteria[0].Steps[0].Request.Path = "//evil.example/x"
		}, true},
		{"a relative path", func(v *maulv1.PreviewVerification) { v.Spec.Criteria[0].Steps[0].Request.Path = "api" }, true},
		{"a malformed capture name", func(v *maulv1.PreviewVerification) {
			v.Spec.Criteria[0].Steps[0].Capture = map[string]string{"Battle": "/id"}
		}, true},
		{"a malformed header name", func(v *maulv1.PreviewVerification) {
			v.Spec.Criteria[0].Steps[0].Request.Headers = map[string]string{"X Trace": "maul"}
		}, true},
		{"a command criterion in two places", func(v *maulv1.PreviewVerification) {
			v.Spec.Criteria = schemaCriteria()[1:2]
			v.Spec.Criteria[0].In.Repository = "api"
		}, true},
		{"a command criterion without argv", func(v *maulv1.PreviewVerification) {
			v.Spec.Criteria = schemaCriteria()[1:2]
			v.Spec.Criteria[0].Argv = nil
		}, true},
	} {
		t.Run("create with "+tt.name, func(t *testing.T) {
			v := schemaVerification("target-1-v2-" + shortHash(tt.name))
			tt.mutate(v)
			err := c.Create(ctx, v)
			if (err != nil) != tt.wantErr {
				t.Errorf("Create(verification with %s) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}

	for _, tt := range []struct {
		name   string
		mutate func(*maulv1.PreviewVerificationStatus)
	}{
		{"a free-text reason", func(s *maulv1.PreviewVerificationStatus) { s.Reason = "the app said hi" }},
		{"an unknown phase", func(s *maulv1.PreviewVerificationStatus) { s.Phase = "Passed" }},
		{"an unknown transport error", func(s *maulv1.PreviewVerificationStatus) {
			s.Observations[0].Tries[0].Steps[0].TransportError = "weird"
		}},
		{"three tries", func(s *maulv1.PreviewVerificationStatus) {
			s.Observations[0].Tries = append(s.Observations[0].Tries, s.Observations[0].Tries[0])
		}},
		{"an overlong pointer value", func(s *maulv1.PreviewVerificationStatus) {
			s.Observations[0].Tries[0].Steps[0].Pointers["/id"] = strings.Repeat("x", 257)
		}},
		{"an overlong capture", func(s *maulv1.PreviewVerificationStatus) {
			s.Observations[0].Tries[0].Steps[0].Captures["battle"] = strings.Repeat("x", 129)
		}},
		{"an overlong log tail", func(s *maulv1.PreviewVerificationStatus) {
			s.Environment[0].LogTail = strings.Repeat("x", 4097)
		}},
		{"a waiting reason with markup", func(s *maulv1.PreviewVerificationStatus) {
			s.Environment[0].Waiting = "<b>CrashLoopBackOff</b>"
		}},
		{"an unknown part role", func(s *maulv1.PreviewVerificationStatus) { s.Environment[0].Role = "sidecar" }},
		{"thirteen parts", func(s *maulv1.PreviewVerificationStatus) {
			for len(s.Environment) < 13 {
				s.Environment = append(s.Environment, s.Environment[0])
			}
		}},
		{"a body prefix that is not base64", func(s *maulv1.PreviewVerificationStatus) {
			s.Observations[0].Tries[0].Steps[0].BodyPrefix = "<script>"
		}},
		{"a malformed try token", func(s *maulv1.PreviewVerificationStatus) {
			s.Observations[0].Tries[0].Run = "anything"
		}},
		{"an observation with tries marked too large", func(s *maulv1.PreviewVerificationStatus) {
			s.Observations[0].TooLarge = true
		}},
		{"an observation with neither tries nor the marker", func(s *maulv1.PreviewVerificationStatus) {
			s.Observations[1].TooLarge = false
		}},
	} {
		t.Run("status with "+tt.name, func(t *testing.T) {
			v := &maulv1.PreviewVerification{}
			if err := c.Get(ctx, client.ObjectKey{Name: "target-1-v1-a1", Namespace: "default"}, v); err != nil {
				t.Fatal(err)
			}
			v.Status = fullVerificationStatus()
			tt.mutate(&v.Status)
			if err := c.Status().Update(ctx, v); err == nil {
				t.Errorf("Status().Update(%s) = nil, want rejection", tt.name)
			}
		})
	}
}
