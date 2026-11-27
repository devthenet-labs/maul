// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"encoding/json"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestCitedCallOwnLabel: the comment labels a cited call the evaluator's
// own page script or program (owner decision 1) by toolledger.OwnCalls: a
// run, and a slot exec, from the verified index's verb alone, whether or
// not maul kept the call's ledger line; an agents exec unless its kept line
// shows exactly the built binary with no files (with no line it is labelled
// its own); a browser call unless its kept line shows a verb other than
// eval (with no line, or its args dropped to fit, it is labelled its own).
func TestCitedCallOwnLabel(t *testing.T) {
	const bin = "/work/bin/tool"
	sum := strings.Repeat("b", 64)
	line := func(seq int64, verb toolwire.Verb, req any) toolwire.LedgerEntry {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return toolwire.LedgerEntry{Seq: seq, Kind: toolwire.KindAgent, Verb: verb, Args: raw, OutputSHA256: sum}
	}
	for _, tt := range []struct {
		name   string
		agents bool
		verb   v1alpha1.LedgerVerb
		line   *toolwire.LedgerEntry // at seq 1; nil: maul kept no line
		want   bool
	}{
		{"slot run, no line", false, v1alpha1.LedgerVerbRun, nil, true},
		{"slot exec, no line", false, v1alpha1.LedgerVerbExec, nil, true},
		{"slot exec, line of another verb", false, v1alpha1.LedgerVerbExec,
			new(line(1, toolwire.VerbHTTP, toolwire.HTTPRequest{Method: "GET", Path: "/"})), true},
		{"agents run, no line", true, v1alpha1.LedgerVerbRun, nil, true},
		{"agents exec of the binary", true, v1alpha1.LedgerVerbExec,
			new(line(1, toolwire.VerbExec, toolwire.ExecRequest{Argv: []string{bin, "--version"}})), false},
		{"agents exec of {{bin}}", true, v1alpha1.LedgerVerbExec,
			new(line(1, toolwire.VerbExec, toolwire.ExecRequest{Argv: []string{"{{bin}}"}})), false},
		{"agents exec with files", true, v1alpha1.LedgerVerbExec,
			new(line(1, toolwire.VerbExec, toolwire.ExecRequest{Argv: []string{bin},
				Files: []toolwire.File{{Name: "in.txt", Content: []byte("x")}}})), true},
		{"agents exec of a shell", true, v1alpha1.LedgerVerbExec,
			new(line(1, toolwire.VerbExec, toolwire.ExecRequest{Argv: []string{"sh", "-c", "echo hi"}})), true},
		{"agents exec, no line", true, v1alpha1.LedgerVerbExec, nil, true},
		{"browser eval", false, v1alpha1.LedgerVerbBrowser,
			new(line(1, toolwire.VerbBrowser, toolwire.BrowserRequest{Verb: "eval", Args: []string{"() => 1"}})), true},
		{"browser click", false, v1alpha1.LedgerVerbBrowser,
			new(line(1, toolwire.VerbBrowser, toolwire.BrowserRequest{Verb: "click", Args: []string{"e2"}})), false},
		{"browser, no line", false, v1alpha1.LedgerVerbBrowser, nil, true},
		{"browser, args dropped to fit", false, v1alpha1.LedgerVerbBrowser,
			&toolwire.LedgerEntry{Seq: 1, Kind: toolwire.KindAgent, Verb: toolwire.VerbBrowser,
				Args: json.RawMessage(`{"maul":"args dropped to fit"}`), OutputSHA256: sum}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rc := roundCalls{index: []v1alpha1.LedgerIndexEntry{{Seq: 1, Kind: v1alpha1.LedgerKindAgent, Verb: tt.verb,
				ResultSHA256: sum}}, lines: map[int64]toolwire.LedgerEntry{}, agents: tt.agents}
			if tt.agents {
				rc.binary = bin
			}
			if tt.line != nil {
				rc.lines[1] = *tt.line
			}
			if got := rc.call(1).Own; got != tt.want {
				t.Errorf("call(1).Own = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCitedCallOwnLabelAfterOwnCalls (FC 3): on an agents round a cited
// bare exec of the built binary is labelled the evaluator's own once an
// earlier call at the same side ran something of its own there, since
// /work is writable; with no such call it is not.
func TestCitedCallOwnLabelAfterOwnCalls(t *testing.T) {
	const bin = "/work/bin/tool"
	sum := strings.Repeat("b", 64)
	line := func(seq int64, req toolwire.ExecRequest) toolwire.LedgerEntry {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return toolwire.LedgerEntry{Seq: seq, Kind: toolwire.KindAgent, Verb: toolwire.VerbExec, Args: raw,
			OutputSHA256: sum}
	}
	for _, tt := range []struct {
		name  string
		first toolwire.ExecRequest
		want  bool
	}{
		{"after a bare run of the binary", toolwire.ExecRequest{Argv: []string{"{{bin}}"}}, false},
		{"after a shell writing /work", toolwire.ExecRequest{Argv: []string{"sh", "-c", "cp /tmp/x " + bin}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exec := v1alpha1.LedgerIndexEntry{Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbExec,
				ResultSHA256: sum}
			first, second := exec, exec
			first.Seq, second.Seq = 1, 2
			rc := roundCalls{index: []v1alpha1.LedgerIndexEntry{first, second}, agents: true, binary: bin,
				lines: map[int64]toolwire.LedgerEntry{1: line(1, tt.first),
					2: line(2, toolwire.ExecRequest{Argv: []string{"{{bin}}", "--version"}})}}
			if got := rc.call(2).Own; got != tt.want {
				t.Errorf("call(2).Own = %v, want %v", got, tt.want)
			}
		})
	}
}
