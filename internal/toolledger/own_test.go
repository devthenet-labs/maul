// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

const ownBin = "/work/bin/tool"

func ownExec(t *testing.T, argv []string, files int) json.RawMessage {
	t.Helper()
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

func ownBrowser(t *testing.T, verb string, args ...string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(toolwire.BrowserRequest{Verb: verb, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ownCall is one call's label alone: an index of that one entry.
func ownCall(verb v1alpha1.LedgerVerb, args json.RawMessage, agents bool, binary string) bool {
	ix := []v1alpha1.LedgerIndexEntry{{Seq: 1, Kind: v1alpha1.LedgerKindAgent, Verb: verb}}
	return OwnCalls(ix, func(int32) json.RawMessage { return args }, agents, binary)[0]
}

// TestOwnCallsOneCall: a call is the evaluator's own page script or program
// (owner decision 1) when it is a browser eval, any Go program (run), any
// exec on a slot round (the toolbox runs only what it is sent) and, on an
// agents round, any exec that is not exactly the built binary with no files
// of its own (workspaceExec runs any argv plus the files the evaluator
// supplies). Only a browser call and an agents exec need their arguments,
// and both fail closed: with none (maul kept no line, or dropped its args to
// fit) an agents exec is labelled its own (unlabelled needs proof), and so
// is a browser call (unlabelled needs its line to show a verb other than
// eval). A run or slot exec is its own from the verb alone.
func TestOwnCallsOneCall(t *testing.T) {
	dropped := json.RawMessage(droppedArgs)
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
		{"slot exec", v1alpha1.LedgerVerbExec, ownExec(t, []string{"python3", "-c", "print(1)"}, 0), false, "", true},
		{"agents run, no line", v1alpha1.LedgerVerbRun, nil, true, ownBin, true},
		{"agents exec of {{bin}}", v1alpha1.LedgerVerbExec, ownExec(t, []string{"{{bin}}", "--help"}, 0), true, ownBin,
			false},
		{"agents exec of the binary's path", v1alpha1.LedgerVerbExec, ownExec(t, []string{ownBin}, 0), true, ownBin,
			false},
		{"agents exec of {{bin}} with files", v1alpha1.LedgerVerbExec, ownExec(t, []string{"{{bin}}"}, 1), true,
			ownBin, true},
		{"agents exec of the path with files", v1alpha1.LedgerVerbExec, ownExec(t, []string{ownBin, "a.go"}, 2), true,
			ownBin, true},
		{"agents exec through a shell", v1alpha1.LedgerVerbExec, ownExec(t, []string{"sh", "-c", ownBin}, 0), true,
			ownBin, true},
		{"agents exec of another binary", v1alpha1.LedgerVerbExec, ownExec(t, []string{ownBin + "2"}, 0), true, ownBin,
			true},
		{"agents exec of a path prefix", v1alpha1.LedgerVerbExec, ownExec(t, []string{"/work/bin/too"}, 0), true,
			ownBin, true},
		{"agents exec, no line", v1alpha1.LedgerVerbExec, nil, true, ownBin, true},
		{"agents exec, args dropped to fit", v1alpha1.LedgerVerbExec, dropped, true, ownBin, true},
		{"agents exec, args unreadable", v1alpha1.LedgerVerbExec, json.RawMessage(`{"argv":7}`), true, ownBin, true},
		{"agents exec, no argv", v1alpha1.LedgerVerbExec, ownExec(t, nil, 0), true, ownBin, true},
		{"library exec of {{bin}}", v1alpha1.LedgerVerbExec, ownExec(t, []string{"{{bin}}"}, 0), true, "", true},
		{"browser eval", v1alpha1.LedgerVerbBrowser, ownBrowser(t, "eval", "() => 1"), false, "", true},
		{"browser click", v1alpha1.LedgerVerbBrowser, ownBrowser(t, "click", "e3"), false, "", false},
		// Fail closed (FC 4): nothing shows the call was not an eval.
		{"browser, no line", v1alpha1.LedgerVerbBrowser, nil, false, "", true},
		{"browser, args dropped to fit", v1alpha1.LedgerVerbBrowser, dropped, false, "", true},
		{"browser, args unreadable", v1alpha1.LedgerVerbBrowser, json.RawMessage(`{"verb":7}`), false, "", true},
		{"browser, no verb", v1alpha1.LedgerVerbBrowser, json.RawMessage(`{}`), false, "", true},
		{"http", v1alpha1.LedgerVerbHTTP, nil, false, "", false},
		{"files", v1alpha1.LedgerVerbFiles, nil, true, ownBin, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownCall(tt.verb, tt.args, tt.agents, tt.binary); got != tt.want {
				t.Errorf("own(%s, %s, agents %v, %q) = %v, want %v", tt.verb, tt.args, tt.agents, tt.binary, got,
					tt.want)
			}
		})
	}
}

// ownEntry is one entry of an agents round's index, with the args its kept
// line holds (nil: none kept).
type ownEntry struct {
	kind    v1alpha1.LedgerKind
	verb    v1alpha1.LedgerVerb
	side    v1alpha1.TreeSide
	refused bool
	args    json.RawMessage
}

// TestOwnCallsAfterTheEvaluatorsOwn (FC 3): /work is writable, so once an
// earlier call at a side ran something of the evaluator's there (an exec
// labelled its own, files of its own, any run, or an exec maul kept no
// readable line of), a later bare exec of the built binary at that side may
// run what that call left (a replaced binary, a planted file) and is
// labelled the evaluator's own too. A refused call ran nothing, maul's own
// build is the binary, and the other side's sandbox is another volume.
func TestOwnCallsAfterTheEvaluatorsOwn(t *testing.T) {
	bin := ownExec(t, []string{"{{bin}}", "--version"}, 0)
	agent := func(verb v1alpha1.LedgerVerb, side v1alpha1.TreeSide, args json.RawMessage) ownEntry {
		return ownEntry{kind: v1alpha1.LedgerKindAgent, verb: verb, side: side, args: args}
	}
	build := ownEntry{kind: v1alpha1.LedgerKindRelay, verb: v1alpha1.LedgerVerbBuild}
	refusedShell := agent(v1alpha1.LedgerVerbExec, "", ownExec(t, []string{"sh", "-c", "cp x /work/bin/tool"}, 0))
	refusedShell.refused = true
	for _, tt := range []struct {
		name    string
		earlier []ownEntry
		want    bool // the final bare {{bin}} exec at the head
	}{
		{"alone", nil, false},
		{"after maul's builds", []ownEntry{build, build}, false},
		{"after a bare run of the binary", []ownEntry{agent(v1alpha1.LedgerVerbExec, "", bin)}, false},
		{"after files and logs", []ownEntry{agent(v1alpha1.LedgerVerbFiles, "", nil),
			agent(v1alpha1.LedgerVerbLogs, "", nil)}, false},
		{"after a shell exec at the head", []ownEntry{
			agent(v1alpha1.LedgerVerbExec, "", ownExec(t, []string{"sh", "-c", "cp x /work/bin/tool"}, 0))}, true},
		{"after the binary run with files", []ownEntry{agent(v1alpha1.LedgerVerbExec, "",
			ownExec(t, []string{"{{bin}}"}, 1))}, true},
		{"after a run at the head", []ownEntry{agent(v1alpha1.LedgerVerbRun, "", nil)}, true},
		{"after an exec with no kept line", []ownEntry{agent(v1alpha1.LedgerVerbExec, "", nil)}, true},
		{"after an exec whose args were dropped", []ownEntry{agent(v1alpha1.LedgerVerbExec, "",
			json.RawMessage(droppedArgs))}, true},
		{"after a tainted bare run", []ownEntry{agent(v1alpha1.LedgerVerbRun, "", nil),
			agent(v1alpha1.LedgerVerbExec, "", bin)}, true},
		{"after a refused shell exec", []ownEntry{refusedShell}, false},
		{"after a shell exec at the base", []ownEntry{agent(v1alpha1.LedgerVerbExec, v1alpha1.TreeBase,
			ownExec(t, []string{"sh", "-c", "true"}, 0))}, false},
		{"after a run at the base", []ownEntry{agent(v1alpha1.LedgerVerbRun, v1alpha1.TreeBase, nil)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entries := append(slices.Clone(tt.earlier), agent(v1alpha1.LedgerVerbExec, "", bin))
			got := ownLabels(entries, true, ownBin)
			if last := got[len(got)-1]; last != tt.want {
				t.Errorf("the last bare exec is own %v, want %v (labels %v)", last, tt.want, got)
			}
		})
	}
	// The base is tainted by its own side alone: a shell exec at the head
	// leaves a later bare exec at the base unlabelled, and one at the base
	// labels it.
	head := agent(v1alpha1.LedgerVerbExec, "", ownExec(t, []string{"sh", "-c", "true"}, 0))
	base := agent(v1alpha1.LedgerVerbExec, v1alpha1.TreeBase, ownExec(t, []string{"sh", "-c", "true"}, 0))
	baseBin := agent(v1alpha1.LedgerVerbExec, v1alpha1.TreeBase, bin)
	if got := ownLabels([]ownEntry{head, baseBin}, true, ownBin); got[1] {
		t.Errorf("a head exec labels a later base run of the binary: %v", got)
	}
	if got := ownLabels([]ownEntry{base, baseBin}, true, ownBin); !got[1] {
		t.Errorf("a base exec does not label a later base run of the binary: %v", got)
	}
	// A slot round is unchanged: every exec is its own already.
	if got := ownLabels([]ownEntry{agent(v1alpha1.LedgerVerbExec, "", bin)}, false, ""); !got[0] {
		t.Errorf("a slot exec is not its own: %v", got)
	}
}

// ownLabels is OwnCalls over entries numbered from 1.
func ownLabels(entries []ownEntry, agents bool, binary string) []bool {
	ix := make([]v1alpha1.LedgerIndexEntry, len(entries))
	args := map[int32]json.RawMessage{}
	for i, e := range entries {
		seq := int32(i + 1)
		ix[i] = v1alpha1.LedgerIndexEntry{Seq: seq, Kind: e.kind, Verb: e.verb, Side: e.side, Refused: e.refused,
			ResultSHA256: fmt.Sprintf("%064d", i)}
		args[seq] = e.args
	}
	return OwnCalls(ix, func(seq int32) json.RawMessage { return args[seq] }, agents, binary)
}
