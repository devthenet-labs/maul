// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// The check reviewer stage is being retired (the owner's decision 8), so the
// page offers neither its model nor its limits: they are outside the
// editable part, which the admission policy pins. The controller still runs
// the stage, so its limits keep counting toward a run's budget (RunBudget,
// StageNames), read from the live spec only.
func TestCheckReviewIsOutsideTheEditablePart(t *testing.T) {
	for _, f := range Fields() {
		if strings.Contains(strings.ToLower(f.ID), "checkreview") {
			t.Errorf("field %s is editable; the check reviewer's settings are not", f.ID)
		}
	}
	p := baseProject(func(p *v1alpha1.Project) {
		p.Spec.Models = &v1alpha1.ProjectModels{Plan: "anthropic/claude-a", CheckReview: "anthropic/claude-b"}
		p.Spec.Limits.CheckReview = v1alpha1.StageLimits{MaxTurns: 4, TokenBudget: 50_000, MaxCostMicroUSD: 300_000,
			Timeout: dur(5 * time.Minute)}
	})
	for name, v := range map[string]any{"document": Read(&p.Spec), "view": View(p, openPolicy(), 1, testNow)} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(raw)), "checkreview") {
			t.Errorf("the %s names the check reviewer:\n%s", name, raw)
		}
	}

	// A save of everything the page can change, the check reviewer's
	// settings in the Project beside it, leaves them exactly as they are.
	doc := Read(&p.Spec)
	doc.Models.Plan, doc.Models.Evaluate = "", "anthropic/claude-dear"
	doc.Stages.Evaluate = ProjectSettingsStage{MaxTurns: 50, TokenBudget: 200_000, MaxCostMicroUSD: 2_000_000,
		TimeoutSeconds: 900}
	doc.Limits.MaxEvaluateCostMicroUSD = 5_000_000
	out := p.DeepCopy()
	changes, err := Apply(out, doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if strings.Contains(strings.ToLower(c.Field), "checkreview") {
			t.Errorf("a change to %s", c.Field)
		}
	}
	if out.Spec.Models.CheckReview != "anthropic/claude-b" ||
		!reflect.DeepEqual(out.Spec.Limits.CheckReview, p.Spec.Limits.CheckReview) {
		t.Errorf("the check reviewer's settings moved: model %q, limits %+v", out.Spec.Models.CheckReview,
			out.Spec.Limits.CheckReview)
	}
	if got := StageNames(); !strings.Contains(strings.Join(got, ","), "checkReview") {
		t.Errorf("StageNames = %v: the controller still runs the check reviewer, whose budget counts", got)
	}
}
