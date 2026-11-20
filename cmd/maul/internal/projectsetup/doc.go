// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package projectsetup is the engine behind `maul setup project`: it holds
// the flags a person typed to the Project CRD's own rules (Plan), so a
// manifest the command writes is one the API server admits, and renders
// that Project as a plain manifest to keep and apply with kubectl (Render).
//
// It is pure: no Kubernetes client and no GitHub call. The manifest carries
// only what was given; every other field is left to the CRD's defaults,
// and the trigger and approve labels are created on the intent repository
// by intent-controller, not here.
package projectsetup
