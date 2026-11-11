// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestExecTimeoutIsTheShims pins the bound a check may claim to the bound
// the exec shim holds a program to: a check admitted with more time than
// the sandbox gives would be cut short of what it asked for.
func TestExecTimeoutIsTheShims(t *testing.T) {
	if checks.MaxExecTimeout != toolexec.Timeout {
		t.Fatalf("checks.MaxExecTimeout = %s, toolexec.Timeout = %s: a check may claim time the shim cuts",
			checks.MaxExecTimeout, toolexec.Timeout)
	}
}

// timeLimitDetail is the words every step cut at a time limit carries.
const timeLimitDetail = "killed at its time limit"

// TestSlotExecCutIsATimeoutError: a slot exec check the shim killed at its
// time limit is a check error naming the time limit, never a fail (by
// assertion or bare) and never a pass, whatever it printed before the cut:
// a program cut short judged nothing. So is one whose own timeout ran out
// before the shim answered.
func TestSlotExecCutIsATimeoutError(t *testing.T) {
	marked := "%M upset-stake: the stake was 1, expected more than 1\n"
	tests := []struct {
		name    string
		check   string
		run     func(ctx context.Context, req *toolwire.ExecRequest) (toolexec.Result, error)
		wantCut bool
	}{
		{"the shim cut it, nothing printed", markedExecCheck,
			func(context.Context, *toolwire.ExecRequest) (toolexec.Result, error) {
				return toolexec.Result{Exit: -1, TimedOut: true, Output: toolexec.TimeoutMarker}, nil
			}, true},
		{"the shim cut it after its marker", markedExecCheck,
			func(_ context.Context, req *toolwire.ExecRequest) (toolexec.Result, error) {
				return toolexec.Result{Exit: -1, TimedOut: true, Output: marked + toolexec.TimeoutMarker}, nil
			}, true},
		{"the shim cut it after the expected output", markedExecCheck,
			func(context.Context, *toolwire.ExecRequest) (toolexec.Result, error) {
				return toolexec.Result{Exit: -1, TimedOut: true, Output: "DONE\n" + toolexec.TimeoutMarker}, nil
			}, true},
		{"its own timeout ran out first", timedSlotExecCheck,
			func(ctx context.Context, _ *toolwire.ExecRequest) (toolexec.Result, error) {
				<-ctx.Done()
				return toolexec.Result{}, ctx.Err()
			}, true},
		{"the shim did not answer", markedExecCheck,
			func(context.Context, *toolwire.ExecRequest) (toolexec.Result, error) {
				return toolexec.Result{}, context.Canceled
			}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ex := &fakeExec{runCtx: func(ctx context.Context, req *toolwire.ExecRequest, _ int) (toolexec.Result,
				error) {
				res, err := tc.run(ctx, req)
				res.Output = strings.ReplaceAll(res.Output, "%M", attemptMarker(t, req))
				return res, err
			}}
			h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
			rec := h.relay.runCheck(context.Background(), parseCheck(t, tc.check), toolwire.ModeDryRun)
			if rec.Result != toolwire.RunError {
				t.Fatalf("got %s, want error (%+v)", rec.Result, rec)
			}
			if _, ok := rec.AssertionStep(); ok {
				t.Errorf("a cut run is red by assertion: %+v", rec)
			}
			for _, a := range rec.Attempts {
				if len(a.Steps) != 1 || a.Steps[0].Result != toolwire.ResultError {
					t.Fatalf("attempt %+v: want one error step", a)
				}
				if got := strings.Contains(a.Steps[0].Detail, timeLimitDetail); got != tc.wantCut {
					t.Errorf("detail %q names the time limit: %v, want %v", a.Steps[0].Detail, got, tc.wantCut)
				}
			}
		})
	}
}

// timedSlotExecCheck is markedExecCheck with a timeout of its own.
const timedSlotExecCheck = `{"id":"ws.timed","outcome":"live","form":"exec","exec":{
 "argv":["python3","client.py","{{run}}"],"files":[{"path":"client.py","content":"print('{{marker}} x: y')\n"}],
 "timeout":"50ms","expect":{"exit":0,"stdoutContains":"DONE"}}}`

// timedRunCheck is cliRunCheck with a timeout of its own.
const timedRunCheck = `{"id":"count.timedrun","outcome":"count","form":"run","run":{"language":"go","files":[
 {"path":"main.go","content":"package main\n// runs {{bin}} on {{tree}}; {{marker}}\nfunc main() {}\n"}],
 "args":["{{run}}"],"timeout":"200ms","expect":{"exit":0,"stdoutContains":"ok"}}}`

// TestRunCheckTimeoutGoesToTheShim: a run check's timeout bounds its
// program, which the shim holds it to and reports cut (TimedOut) as the
// exec form's is: the program's run request carries it, never its compile,
// and a cut is an error naming the time limit, never a fail or a pass.
func TestRunCheckTimeoutGoesToTheShim(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  toolexec.Result
		want toolwire.RunResult
	}{
		{"ends in time", toolexec.Result{Output: "ok"}, toolwire.RunPass},
		{"cut by the shim", toolexec.Result{Exit: -1, TimedOut: true, Output: "ok" + toolexec.TimeoutMarker},
			toolwire.RunError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := &fakeSandbox{do: compileThen(toolexec.Result{}, tc.res, nil)}
			h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
			_, resp := h.dryRunKeyed(timedRunCheck, "")
			run := resp.Observed.CheckRun
			if resp.Refused != "" || run == nil || run.Result != tc.want {
				t.Fatalf("got %+v, want %s", resp, tc.want)
			}
			if tc.want == toolwire.RunError &&
				!strings.Contains(run.Attempts[0].Steps[0].Detail, timeLimitDetail) {
				t.Errorf("detail %q does not name the time limit", run.Attempts[0].Steps[0].Detail)
			}
			runs := 0
			for _, req := range head.nonBuilds() {
				switch {
				case req.Argv[0] == "./"+consumerBinary:
					runs++
					if req.TimeoutMs != (200 * time.Millisecond).Milliseconds() {
						t.Errorf("the program's run carries timeout %dms, want 200", req.TimeoutMs)
					}
				case req.TimeoutMs != 0:
					t.Errorf("compile step %q carries the check's timeout %dms", req.Argv, req.TimeoutMs)
				}
			}
			if runs == 0 {
				t.Fatalf("the program never ran: %+v", head.nonBuilds())
			}
		})
	}
}
