// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package preview deploys an intent's immutable runtime images, one to four
// components behind one host, into a fixed, chart-guarded namespace slot. It
// has no GitHub, ECR, cloud, namespace or RBAC client. With
// verification on (a tester image) it also writes, in its slots only, one
// NetworkPolicy per part of a verification-capable Preview, admitting
// exactly the edges re-derived from the operator's Project beside the
// chart's preview-isolation, and runs the tester as a Job; off, it never
// touches either kind. Its VerificationReconciler answers each
// PreviewVerification request in the release namespace: it re-checks the
// request against the Preview and the Intent's approved criteria, reads the
// tester's log through pods/log (and only the log of the Job's own pod), and
// writes observations, never verdicts, to the request's status. With the
// evaluator on (--preview-evaluator), an outcome verification instead runs
// the slot tool pod (toolpod.go, toolpolicy.go, toolrun.go): a Job of the
// tools relay with browser and sandbox sidecars, its CDP key Secret and
// tool ConfigMap (the only Secret and ConfigMap it writes in a slot, both
// owned by the Job and never read back), and one ingress policy admitting
// exactly its evaluator; it publishes status.tools once tools is Ready,
// then verifies the tools container's ledger (ledger.go) and stores it, and
// the evidence images it cites (evidence.go), in ConfigMaps the request
// owns in the release namespace. That ingress policy cannot be owned across
// namespaces, so a request deleted while it runs leaves it behind; the
// orphan sweep deletes an evaluator ingress policy whose request is gone once
// it is past a short grace. Every rendered object is deterministic, a Preview
// without an environment renders exactly what it did before verification
// (and a single-component one what it did before multi-component previews)
// existed, and every slot is released only after finalizer cleanup sees no
// workload left.
package preview
