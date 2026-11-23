// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/devthenet-labs/maul/internal/transcript"
)

// TestTranscriptTurnMatchesTypeScript keeps the transcript's wire turn and
// the SPA's TranscriptTurn in lockstep: the two names differ, so the intents
// pin, which matches by name, does not see them.
func TestTranscriptTurnMatchesTypeScript(t *testing.T) {
	src, err := os.ReadFile("ui/src/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	want, ok := tsInterfaces(t, string(src))["TranscriptTurn"]
	if !ok {
		t.Fatal("ui/src/types.ts has no interface TranscriptTurn")
	}
	if got := jsonFields(reflect.TypeFor[Turn]()); !slices.Equal(got, want) {
		t.Errorf("Turn: Go fields %v, TypeScript fields %v", got, want)
	}
}

// TestWireTurnCarriesCallAndParent: both transcript views ship a turn's
// call and parent, and only as plain ids: a stored record is read back as
// it was written, so the status server holds them to the recorder's rule
// again.
func TestWireTurnCarriesCallAndParent(t *testing.T) {
	in := transcript.Turn{Seq: 2, Role: transcript.RoleAssistant, Kind: transcript.KindToolUse, Tool: "Bash",
		Call: "c_1", Parent: "t_agent"}
	for name, w := range map[string]Turn{"findings": wireTurn(in), "intents": plainTurn(in)} {
		if w.Call != "c_1" || w.Parent != "t_agent" {
			t.Errorf("%s: call %q parent %q, want c_1 t_agent", name, w.Call, w.Parent)
		}
	}
	bad := in
	bad.Call, bad.Parent = "c 1", "\u202et_agent"
	for name, w := range map[string]Turn{"findings": wireTurn(bad), "intents": plainTurn(bad)} {
		if w.Call != "" || w.Parent != "" {
			t.Errorf("%s: call %q parent %q, want both dropped", name, w.Call, w.Parent)
		}
	}
}
