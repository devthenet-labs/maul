// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/controller/intent"
	"github.com/devthenet-labs/maul/internal/kube"
)

// TestSlice1CensusRefusesStartup: intent-controller does not start while
// its census finds a slice-1 record (an open Intent of a verified Project
// recording a criteria digest, a PreviewVerification with no outcomes), and
// the refusal names each one and the migration section; it starts with
// none; and a census that cannot list refuses rather than start blind.
func TestSlice1CensusRefusesStartup(t *testing.T) {
	const ns = "maul"
	set := intent.Settings{Namespace: ns, Verification: true, Previews: true, Evaluator: true}
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "beatbattle", Namespace: ns},
		Spec: v1alpha1.ProjectSpec{Repositories: []v1alpha1.ProjectRepository{{Name: "app"}},
			Preview:      &v1alpha1.ProjectPreview{ImageRepository: "registry.example/maul/previews/app"},
			Verification: &v1alpha1.ProjectVerification{Enabled: true}}}
	open := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "beatbattle-48", Namespace: ns},
		Spec: v1alpha1.IntentSpec{Project: "beatbattle"}}
	open.Status.Phase = v1alpha1.IntentInReview
	open.Status.Acceptance = &v1alpha1.IntentAcceptance{PlanRevision: 1, Digest: "sha256:" + strings.Repeat("a", 64)}
	pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: "beatbattle-48-v1-a1", Namespace: ns}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reader := func(funcs interceptor.Funcs, objs ...client.Object) client.Reader {
		return crfake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
			WithInterceptorFuncs(funcs).Build()
	}

	err := slice1Census(context.Background(), log, reader(interceptor.Funcs{}, project, open, pv), set)
	if err == nil {
		t.Fatal("started with slice-1 records left")
	}
	for _, want := range []string{"Intent maul/beatbattle-48", "PreviewVerification maul/beatbattle-48-v1-a1",
		verificationMigration} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if err := slice1Census(context.Background(), log, reader(interceptor.Funcs{}, project), set); err != nil {
		t.Errorf("refused with nothing left: %v", err)
	}
	refused := errors.New("etcd unavailable")
	failing := reader(interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList,
		...client.ListOption) error {
		return refused
	}})
	if err := slice1Census(context.Background(), log, failing, set); !errors.Is(err, refused) {
		t.Errorf("a census that cannot list = %v, want the refusal", err)
	}
}

// TestCheckEraCensusRefusesStartup: intent-controller does not start while
// its check-era census finds a record only 0.14.18 settles (an open Intent
// whose unmarked verification an earlier release judged, one 0.14.18
// withdrew whose fresh round has not started, an unfinished checkreview
// run), and the refusal names each one and the migration section; a clean
// cluster starts, a fresh install and one 0.14.18 already settled alike;
// and a census that cannot list refuses rather than start blind.
func TestCheckEraCensusRefusesStartup(t *testing.T) {
	const ns = "maul"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reader := func(funcs interceptor.Funcs, objs ...client.Object) client.Reader {
		return crfake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
			WithInterceptorFuncs(funcs).Build()
	}
	intentWith := func(name string, v *v1alpha1.IntentVerification, conds ...metav1.Condition) *v1alpha1.Intent {
		in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: v1alpha1.IntentSpec{Project: "beatbattle"}}
		in.Status.Phase, in.Status.Verification, in.Status.Conditions = v1alpha1.IntentInReview, v, conds
		return in
	}
	superseded := metav1.Condition{Type: v1alpha1.ConditionVerificationSuperseded, Status: metav1.ConditionTrue,
		Reason: "VerificationRulesChanged"}
	run := func(name string, stage v1alpha1.IntentStage, phase v1alpha1.RunPhase) *v1alpha1.IntentRun {
		r := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: v1alpha1.IntentRunSpec{Stage: stage}}
		r.Status.Phase = phase
		return r
	}
	judged := intentWith("beatbattle-48", &v1alpha1.IntentVerification{Round: 2, Passed: 1,
		Outcomes: []v1alpha1.OutcomeResult{{ID: "version-served"}}})
	waiting := intentWith("beatbattle-49", &v1alpha1.IntentVerification{Round: 2}, superseded)
	review := run("beatbattle-48-cr-r1-a1", "checkreview", v1alpha1.RunRunning)

	for _, tc := range []struct {
		name string
		objs []client.Object
		want []string
	}{
		{name: "an unmarked judgement", objs: []client.Object{judged}, want: []string{"Intent maul/beatbattle-48"}},
		{name: "a withdrawal waiting", objs: []client.Object{waiting}, want: []string{"Intent maul/beatbattle-49"}},
		{name: "an unfinished check review", objs: []client.Object{review},
			want: []string{"IntentRun maul/beatbattle-48-cr-r1-a1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkEraCensus(context.Background(), log, reader(interceptor.Funcs{}, tc.objs...), ns)
			if err == nil {
				t.Fatal("started with check-era records left")
			}
			for _, want := range append(tc.want, savedChecksMigration) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
		})
	}

	settled := intentWith("beatbattle-50", &v1alpha1.IntentVerification{Judge: v1alpha1.JudgeObserved, Round: 3,
		Passed: 1}, superseded)
	merged := intentWith("beatbattle-51", &v1alpha1.IntentVerification{Passed: 1})
	merged.Status.Phase = v1alpha1.IntentMerged
	for _, tc := range []struct {
		name string
		objs []client.Object
	}{
		{name: "a fresh install"},
		{name: "settled by 0.14.18", objs: []client.Object{settled, merged,
			run("beatbattle-48-cr-r1-a1", "checkreview", v1alpha1.RunFailed),
			run("beatbattle-50-ev-r3-a1", v1alpha1.IntentStageEvaluate, v1alpha1.RunRunning)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkEraCensus(context.Background(), log, reader(interceptor.Funcs{}, tc.objs...), ns); err != nil {
				t.Errorf("refused a clean cluster: %v", err)
			}
		})
	}

	refused := errors.New("etcd unavailable")
	failing := reader(interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList,
		...client.ListOption) error {
		return refused
	}})
	if err := checkEraCensus(context.Background(), log, failing, ns); !errors.Is(err, refused) {
		t.Errorf("a census that cannot list = %v, want the refusal", err)
	}
}
