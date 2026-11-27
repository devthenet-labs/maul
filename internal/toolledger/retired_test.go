// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestParseRefusesAnEarlierReleasesCheckRun (the mixed-version roll): an
// earlier tools image's log whose entry carries a check run record is Infra
// ledger-mismatch, never read; the same log without the record parses.
func TestParseRefusesAnEarlierReleasesCheckRun(t *testing.T) {
	entry := func(run string) string {
		return `{"seq":1,"ts":"2026-11-20T10:00:00Z","kind":"relay","verb":"check-run","args":{},"exit":0,` +
			`"output":"","outputSHA256":"` + sha256Hex(nil) + `"` + run + `}`
	}
	seal := func(e string) ([]byte, string) {
		line := toolwire.EntryPrefix + `{"v":2,"entry":` + e + "}\n"
		h := sha256.Sum256([]byte(line))
		s := toolwire.Summary{Count: 1, SHA256: hex.EncodeToString(h[:])}
		return []byte(line + toolwire.EntryPrefix + `{"v":2,"summary":` + s.TerminationMessage() + "}\n"),
			s.TerminationMessage()
	}
	if out := Parse(seal(entry(""))); out.Reason != "" || len(out.Ledger.Entries) != 1 {
		t.Fatalf("the entry without its record: %+v", out)
	}
	run := `,"checkRun":{"digest":"` + strings.Repeat("a", 64) + `","checkID":"rate.name","mode":"replay",` +
		`"result":"pass","attempts":[{"run":"tok1","result":"pass","durationMs":3}]}`
	if out := Parse(seal(entry(run))); out.Reason != v1alpha1.VerificationReasonLedgerMismatch {
		t.Errorf("an earlier relay's check run: reason %q, want %q", out.Reason,
			v1alpha1.VerificationReasonLedgerMismatch)
	}
}
