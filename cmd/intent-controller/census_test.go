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
