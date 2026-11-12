// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command_test

import (
	"slices"
	"testing"
	"testing/quick"

	"github.com/devthenet-labs/maul/internal/command"
)

// TestMisplaced: a comment whose first non-blank line is no command but a
// later line reads as one the surface offers is reported, with the verb, so
// its author can be told commands go on the first line (defect gate 2: a
// "/maul replan" ending a comment was silently ignored). A command on the
// first line, a later line quoted, in a code block or span, indented as
// code, or naming a verb the surface does not offer, is not.
func TestMisplaced(t *testing.T) {
	tests := []struct {
		name, body string
		surface    command.Surface
		want       string
	}{
		{"last line", "Please also cover readable and clickable.\n/maul replan", command.IntentIssue, "replan"},
		{"after a blank", "Looks good.\n\n/maul approve", command.IntentIssue, "approve"},
		{"indented by less than code", "Note:\n   /MAUL Accept all", command.IntentIssue, "accept"},
		{"crlf", "first\r\n/maul cancel\r\n", command.IntentIssue, "cancel"},
		{"the first command named", "x\n/maul ship\n/maul verify fix", command.IntentIssue, "verify"},
		{"pr surface", "nit\n/maul revise tighten it", command.IntentPR, "revise"},

		{"on the first line", "/maul replan\nmore", command.IntentIssue, ""},
		{"first line after blanks", "\n\n  /maul replan", command.IntentIssue, ""},
		{"no command", "nothing to see\nhere", command.IntentIssue, ""},
		{"verb not offered", "x\n/maul revise", command.IntentIssue, ""},
		{"no verb", "x\n/maul", command.IntentIssue, ""},
		{"prefix not a word", "x\n/maulreplan", command.IntentIssue, ""},
		{"mid-line", "x\nplease /maul replan", command.IntentIssue, ""},
		{"quoted", "x\n> /maul replan", command.IntentIssue, ""},
		{"quote continued", "x\n> you said\n/maul replan", command.IntentIssue, ""},
		{"fenced", "x\n```\n/maul replan\n```", command.IntentIssue, ""},
		{"tilde fenced", "x\n~~~text\n/maul replan\n~~~", command.IntentIssue, ""},
		{"fence never closed", "x\n```\ny\n/maul approve", command.IntentIssue, ""},
		{"indented code", "x\n\n    /maul replan", command.IntentIssue, ""},
		{"code span over lines", "x `y\n/maul replan` z", command.IntentIssue, ""},
		{"html comment", "x <!--\n/maul replan\n-->", command.IntentIssue, ""},
		{"line in a code span", "x\n/maul replan `y`", command.IntentIssue, ""},
		{"other line break", "x\n /maul replan", command.IntentIssue, ""},
		{"unknown surface", "x\n/maul replan", "pull-request", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verb, ok := command.Misplaced(tt.body, tt.surface)
			if verb != tt.want || ok != (tt.want != "") {
				t.Errorf("Misplaced(%q) = %q, %v; want %q", tt.body, verb, ok, tt.want)
			}
		})
	}
}

// TestMisplacedNeverACommandProperty: a comment that parses as a command is
// never also misplaced, and a misplaced verb is always one the surface
// offers, so the reply can never stand in for a command or name a verb the
// surface does not have.
func TestMisplacedNeverACommandProperty(t *testing.T) {
	prop := func(surface, body string) bool {
		verb, ok := command.Misplaced(body, command.Surface(surface))
		if !ok {
			return verb == ""
		}
		_, parses := command.Parse(body)
		return !parses && slices.Contains(command.Available(command.Surface(surface)), verb)
	}
	if err := quick.Check(prop, quickConfig(20261112, genSurface, genHostile)); err != nil {
		t.Error(err)
	}
}
