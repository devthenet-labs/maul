// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// request is a save of p's own settings after edit, at p's generation.
func request(p *v1alpha1.Project, edit func(*ProjectSettings), confirm ...string) ProjectSettingsRequest {
	doc := Read(&p.Spec)
	if edit != nil {
		edit(&doc)
	}
	return ProjectSettingsRequest{Generation: p.Generation, Schema: Schema, Settings: doc, Confirm: confirm}
}

func TestDecide(t *testing.T) {
	raise := func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(7) }
	off := func(d *ProjectSettings) { d.Verification.Enabled = false }
	for _, tc := range []struct {
		name    string
		project func(*v1alpha1.Project)
		policy  func(*Policy)
		id      int64
		req     func(*v1alpha1.Project) ProjectSettingsRequest
		reason  string
		written bool
		changes int
	}{
		{name: "a change by an approver is written", id: 1, written: true, changes: 1,
			req: func(p *v1alpha1.Project) ProjectSettingsRequest { return request(p, raise) }},
		{name: "nothing changed: nothing written, no reason", id: 1,
			req: func(p *v1alpha1.Project) ProjectSettingsRequest { return request(p, nil) }},
		{name: "no approver: the gate's reason, and nothing of the save is told", id: 99, reason: ReasonNotApprover,
			req: func(p *v1alpha1.Project) ProjectSettingsRequest { return request(p, raise) }},
		{name: "the gate comes before a stale page", id: 99, reason: ReasonNotApprover,
			req: func(p *v1alpha1.Project) ProjectSettingsRequest {
				r := request(p, raise)
				r.Generation--
				return r
			}},
		{name: "the spec changed since the page was served", id: 1, reason: ResultGenerationChanged,
			req: func(p *v1alpha1.Project) ProjectSettingsRequest {
				r := request(p, raise)
				r.Generation--
				return r
			}},
		{name: "a page of another schema", id: 1, reason: ResultSchemaChanged,
			req: func(p *v1alpha1.Project) ProjectSettingsRequest {
				r := request(p, raise)
				r.Schema++
				return r
			}},
		{name: "a document the page could not have posted", id: 1, reason: ResultInvalidDocument,
			req: func(p *v1alpha1.Project) ProjectSettingsRequest {
				return request(p, func(d *ProjectSettings) {
					d.Repositories = append(d.Repositories, ProjectSettingsRepository{Name: "evil"})
				})
			}},
		{name: "a change the policy refuses", id: 1, reason: ResultRefused, changes: 1,
			req: func(p *v1alpha1.Project) ProjectSettingsRequest { return request(p, off) }},
		{name: "a weakening change not yet confirmed", id: 1, reason: ResultConfirmRequired, changes: 1,
			policy: func(p *Policy) { p.Allow.DisableEvaluator = true },
			req:    func(p *v1alpha1.Project) ProjectSettingsRequest { return request(p, off) }},
		{name: "a confirmation for something else", id: 1, reason: ResultConfirmRequired, changes: 1,
			policy: func(p *Policy) { p.Allow.DisableEvaluator = true },
			req:    func(p *v1alpha1.Project) ProjectSettingsRequest { return request(p, off, "checks-empty") }},
		{name: "confirmed: written", id: 1, written: true, changes: 1,
			policy: func(p *Policy) { p.Allow.DisableEvaluator = true },
			req:    func(p *v1alpha1.Project) ProjectSettingsRequest { return request(p, off, "evaluator-off") }},
		{name: "a refusal is told before a confirmation is asked", id: 1, reason: ResultRefused, changes: 2,
			policy: func(p *Policy) { p.Allow.DisableEvaluator = true },
			req: func(p *v1alpha1.Project) ProjectSettingsRequest {
				return request(p, func(d *ProjectSettings) {
					d.Verification.Enabled = false
					d.Limits.MaxRevisions = i32(11)
				})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var muts []func(*v1alpha1.Project)
			if tc.project != nil {
				muts = append(muts, tc.project)
			}
			p := baseProject(muts...)
			before := p.DeepCopy()
			pol := DefaultPolicy()
			if tc.policy != nil {
				tc.policy(&pol)
			}
			d := Decide(p, tc.req(p), pol, tc.id, testNow)
			if d.Result.Reason != tc.reason || (d.Patched != nil) != tc.written || len(d.Result.Changes) != tc.changes {
				t.Fatalf("Decide: reason %q, written %v, %d changes; want %q, %v, %d (result %+v)", d.Result.Reason,
					d.Patched != nil, len(d.Result.Changes), tc.reason, tc.written, tc.changes, d.Result)
			}
			if !reflect.DeepEqual(p, before) {
				t.Error("Decide changed the live Project")
			}
			if d.Result.Generation != p.Generation {
				t.Errorf("result generation %d, want the live %d", d.Result.Generation, p.Generation)
			}
			if d.Result.Changes == nil || d.Result.Refusals == nil || d.Result.Confirm == nil {
				t.Errorf("a list of the result is null: %+v", d.Result)
			}
			if (d.Result.Reason == ResultRefused || d.Result.Reason == ResultInvalidDocument) != (len(d.Result.Refusals) > 0) {
				t.Errorf("reason %q with refusals %+v", d.Result.Reason, d.Result.Refusals)
			}
			if (d.Result.Reason == ResultConfirmRequired) != (len(d.Result.Confirm) > 0) {
				t.Errorf("reason %q with confirmations %+v", d.Result.Reason, d.Result.Confirm)
			}
		})
	}
}

// What is written is the change and the record of it, and nothing else:
// the patch holds the changed leaves and the one annotation.
func TestDecideStampsTheSave(t *testing.T) {
	p := baseProject()
	d := Decide(p, request(p, func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(7) }), DefaultPolicy(), 2, testNow)
	if d.Patched == nil || !d.Gate.OK || d.Gate.Login != "bob" {
		t.Fatalf("Decide = %+v", d)
	}
	const patch = `{"metadata":{"annotations":{"maul.run/settings-edited":"{\"githubID\":2,\"login\":\"bob\",` +
		`\"at\":\"2026-11-23T12:00:00Z\",\"fields\":[\"limits.maxRevisions\"],\"more\":0}"}},` +
		`"spec":{"limits":{"maxRevisions":7}}}`
	if got := mergePatch(t, p, d.Patched); got != patch {
		t.Errorf("merge patch = %s\nwant         %s", got, patch)
	}
	// A Project with no annotations at all gets the one.
	bare := baseProject(func(p *v1alpha1.Project) { p.Annotations = nil })
	d = Decide(bare, request(bare, func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(7) }), DefaultPolicy(), 1,
		testNow)
	if d.Patched == nil || len(d.Patched.Annotations) != 1 {
		t.Fatalf("Decide on a Project with no annotations = %+v", d.Patched)
	}
	if e, ok := ParseEdit(d.Patched.Annotations[EditedAnnotation]); !ok || e.Login != "alice" || e.GitHubID != 1 {
		t.Errorf("the record reads back as %+v, %v", e, ok)
	}
}

// The write path's whole promise in one property. Whatever the Project, the
// policy, the save and whoever sends it, Decide hands back an object to
// patch only when every step agreed: the gate opened for that account, the
// page was current, something changed, Check refused nothing and nothing
// was left unconfirmed. And what it hands back differs from the live
// Project only in editable fields and the one annotation.
func TestDecideWritesOnlyWhatEveryStepAllows(t *testing.T) {
	written := 0
	prop := func(p *v1alpha1.Project, pol Policy, editSeed int64) bool {
		r := rand.New(rand.NewSource(editSeed))
		req := request(p, func(d *ProjectSettings) { genEdit(r, d) })
		if r.Intn(10) == 0 {
			req.Generation++
		}
		if r.Intn(10) == 0 {
			req.Schema++
		}
		// Mostly an account the gate opens for, when the Project has one.
		id := int64(r.Intn(8))
		for candidate := int64(1); candidate < 8 && r.Intn(4) != 0; candidate++ {
			if Gate(p, candidate, testNow).OK {
				id = candidate
				break
			}
		}
		out := p.DeepCopy()
		if _, err := Apply(out, req.Settings); err != nil {
			return false
		}
		need := Confirmations(p, out, pol)
		if r.Intn(2) == 0 {
			for _, c := range need {
				req.Confirm = append(req.Confirm, c.Key)
			}
		}
		d := Decide(p, req, pol, id, testNow)
		if d.Patched == nil {
			return d.Result.Reason != "" || len(d.Result.Changes) == 0
		}
		written++
		gate := Gate(p, id, testNow)
		switch {
		case !gate.OK, req.Generation != p.Generation, req.Schema != Schema, d.Result.Reason != "",
			len(d.Result.Changes) == 0, len(Check(p, out, pol, gate.Login)) != 0,
			len(Unconfirmed(need, req.Confirm)) != 0:
			t.Logf("written though a step disagreed: gate %+v, request gen %d schema %d, result %+v", gate,
				req.Generation, req.Schema, d.Result)
			return false
		}
		if !reflect.DeepEqual(d.Patched.Spec, out.Spec) || !reflect.DeepEqual(d.Patched.Status, p.Status) {
			t.Log("what is written is not what Apply made")
			return false
		}
		for _, leaf := range differing(t, p.ObjectMeta, d.Patched.ObjectMeta) {
			if leaf != "annotations."+EditedAnnotation {
				t.Logf("the save changes metadata %s", leaf)
				return false
			}
		}
		return true
	}
	cfg := quickConfig(20000, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genProject(r))
		args[1] = reflect.ValueOf(genPolicy(r))
		args[2] = reflect.ValueOf(r.Int63())
	})
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
	if written < 500 {
		t.Errorf("only %d of 20000 generated saves were written: the property judged too few", written)
	}
	if testing.Verbose() {
		t.Logf("%d saves written", written)
	}
}
