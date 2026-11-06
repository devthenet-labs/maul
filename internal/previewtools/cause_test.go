// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// compileThen answers a run program's compile steps with compile and its
// run with run; builds pass.
func compileThen(compile, run toolexec.Result, compileErr error) func(*toolexec.Request, int) (toolexec.Result,
	error) {
	return func(req *toolexec.Request, _ int) (toolexec.Result, error) {
		switch {
		case req.Kind == toolexec.KindBuild:
			return toolexec.Result{}, nil
		case req.Argv[0] == "go" && len(req.Argv) > 1 && req.Argv[1] == "build":
			return compile, compileErr
		case req.Argv[0] == "go":
			return toolexec.Result{}, nil // go work init
		}
		return run, nil
	}
}

// TestBaseErrorCauses is the relay's half of the base-error rule (SPEC-S2
// S2-20, decided: zero false passes outranks the confirmed rate). Every
// error a base run can end in is listed with the cause the ledger records:
// only a compile the compiler refused to its end (exit 1, clean), a tree a
// run program cannot be built against and a base that did not build carry
// one (acceptance reads those as "no base comparison"); a timeout, processes
// left behind, cut output, a bind or connection error, a compile killed,
// cut short or refused, and a sandbox that did not answer carry none, and
// so give no base evidence.
func TestBaseErrorCauses(t *testing.T) {
	ok := toolexec.Result{Output: "ok"}
	tests := []struct {
		name  string
		check string
		do    func(*toolexec.Request, int) (toolexec.Result, error)
		tree  map[string]string
		want  toolwire.StepCause
	}{
		{name: "a compile error", check: cliRunCheck,
			do:   compileThen(toolexec.Result{Exit: 1, Output: "./main.go:5:9: undefined: bb.Parse"}, ok, nil),
			want: toolwire.CauseCompile},
		{name: "a compile killed by a signal", check: cliRunCheck,
			do: compileThen(toolexec.Result{Exit: -1, Output: "signal: killed"}, ok, nil)},
		{name: "a compile that timed out", check: cliRunCheck,
			do: compileThen(toolexec.Result{Exit: 1, TimedOut: true}, ok, nil)},
		{name: "a compile that left processes", check: cliRunCheck,
			do: compileThen(toolexec.Result{Exit: 1, Unclean: true}, ok, nil)},
		{name: "a compile the sandbox refused", check: cliRunCheck,
			do: compileThen(toolexec.Result{Exit: -1, Refused: "busy"}, ok, nil)},
		{name: "a compile the sandbox did not answer", check: cliRunCheck,
			do: compileThen(toolexec.Result{}, ok, errors.New("no shim"))},
		{name: "go not found", check: cliRunCheck,
			do: compileThen(toolexec.Result{Exit: 127}, ok, nil)},
		{name: "a vendored tree", check: cliRunCheck, do: compileThen(toolexec.Result{}, ok, nil),
			tree: map[string]string{"vendor/modules.txt": "# x\n"}, want: toolwire.CauseVendoredTree},
		{name: "a replace outside the tree", check: cliRunCheck, do: compileThen(toolexec.Result{}, ok, nil),
			tree: map[string]string{"go.mod": "module example.com/bb\n\ngo 1.24\n\nreplace example.com/x => ../x\n"},
			want: toolwire.CauseReplaceOutsideTree},
		{name: "no module", check: cliRunCheck, do: compileThen(toolexec.Result{}, ok, nil),
			tree: map[string]string{"go.mod": "go 1.24\n"}, want: toolwire.CauseNoModule},
		{name: "a run that timed out", check: cliRunCheck,
			do: compileThen(toolexec.Result{}, toolexec.Result{TimedOut: true, Exit: -1}, nil)},
		{name: "a run that left processes", check: cliRunCheck,
			do: compileThen(toolexec.Result{}, toolexec.Result{Unclean: true, Output: "ok"}, nil)},
		{name: "a run whose output was cut", check: cliRunCheck,
			do: compileThen(toolexec.Result{}, toolexec.Result{Truncated: true, Output: "x"}, nil)},
		{name: "a run that named a connection error", check: cliRunCheck,
			do: compileThen(toolexec.Result{}, toolexec.Result{NetError: true, Exit: 1, Output: "refused"}, nil)},
		{name: "an exec that timed out", check: cliExecCheck,
			do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
				if req.Kind == toolexec.KindBuild {
					return toolexec.Result{}, nil
				}
				return toolexec.Result{TimedOut: true, Exit: -1}, nil
			}},
		{name: "an exec whose output was cut", check: cliExecCheck,
			do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
				if req.Kind == toolexec.KindBuild {
					return toolexec.Result{}, nil
				}
				return toolexec.Result{Truncated: true, Output: "count=2"}, nil
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newWorkspaceHarness(t, wsConfig(), &fakeSandbox{}, &fakeSandbox{do: tt.do})
			if tt.tree != nil {
				writeTree(t, filepath.Join(h.relay.trees, string(toolwire.SideBase)), tt.tree)
			}
			_, resp := h.dryRunKeyed(tt.check, toolwire.SideBase)
			run := resp.Observed.CheckRun
			if resp.Refused != "" || run == nil || run.Result != toolwire.RunError {
				t.Fatalf("not an error run: %+v", resp)
			}
			if got := run.Cause(); got != tt.want {
				t.Errorf("cause = %q, want %q (%+v)", got, tt.want, run.Attempts)
			}
		})
	}
}

// TestBaseUnbuiltRunCause: a base run on a base that did not build records
// that cause, so it reads as unbuildable wherever it is read.
func TestBaseUnbuiltRunCause(t *testing.T) {
	c, err := checks.Parse([]byte(cliExecCheck))
	if err != nil {
		t.Fatal(err)
	}
	if got := baseUnbuiltRun(c, toolwire.ModeReplay).Cause(); got != toolwire.CauseBaseUnbuildable {
		t.Errorf("cause = %q, want %q", got, toolwire.CauseBaseUnbuildable)
	}
}

// TestUnreadableTreeHasNoCause: a tree maul cannot open at all is maul's
// own failure, not the tree's: no cause.
func TestUnreadableTreeHasNoCause(t *testing.T) {
	h := newWorkspaceHarness(t, wsConfig(), &fakeSandbox{}, &fakeSandbox{do: compileThen(toolexec.Result{},
		toolexec.Result{Output: "ok"}, nil)})
	if err := os.RemoveAll(filepath.Join(h.relay.trees, string(toolwire.SideBase))); err != nil {
		t.Fatal(err)
	}
	_, resp := h.dryRunKeyed(cliRunCheck, toolwire.SideBase)
	run := resp.Observed.CheckRun
	if run == nil || run.Result != toolwire.RunError || run.Cause() != "" {
		t.Errorf("an unreadable tree: %+v", resp)
	}
}
