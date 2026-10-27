// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/intentview"
	"github.com/devthenet-labs/maul/internal/stats"
	"github.com/devthenet-labs/maul/internal/usagerollup"
)

// Every place that treats a revise round by its trigger decides, for each
// trigger the schema admits, what that round is: an approver's (review,
// command), a check-fix (checks) or an acceptance fix (verify). A trigger
// added to the schema and to v1alpha1.IntentRunTriggers fails here until
// every site has a row for it, so no site silently treats it as a review.

const (
	tReview  = v1alpha1.IntentRunTriggerReview
	tCommand = v1alpha1.IntentRunTriggerCommand
	tChecks  = v1alpha1.IntentRunTriggerChecks
	tVerify  = v1alpha1.IntentRunTriggerVerify
)

// reviseRunOf is one revise run of round 1 of trigger t at plan revision 1.
func reviseRunOf(t v1alpha1.IntentRunTrigger) *v1alpha1.IntentRun {
	return &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageRevise, Round: 1,
		Attempt: 1, Trigger: t, Repository: v1alpha1.IntentRunRepository{URL: "https://github.com/acme/app"},
		Inputs: v1alpha1.IntentRunInputs{PlanRevision: 1}}}
}

type counted struct{ revisions, checkFixes, verifyFixes int32 }

func TestTriggerSitesExhaustive(t *testing.T) {
	approved := &v1alpha1.Intent{Status: v1alpha1.IntentStatus{Approval: &v1alpha1.IntentApproval{PlanRevision: 1}}}
	type label struct {
		text        string
		namesChecks bool
	}
	sites := []struct {
		name string
		got  func(v1alpha1.IntentRunTrigger) any
		want map[v1alpha1.IntentRunTrigger]any
	}{
		{"roundKindOf", func(tr v1alpha1.IntentRunTrigger) any { return roundKindOf(tr) },
			map[v1alpha1.IntentRunTrigger]any{tReview: roundHuman, tCommand: roundHuman, tChecks: roundChecks,
				tVerify: roundVerify}},
		{"intent_status fixingChecks", func(tr v1alpha1.IntentRunTrigger) any { return fixingChecks(tr) },
			map[v1alpha1.IntentRunTrigger]any{tReview: false, tCommand: false, tChecks: true, tVerify: false}},
		{"human_pending humanReason", func(tr v1alpha1.IntentRunTrigger) any { return humanReason(tr) },
			map[v1alpha1.IntentRunTrigger]any{tReview: v1alpha1.AutoFixHumanReview,
				tCommand: v1alpha1.AutoFixHumanCommand, tChecks: v1alpha1.AutoFixReason(""),
				tVerify: v1alpha1.AutoFixReason("")}},
		{"verify_loop humanRoundHalt", func(tr v1alpha1.IntentRunTrigger) any {
			in := approved.DeepCopy()
			in.Status.Verification = &v1alpha1.IntentVerification{}
			return (&pass{in: in, runs: []*v1alpha1.IntentRun{reviseRunOf(tr)}}).humanRoundHalt()
		}, map[v1alpha1.IntentRunTrigger]any{tReview: v1alpha1.AutoFixHumanReview,
			tCommand: v1alpha1.AutoFixHumanCommand, tChecks: v1alpha1.AutoFixReason(""),
			tVerify: v1alpha1.AutoFixReason("")}},
		{"intent_revise acksCommand", func(tr v1alpha1.IntentRunTrigger) any { return acksCommand(tr) },
			map[v1alpha1.IntentRunTrigger]any{tReview: false, tCommand: true, tChecks: false, tVerify: false}},
		{"intent_revise countCompletedRound", func(tr v1alpha1.IntentRunTrigger) any {
			var st v1alpha1.IntentStatus
			countCompletedRound(&st, tr)
			return counted{st.Revisions, st.CheckFixes, st.VerifyFixes}
		}, map[v1alpha1.IntentRunTrigger]any{tReview: counted{1, 0, 0}, tCommand: counted{1, 0, 0},
			tChecks: counted{0, 1, 0}, tVerify: counted{0, 0, 1}}},
		{"intent_revise retriesHeadMoved", func(tr v1alpha1.IntentRunTrigger) any { return retriesHeadMoved(tr) },
			map[v1alpha1.IntentRunTrigger]any{tReview: true, tCommand: true, tChecks: false, tVerify: false}},
		{"intent_revise ceilingBlocks", func(tr v1alpha1.IntentRunTrigger) any { return ceilingBlocks(tr) },
			map[v1alpha1.IntentRunTrigger]any{tReview: true, tCommand: true, tChecks: true, tVerify: false}},
		{"intent_revise requiresFeedback", func(tr v1alpha1.IntentRunTrigger) any { return requiresFeedback(tr) },
			map[v1alpha1.IntentRunTrigger]any{tReview: true, tCommand: true, tChecks: false, tVerify: false}},
		{"intent_revise roundNoticeLabel", func(tr v1alpha1.IntentRunTrigger) any {
			text, names := roundNoticeLabel(tr)
			return label{text, names}
		}, map[v1alpha1.IntentRunTrigger]any{tReview: label{"Revision round", false},
			tCommand: label{"Revision round", false}, tChecks: label{"", true},
			tVerify: label{"Acceptance fix round", false}}},
		{"revisionRounds", func(tr v1alpha1.IntentRunTrigger) any {
			return (&pass{runs: []*v1alpha1.IntentRun{reviseRunOf(tr)}}).revisionRounds()
		}, map[v1alpha1.IntentRunTrigger]any{tReview: int32(1), tCommand: int32(1), tChecks: int32(0),
			tVerify: int32(0)}},
		{"checkFixRounds", func(tr v1alpha1.IntentRunTrigger) any {
			return (&pass{runs: []*v1alpha1.IntentRun{reviseRunOf(tr)}}).checkFixRounds()
		}, map[v1alpha1.IntentRunTrigger]any{tReview: int32(0), tCommand: int32(0), tChecks: int32(1),
			tVerify: int32(0)}},
		{"verifyFixRounds", func(tr v1alpha1.IntentRunTrigger) any {
			return (&pass{in: approved, runs: []*v1alpha1.IntentRun{reviseRunOf(tr)}}).verifyFixRounds()
		}, map[v1alpha1.IntentRunTrigger]any{tReview: int32(0), tCommand: int32(0), tChecks: int32(0),
			tVerify: int32(1)}},
		{"intent_checks isCheckFix", func(tr v1alpha1.IntentRunTrigger) any { return isCheckFix(tr) },
			map[v1alpha1.IntentRunTrigger]any{tReview: false, tCommand: false, tChecks: true, tVerify: false}},
		{"resources stageNoun", func(tr v1alpha1.IntentRunTrigger) any { return stageNoun(reviseRunOf(tr)) },
			map[v1alpha1.IntentRunTrigger]any{tReview: "revise round", tCommand: "revise round",
				tChecks: "check-fix round", tVerify: "acceptance-fix round"}},
		{"replan_context failedRunNoun", func(tr v1alpha1.IntentRunTrigger) any {
			return failedRunNoun(reviseRunOf(tr))
		}, map[v1alpha1.IntentRunTrigger]any{tReview: "revise", tCommand: "revise", tChecks: "check-fix",
			tVerify: "acceptance-fix"}},
		{"working_notes workingNotesOf", func(tr v1alpha1.IntentRunTrigger) any {
			return workingNotesOf(reviseRunOf(tr))
		}, map[v1alpha1.IntentRunTrigger]any{tReview: "revise round 1 (attempt 1)",
			tCommand: "revise round 1 (attempt 1)", tChecks: "check-fix round 1 (attempt 1)",
			tVerify: "acceptance-fix round 1 (attempt 1)"}},
		{"revise_thread actsOnThread", func(tr v1alpha1.IntentRunTrigger) any { return actsOnThread(tr) },
			map[v1alpha1.IntentRunTrigger]any{tReview: true, tCommand: true, tChecks: false, tVerify: false}},
		{"revise_thread roundHeading", func(tr v1alpha1.IntentRunTrigger) any {
			heading, _ := roundHeading(tr)
			return heading
		}, map[v1alpha1.IntentRunTrigger]any{tReview: "Approver feedback", tCommand: "Approver feedback",
			tChecks: "Check failures", tVerify: "Acceptance failures"}},
		{"run_controller legacyEmptyHandoffPossible", func(tr v1alpha1.IntentRunTrigger) any {
			return legacyEmptyHandoffPossible(tr)
		}, map[v1alpha1.IntentRunTrigger]any{tReview: true, tCommand: false, tChecks: false, tVerify: false}},
		{"intentview RevisionRounds", func(tr v1alpha1.IntentRunTrigger) any {
			return intentview.RevisionRounds([]*v1alpha1.IntentRun{reviseRunOf(tr)})
		}, map[v1alpha1.IntentRunTrigger]any{tReview: int32(1), tCommand: int32(1), tChecks: int32(0),
			tVerify: int32(0)}},
		{"intentview CheckFixRounds", func(tr v1alpha1.IntentRunTrigger) any {
			return intentview.CheckFixRounds([]*v1alpha1.IntentRun{reviseRunOf(tr)})
		}, map[v1alpha1.IntentRunTrigger]any{tReview: int32(0), tCommand: int32(0), tChecks: int32(1),
			tVerify: int32(0)}},
		{"usagerollup IntentRunKind", func(tr v1alpha1.IntentRunTrigger) any {
			return usagerollup.IntentRunKind(reviseRunOf(tr).Spec)
		}, map[v1alpha1.IntentRunTrigger]any{tReview: stats.UsageKindRevise, tCommand: stats.UsageKindRevise,
			tChecks: stats.UsageKindCheckFix, tVerify: stats.UsageKindVerifyFix}},
	}
	for _, site := range sites {
		// A run with no trigger (one recorded before triggers were, or a
		// fixture) reads as a review round at every site.
		if got, want := site.got(""), site.got(tReview); !reflect.DeepEqual(got, want) {
			t.Errorf("%s(\"\") = %#v, want the review round's %#v", site.name, got, want)
		}
		if len(site.want) != len(v1alpha1.IntentRunTriggers) {
			t.Errorf("%s: %d rows for %d triggers", site.name, len(site.want), len(v1alpha1.IntentRunTriggers))
		}
		for _, tr := range v1alpha1.IntentRunTriggers {
			want, ok := site.want[tr]
			if !ok {
				t.Errorf("%s: no row for trigger %q", site.name, tr)
				continue
			}
			if got := site.got(tr); !reflect.DeepEqual(got, want) {
				t.Errorf("%s(%q) = %#v, want %#v", site.name, tr, got, want)
			}
		}
	}
}

// The scope sentence a round's handoff states follows its trigger, and an
// acceptance-fix round's names the approved criteria as fixed.
func TestRoundTextScopeByTrigger(t *testing.T) {
	want := map[v1alpha1.IntentRunTrigger]string{
		tReview:  "address only authorised review feedback",
		tCommand: "address only authorised review feedback",
		tChecks:  "fix only the failing checks",
		tVerify: "make the failing acceptance criteria pass; the criteria are fixed and approved; do not " +
			"special-case maul's checks",
	}
	for _, tr := range v1alpha1.IntentRunTriggers {
		got := roundText(reviseRunOf(tr), "abc", roundContext{}, "feedback", "patch")
		if !strings.Contains(got, want[tr]) {
			t.Errorf("roundText(%q) = %q, want it to state %q", tr, got, want[tr])
		}
	}
}

// verifyFixRounds counts the distinct acceptance-fix rounds of the approved
// plan revision, a failed round included, a round a human's review
// superseded before it launched not.
func TestVerifyFixRounds(t *testing.T) {
	run := func(round, attempt, rev int32, tr v1alpha1.IntentRunTrigger, phase v1alpha1.RunPhase,
		outcome string) *v1alpha1.IntentRun {
		r := reviseRunOf(tr)
		r.Spec.Round, r.Spec.Attempt, r.Spec.Inputs.PlanRevision = round, attempt, rev
		r.Status.Phase, r.Status.Outcome = phase, outcome
		return r
	}
	approval := func(rev int32) *v1alpha1.Intent {
		return &v1alpha1.Intent{Status: v1alpha1.IntentStatus{Approval: &v1alpha1.IntentApproval{PlanRevision: rev}}}
	}
	tests := []struct {
		name string
		in   *v1alpha1.Intent
		runs []*v1alpha1.IntentRun
		want int32
	}{
		{"no approval", &v1alpha1.Intent{}, []*v1alpha1.IntentRun{
			run(1, 1, 1, tVerify, v1alpha1.RunComplete, "ok")}, 0},
		{"one round two attempts", approval(2), []*v1alpha1.IntentRun{
			run(3, 1, 2, tVerify, v1alpha1.RunFailed, "runtime_error"),
			run(3, 2, 2, tVerify, v1alpha1.RunComplete, "ok")}, 1},
		{"a failed round counts", approval(2), []*v1alpha1.IntentRun{
			run(3, 1, 2, tVerify, v1alpha1.RunFailed, "runtime_error"),
			run(4, 1, 2, tVerify, v1alpha1.RunComplete, "ok")}, 2},
		{"an earlier plan revision's rounds do not", approval(2), []*v1alpha1.IntentRun{
			run(1, 1, 1, tVerify, v1alpha1.RunComplete, "ok"),
			run(4, 1, 2, tVerify, v1alpha1.RunComplete, "ok")}, 1},
		{"superseded by a human does not", approval(2), []*v1alpha1.IntentRun{
			run(3, 1, 2, tVerify, v1alpha1.RunFailed, OutcomeSupersededByHuman),
			run(4, 1, 2, tVerify, v1alpha1.RunComplete, "ok")}, 1},
		{"other triggers do not", approval(1), []*v1alpha1.IntentRun{
			run(1, 1, 1, tReview, v1alpha1.RunComplete, "ok"),
			run(2, 1, 1, tChecks, v1alpha1.RunComplete, "ok"),
			run(3, 1, 1, tCommand, v1alpha1.RunComplete, "ok")}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&pass{in: tt.in, runs: tt.runs}).verifyFixRounds(); got != tt.want {
				t.Errorf("verifyFixRounds = %d, want %d", got, tt.want)
			}
		})
	}
}

// No code compares a run's trigger with == or != any more: each site
// switches over every trigger (or asks a helper that does), so the table
// above is the whole of the trigger's meaning.
func TestNoTriggerEqualityComparisons(t *testing.T) {
	dirs := []string{".", "../../intentview", "../../usagerollup"}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, file, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				be, ok := n.(*ast.BinaryExpr)
				if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
					return true
				}
				if isTriggerConst(be.X) || isTriggerConst(be.Y) {
					t.Errorf("%s: a trigger compared with %s; switch over every trigger instead",
						fset.Position(be.Pos()), be.Op)
				}
				return true
			})
		}
	}
}

func isTriggerConst(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && strings.HasPrefix(sel.Sel.Name, "IntentRunTrigger")
}
