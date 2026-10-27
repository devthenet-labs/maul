// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package crdconv is the one adapter between the acceptance grammar
// (internal/acceptance, stdlib only) and its hand-written CRD mirrors in
// api/v1alpha1: criteria, the tester's observations and the environment's
// part facts. intent-controller and preview-controller both convert through
// it, so the two can never disagree on one object.
//
// Every conversion is strict: the value is re-encoded as JSON and decoded
// into the other form with unknown fields refused, so a field one side has
// and the other lacks is an error, never silently dropped. The mirrors are
// pinned field for field by the tests here and by internal/acceptance's
// crd_pin_test.go.
//
// It imports only the standard library, internal/acceptance and
// api/v1alpha1 (a test pins that).
package crdconv
