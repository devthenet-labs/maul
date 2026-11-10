// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"bytes"
	"encoding/json"
	stdimage "image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The fixtures preview-controller's tool pod tests use, as this package
// needs them.

const testCheckBody = `{"id":"items.body","outcome":"items","form":"http","http":{"target":"api","steps":[` +
	`{"request":{"method":"POST","path":"/api/echo","json":{"n":"{{run}}"}},"expect":{"bodyContains":"{{run}}"}}]}}`

// testOutcomes are an approved plan's outcomes, a manual one included.
func testOutcomes() []v1alpha1.AcceptanceOutcome {
	return []v1alpha1.AcceptanceOutcome{
		{ID: "items", Outcome: "the items list shows new items", Kind: v1alpha1.OutcomeKindChange},
		{ID: "feel", Outcome: "it feels right", Kind: v1alpha1.OutcomeKindChange, Manual: true, Reason: "taste"},
	}
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := stdimage.NewNRGBA(stdimage.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// toolLedger is what the tools container writes for a full round against
// the saved check: its readiness wait, the replay, the agent's green dry
// run of a new check and a screenshot, the runner's confirm and the relay's
// confirmation, the finish, the screenshot as evidence, and the summary.
func toolLedger(t *testing.T, shot []byte) ([]byte, string) {
	t.Helper()
	var out bytes.Buffer
	w := toolwire.NewWriter(&out)
	at := time.Date(2026, 10, 1, 12, 1, 0, 0, time.UTC)
	add := func(e toolwire.LedgerEntry) toolwire.LedgerEntry {
		t.Helper()
		e.TS = at
		got, err := w.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	readiness, _ := json.Marshal([]httpstep.TargetWait{{Target: "api", WaitMs: 40, Attempts: 1},
		{Target: "web", WaitMs: 40, Attempts: 1}})
	add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbHTTP,
		Args: json.RawMessage(`{"readiness":{"targets":["api","web"]}}`), Output: string(readiness)})
	saved, err := checks.Parse([]byte(testCheckBody))
	if err != nil {
		t.Fatal(err)
	}
	pass := []toolwire.Attempt{{Run: "0123456789abcdef", Result: toolwire.ResultPass}}
	add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbCheckRun,
		Args: json.RawMessage(`{}`), CheckRun: &toolwire.CheckRunRecord{
			Digest: strings.TrimPrefix(checks.Digest(saved), "sha256:"), CheckID: saved.ID, Mode: toolwire.ModeReplay,
			Result: toolwire.RunPass, Attempts: pass}})
	proposed, err := checks.Parse([]byte(strings.Replace(testCheckBody, `"items.body"`, `"items.echo"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(checks.Digest(proposed), "sha256:")
	args, _ := json.Marshal(toolwire.CheckRunRequest{Spec: checks.Canonical(proposed)})
	add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbCheckRun, Args: args,
		CheckRun: &toolwire.CheckRunRecord{Digest: digest, CheckID: proposed.ID, Mode: toolwire.ModeDryRun,
			Result: toolwire.RunPass, Attempts: pass}})
	browse := add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbBrowser,
		Args: json.RawMessage(`{"verb":"screenshot"}`), Output: "shot",
		Artifacts: []toolwire.Artifact{{Name: "page.png", SHA256: sha256Hex(shot), Bytes: int64(len(shot))}}})
	add(toolwire.LedgerEntry{Kind: toolwire.KindRunner, Verb: toolwire.VerbConfirm,
		Args: json.RawMessage(`{"digests":["` + digest + `"]}`)})
	add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbConfirm,
		Args: json.RawMessage(`{"digest":"` + digest + `"}`), CheckRun: &toolwire.CheckRunRecord{Digest: digest,
			CheckID: proposed.ID, Mode: toolwire.ModeConfirm, Result: toolwire.RunPass,
			Attempts: []toolwire.Attempt{{Run: "fedcba9876543210", Result: toolwire.ResultPass}}}})
	add(toolwire.LedgerEntry{Kind: toolwire.KindRunner, Verb: toolwire.VerbFinish, Args: json.RawMessage(`{}`)})
	if err := w.Evidence(browse.Seq, shot); err != nil {
		t.Fatal(err)
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), sum.TerminationMessage()
}
