// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var ts = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var pngBytes = []byte("\x89PNG\r\n\x1a\nnot really a png")

func browserEntry() LedgerEntry {
	return LedgerEntry{
		TS: ts, Kind: KindAgent, Verb: VerbBrowser,
		Args:      json.RawMessage(`{"verb":"screenshot","args":[]}`),
		Output:    "- button \"Play\" [ref=e12]\n",
		Inline:    "- button \"Play\" [ref=e12]\n",
		Artifacts: []Artifact{{Name: "shot.png", SHA256: sum(pngBytes), Bytes: int64(len(pngBytes))}},
		Locators:  []string{`getByRole('button', { name: 'Play' })`},
	}
}

func checkRunEntry(kind Kind, verb Verb, mode CheckRunMode, attempts ...AttemptResult) LedgerEntry {
	rec := &CheckRunRecord{Digest: strings.Repeat("a", 64), CheckID: "winner-rises", Mode: mode}
	for i, r := range attempts {
		rec.Attempts = append(rec.Attempts, Attempt{Run: "run" + string(rune('a'+i)), Result: r, DurationMs: 12,
			Steps: []StepRecord{{Index: 0, Result: r}}})
	}
	rec.Result = DeriveResult(rec.Attempts)
	return LedgerEntry{TS: ts, Kind: kind, Verb: verb, Args: json.RawMessage(`{}`), CheckRun: rec}
}

func TestWriterSealsEntries(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	e := browserEntry()
	e.Output = "bad \xff byte"
	got, err := w.Append(e)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 1 {
		t.Errorf("seq = %d, want 1", got.Seq)
	}
	if got.Output != "bad � byte" {
		t.Errorf("output not made valid UTF-8: %q", got.Output)
	}
	if got.OutputSHA256 != sum([]byte(got.Output)) {
		t.Errorf("output sha256 = %s, want the recorded output's", got.OutputSHA256)
	}
	second, err := w.Append(browserEntry())
	if err != nil || second.Seq != 2 {
		t.Fatalf("second append: seq %d err %v", second.Seq, err)
	}
	if !strings.HasPrefix(buf.String(), EntryPrefix) {
		t.Errorf("line does not start with %q: %q", EntryPrefix, buf.String())
	}
}

func TestWriterBoundsOutputAndInline(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	e := browserEntry()
	e.Output = strings.Repeat("x", MaxOutputBytes+10)
	e.Inline = strings.Repeat("y", MaxInlineBytes+10)
	got, err := w.Append(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Output) > MaxOutputBytes || !strings.HasSuffix(got.Output, TruncationMarker) {
		t.Errorf("output not cut to its bound with the marker: %d bytes", len(got.Output))
	}
	if len(got.Inline) > MaxInlineBytes || !strings.HasSuffix(got.Inline, TruncationMarker) {
		t.Errorf("inline not cut to its bound with the marker: %d bytes", len(got.Inline))
	}
}

func TestEntryCheckRefuses(t *testing.T) {
	ok := browserEntry()
	ok.Seq = 1
	ok.OutputSHA256 = sum([]byte(ok.Output))
	if err := ok.Check(); err != nil {
		t.Fatalf("valid entry refused: %v", err)
	}
	cases := map[string]func(*LedgerEntry){
		"seq zero":      func(e *LedgerEntry) { e.Seq = 0 },
		"no timestamp":  func(e *LedgerEntry) { e.TS = time.Time{} },
		"unknown kind":  func(e *LedgerEntry) { e.Kind = "app" },
		"unknown verb":  func(e *LedgerEntry) { e.Verb = "run-code" },
		"args not json": func(e *LedgerEntry) { e.Args = json.RawMessage(`{`) },
		"args too large": func(e *LedgerEntry) {
			e.Args = json.RawMessage(`"` + strings.Repeat("a", MaxArgsBytes) + `"`)
		},
		"output sha mismatch": func(e *LedgerEntry) { e.Output += "!" },
		"output not utf8":     func(e *LedgerEntry) { e.Output = "\xff"; e.OutputSHA256 = sum([]byte(e.Output)) },
		"inline too large":    func(e *LedgerEntry) { e.Inline = strings.Repeat("y", MaxInlineBytes+1) },
		"artifact bad sha":    func(e *LedgerEntry) { e.Artifacts[0].SHA256 = "ABC" },
		"artifact no name":    func(e *LedgerEntry) { e.Artifacts[0].Name = "" },
		"artifact path":       func(e *LedgerEntry) { e.Artifacts[0].Name = "../x.png" },
		"artifact negative":   func(e *LedgerEntry) { e.Artifacts[0].Bytes = -1 },
		"too many locators": func(e *LedgerEntry) {
			e.Locators = make([]string, MaxLocators+1)
			for i := range e.Locators {
				e.Locators[i] = "x"
			}
		},
		"refused with artifacts": func(e *LedgerEntry) { e.Refused = "verb-not-allowed" },
		"refusal not a code":     func(e *LedgerEntry) { e.Refused = "Not Allowed!"; e.Artifacts = nil },
		"check run on browser": func(e *LedgerEntry) {
			e.CheckRun = checkRunEntry(KindAgent, VerbCheckRun, ModeDryRun, ResultPass).CheckRun
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			e := browserEntry()
			e.Seq = 1
			e.Artifacts = append([]Artifact(nil), e.Artifacts...)
			e.OutputSHA256 = sum([]byte(e.Output))
			mut(&e)
			if err := e.Check(); err == nil {
				t.Error("Check accepted it")
			}
		})
	}
}

func TestCheckRunConsistency(t *testing.T) {
	cases := []struct {
		name string
		e    LedgerEntry
		ok   bool
	}{
		{"agent dry run", checkRunEntry(KindAgent, VerbCheckRun, ModeDryRun, ResultPass), true},
		{"relay replay quarantined", checkRunEntry(KindRelay, VerbCheckRun, ModeReplay, ResultFail, ResultPass), true},
		{"relay confirm fail", checkRunEntry(KindRelay, VerbConfirm, ModeConfirm, ResultFail, ResultFail), true},
		{"agent replay", checkRunEntry(KindAgent, VerbCheckRun, ModeReplay, ResultPass), false},
		{"relay dry run", checkRunEntry(KindRelay, VerbCheckRun, ModeDryRun, ResultPass), false},
		{"confirm on check-run verb", checkRunEntry(KindRelay, VerbCheckRun, ModeConfirm, ResultPass), false},
		{"rerun after pass", checkRunEntry(KindRelay, VerbCheckRun, ModeReplay, ResultPass, ResultPass), false},
		{"three attempts", checkRunEntry(KindRelay, VerbCheckRun, ModeReplay, ResultFail, ResultFail, ResultFail), false},
		{"no attempts", checkRunEntry(KindRelay, VerbCheckRun, ModeReplay), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.e.Seq = 1
			c.e.OutputSHA256 = sum(nil)
			if err := c.e.Check(); (err == nil) != c.ok {
				t.Errorf("Check() = %v, want ok %v", err, c.ok)
			}
		})
	}
	t.Run("result not derived", func(t *testing.T) {
		e := checkRunEntry(KindRelay, VerbCheckRun, ModeReplay, ResultFail, ResultPass)
		e.Seq, e.OutputSHA256 = 1, sum(nil)
		e.CheckRun.Result = RunPass
		if e.Check() == nil {
			t.Error("a fail-then-pass recorded as pass was accepted")
		}
	})
}

func TestDeriveResult(t *testing.T) {
	cases := []struct {
		in   []AttemptResult
		want RunResult
	}{
		{[]AttemptResult{ResultPass}, RunPass},
		{[]AttemptResult{ResultFail}, RunFail},
		{[]AttemptResult{ResultFail, ResultFail}, RunFail},
		{[]AttemptResult{ResultFail, ResultPass}, RunQuarantined},
		{[]AttemptResult{ResultError, ResultPass}, RunQuarantined},
		{[]AttemptResult{ResultError}, RunError},
		{[]AttemptResult{ResultFail, ResultError}, RunError},
		{nil, ""},
		{[]AttemptResult{ResultPass, ResultPass}, ""},
	}
	for _, c := range cases {
		var as []Attempt
		for _, r := range c.in {
			as = append(as, Attempt{Result: r})
		}
		if got := DeriveResult(as); got != c.want {
			t.Errorf("DeriveResult(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriterRefuses(t *testing.T) {
	t.Run("invalid entry", func(t *testing.T) {
		w := NewWriter(&bytes.Buffer{})
		e := browserEntry()
		e.Verb = "run-code"
		if _, err := w.Append(e); err == nil {
			t.Error("Append accepted an unknown verb")
		}
	})
	t.Run("evidence for an unknown artifact", func(t *testing.T) {
		w := NewWriter(&bytes.Buffer{})
		if _, err := w.Append(browserEntry()); err != nil {
			t.Fatal(err)
		}
		if err := w.Evidence(1, []byte("other bytes")); err == nil {
			t.Error("Evidence accepted bytes no entry recorded")
		}
		if err := w.Evidence(2, pngBytes); err == nil {
			t.Error("Evidence accepted a seq never written")
		}
	})
	t.Run("too much evidence", func(t *testing.T) {
		w := NewWriter(&bytes.Buffer{})
		for range MaxEvidence + 1 {
			e := browserEntry()
			if _, err := w.Append(e); err != nil {
				t.Fatal(err)
			}
		}
		var err error
		for seq := int64(1); seq <= MaxEvidence+1 && err == nil; seq++ {
			err = w.Evidence(seq, pngBytes)
		}
		if err == nil {
			t.Error("Evidence wrote more than MaxEvidence lines")
		}
	})
	t.Run("after close", func(t *testing.T) {
		w := NewWriter(&bytes.Buffer{})
		if _, err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Append(browserEntry()); err == nil {
			t.Error("Append after Close accepted")
		}
		if err := w.Evidence(1, pngBytes); err == nil {
			t.Error("Evidence after Close accepted")
		}
		if _, err := w.Close(); err == nil {
			t.Error("Close twice accepted")
		}
	})
	t.Run("ledger full", func(t *testing.T) {
		w := NewWriter(&bytes.Buffer{})
		var err error
		for i := 0; i <= MaxEntries && err == nil; i++ {
			_, err = w.Append(LedgerEntry{TS: ts, Kind: KindAgent, Verb: VerbEvents, Args: json.RawMessage(`{}`)})
		}
		if !errors.Is(err, ErrLedgerFull) {
			t.Errorf("err = %v, want ErrLedgerFull", err)
		}
	})
}
