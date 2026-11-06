// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package source

import (
	"errors"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/runnerimage"
)

const pinnedCommit = "0123456789abcdef0123456789abcdef01234567"

func commitRepository() *v1alpha1.Repository {
	repo := testRepository()
	repo.Spec.Ref.Commit = pinnedCommit
	return repo
}

func notFoundErr() error {
	return fmt.Errorf("archive link: %w", ghclient.ErrArchiveNotFound)
}

func TestCommitRefPinsTheCommitWithoutResolvingABranch(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: "tar-bytes"}
	r, c := harness(t, gh, testForge("gh"), commitRepository())

	reconcile(t, r)

	repo := getRepo(t, c)
	if repo.Status.ResolvedSHA != pinnedCommit {
		t.Errorf("resolvedSHA = %q, want the commit %s", repo.Status.ResolvedSHA, pinnedCommit)
	}
	if gh.branchCalls != 0 || gh.headCalls != 0 {
		t.Errorf("branch resolved (default %d, head %d), want no branch resolution for a commit",
			gh.branchCalls, gh.headCalls)
	}
	if len(gh.tarballRefs) != 1 || gh.tarballRefs[0] != pinnedCommit {
		t.Errorf("tarball refs = %v, want exactly [%s]", gh.tarballRefs, pinnedCommit)
	}
	if art := repo.Status.Artifact; art == nil || art.Digest == "" {
		t.Errorf("artifact = %+v, want a stored digest", art)
	}
	if !meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
		t.Errorf("Ready = %+v, want True", repo.Status.Conditions)
	}
}

func TestCommitRefNeverPinsARunnerImage(t *testing.T) {
	files := map[string]string{runnerimage.AgentYAMLPath: "image: ghcr.io/acme/agent:1\n"}
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarball: tarball(t, files)}
	fr := &fakeResolver{}
	r, c := imageHarness(t, gh, fr)
	// imageHarness seeds the branch Repository; swap in a commit one.
	if err := c.Delete(t.Context(), testRepository()); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), commitRepository()); err != nil {
		t.Fatal(err)
	}

	reconcile(t, r)

	repo := getRepo(t, c)
	if repo.Status.RunnerImage != nil {
		t.Errorf("runnerImage = %+v, want none for a commit Repository", repo.Status.RunnerImage)
	}
	if len(fr.calls) != 0 {
		t.Errorf("resolver called %d times, want 0", len(fr.calls))
	}
	if !meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
		t.Errorf("Ready = %+v, want True", repo.Status.Conditions)
	}
}

func TestCommitRefNotFoundIsTerminal(t *testing.T) {
	gh := &fakeForgeClient{tarballErr: notFoundErr()}
	r, c := harness(t, gh, testForge("gh"), commitRepository())

	if _, err := reconcileErr(t, r); err != nil {
		t.Fatalf("Reconcile = %v, want nil: a missing commit is terminal, not a retry", err)
	}

	repo := getRepo(t, c)
	stalled := condition(t, repo, v1alpha1.ConditionStalled)
	if stalled == nil || stalled.Reason != v1alpha1.ReasonCommitNotFound {
		t.Errorf("Stalled = %+v, want reason %s", stalled, v1alpha1.ReasonCommitNotFound)
	}
	if meta.IsStatusConditionTrue(repo.Status.Conditions, v1alpha1.ConditionReady) {
		t.Error("Ready = True, want False")
	}
	if repo.Status.Artifact != nil {
		t.Errorf("artifact = %+v, want none", repo.Status.Artifact)
	}
}

func TestCommitRefOtherFetchFailureRetries(t *testing.T) {
	gh := &fakeForgeClient{tarballErr: errors.New("boom")}
	r, c := harness(t, gh, testForge("gh"), commitRepository())

	if _, err := reconcileErr(t, r); err == nil {
		t.Fatal("Reconcile = nil, want the transient error returned for backoff")
	}
	if cond := condition(t, getRepo(t, c), v1alpha1.ConditionStalled); cond != nil {
		t.Errorf("Stalled = %+v, want none for a transient failure", cond)
	}
}

func TestBranchRefNotFoundStaysAFetchFailure(t *testing.T) {
	gh := &fakeForgeClient{defaultBranch: "main", headSHA: "abc123", tarballErr: notFoundErr()}
	r, c := harness(t, gh, testForge("gh"), testRepository())

	if _, err := reconcileErr(t, r); err == nil {
		t.Fatal("Reconcile = nil, want the error returned: only a commit ref 404 is terminal")
	}
	ready := condition(t, getRepo(t, c), v1alpha1.ConditionReady)
	if ready == nil || ready.Reason != "FetchFailed" {
		t.Errorf("Ready = %+v, want FetchFailed", ready)
	}
}
