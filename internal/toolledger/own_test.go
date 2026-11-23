// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"encoding/json"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestOwnCall: a cited call is the evaluator's own page script or program
// (owner decision 1) when it is a browser eval, any Go program (run), any
// exec on a slot round (the toolbox runs only what it is sent) and, on an
// agents round, any exec that is not exactly the built binary with no files
// of its own (workspaceExec runs any argv plus the files the evaluator
// supplies). Only a browser call and an agents exec need their arguments:
// with none (maul did not keep the line) a run or slot exec is still its
// own, an agents exec is labelled its own (unlabelled needs proof), and a
// browser call is not (nothing shows it was an eval).
func TestOwnCall(t *testing.T) {
	const bin = "/work/bin/tool"
	exec := func(argv []string, files int) json.RawMessage {
		req := toolwire.ExecRequest{Argv: argv}
		for range files {
			req.Files = append(req.Files, toolwire.File{Name: "a.go", Content: []byte("package main")})
		}
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	browser := func(verb string, args ...string) json.RawMessage {
		raw, err := json.Marshal(toolwire.BrowserRequest{Verb: verb, Args: args})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, tt := range []struct {
		name   string
		verb   v1alpha1.LedgerVerb
		args   json.RawMessage
		agents bool
		binary string
		want   bool
	}{
		{"slot run", v1alpha1.LedgerVerbRun, nil, false, "", true},
		{"slot exec, no line", v1alpha1.LedgerVerbExec, nil, false, "", true},
		{"slot exec", v1alpha1.LedgerVerbExec, exec([]string{"python3", "-c", "print(1)"}, 0), false, "", true},
		{"agents run, no line", v1alpha1.LedgerVerbRun, nil, true, bin, true},
		{"agents exec of {{bin}}", v1alpha1.LedgerVerbExec, exec([]string{"{{bin}}", "--help"}, 0), true, bin, false},
		{"agents exec of the binary's path", v1alpha1.LedgerVerbExec, exec([]string{bin}, 0), true, bin, false},
		{"agents exec of {{bin}} with files", v1alpha1.LedgerVerbExec, exec([]string{"{{bin}}"}, 1), true, bin, true},
		{"agents exec of the path with files", v1alpha1.LedgerVerbExec, exec([]string{bin, "a.go"}, 2), true, bin,
			true},
		{"agents exec through a shell", v1alpha1.LedgerVerbExec, exec([]string{"sh", "-c", bin}, 0), true, bin, true},
		{"agents exec of another binary", v1alpha1.LedgerVerbExec, exec([]string{bin + "2"}, 0), true, bin, true},
		{"agents exec of a path prefix", v1alpha1.LedgerVerbExec, exec([]string{"/work/bin/too"}, 0), true, bin,
			true},
		{"agents exec, no line", v1alpha1.LedgerVerbExec, nil, true, bin, true},
		{"agents exec, args unreadable", v1alpha1.LedgerVerbExec, json.RawMessage(`{"argv":7}`), true, bin, true},
		{"agents exec, no argv", v1alpha1.LedgerVerbExec, exec(nil, 0), true, bin, true},
		{"library exec of {{bin}}", v1alpha1.LedgerVerbExec, exec([]string{"{{bin}}"}, 0), true, "", true},
		{"browser eval", v1alpha1.LedgerVerbBrowser, browser("eval", "() => 1"), false, "", true},
		{"browser click", v1alpha1.LedgerVerbBrowser, browser("click", "e3"), false, "", false},
		{"browser, no line", v1alpha1.LedgerVerbBrowser, nil, false, "", false},
		{"http", v1alpha1.LedgerVerbHTTP, nil, false, "", false},
		{"files", v1alpha1.LedgerVerbFiles, nil, true, bin, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := OwnCall(tt.verb, tt.args, tt.agents, tt.binary); got != tt.want {
				t.Errorf("OwnCall(%s, %s, agents %v, %q) = %v, want %v", tt.verb, tt.args, tt.agents, tt.binary,
					got, tt.want)
			}
		})
	}
}
