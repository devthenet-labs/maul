// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package command_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/command"
)

// TestArgs: a command's arguments are the words after its verb on the
// command's own line, never text below it, and a body that is no command
// has none.
func TestArgs(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		want   []string
		wantOK bool
	}{
		{"no arguments", "/maul verify", nil, true},
		{"one argument", "/maul verify fix", []string{"fix"}, true},
		{"several, any white space", "/maul accept \t a  b", []string{"a", "b"}, true},
		{"the line below is the note, not arguments", "/maul verify\nfix it please", nil, true},
		{"a Unicode line break ends the line", "/maul verify --reset", nil, true},
		{"after blank lines", "\n\n  /maul accept login-works\nthanks", []string{"login-works"}, true},
		{"no command", "please /maul verify fix", nil, false},
		{"indented code is text", "    /maul verify fix", nil, false},
		{"a quoted command is text", "> /maul verify fix", nil, false},
		{"no verb", "/maul", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := command.Args(tc.body)
			if ok != tc.wantOK || !slices.Equal(got, tc.want) {
				t.Errorf("Args(%q) = %q, %v; want %q, %v", tc.body, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestParseVerify pins /maul verify's grammar: no argument, fix, stop or
// --reset, one of them at most; anything else asks for the help.
func TestParseVerify(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		want   command.VerifyArgs
		wantOK bool
	}{
		{"plain", "/maul verify", command.VerifyArgs{}, true},
		{"plain with a note below", "/maul verify\nthe login page looks off", command.VerifyArgs{}, true},
		{"fix", "/maul verify fix", command.VerifyArgs{Fix: true}, true},
		{"fix, any case", "/MAUL Verify FIX", command.VerifyArgs{Fix: true}, true},
		{"reset", "/maul verify --reset", command.VerifyArgs{Reset: true}, true},
		{"reset, any case", "/maul verify --RESET", command.VerifyArgs{Reset: true}, true},
		{"stop", "/maul verify stop", command.VerifyArgs{Stop: true}, true},
		{"stop, any case", "/maul verify STOP", command.VerifyArgs{Stop: true}, true},
		{"stop and fix", "/maul verify stop fix", command.VerifyArgs{}, false},
		{"stop is a modifier, never a verb", "/maul stop", command.VerifyArgs{}, false},
		{"both", "/maul verify fix --reset", command.VerifyArgs{}, false},
		{"twice", "/maul verify fix fix", command.VerifyArgs{}, false},
		{"reset is a modifier, never a verb", "/maul reset", command.VerifyArgs{}, false},
		{"a single dash", "/maul verify -reset", command.VerifyArgs{}, false},
		{"words", "/maul verify the login page", command.VerifyArgs{}, false},
		{"another verb", "/maul accept fix", command.VerifyArgs{}, false},
		{"no command", "verify fix", command.VerifyArgs{}, false},
		{"hidden in code", "    /maul verify --reset", command.VerifyArgs{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := command.ParseVerify(tc.body)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("ParseVerify(%q) = %+v, %v; want %+v, %v", tc.body, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestParseAccept pins /maul accept's grammar: exactly one criterion id, as
// the plan grammar writes ids, so a non-empty result is safe to echo.
func TestParseAccept(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		want   string
		wantOK bool
	}{
		{"an id", "/maul accept sounds-good", "sounds-good", true},
		{"an id and a note below", "/maul accept login\nchecked on my phone", "login", true},
		{"all", "/maul accept all", "all", true},
		{"all and a note below", "/maul accept all\nmaul could not reach the page; I checked by hand", "all", true},
		{"the longest id", "/maul accept a" + strings.Repeat("b", 31), "a" + strings.Repeat("b", 31), true},
		{"too long", "/maul accept a" + strings.Repeat("b", 32), "", false},
		{"no id", "/maul accept", "", false},
		{"the id below the command line", "/maul accept\nlogin", "", false},
		{"two ids", "/maul accept login logout", "", false},
		{"upper case is not an id", "/maul accept Login", "", false},
		{"a digit first", "/maul accept 1login", "", false},
		{"markup", "/maul accept <b>login</b>", "", false},
		{"a mention", "/maul accept @peter", "", false},
		{"non-ASCII", "/maul accept lоgin", "", false},
		{"another verb", "/maul verify login", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := command.ParseAccept(tc.body)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("ParseAccept(%q) = %q, %v; want %q, %v", tc.body, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestParsePreview pins /maul preview's grammar: no argument, a note below
// allowed; anything else asks for the help.
func TestParsePreview(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"plain", "/maul preview", true},
		{"any case", "/MAUL Preview", true},
		{"a note below", "/maul preview\nI want to look now", true},
		{"after blank lines", "\n\n  /maul preview", true},
		{"an argument", "/maul preview now", false},
		{"a modifier of verify", "/maul preview --reset", false},
		{"another verb", "/maul verify", false},
		{"no command", "preview", false},
		{"hidden in code", "    /maul preview", false},
		{"quoted", "> /maul preview", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := command.ParsePreview(tc.body); got != tc.want {
				t.Errorf("ParsePreview(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
