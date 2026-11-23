// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"bytes"
	"encoding/json"
	stdimage "image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The fixtures preview-controller's tool pod tests use, as this package
// needs them.

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

// toolLedger is what the tools container writes for a round: its
// readiness wait, the agent's http, exec and screenshot calls, its logs and
// events, the runner's finish, the screenshot as evidence, and the summary.
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
	add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbHTTP,
		Args: json.RawMessage(`{"method":"GET","path":"/api/items"}`), Output: "HTTP 200\n\n[]"})
	add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbExec,
		Args: json.RawMessage(`{"argv":["true"]}`)})
	browse := add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbBrowser,
		Args: json.RawMessage(`{"verb":"screenshot"}`), Output: "shot",
		Artifacts: []toolwire.Artifact{{Name: "page.png", SHA256: sha256Hex(shot), Bytes: int64(len(shot))}}})
	add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbLogs,
		Args: json.RawMessage(`{"component":"api"}`), Output: "started"})
	add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbEvents, Args: json.RawMessage(`{}`)})
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
