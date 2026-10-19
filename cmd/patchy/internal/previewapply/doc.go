// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package previewapply runs `patchy setup previews --apply`
// (setup-previews-easy (d)): the in-cluster steps previewsetup.ApplySteps
// plans from the files the command wrote, in order, with the caller's own
// kubeconfig, stopping at the first failure and saying how to undo what
// the run changed.
//
// The parts, each its own seam:
//
//   - Runner executes a plan over two ports and a check: Cluster (the
//     Kubernetes API) and Helm (helm releases). Every step is logged as it
//     starts and ends; a failure prints the undo commands for the steps
//     this run completed, newest first (helm rollback to the revision it
//     replaced, or helm uninstall for one it installed; kubectl delete for
//     manifests; the namespace it created), and NEXT-STEPS.md's "Rolling
//     back" for the whole install. Each command carries the plan's
//     previewsetup.Connection, the caller's --kubeconfig and --context, so
//     a pasted undo reaches the cluster the run changed. A Secret that was
//     already there is replaced in place, and the undo says no command
//     restores it. Before any step, a NoReplace file's object (a Dex client
//     secret drawn anew) already in the cluster stops the run
//     (ErrWouldReplace), reading metadata only. Re-running is safe: a server-side apply
//     of the same manifest, an upgrade to the same chart and values, and a
//     wait already met each change nothing.
//   - Objects decodes a manifest file into the objects to apply. A secret
//     file's decode or apply error never quotes its content: only the
//     object's kind, namespace, name and the API server's status reason.
//   - KubeCluster is the Cluster over a controller-runtime client:
//     server-side apply as previewsetup.ApplyFieldManager (forcing
//     ownership, as `kubectl apply --server-side --force-conflicts` does,
//     since the files are the command's own and a hand-applied copy may
//     hold their fields), a Secret's existence read as metadata only
//     (never its data), and polled waits for a Certificate's Ready
//     condition and a Deployment's rollout.
//   - ExecHelm is the Helm port over the helm binary on PATH (helm 3.8 or
//     later, or 4), run with the caller's --kubeconfig and --context: the
//     upgrades are exactly the commands NEXT-STEPS.md prints, with the
//     caller's own repository and registry configuration, so an OCI chart
//     the caller has logged in to pulls as it would by hand. The CLI links
//     the Helm SDK only to render charts (internal/mirror/render); it does
//     not install with it.
package previewapply
