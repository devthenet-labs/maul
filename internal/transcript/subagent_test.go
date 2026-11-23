// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package transcript

import (
	"strings"
	"testing"
)

// TestTurnCallAndParentRoundTrip: a turn names the tool call it is (call)
// and the call it ran under (parent), and both survive the stdout line.
func TestTurnCallAndParentRoundTrip(t *testing.T) {
	want := Turn{Seq: 4, Role: RoleAssistant, Kind: KindToolUse, Tool: "Bash", Text: "true",
		Call: "c_1", Parent: "t_agent"}
	line, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, ok := Decode([]byte(line))
	if !ok {
		t.Fatalf("Decode(%q) = not ok", line)
	}
	if got.Call != want.Call || got.Parent != want.Parent {
		t.Errorf("Decode = call %q parent %q, want %q %q", got.Call, got.Parent, want.Call, want.Parent)
	}
}

// TestTurnWithoutCallOrParentIsUnchanged: a turn of the main thread that
// is not a tool call encodes exactly as before the two fields existed, and
// a line written before them still decodes, so Version stays 1.
func TestTurnWithoutCallOrParentIsUnchanged(t *testing.T) {
	line, err := Encode(Turn{Seq: 1, Role: RoleAssistant, Kind: KindText, Text: "hi"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if want := Prefix + `{"v":1,"seq":1,"role":"assistant","kind":"text","text":"hi"}`; line != want {
		t.Errorf("Encode = %s, want %s", line, want)
	}
	old := Prefix + `{"v":1,"seq":2,"role":"assistant","kind":"tool_use","tool":"Bash","text":"go test"}`
	got, ok := Decode([]byte(old))
	if !ok || got.Tool != "Bash" || got.Call != "" || got.Parent != "" {
		t.Errorf("Decode(%s) = %+v, %v; want the old turn with no call or parent", old, got, ok)
	}
}

func TestCleanRef(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"toolu_01AbCdEfGhIjKlMnOpQrStUv", "toolu_01AbCdEfGhIjKlMnOpQrStUv"},
		{"t_agent", "t_agent"},
		{"tooluse-a1.b2", "tooluse-a1.b2"},
		{"", ""},
		{"has space", ""},
		{"line\nbreak", ""},
		{"\x1b[31mred", ""},
		{"ключ", ""},
		{"a/b", ""},
		{strings.Repeat("a", MaxRefBytes), strings.Repeat("a", MaxRefBytes)},
		{strings.Repeat("a", MaxRefBytes+1), ""},
	} {
		if got := CleanRef(tt.in); got != tt.want {
			t.Errorf("CleanRef(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestRecorderCleansCallAndParent: the recorder keeps a call or parent id
// only when it is a plain id, drops one a scrub would change (an id is
// never redacted in part), and counts the ids against the byte cap.
func TestRecorderCleansCallAndParent(t *testing.T) {
	secret := "sk-ant-secret-0123456789"
	var got []Turn
	rec := NewRecorder(Limits{}, []string{secret}, func(t Turn) { got = append(got, t) })
	rec.RecordAll([]Turn{
		{Role: RoleAssistant, Kind: KindToolUse, Tool: "Agent", Call: "t_agent"},
		{Role: RoleAssistant, Kind: KindToolUse, Tool: "Bash", Call: "c_1", Parent: "t_agent"},
		{Role: RoleAssistant, Kind: KindToolUse, Tool: "Bash", Call: "bad id", Parent: "\x1b[2Jx"},
		{Role: RoleAssistant, Kind: KindText, Text: "x", Parent: secret},
	})
	want := []struct{ call, parent string }{{"t_agent", ""}, {"c_1", "t_agent"}, {"", ""}, {"", ""}}
	if len(got) != len(want) {
		t.Fatalf("recorded %d turns, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Call != w.call || got[i].Parent != w.parent {
			t.Errorf("turn %d: call %q parent %q, want %q %q", i, got[i].Call, got[i].Parent, w.call, w.parent)
		}
	}
}

func TestRecorderCountsRefsAgainstTheByteCap(t *testing.T) {
	var got []Turn
	rec := NewRecorder(Limits{MaxTotalBytes: 100}, nil, func(t Turn) { got = append(got, t) })
	ref := strings.Repeat("a", MaxRefBytes)
	for range 3 {
		rec.Record(Turn{Role: RoleAssistant, Kind: KindToolUse, Tool: "Bash", Call: ref, Parent: ref})
	}
	if len(got) != 2 || !got[1].Truncated || got[1].Kind != KindNotice {
		t.Fatalf("recorded %+v; want one turn and then the byte cap's notice", got)
	}
}
