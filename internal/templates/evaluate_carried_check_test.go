// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestEvaluatePromptCarriedCheckComparesByType (gate 4, the #414 caveat):
// render-library-100's saved check of repeat-others-kept compared the raw
// fields as `string(q.ID) != string(p.ID)`, which reads a nil and an empty
// json.RawMessage alike, so gate 3's planted copy (an absent ID turned into
// an empty one, whose encoding fails) still passed it. Every evaluate
// prompt now asks the evaluator to compare a carried-over promise in the
// values' own types (a program of its own in a workspace, the JSON type of
// an answer in a preview), never through text that collapses nil, absent
// and empty, and to look for that wrong build. Against a false fail, it
// keeps #414's rule: a nil against an empty value no use tells apart is no
// failure, so it is never judged by `== nil` or DeepEqual alone.
func TestEvaluatePromptCarriedCheckComparesByType(t *testing.T) {
	shared := []string{
		"a kept value turned from nil, zero or absent into empty, or the reverse",
	}
	workspace := []string{
		"When a program of yours compares a copy with its original, compare what using the result gives, in its " +
			"own types:",
		"its encoding against the original's, as it is written out, once what the outcome says changes is set alike " +
			"in both (for a Go value of a type with JSON tags, `json.Marshal` of each: the same bytes, and an error " +
			"from both or neither), beside each field's value",
		"Never compare only through text or a comparison that shows a nil, an absent and an empty value alike " +
			"(`string(x)`, `bytes.Equal`, `fmt.Sprint`, `%v`): a copy that turns one into the other passes it.",
		"Give it originals with those fields set, zero, empty and absent",
		"print both with `%#v` where they differ",
		"A nil against an empty value that no use tells apart is no failure (**Judge what is carried over**), so " +
			"never judge one by `== nil` or `reflect.DeepEqual` alone.",
	}
	slot := []string{
		"For a field the outcome says is carried over, compare it with the original in its own JSON type: " +
			"`null`, `\"\"`, `0`, `[]` and a missing key are different answers.",
	}
	for name, tt := range map[string]struct {
		p    EvaluatePrompt
		want []string
		not  []string
	}{
		"preview": {testEvaluatePrompt(), append(shared, slot...), workspace[:1]},
		"cli":     {testWorkspacePrompt(testCLIWorkspace), append(shared, workspace...), slot[:1]},
		"library": {testWorkspacePrompt(testLibraryWorkspace), append(shared, workspace...), slot[:1]},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RenderEvaluatePrompt(tt.p)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(flat(got), want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
			for _, not := range tt.not {
				if strings.Contains(flat(got), not) {
					t.Errorf("prompt holds the other placement's %q", not)
				}
			}
		})
	}
}

// gate3Pattern is the shape of beatbattle-render's synth.Pattern that the
// carried-over outcomes of gates 3 and 4 were about: server-assigned raw
// fields beside plain ones, and track lists.
type gate3Pattern struct {
	ID        json.RawMessage `json:"id"`
	Name      string          `json:"name"`
	Kick      []bool          `json:"kick"`
	Hat       []bool          `json:"hat,omitempty"`
	Votes     json.RawMessage `json:"votes"`
	CreatedAt json.RawMessage `json:"createdAt"`
}

// encodedAlike is the comparison the workspace list asks a check of a
// carried-over promise to make: the same encoding, and an error from both
// or neither.
func encodedAlike(a, b gate3Pattern) bool {
	ea, errA := json.Marshal(a)
	eb, errB := json.Marshal(b)
	return (errA == nil) == (errB == nil) && bytes.Equal(ea, eb)
}

// stringsAlike is how render-library-100's saved check rep-others compared
// the raw fields (`string(q.ID) != string(p.ID)`).
func stringsAlike(a, b gate3Pattern) bool {
	return string(a.ID) == string(b.ID) && string(a.Votes) == string(b.Votes) &&
		string(a.CreatedAt) == string(b.CreatedAt)
}

// TestCarriedCheckAdviceHoldsBothWays: what the prompt asks a carried-over
// check to compare catches gate 3's planted copy, which the string
// comparison of gate 4's saved check passes, and fails no correct copy: a
// bytes.Clone copy, and an omitempty list made anew (a nil against an empty
// value that nothing using it tells apart), both encode as the original.
func TestCarriedCheckAdviceHoldsBothWays(t *testing.T) {
	orig := gate3Pattern{Name: "nm", Kick: make([]bool, 16)} // ID, Votes, CreatedAt and Hat absent
	planted := orig                                          // gate 3's G6 plant
	planted.ID = append(json.RawMessage{}, orig.ID...)
	planted.Votes = append(json.RawMessage{}, orig.Votes...)
	planted.CreatedAt = append(json.RawMessage{}, orig.CreatedAt...)
	cloned := orig
	cloned.ID, cloned.Votes = bytes.Clone(orig.ID), bytes.Clone(orig.Votes)
	cloned.CreatedAt = bytes.Clone(orig.CreatedAt)
	emptied := orig
	emptied.Hat = make([]bool, len(orig.Hat))
	if !stringsAlike(orig, planted) {
		t.Fatal("the string comparison tells the planted copy apart; the caveat no longer holds")
	}
	if encodedAlike(orig, planted) {
		t.Error("the encoding comparison passes gate 3's planted copy")
	}
	for name, c := range map[string]gate3Pattern{"bytes.Clone": cloned, "an omitempty list made anew": emptied} {
		if !encodedAlike(orig, c) {
			t.Errorf("the encoding comparison fails a correct copy (%s)", name)
		}
	}
}
