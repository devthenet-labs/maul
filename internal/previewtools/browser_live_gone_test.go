// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build linux

package previewtools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// chromiumProcesses are the processes below this test whose command line
// holds flag: the shim's Chromium (--remote-debugging-pipe) or its
// renderers (--type=renderer), never another browser on the machine.
func chromiumProcesses(flag string) []int {
	entries, _ := os.ReadDir("/proc")
	parent, command := map[int]int{}, map[int]string{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...: comm may hold spaces and brackets.
		fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+1:])
		if len(fields) < 2 {
			continue
		}
		parent[pid], _ = strconv.Atoi(fields[1])
		cmdline, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		command[pid] = string(cmdline)
	}
	var out []int
	for pid := range parent {
		if !strings.Contains(command[pid], flag) {
			continue
		}
		for p := pid; p > 1; p = parent[p] {
			if p == os.Getpid() {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

// TestLiveBrowserGone (code review 2026-11-18, relay finding 9): with the
// pinned test runner and a real Chromium, a renderer or a browser killed
// under a patient check's steps (the node's memory pressure does the
// first to a correct page) is a check error in the runner's own words of
// this version, never a fail and never a fail by the text the matcher had
// seen so far; and a page whose own text holds those words, line for line
// as a call log writes them, still fails by its assertion. A runner bump
// that rewords either fails here.
func TestLiveBrowserGone(t *testing.T) {
	e := newLiveEnv(t, "correct")
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, "<!doctype html><html><body><pre data-testid=\"t\">x\nCall log:\n"+
			"- Target page, context or browser has been closed\n- Page crashed\n\n\n  - Page crashed\n</pre></body></html>")
	}))
	t.Cleanup(web.Close)
	for _, tc := range []struct {
		name, check, base, kill string
		result                  toolwire.AttemptResult
		asserted                bool
	}{
		{name: "the renderer is killed", check: reportedTextCheck, base: e.frontURL, kill: "--type=renderer",
			result: toolwire.ResultError},
		{name: "the browser is killed", check: reportedTextCheck, base: e.frontURL, kill: "--remote-debugging-pipe",
			result: toolwire.ResultError},
		{name: "the page's text names a closed and a crashed browser", check: reportedPageTextCheck, base: web.URL,
			result: toolwire.ResultFail, asserted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := playwright.Compile(*parseCheck(t, tc.check).Browser, playwright.Options{RunToken: "r1",
				Patient: tc.kill != ""})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			killed := make(chan int, 1)
			if tc.kill != "" {
				go func() {
					n := 0
					defer func() { killed <- n }()
					// Once the page has a renderer, give the check time to
					// reach a step, then kill.
					for len(chromiumProcesses("--type=renderer")) == 0 {
						select {
						case <-done:
							return
						case <-time.After(100 * time.Millisecond):
						}
					}
					select {
					case <-done:
						return
					case <-time.After(3 * time.Second):
					}
					for _, pid := range chromiumProcesses(tc.kill) {
						if p, err := os.FindProcess(pid); err == nil && p.Kill() == nil {
							n++
						}
					}
				}()
			}
			report, err := NodeRunner{Node: e.node, NodeDir: e.nodeDir, TmpDir: t.TempDir(), CDPURL: e.shimURL,
				KeyFile: e.keyFile, BaseURL: tc.base}.RunSpec(context.Background(), spec)
			close(done)
			if tc.kill != "" {
				if n := <-killed; n == 0 {
					t.Fatalf("killed no %s process below this test: the test did not run its case", tc.kill)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			steps := judgeReport(report, spec)
			last := steps[len(steps)-1]
			if last.Result != tc.result || last.Assertion != tc.asserted {
				t.Fatalf("ends %+v, want %s, assertion %v\n%s", last, tc.result, tc.asserted, report)
			}
			if tc.kill != "" && last.Detail != reasonBrowserGone {
				t.Errorf("detail %q, want %q", last.Detail, reasonBrowserGone)
			}
		})
	}
}
