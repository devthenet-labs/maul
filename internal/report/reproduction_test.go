// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf8"
)

const reproducedRemediation = `---
success: true
confidence: 0.9
reproduction:
  reproduced: true
  command: "go test ./internal/render -run TestEscapesName"
  before: |
    --- FAIL: TestEscapesName (0.00s)
        render_test.go:14: got "<b>", want "&lt;b&gt;"
    FAIL
  after: |
    ok  	example.com/app/internal/render	0.012s
---
Escaped the sink; the regression test failed before the fix and passes after.
`

// TestParseRemediationWithoutReproduction: a report written before the
// reproduction fields existed (or by an agent that left them out) is still a
// valid report; the absence is recorded as nil, never as "not reproduced",
// so a reader can tell "said no" from "said nothing".
func TestParseRemediationWithoutReproduction(t *testing.T) {
	r, err := ParseRemediation([]byte(validRemediation))
	if err != nil {
		t.Fatalf("ParseRemediation() error = %v", err)
	}
	if r.Reproduction != nil {
		t.Errorf("Reproduction = %+v, want nil for a report without the block", r.Reproduction)
	}
}

func TestParseRemediationReproduced(t *testing.T) {
	r, err := ParseRemediation([]byte(reproducedRemediation))
	if err != nil {
		t.Fatalf("ParseRemediation() error = %v", err)
	}
	p := r.Reproduction
	if p == nil || p.Reproduced == nil || !*p.Reproduced {
		t.Fatalf("Reproduction = %+v, want reproduced true", p)
	}
	if p.Command != "go test ./internal/render -run TestEscapesName" {
		t.Errorf("Command = %q", p.Command)
	}
	if !strings.HasPrefix(p.Before, "--- FAIL: TestEscapesName") || !strings.HasSuffix(p.Before, "FAIL") {
		t.Errorf("Before = %q, want the failing run, trimmed", p.Before)
	}
	if !strings.HasPrefix(p.After, "ok") {
		t.Errorf("After = %q", p.After)
	}
}

// TestParseRemediationNotReproduced: an agent that could not make its test
// fail on the unfixed code (or could not run it at all) says so; nothing
// else is required of it, and the explanation lives in the body.
func TestParseRemediationNotReproduced(t *testing.T) {
	src := "---\nsuccess: true\nconfidence: 0.6\nreproduction:\n  reproduced: false\n---\nno toolchain here\n"
	r, err := ParseRemediation([]byte(src))
	if err != nil {
		t.Fatalf("ParseRemediation() error = %v", err)
	}
	if r.Reproduction == nil || r.Reproduction.Reproduced == nil || *r.Reproduction.Reproduced {
		t.Errorf("Reproduction = %+v, want an explicit false", r.Reproduction)
	}
}

func TestParseRemediationReproductionErrors(t *testing.T) {
	head := "---\nsuccess: true\nconfidence: 0.9\nreproduction:\n"
	tests := []struct {
		name, block, want string
	}{
		{"reproduced missing", "  command: go test ./...\n", "reproduced is required"},
		{"reproduced not a bool", "  reproduced: maybe\n", "frontmatter"},
		{"unknown key", "  reproduced: false\n  why: no tests\n", "frontmatter"},
		{"command missing", "  reproduced: true\n  before: FAIL\n  after: ok\n", "command is required"},
		{"before missing", "  reproduced: true\n  command: go test ./...\n  after: ok\n", "before is required"},
		{"after missing", "  reproduced: true\n  command: go test ./...\n  before: FAIL\n", "after is required"},
		{"blank before", "  reproduced: true\n  command: go test ./...\n  before: \"  \"\n  after: ok\n",
			"before is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRemediation([]byte(head + tt.block + "---\nbody\n"))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseRemediation() error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// TestParseRemediationBoundsReproduction: test output is long, and a fix
// must not be thrown away for the length of the evidence it quotes, so an
// over-long excerpt or command is cut to its bound (on a rune boundary,
// marked as cut), never refused; a command is kept on one line.
func TestParseRemediationBoundsReproduction(t *testing.T) {
	long := strings.Repeat("é", ReproductionExcerptMaxBytes) // two bytes each
	src := "---\nsuccess: true\nconfidence: 0.9\nreproduction:\n  reproduced: true\n" +
		"  command: |\n    go test \\\n      ./... " + strings.Repeat("x", ReproductionCommandMaxBytes) + "\n" +
		"  before: \"" + long + "\"\n  after: ok\n---\nbody\n"
	r, err := ParseRemediation([]byte(src))
	if err != nil {
		t.Fatalf("ParseRemediation() error = %v", err)
	}
	p := r.Reproduction
	if len(p.Before) > ReproductionExcerptMaxBytes || !utf8.ValidString(p.Before) {
		t.Errorf("Before is %d bytes (valid UTF-8 %v), want at most %d", len(p.Before), utf8.ValidString(p.Before),
			ReproductionExcerptMaxBytes)
	}
	if !strings.HasSuffix(p.Before, Truncated) {
		t.Errorf("Before does not end with the cut marker: %q", p.Before[len(p.Before)-40:])
	}
	if len(p.Command) > ReproductionCommandMaxBytes || strings.ContainsAny(p.Command, "\r\n") {
		t.Errorf("Command = %d bytes %q, want one line of at most %d", len(p.Command), p.Command[:40],
			ReproductionCommandMaxBytes)
	}
	if p.After != "ok" {
		t.Errorf("After = %q, want it untouched", p.After)
	}
}

// TestBoundProperty states Bound's contract as an invariant over arbitrary
// byte strings (invalid UTF-8 and multi-byte runes included) and limits:
// the result is valid UTF-8 within the limit, a value already in bound and
// valid is returned unchanged, a cut value ends in the marker, and bounding
// twice is bounding once.
func TestBoundProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 1000,
		Rand:     rand.New(rand.NewSource(20261014)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			alphabet := []string{"a", "é", "€", "𝄞", "\xff", "\n", " "}
			var b strings.Builder
			for range r.Intn(120) {
				b.WriteString(alphabet[r.Intn(len(alphabet))])
			}
			args[0] = reflect.ValueOf(b.String())
			args[1] = reflect.ValueOf(len(Truncated) + r.Intn(100))
		},
	}
	holds := func(s string, limit int) bool {
		got := Bound(s, limit)
		switch {
		case len(got) > limit || !utf8.ValidString(got):
			return false
		case utf8.ValidString(s) && len(s) <= limit && got != s:
			return false
		case got != strings.ToValidUTF8(s, string(utf8.RuneError)) && !strings.HasSuffix(got, Truncated):
			return false
		}
		return Bound(got, limit) == got
	}
	if err := quick.Check(holds, cfg); err != nil {
		t.Error(err)
	}
}
