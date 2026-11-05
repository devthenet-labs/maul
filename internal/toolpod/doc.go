// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package toolpod holds what every host of the agent evaluator's tool pod
// renders the same way (DESIGN 2.1, 4.2; SPEC S2-02): a container's
// restricted posture under its own UID (Security), the split of a
// verification's timeout into the Job's deadline, the pod's below it and
// the relay's wall clock below that (Deadlines), the per-evaluation key
// (NewKey), and the relay's configuration data, tools.json beside the
// outcomes under evaluation (ConfigData). Nothing here knows a slot, a
// Preview or a namespace: preview-controller's slot tool pod builds on it
// today, the agents namespace's next.
//
// It renders values only: the standard library, the core API types, the
// CRD types and the relay's configuration (a test pins that), no client.
package toolpod
