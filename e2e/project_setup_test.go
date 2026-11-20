// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/e2e/fakegithub"
)

// TestProjectSetupEndToEnd applies the Project `maul setup project` writes,
// as a person does after the command, beside a Forge: the Project becomes
// Ready, and intent-controller has created the trigger and approve labels
// on the intent repository, so a new Project needs no hand label step.
func TestProjectSetupEndToEnd(t *testing.T) {
	cl, gh, _ := startSourceStack(t, appRepo)
	gh.SetRole(approver.Login, "write")
	cl.stoppableController(t, "intent-controller", intentArgs...)

	file := filepath.Join(t.TempDir(), projectName+".project.yaml")
	runMaul(t, build(t, "maul"), "setup", "project", projectName, "--intent-repo", intentRepoURL, "--repo",
		appRepoURL, "--approver", approver.Login, "-n", namespace, "-o", file)
	if labels := repoLabelNames(gh.RepoLabels(intentOwner, intentRepo)); len(labels) != 0 {
		t.Fatalf("the intent repository has labels before the Project exists: %v", labels)
	}
	for _, obj := range decodeAll(t, []byte(readFile(t, file))) {
		if err := cl.client.Create(context.Background(), obj); err != nil {
			t.Fatalf("apply %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}

	eventually(t, "the project to be Ready", func() bool {
		var p v1alpha1.Project
		key := types.NamespacedName{Namespace: namespace, Name: projectName}
		if err := cl.client.Get(context.Background(), key, &p); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady)
	})
	labels := repoLabelNames(gh.RepoLabels(intentOwner, intentRepo))
	if !slices.Contains(labels, triggerLabel) || !slices.Contains(labels, approveLabel) {
		t.Errorf("intent repository labels = %v, want %s and %s created by intent-controller", labels,
			triggerLabel, approveLabel)
	}
}

// repoLabelNames is the names of labels.
func repoLabelNames(labels []fakegithub.RepoLabel) []string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names
}
