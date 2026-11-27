// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// A browser call that failed showed nothing, so a verdict citing it counts
// for nothing ("evaluator observed": acceptance.ValidCite admits a browser
// entry only with exit 0). That rests on the pinned Playwright CLI exiting
// non-zero when an action or a page script fails, which FA measured on
// @playwright/cli 0.1.22 (playwright-core 1.64.1): a throwing or rejecting
// eval, a script that is not JavaScript, a click, fill or eval on a ref
// the snapshot does not hold and an unreachable goto each exit 1 under
// "### Error", and any command on a session that is not open exits 1
// saying so. TestLiveBrowserFailedCallsExitNonZero holds a renovate bump
// to it; TestBrowserFailedCallRecordsItsExit holds the relay to recording
// that exit as the call's.

// measuredEvalThrow is the pinned CLI's answer to an eval that throws, as
// FA measured it.
const measuredEvalThrow = "### Error\nError: boom from eval\n" +
	"    at eval (eval at <anonymous> (eval at evaluate (:368:30)), <anonymous>:1:16)\n" +
	"    at eval (eval at evaluate (:368:30), <anonymous>:4:50)\n"

// TestBrowserFailedCallRecordsItsExit: the relay records the CLI's exit
// status as the browser call's, in its observation and its ledger entry,
// so a failed action or page script is never an exit-0 entry a verdict
// could cite.
func TestBrowserFailedCallRecordsItsExit(t *testing.T) {
	h, _, _ := browserHarness(t, func(args []string, n int) (string, int) {
		switch args[0] {
		case "eval":
			return measuredEvalThrow, 1
		case "click":
			return "### Error\nError: Ref e9999 not found in the current page snapshot. Try capturing new " +
				"snapshot.\n", 1
		}
		return defaultScript(args, n)
	})
	for _, c := range []struct {
		verb string
		args []string
	}{{"eval", []string{"() => { throw new Error('boom from eval') }"}}, {"click", []string{"e9999"}}} {
		resp := browserCall(t, h, c.verb, c.args...)
		if resp.Refused != "" || resp.Observed == nil || resp.Observed.Exit == nil || *resp.Observed.Exit != 1 {
			t.Errorf("%s: %+v, want answered with exit 1", c.verb, resp)
		}
	}
	l := h.finish()
	var failed int
	for _, e := range l.Entries {
		if e.Kind == toolwire.KindAgent && e.Verb == toolwire.VerbBrowser {
			if e.Exit != 1 {
				t.Errorf("entry %d (%s) exit %d, want 1", e.Seq, e.Args, e.Exit)
			}
			failed++
		}
	}
	if failed != 2 {
		t.Errorf("%d browser entries, want 2", failed)
	}
}

// TestLiveBrowserFailedCallsExitNonZero: the pinned CLI, against a real
// Chromium, exits non-zero with its error section for every failure an
// evaluator's browser call can meet, and 0 for a page script that returns.
func TestLiveBrowserFailedCallsExitNonZero(t *testing.T) {
	e := newLiveEnv(t, "correct")
	ctx := context.Background()
	cli := e.cli(t, "maul-exit")
	t.Cleanup(func() { _, _ = cli.Run(context.Background(), []string{"close"}) })
	run := func(args ...string) CLIResult {
		t.Helper()
		out, err := cli.Run(ctx, args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}
	gone := e.cli(t, "maul-exit-never-opened")
	if out, err := gone.Run(ctx, []string{"eval", "() => 1"}); err != nil || out.Exit == 0 ||
		!strings.Contains(out.Output, lostSession) {
		t.Errorf("a session that is not open: exit %d (%v), want non-zero saying %q:\n%s", out.Exit, err,
			lostSession, out.Output)
	}
	if out := run("open", "--config="+e.cliConfig(t)); out.Exit != 0 {
		t.Fatalf("open: %s", out.Output)
	}
	if out := run("goto", e.frontURL+"/battles/1"); out.Exit != 0 {
		t.Fatalf("goto: %s", out.Output)
	}
	if out := run("eval", "() => 41 + 1"); out.Exit != 0 || !strings.Contains(out.Output, "42") {
		t.Errorf("a returning eval: exit %d\n%s", out.Exit, out.Output)
	}
	for _, args := range [][]string{
		{"eval", "() => { throw new Error('boom from eval') }"},
		{"eval", "() => Promise.reject(new Error('rejected'))"},
		{"eval", "() => document.querySelector('#nope').textContent"},
		{"eval", "this is not javascript ("},
		{"eval", "(el) => el.textContent", "e9999"},
		{"click", "e9999"},
		{"fill", "e9999", "x"},
		{"goto", "http://127.0.0.1:1/nothing"},
	} {
		out := run(args...)
		if out.Exit == 0 || !strings.Contains(stripANSI(out.Output), "### Error") {
			t.Errorf("%q: exit %d, want non-zero with an error section:\n%s", args, out.Exit, out.Output)
		}
	}
}
