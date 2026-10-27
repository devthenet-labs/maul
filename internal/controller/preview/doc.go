// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package preview deploys an intent's immutable runtime images, one to four
// components behind one host, into a fixed, chart-guarded namespace slot. It
// has no GitHub, ECR, cloud, namespace, RBAC or Secret client. With
// verification on (a tester image) it also writes, in its slots only, one
// NetworkPolicy per part of a verification-capable Preview, admitting
// exactly the edges re-derived from the operator's Project beside the
// chart's preview-isolation, and runs the tester as a Job; off, it never
// touches either kind. Every rendered object is deterministic, a Preview
// without an environment renders exactly what it did before verification
// (and a single-component one what it did before multi-component previews)
// existed, and every slot is released only after finalizer cleanup sees no
// workload left.
package preview
