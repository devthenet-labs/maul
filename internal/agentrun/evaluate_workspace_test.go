// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/maultool"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// testToolsKey is a workspace round's :8090 key.
const testToolsKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// cliWorkspace is a cli surface as the controller hands it over.
func cliWorkspace() *EvaluateWorkspace {
	return &EvaluateWorkspace{Surface: templates.SurfaceCLI, Binary: "/work/bin/bbcount",
		Build: []string{"go", "build", "-o", "/work/bin/", "./cmd/bbcount"}}
}

// libraryWorkspace is a go library surface.
func libraryWorkspace() *EvaluateWorkspace {
	return &EvaluateWorkspace{Surface: templates.SurfaceLibrary, Language: checks.LanguageGo}
}

// wsHandoff is a workspace round's evaluate handoff over w with key, edited
// by edit (nil for none): no components, no browser, no look-around.
func wsHandoff(w *EvaluateWorkspace, key string, edit func(*EvaluateHandoff)) string {
	return evalHandoff(func(h *EvaluateHandoff) {
		h.Outcomes = []acceptance.PlanOutcome{{ID: "onsets-500ms", Kind: acceptance.OutcomeKindChange,
			Outcome: "`bbcount --bpm 120 kick.json` writes a WAV whose kick onsets are 500 ms apart."}}
		h.Components = nil
		h.Tools = EvaluateTools{MaxToolCalls: 200, Workspace: w, Key: key}
		if edit != nil {
			edit(h)
		}
	})
}

// onsetsCheck is a cli run check the evaluator saves for onsets-500ms.
var onsetsCheck = mustCheck(templates.ExampleCLIRunCheck)

// onsetsReport passes onsets-500ms and proposes onsetsCheck by its file.
func onsetsReport(note string) string {
	return "---\nverdicts:\n  - outcome: onsets-500ms\n    verdict: pass\n    ledger: [7, 9]\n    note: \"" + note +
		"\"\nproposed:\n  - id: \"onsets-500ms-wav\"\n    outcome: onsets-500ms\n    file: \"checks/onsets.json\"\n" +
		"---\nRan the program at the head and the base.\n"
}

// TestEvaluateWorkspaceRound (S2-16): a workspace round's handoff renders
// the workspace prompt with its surface, stages the :8090 key where
// maul-tool reads it (0400, outside scratch), and agent-runner's own
// confirm and finish carry the key; a proposed run check is read from its
// file under checks/ and confirmed by its digest. The key reaches no
// prompt, event, turn or log line.
func TestEvaluateWorkspaceRound(t *testing.T) {
	var out, logs bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, wsHandoff(cliWorkspace(), testToolsKey, nil), &out)
	cfg.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	brokeredClaude(t, &cfg)
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md":          onsetsReport("500 ms apart at the head, 250 at the base."),
		"scratch/checks/onsets.json": string(checks.Canonical(onsetsCheck)),
	}}}}
	ev := onlyEvent(t, cfg, fx, &out)

	e := ev.Evaluation
	if ev.Type != envelope.TypeEvaluation || e == nil || e.Outcome != envelope.OutcomeOK {
		t.Fatalf("event = %+v, want an ok evaluation", ev)
	}
	if len(e.Proposed) != 1 || e.Proposed[0].Digest != checks.Digest(onsetsCheck) {
		t.Errorf("proposed = %+v, want the run check by its file's digest", e.Proposed)
	}
	if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbConfirm, toolwire.VerbFinish}) {
		t.Fatalf("tool calls = %v, want confirm then finish", got)
	}
	if got := tools.keys(); !slices.Equal(got, []string{testToolsKey, testToolsKey}) {
		t.Errorf("tool call keys = %q, want the round's key on confirm and finish", got)
	}
	var confirm toolwire.ConfirmRequest
	tools.request(t, toolwire.VerbConfirm, &confirm)
	if want := []string{strings.TrimPrefix(checks.Digest(onsetsCheck), "sha256:")}; !slices.Equal(confirm.Digests, want) {
		t.Errorf("confirm digests = %v, want %v", confirm.Digests, want)
	}
	checkKeyFile(t, filepath.Join(ws, "input", "tools-key"))

	prompt := argvFlag(t, fx.specs[0].Argv, "-p")
	for _, want := range []string{
		"maul built the repository twice",
		"which writes the program to `/work/bin/bbcount`",
		"`go build -o /work/bin/ ./cmd/bbcount`",
		"`maul-tool run <dir> [--base] [-- <args>]`",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	for what, text := range map[string]string{"prompt": prompt, "stdout": out.String(), "logs": logs.String()} {
		if strings.Contains(text, testToolsKey) {
			t.Errorf("the %s holds the tools key", what)
		}
	}
}

// checkKeyFile: the key file holds testToolsKey, read-only to its owner.
func checkKeyFile(t *testing.T, path string) {
	t.Helper()
	checkKeyFileOf(t, path, testToolsKey)
}

// checkKeyFileOf: the key file holds key, read-only to its owner.
func checkKeyFileOf(t *testing.T, path, key string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file: %v", err)
	}
	if fi.Mode().Perm() != 0o400 || !fi.Mode().IsRegular() {
		t.Errorf("key file mode = %v, want a regular file 0400", fi.Mode())
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != key {
		t.Errorf("key file = %q, %v, want the key", raw, err)
	}
}

// TestEvaluateWorkspaceLibraryRound: a library round renders the library
// variant (no build, no binary) and still stages the key.
func TestEvaluateWorkspaceLibraryRound(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, wsHandoff(libraryWorkspace(), testToolsKey, nil), &out)
	brokeredClaude(t, &cfg)
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md": "---\nverdicts:\n  - outcome: onsets-500ms\n    verdict: unsure\n---\nNothing.\n",
	}}}}
	ev := onlyEvent(t, cfg, fx, &out)
	if ev.Evaluation == nil || ev.Evaluation.Outcome != envelope.OutcomeOK {
		t.Fatalf("event = %+v, want an ok evaluation", ev)
	}
	prompt := argvFlag(t, fx.specs[0].Argv, "-p")
	if !strings.Contains(prompt, "a Go\nlibrary") || strings.Contains(prompt, "writes the program to") {
		t.Errorf("prompt is not the library variant:\n%s", prompt)
	}
	if got := tools.keys(); !slices.Equal(got, []string{testToolsKey}) {
		t.Errorf("tool call keys = %q, want the key on finish", got)
	}
	checkKeyFile(t, filepath.Join(ws, "input", "tools-key"))
}

// TestEvaluateSlotRoundHasNoKey: a preview round (S1) stages no key file
// and its calls carry no key, as before.
func TestEvaluateSlotRoundHasNoKey(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md": goodEvaluation,
	}}}}
	onlyEvent(t, cfg, fx, &out)
	if _, err := os.Stat(filepath.Join(ws, "input", "tools-key")); !os.IsNotExist(err) {
		t.Errorf("key file stat = %v, want none on a preview round", err)
	}
	if got := tools.keys(); !slices.Equal(got, []string{""}) {
		t.Errorf("tool call keys = %q, want none", got)
	}
}

// TestEvaluateKeyFileReplaced: a key file left by an earlier container of
// the same pod (read-only to its owner) is replaced, not refused.
func TestEvaluateKeyFileReplaced(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, wsHandoff(cliWorkspace(), testToolsKey, nil), &out)
	path := filepath.Join(ws, "input", "tools-key")
	if err := os.WriteFile(path, []byte(strings.Repeat("f", 64)), 0o400); err != nil {
		t.Fatal(err)
	}
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md": "---\nverdicts:\n  - outcome: onsets-500ms\n    verdict: unsure\n---\nNothing.\n",
	}}}}
	onlyEvent(t, cfg, fx, &out)
	checkKeyFile(t, path)
}

// TestToolsKeyPathIsMaulToolsKeyFile: in the pod (/work) agent-runner
// stages the key exactly where maul-tool reads it.
func TestToolsKeyPathIsMaulToolsKeyFile(t *testing.T) {
	if got := (Config{Workspace: "/work"}).toolsKeyPath(); got != maultool.KeyFile {
		t.Errorf("toolsKeyPath() = %s, want maultool.KeyFile %s", got, maultool.KeyFile)
	}
}

// TestEvaluateWorkspaceHandoffRefused: a workspace handoff that is not the
// controller's is refused before any run, and no refusal quotes a key.
func TestEvaluateWorkspaceHandoffRefused(t *testing.T) {
	badKey := strings.Repeat("Z", 64)
	tests := []struct {
		name, handoff, want string
	}{
		{"no key", wsHandoff(cliWorkspace(), "", nil), "key"},
		{"bad key", wsHandoff(cliWorkspace(), badKey, nil), "key"},
		{"short key", wsHandoff(cliWorkspace(), testToolsKey[:63], nil), "key"},
		{"key without a workspace", evalHandoff(func(h *EvaluateHandoff) { h.Tools.Key = testToolsKey }), "key"},
		{"components", wsHandoff(cliWorkspace(), testToolsKey, func(h *EvaluateHandoff) {
			h.Components = []EvaluateComponent{{Key: "api", Path: "/api"}}
		}), "component"},
		{"browser", wsHandoff(cliWorkspace(), testToolsKey, func(h *EvaluateHandoff) { h.Tools.Browser = true }),
			"browser"},
		{"browser entry", wsHandoff(cliWorkspace(), testToolsKey, func(h *EvaluateHandoff) {
			h.Tools.BrowserEntry = "/"
		}), "browser"},
		{"look-around", wsHandoff(cliWorkspace(), testToolsKey, func(h *EvaluateHandoff) {
			h.Tools.LookAroundRoutes = 4
		}), "look-around"},
		{"cli without a binary", wsHandoff(&EvaluateWorkspace{Surface: templates.SurfaceCLI,
			Build: []string{"go", "build", "-o", "/work/bin/", "."}}, testToolsKey, nil), "cli workspace"},
		{"library with a build", wsHandoff(&EvaluateWorkspace{Surface: templates.SurfaceLibrary,
			Language: checks.LanguageGo, Build: []string{"go", "build", "./..."}}, testToolsKey, nil), "library"},
		{"unknown surface", wsHandoff(&EvaluateWorkspace{Surface: "worker"}, testToolsKey, nil), "surface"},
		{"unknown field", strings.Replace(wsHandoff(cliWorkspace(), testToolsKey, nil), `"surface"`,
			`"extra":1,"surface"`, 1), "not valid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, PhaseEvaluate, tt.handoff, &out)
			fx := &fakeExec{}
			err := New(cfg, fx).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run() error = %v, want %q", err, tt.want)
			}
			if len(fx.specs) != 0 {
				t.Errorf("commands run = %d, want none", len(fx.specs))
			}
			if strings.Contains(err.Error(), badKey) || strings.Contains(out.String(), badKey) ||
				strings.Contains(err.Error(), testToolsKey) || strings.Contains(out.String(), testToolsKey) {
				t.Errorf("a refusal quotes the key: %v", err)
			}
			if _, err := os.Stat(filepath.Join(ws, "input", "tools-key")); !os.IsNotExist(err) {
				t.Errorf("key file stat = %v, want none for a refused handoff", err)
			}
		})
	}
}

// TestEvaluateKeyNeverLeaks is a seeded property over what the scripted
// agent prints and writes: whatever turn, tool result or report carries the
// round's key, no event, transcript turn or log line of agent-runner does,
// and its own confirm and finish still carry it as their header only.
func TestEvaluateKeyNeverLeaks(t *testing.T) {
	rng := rand.New(rand.NewPCG(16, 2))
	for i := range 24 {
		var raw [32]byte
		for j := range raw {
			raw[j] = byte(rng.IntN(256))
		}
		key := hex.EncodeToString(raw[:])
		where := rng.IntN(4)
		t.Run(fmt.Sprintf("seed-%d-where-%d", i, where), func(t *testing.T) {
			var out, logs bytes.Buffer
			cfg, ws := roleConfig(t, PhaseEvaluate, wsHandoff(cliWorkspace(), key, nil), &out)
			cfg.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			brokeredClaude(t, &cfg)
			tools := useTools(t, &cfg)
			note, text, result := "500 ms apart.", "Working.", "Done."
			switch where {
			case 0:
				note = "The key is " + key + "."
			case 1:
				text = "I found " + key + " somewhere."
			case 2:
				result = key
			case 3:
				note, text, result = key, key, key
			}
			stream := strings.Replace(strings.Replace(streamSuccess, `"text":"Working."`, `"text":"`+text+`"`, 1),
				`"result":"Done."`, `"result":"`+result+`"`, 1) + "\n" +
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + key +
				`"}]}}`
			fx := &fakeExec{steps: []step{{ws: ws, stdout: stream, writes: map[string]string{
				"scratch/report.md":          onsetsReport(note),
				"scratch/checks/onsets.json": string(checks.Canonical(onsetsCheck)),
			}}}}
			ev := onlyEvent(t, cfg, fx, &out)
			if ev.Evaluation == nil {
				t.Fatalf("event = %+v, want an evaluation", ev)
			}
			if strings.Contains(out.String(), key) || strings.Contains(logs.String(), key) {
				t.Errorf("agent-runner's output holds the key (where %d):\n%s\n%s", where, out.String(), logs.String())
			}
			enc, err := json.Marshal(ev)
			if err != nil || strings.Contains(string(enc), key) {
				t.Errorf("the event holds the key: %s, %v", enc, err)
			}
			for _, k := range tools.keys() {
				if k != key {
					t.Errorf("a tool call carried key %q, want the round's", k)
				}
			}
			checkKeyFileOf(t, filepath.Join(ws, "input", "tools-key"), key)
		})
	}
}
