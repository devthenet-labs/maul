// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

func parseCheck(t *testing.T, src string) checks.Check {
	t.Helper()
	c, err := checks.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHTTPCheckRun(t *testing.T) {
	tests := []struct {
		name     string
		score    func(n int) int
		want     toolwire.RunResult
		attempts int
		failStep int // the step that failed on the last attempt, -1 for none
	}{
		{"in range", func(int) int { return 5 }, toolwire.RunPass, 1, -1},
		{"out of range", func(int) int { return 99 }, toolwire.RunFail, 2, 1},
		{"fails, then passes", func(n int) int { return []int{99, 5}[n] }, toolwire.RunQuarantined, 2, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := fakeAPI(t, tc.score)
			h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
			rec := h.relay.runCheck(context.Background(), parseCheck(t, scoreCheck), toolwire.ModeReplay)
			if rec.Result != tc.want || len(rec.Attempts) != tc.attempts {
				t.Fatalf("got %s in %d attempts (%+v), want %s in %d", rec.Result, len(rec.Attempts), rec, tc.want,
					tc.attempts)
			}
			if rec.CheckID != "score.shows" || rec.Digest != bareDigest(t, scoreCheck) {
				t.Errorf("record names %q %q", rec.CheckID, rec.Digest)
			}
			if toolwire.DeriveResult(rec.Attempts) != rec.Result {
				t.Errorf("result %s is not its attempts'", rec.Result)
			}
			last := rec.Attempts[len(rec.Attempts)-1]
			failed := -1
			for _, s := range last.Steps {
				if s.Result != toolwire.ResultPass {
					failed = s.Index
					if s.Detail == "" {
						t.Errorf("a failed step gives maul's detail")
					}
				}
			}
			if failed != tc.failStep {
				t.Errorf("failed step %d, want %d (%+v)", failed, tc.failStep, last.Steps)
			}
			tokens := map[string]bool{}
			for _, a := range rec.Attempts {
				if tokens[a.Run] {
					t.Errorf("token %q reused across attempts", a.Run)
				}
				tokens[a.Run] = true
			}
			for _, r := range api.requests() {
				if r.Method == http.MethodPost && !strings.Contains(r.Body, "tok") {
					t.Errorf("POST %q carries no run token", r.Body)
				}
			}
		})
	}
}

func TestHTTPCheckStopsAtTheFailingStep(t *testing.T) {
	api := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
	rec := h.relay.runCheck(context.Background(), parseCheck(t, scoreCheck), toolwire.ModeReplay)
	if rec.Result != toolwire.RunFail {
		t.Fatalf("got %+v", rec)
	}
	for _, a := range rec.Attempts {
		if len(a.Steps) != 1 || a.Steps[0].Result != toolwire.ResultFail || !strings.Contains(a.Steps[0].Detail, "500") {
			t.Errorf("attempt %+v: want one failed step naming the status", a)
		}
	}
	if n := len(api.requests()); n != 2 {
		t.Errorf("%d requests, want one per attempt", n)
	}
}

func TestHTTPCheckNoResponseFails(t *testing.T) {
	h := newHarness(t, baseConfig(), routes{}, nil)
	rec := h.relay.runCheck(context.Background(), parseCheck(t, scoreCheck), toolwire.ModeReplay)
	if rec.Result != toolwire.RunFail || !strings.Contains(rec.Attempts[0].Steps[0].Detail, "no response") {
		t.Fatalf("got %+v: an application that does not answer fails", rec)
	}
}

const execCheck = `{"id":"ws.echo","outcome":"live","form":"exec","exec":{
 "argv":["python3","client.py","{{run}}"],"files":[{"path":"client.py","content":"print('{{run}}')\n"}],
 "expect":{"exit":0,"stdoutContains":"{{run}}-ok","stdoutJSON":{"/players":{"$includes":{"name":"{{run}}-alice"}}}}}}`

func TestExecCheckRun(t *testing.T) {
	tests := []struct {
		name string
		run  func(req *toolwire.ExecRequest, n int) (toolexec.Result, error)
		want toolwire.RunResult
	}{
		{"passes", func(req *toolwire.ExecRequest, _ int) (toolexec.Result, error) {
			tok := req.Argv[2]
			if string(req.Files[0].Content) != "print('"+tok+"')\n" || req.Files[0].Name != "client.py" {
				return toolexec.Result{Exit: 9, Output: "files not substituted"}, nil
			}
			return toolexec.Result{Exit: 0, Output: `{"players":[{"name":"` + tok + `-alice"}],"x":"` + tok + `-ok"}`}, nil
		}, toolwire.RunPass},
		{"wrong exit", func(req *toolwire.ExecRequest, _ int) (toolexec.Result, error) {
			return toolexec.Result{Exit: 1, Output: `{"players":[{"name":"` + req.Argv[2] + `-alice"}]} ` +
				req.Argv[2] + `-ok`}, nil
		}, toolwire.RunFail},
		{"another run's data", func(*toolwire.ExecRequest, int) (toolexec.Result, error) {
			return toolexec.Result{Exit: 0, Output: `{"players":[{"name":"tok1-alice"}],"x":"tok1-ok"}`}, nil
		}, toolwire.RunFail},
		{"the shim refuses", func(*toolwire.ExecRequest, int) (toolexec.Result, error) {
			return toolexec.Result{Refused: toolwire.RefusedInvalid, Reason: "bad"}, nil
		}, toolwire.RunError},
		{"no shim", func(*toolwire.ExecRequest, int) (toolexec.Result, error) {
			return toolexec.Result{}, errors.New("dial unix: no such file")
		}, toolwire.RunError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ex := &fakeExec{run: tc.run}
			h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
			// Skip tok1 so "another run's data" names a token no attempt has.
			h.relay.token()
			rec := h.relay.runCheck(context.Background(), parseCheck(t, execCheck), toolwire.ModeReplay)
			if rec.Result != tc.want {
				t.Fatalf("got %+v, want %s", rec, tc.want)
			}
			for i, req := range ex.reqs {
				if req.Argv[2] != rec.Attempts[i].Run {
					t.Errorf("attempt %d ran argv %q under token %q", i, req.Argv, rec.Attempts[i].Run)
				}
			}
		})
	}
}

func TestExecCheckWithoutASandbox(t *testing.T) {
	h := newHarness(t, baseConfig(), routes{}, nil)
	rec := h.relay.runCheck(context.Background(), parseCheck(t, execCheck), toolwire.ModeReplay)
	if rec.Result != toolwire.RunError {
		t.Fatalf("got %+v: an exec check with no sandbox is a check error", rec)
	}
}

func TestBrowserCheckWithoutABrowserIsAnError(t *testing.T) {
	const browser = `{"id":"row","outcome":"row","form":"browser","browser":{"steps":[{"goto":"/"},` +
		`{"click":{"role":"button","name":"Add"}},` +
		`{"expect":{"assert":"text","locator":{"role":"row","hasText":"{{run}}"},"text":{"contains":"{{run}}"}}}]}}`
	h := newHarness(t, baseConfig(), routes{}, nil)
	rec := h.relay.runCheck(context.Background(), parseCheck(t, browser), toolwire.ModeReplay)
	if rec.Result != toolwire.RunError || !strings.Contains(rec.Attempts[0].Steps[0].Detail, "unavailable") {
		t.Fatalf("got %+v", rec)
	}
}
