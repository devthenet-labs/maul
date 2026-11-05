// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// testCommitRepositoryStatus: a commit Repository's status may only pin that
// commit and never a runner image, so a source-controller that predates
// commit refs (pinning the default branch's head, or an image) is refused by
// the API server; a branch Repository is unaffected.
func testCommitRepositoryStatus(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	commit := strings.Repeat("c", 40)
	head := strings.Repeat("d", 40)
	image := &maulv1.RunnerImage{Image: "ghcr.io/acme/agent@sha256:" + strings.Repeat("ab", 32)}
	for _, tt := range []struct {
		name   string
		ref    maulv1.RepositoryRef
		status maulv1.RepositoryStatus
		ok     bool
	}{
		{"commit pinned to itself", maulv1.RepositoryRef{Commit: commit},
			maulv1.RepositoryStatus{ResolvedSHA: commit}, true},
		{"commit with no pin yet", maulv1.RepositoryRef{Commit: commit},
			maulv1.RepositoryStatus{ObservedGeneration: 1}, true},
		{"commit pinned to another sha", maulv1.RepositoryRef{Commit: commit},
			maulv1.RepositoryStatus{ResolvedSHA: head}, false},
		{"commit with a runner image", maulv1.RepositoryRef{Commit: commit},
			maulv1.RepositoryStatus{ResolvedSHA: commit, RunnerImage: image}, false},
		{"branch with its head", maulv1.RepositoryRef{Branch: "main"},
			maulv1.RepositoryStatus{ResolvedSHA: head}, true},
		{"branch with a runner image", maulv1.RepositoryRef{Branch: "main"},
			maulv1.RepositoryStatus{ResolvedSHA: head, RunnerImage: image}, true},
		{"default branch with a runner image", maulv1.RepositoryRef{},
			maulv1.RepositoryStatus{ResolvedSHA: head, RunnerImage: image}, true},
	} {
		t.Run("status of repository "+tt.name, func(t *testing.T) {
			r := &maulv1.Repository{
				ObjectMeta: metav1.ObjectMeta{Name: "repo-s2-st-" + shortHash(tt.name), Namespace: "default"},
				Spec:       maulv1.RepositorySpec{URL: "https://github.com/acme/render", Ref: tt.ref},
			}
			if err := c.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			r.Status = tt.status
			err := c.Status().Update(ctx, r)
			if (err == nil) != tt.ok {
				t.Errorf("Status().Update(repository %s) = %v, want ok %v", tt.name, err, tt.ok)
			}
		})
	}
}
