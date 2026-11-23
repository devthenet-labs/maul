// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"errors"
	"time"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// Why a save was not made, beside the gate's own reasons (Reason*).
const (
	// ResultGenerationChanged: the Project's spec changed since the page
	// was served. The page reloads; nothing is merged.
	ResultGenerationChanged = "generation-changed"
	// ResultSchemaChanged: the page is of another Schema.
	ResultSchemaChanged = "schema-changed"
	// ResultInvalidDocument: the document is not one the page could have
	// posted for this Project (a DocumentError, told as the one refusal).
	ResultInvalidDocument = "invalid-document"
	// ResultRefused: Check refused a change (the refusals say which).
	ResultRefused = "refused"
	// ResultConfirmRequired: a weakening change is not confirmed yet (the
	// confirmations say which).
	ResultConfirmRequired = "confirm-required"
)

// Decision is what to do with a save.
type Decision struct {
	// Result is the answer for the page.
	Result ProjectSettingsResult
	// Gate is the gate's answer for the account, with the approver login
	// it resolved to: who the audit line names.
	Gate GateResult
	// Patched is the Project to patch the live one to, nil when nothing is
	// to be written: the save was not allowed, or changed nothing.
	Patched *v1alpha1.Project
}

// Decide runs a save through every step, in the one order they are safe
// in, and never changes live: the gate for the account; the page's
// generation and schema against the live object's; Apply on a copy; Check;
// the confirmations; and the record of the save. Patched is set only when
// all of them agree and something changed, and then differs from live only
// in editable fields and the EditedAnnotation.
//
// The write path calls it inside its conflict retry, on the Project it just
// read uncached, so a write decided on stale state is decided again. The
// gate comes first: someone who may not edit learns nothing else about the
// save, not even whether the page was current.
func Decide(live *v1alpha1.Project, req ProjectSettingsRequest, pol Policy, githubID int64,
	now time.Time) Decision {
	d := Decision{Gate: Gate(live, githubID, now), Result: ProjectSettingsResult{Generation: live.Generation,
		Changes: []ProjectSettingsChange{}, Refusals: []ProjectSettingsRefusal{},
		Confirm: []ProjectSettingsConfirmation{}}}
	switch {
	case !d.Gate.OK:
		d.Result.Reason = d.Gate.Reason
		return d
	case req.Generation != live.Generation:
		d.Result.Reason = ResultGenerationChanged
		return d
	case req.Schema != Schema:
		d.Result.Reason = ResultSchemaChanged
		return d
	}
	out := live.DeepCopy()
	changes, err := Apply(out, req.Settings)
	if err != nil {
		d.Result.Reason = ResultInvalidDocument
		if de := (*DocumentError)(nil); errors.As(err, &de) {
			d.Result.Refusals = append(d.Result.Refusals,
				ProjectSettingsRefusal{Field: de.Field, Item: de.Item, Reason: de.Reason})
		}
		return d
	}
	d.Result.Changes = changes
	if len(changes) == 0 {
		return d
	}
	if refusals := Check(live, out, pol, d.Gate.Login); len(refusals) > 0 {
		d.Result.Reason, d.Result.Refusals = ResultRefused, refusals
		return d
	}
	if need := Unconfirmed(Confirmations(live, out, pol), req.Confirm); len(need) > 0 {
		d.Result.Reason, d.Result.Confirm = ResultConfirmRequired, need
		return d
	}
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	out.Annotations[EditedAnnotation] = EncodeEdit(NewEdit(githubID, d.Gate.Login, now, changes))
	d.Patched = out
	return d
}
