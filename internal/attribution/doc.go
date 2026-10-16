// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package attribution is the pure core of run attribution (the per-user
// credentials design, layer L1): who an agent run is for, and how that is
// written on the run and on its Job.
//
// Every run controller (investigation, remediation, intent and evaluation)
// reads one switch, --attribution-mode (Mode). Under ModeOff, the default,
// nothing is stamped anywhere: run specs and Jobs are exactly what they were
// before the feature. Under ModeAttribute each controller stamps the run's
// immutable spec.attributedTo at creation (Stamp) and every agent Job it
// launches, and that Job's pod template, with the annotation
// v1alpha1.AnnotationAttributedTo (Value), which is mandatory there: "org"
// when no person is behind the run.
//
// A person is a GitHub account, identified by its numeric id; the login is
// for display. To applies the re-attribution rule: a human the attribution
// table names who has no patchy identity (no numeric id is known for them,
// as for an approval made on the status page or in the CLI) makes the run
// the org's, with an "-unregistered" reason saying why.
//
// The annotation's grammar (ValuePattern) is also the chart's admission
// policy's; a seeded property pins that every Value matches it and parses
// back.
//
// The package imports only the standard library and api/v1alpha1 (a test
// pins that), so any controller, the jobs package and the templates may use
// it.
package attribution
