// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"strings"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// LatestBuildRun is the latest of in's own build runs of repoURL among
// runs, nil for none: latest by creation, then by name, so it is the same
// run whatever order runs are listed in, two created in the same second
// included (U24). Its Repository is the runner-image anchor of an agents
// round: the intent reconciler's gate (the image and profile it records on
// the workspace) and this host (the pod it renders) both pick it here, so
// the round runs exactly what was gated.
func LatestBuildRun(in *v1alpha1.Intent, repoURL string, runs []*v1alpha1.IntentRun) *v1alpha1.IntentRun {
	var out *v1alpha1.IntentRun
	for _, run := range runs {
		if run.Spec.Stage != v1alpha1.IntentStageBuild || !sameRepo(run.Spec.Repository.URL, repoURL) ||
			run.Spec.IntentRef.UID != in.UID || !controlledBy(run.OwnerReferences, in.UID) {
			continue
		}
		if out == nil || later(run, out) {
			out = run
		}
	}
	return out
}

// later reports a created after b, or in the same instant with a name
// ordered after b's.
func later(a, b *v1alpha1.IntentRun) bool {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c > 0
	}
	return strings.Compare(a.Name, b.Name) > 0
}
