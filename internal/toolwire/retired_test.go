// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestRetiredVerbsHaveNoEndpoint: the check runner's verbs are retired
// spellings. A ledger an earlier relay wrote may still name them, so they
// stay verbs, but no tool answers them: no path, as the relay's own session
// and build have none.
func TestRetiredVerbsHaveNoEndpoint(t *testing.T) {
	for _, v := range []Verb{VerbCheckRun, VerbCheckResults, VerbConfirm} {
		if p := Path(v); p != "" {
			t.Errorf("Path(%s) = %q, want none: the verb is retired", v, p)
		}
		if !slices.Contains(Verbs, v) || !knownVerb(v) {
			t.Errorf("%s is no longer a verb: an earlier ledger naming it would not read", v)
		}
	}
}

// earlierCheckRunEntry is an entry line as v0.14.15's relay wrote a replay
// of a saved check: its checkRun record beside the fields every entry
// carries. withRun false is the same entry without the record.
func earlierCheckRunEntry(withRun bool) string {
	out := "1 passed"
	run := ""
	if withRun {
		run = `,"checkRun":{"digest":"` + strings.Repeat("a", 64) + `","checkID":"rate.name","mode":"replay",` +
			`"result":"pass","attempts":[{"run":"tok1","result":"pass","durationMs":812,` +
			`"steps":[{"index":0,"result":"pass"}]}]}`
	}
	return `{"seq":1,"ts":"2026-11-20T10:00:00Z","kind":"relay","verb":"check-run","args":{},"exit":0,` +
		`"output":"` + out + `","outputSHA256":"` + sum([]byte(out)) + `"` + run + `}`
}

// sealedLog is a log of one entry line, raw, and its summary.
func sealedLog(entry string) ([]byte, string) {
	line := EntryPrefix + `{"v":2,"entry":` + entry + "}\n"
	h := sha256.Sum256([]byte(line))
	s := Summary{Count: 1, SHA256: hex.EncodeToString(h[:])}
	return []byte(line + EntryPrefix + `{"v":2,"summary":` + s.TerminationMessage() + "}\n"),
		s.TerminationMessage()
}

// TestVerifyRefusesAnEarlierReleasesCheckRun: the check run record is gone
// from the wire, so a log an earlier relay wrote with one does not verify
// (the mixed-version roll: maul's trouble, never a verdict). The same entry
// without the record still does, so the case shows the record refused.
func TestVerifyRefusesAnEarlierReleasesCheckRun(t *testing.T) {
	log, term := sealedLog(earlierCheckRunEntry(false))
	l, err := Verify(log, term)
	if err != nil || len(l.Entries) != 1 || l.Entries[0].Verb != VerbCheckRun {
		t.Fatalf("the entry without its record: %+v, %v", l, err)
	}
	log, term = sealedLog(earlierCheckRunEntry(true))
	if _, err := Verify(log, term); !errors.Is(err, ErrInvalid) {
		t.Errorf("an entry carrying a check run verified (%v), want %v", err, ErrInvalid)
	}
}
