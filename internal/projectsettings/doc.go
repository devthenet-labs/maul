// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package projectsettings is the pure core of the status page's Project
// settings: which fields of a Project an approver may change from the page,
// who counts as an approver there, and what each change is held to. The
// status server's read view and write path are thin over it, the chart's
// admission policy is tested against it, and the page's wire types are its
// types, so the three cannot disagree about a field.
//
// Everything is driven by one table (Fields): the editable fields of
// spec, each with its lock group, its kind, the schema's own range and when
// a change takes effect. A field not in the table cannot be changed here,
// whatever a save holds. The table covers limits, models, checks, the
// evaluator's settings, the build's delegation switches, the approver list
// and, per repository, the one field agentResourceClass. Where agents are pointed is not in it: the
// intent repository, the labels, a repository's name, url or preview, the
// harness and the surfaces.
//
// A save runs through four steps, each a pure function of the Project and
// the save, and Decide runs them in the one order they are safe in, handing
// back the Project to patch only when all of them agree:
//
//   - Gate: may this GitHub account edit? It matches the session's numeric
//     account id to an approver intent-controller resolved
//     (status.approvers), who still is on the live list and has write
//     access to the intent repository, within ApproversMaxAge. No login
//     claim is trusted.
//   - Apply: write the posted document (ProjectSettings) onto a copy of the
//     Project, touching only what differs. An unchanged save changes
//     nothing, so its merge patch is empty.
//   - Check: judge what changed against the operator's Policy (locks,
//     switches, ceilings, floors) and what intent-controller admits
//     (status.admits: models, resource classes, its own ceilings and run
//     budget bounds, the delegation it allows). Only what changed is judged, a cleared field by the
//     value it lands on; a value already out of bounds may be kept or moved
//     back, never further; with nothing published, no model or class is
//     chosen, no delegation switch turned on and nothing the controller
//     bounds is raised.
//   - Confirmations: the weakening changes a person must confirm, each
//     under a key that names the change and its value.
//
// View builds what the page is served: the settings (Read), each field's
// bounds, the approvers as GitHub resolved them, the other field managers
// that apply a field, and the record of the last save (the EditedAnnotation
// annotation, NewEdit, ParseEdit).
//
// The rules are not this package's own. Ranges are the generated CRD's (a
// test reads it), defaults the API's accessors', the controller's rules
// arrive as data in status.admits, and the operator's as a Policy the chart
// renders once for both the status server and the admission policy on its
// ServiceAccount. That policy, not this package, is the containment: a
// compromised status server skips Check, and is still held to it.
// CheckApprovers, the approver-list rule, is also what `maul setup project`
// holds --approver to.
//
// The package imports the standard library and api/v1alpha1, and nothing
// else (a test pins that): no client, no controller, no HTTP.
package projectsettings
