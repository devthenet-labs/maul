// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
)

// TestSubstitute: one pass, in one order, for every check path: the
// attempt's marker, the binary, the tree, then the run token through
// acceptance.Substitute, which still refuses any other placeholder.
func TestSubstitute(t *testing.T) {
	marker := checks.ExecMarker(strings.Repeat("a", 32))
	cli := places{bin: "/work/bin/bb", tree: "/work/tree"}
	tests := []struct {
		name    string
		in      string
		marker  string
		p       places
		want    string
		refused bool
	}{
		{"all four", "{{bin}} {{tree}}/x {{run}} {{marker}}", marker, cli,
			"/work/bin/bb /work/tree/x tok " + marker, false},
		{"run only", "id-{{run}}", "", places{}, "id-tok", false},
		{"no marker outside files", "{{marker}}", "", cli, "", true},
		{"bin in a library", "{{bin}}", "", places{tree: "/work/tree"}, "", true},
		{"tree in the slot", "{{tree}}", "", places{}, "", true},
		{"an unknown placeholder", "{{capture.x}}", "", cli, "", true},
		{"another unknown placeholder", "{{home}}", "", cli, "", true},
		{"unclosed", "{{run", "", cli, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := substitute(tt.in, "tok", tt.marker, tt.p)
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
