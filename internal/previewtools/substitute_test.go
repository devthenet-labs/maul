// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
)

// TestSubstitute: one pass, in one order, for every check path's
// arguments and expectations: the binary, the tree, then the run token
// through httpstep.Substitute, which still refuses any other placeholder,
// the marker included (a program file's alone: TestSubstituteProgram).
func TestSubstitute(t *testing.T) {
	cli := places{bin: "/work/bin/bb", tree: "/work/tree"}
	tests := []struct {
		name    string
		in      string
		p       places
		want    string
		refused bool
	}{
		{"all three", "{{bin}} {{tree}}/x {{run}}", cli, "/work/bin/bb /work/tree/x tok", false},
		{"run only", "id-{{run}}", places{}, "id-tok", false},
		{"no marker outside files", "{{marker}}", cli, "", true},
		{"bin in a library", "{{bin}}", places{tree: "/work/tree"}, "", true},
		{"tree in the slot", "{{tree}}", places{}, "", true},
		{"an unknown placeholder", "{{capture.x}}", cli, "", true},
		{"another unknown placeholder", "{{home}}", cli, "", true},
		{"unclosed", "{{run", cli, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := substitute(tt.in, "tok", tt.p)
			if tt.refused {
				if err == nil {
					t.Fatalf("substitute(%q) = %q, want a refusal", tt.in, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("substitute(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

// TestSubstituteProgram: a program file's content gets the exact
// placeholder tokens only (gate 5 G5R-B-LIB-1); any other "{{" is the
// program's own text, such as a Go nested composite literal, and stays as
// written. A known placeholder the place has no value for is still refused.
func TestSubstituteProgram(t *testing.T) {
	marker := checks.ExecMarker(strings.Repeat("a", 32))
	cli := places{bin: "/work/bin/bb", tree: "/work/tree"}
	lib := places{tree: "/work/tree"}
	const literal = "x := [][2]json.RawMessage{{p.ID, g.ID}, {p.Votes, g.Votes}, {p.CreatedAt, g.CreatedAt}}"
	tests := []struct {
		name    string
		in      string
		p       places
		want    string
		refused bool
	}{
		{"the gate's literal", literal, lib, literal, false},
		{"the literal beside every placeholder",
			literal + "\n{{bin}} {{tree}}/x {{run}} {{marker}} {{run}}", cli,
			literal + "\n/work/bin/bb /work/tree/x tok " + marker + " tok", false},
		{"an unknown placeholder is text", "{{home}} {{capture.x}} {{run", lib, "{{home}} {{capture.x}} {{run", false},
		{"bin in a library", "{{bin}}", lib, "", true},
		{"tree in the slot", "{{tree}}", places{}, "", true},
		{"bin in the slot", "{{bin}}", places{}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := substituteProgram(tt.in, "tok", marker, tt.p)
			if tt.refused {
				if err == nil {
					t.Fatalf("substituteProgram(%q) = %q, want a refusal", tt.in, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("substituteProgram(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
	// The same literal in an argument is still refused: there "{{" opens a
	// placeholder.
	if _, err := substitute(literal, "tok", cli); err == nil {
		t.Errorf("substitute(%q) admitted an unknown placeholder in an argument", literal)
	}
	// A slot exec check's program file takes the same pass.
	req, err := execRequest(checks.ExecCheck{Argv: []string{"go", "run", "main.go"},
		Files: []checks.File{{Path: "main.go", Content: literal + " // {{run}}"}}}, "tok", marker)
	if err != nil || len(req.Files) != 1 || string(req.Files[0].Content) != literal+" // tok" {
		t.Errorf("execRequest = %+v, %v; want the program file with only {{run}} replaced", req, err)
	}
}
