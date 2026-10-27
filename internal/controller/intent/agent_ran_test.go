// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/runnerguard"
)

// agentRanOutcomes is every outcome a run records, and whether a run that
// ended on it had an agent that ran: the one rule for "a round where an
// agent tried a fix" (agentRan).
var agentRanOutcomes = map[string]bool{
	// The agent ran and reported.
	"ok": true, "no_defect": true, "commit_failed": true, "not_built": true, "report_invalid": true,
	"report_missing": true, "timeout": true, "budget_exceeded": true, "changeset_too_large": true,
	"changeset_rejected": true, "dependency_refused": true,
	// The agent ran; what it made could not be pushed or was lost.
	"branch_exists": true, "hold_expired": true, "push_refused": true, "evicted": true,
	// No agent ran: maul's or the cluster's trouble, or a human cancelled.
	"": false, "runtime_error": false, "image_incompatible": false, "aborted": false, "image_required": false,
	"launch_refused": false, "input_unavailable": false, "no_usable_feedback": false, "unschedulable": false,
	"superseded_by_human": false, "stopped_by_human": false,
	// Decided by whether the Job was created: before launch, or at push.
	"head_moved": false,
}

// TestAgentRanIsExhaustive: every run outcome constant, the envelope's and
// intent-controller's own, is classified, so a new outcome cannot slip
// into "an agent tried a fix" (or out of it) unnoticed.
func TestAgentRanIsExhaustive(t *testing.T) {
	var files []string
	local, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range local {
		if !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	files = append(files, filepath.Join("..", "..", "envelope", "envelope.go"))
	found := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Outcome") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, _ := strconv.Unquote(lit.Value)
				found++
				if _, ok := agentRanOutcomes[value]; !ok {
					t.Errorf("%s (%q, %s) is not classified by agentRan's table", name.Name, value, f)
				}
			}
			return true
		})
	}
	if found < 25 {
		t.Fatalf("found %d outcome constants; the scan is broken", found)
	}
}

// TestAgentRan: each outcome as the table says, a head move by whether the
// Job existed, and a sandbox refusal never.
func TestAgentRan(t *testing.T) {
	for outcome, want := range agentRanOutcomes {
		for _, launched := range []bool{false, true} {
			run := &v1alpha1.IntentRun{Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunFailed, Outcome: outcome}}
			if launched {
				run.Status.JobRef = &v1alpha1.JobReference{Name: "job"}
			}
			w := want || (outcome == OutcomeHeadMoved && launched)
			if got := agentRan(run); got != w {
				t.Errorf("agentRan(%q, launched %v) = %v, want %v", outcome, launched, got, w)
			}
		}
	}
	refused := &v1alpha1.IntentRun{Status: v1alpha1.IntentRunStatus{Phase: v1alpha1.RunFailed,
		Outcome: "runtime_error", JobRef: &v1alpha1.JobReference{Name: "job"}}}
	meta.SetStatusCondition(&refused.Status.Conditions, runnerguard.RefusedCondition(1))
	refused.Status.Outcome = "ok"
	if agentRan(refused) {
		t.Error("a run the sandbox probe refused counted as an agent that ran")
	}
}
