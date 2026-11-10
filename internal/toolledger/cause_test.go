// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestCausesMatchAPI pins every toolwire.StepCause to its v1alpha1 and
// acceptance twins: BaseRuns records a run's cause by conversion, and
// intent-controller hands it to Decide by conversion, so a cause one of
// them lacks would be refused on the status write or read as no cause (no
// base comparison lost, never gained).
func TestCausesMatchAPI(t *testing.T) {
	for _, c := range []struct {
		wire toolwire.StepCause
		api  v1alpha1.CheckRunCause
		acc  httpstep.RunCause
	}{
		{toolwire.CauseCompile, v1alpha1.CheckRunCauseCompile, httpstep.CauseCompile},
		{toolwire.CauseVendoredTree, v1alpha1.CheckRunCauseVendoredTree, httpstep.CauseVendoredTree},
		{toolwire.CauseReplaceOutsideTree, v1alpha1.CheckRunCauseReplaceOutsideTree,
			httpstep.CauseReplaceOutsideTree},
		{toolwire.CauseNoModule, v1alpha1.CheckRunCauseNoModule, httpstep.CauseNoModule},
		{toolwire.CauseBaseUnbuildable, v1alpha1.CheckRunCauseBaseUnbuildable, httpstep.CauseBaseUnbuildable},
	} {
		if string(c.wire) != string(c.api) || string(c.wire) != string(c.acc) {
			t.Errorf("toolwire %q, v1alpha1 %q, acceptance %q", c.wire, c.api, c.acc)
		}
		if !c.wire.Known() || !c.acc.NoComparison() {
			t.Errorf("%q is not a known cause on both sides", c.wire)
		}
	}
}

// baseReplay is a relay replay entry at the base whose attempts errored
// with cause, or failed when fail is set.
func baseReplay(seq int64, cause toolwire.StepCause, fail bool) toolwire.LedgerEntry {
	result := toolwire.ResultError
	if fail {
		result = toolwire.ResultFail
	}
	rec := &toolwire.CheckRunRecord{Digest: strings.Repeat("a", 64), CheckID: "count.run",
		Mode: toolwire.ModeReplay}
	for _, run := range []string{"tok1", "tok2"} {
		step := toolwire.StepRecord{Result: result, Side: toolwire.SideBase, Detail: "x"}
		if !fail {
			step.Cause = cause
		}
		rec.Attempts = append(rec.Attempts, toolwire.Attempt{Run: run, Result: result,
			Steps: []toolwire.StepRecord{step}})
	}
	rec.Result = toolwire.DeriveResult(rec.Attempts)
	return toolwire.LedgerEntry{Seq: seq, TS: time.Unix(1, 0), Kind: toolwire.KindRelay, Verb: toolwire.VerbCheckRun,
		Side: toolwire.SideBase, Args: json.RawMessage(`{}`), CheckRun: rec}
}

// TestBaseRunsRecordTheCause: a base run that errored for a recorded cause
// keeps it on status.base.runs; one with no cause, and a run that failed,
// carry none; a head run never does.
func TestBaseRunsRecordTheCause(t *testing.T) {
	head := baseReplay(6, toolwire.CauseCompile, false)
	head.Side = ""
	for i := range head.CheckRun.Attempts {
		head.CheckRun.Attempts[i].Steps[0].Side = ""
	}
	entries := []toolwire.LedgerEntry{
		baseReplay(3, toolwire.CauseCompile, false),
		baseReplay(4, "", false),
		baseReplay(5, toolwire.CauseCompile, true),
		head,
	}
	got := BaseRuns(entries)
	want := []v1alpha1.CheckRunCause{v1alpha1.CheckRunCauseCompile, "", ""}
	if len(got) != len(want) {
		t.Fatalf("base runs = %+v", got)
	}
	for i, r := range got {
		if r.Cause != want[i] {
			t.Errorf("run %d (seq %d): cause %q, want %q", i, r.Seq, r.Cause, want[i])
		}
	}
	for _, r := range ReplayRuns(entries) {
		if r.Cause != "" {
			t.Errorf("a head run carries cause %q", r.Cause)
		}
	}
}
