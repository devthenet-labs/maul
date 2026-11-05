// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// seed drives the seeded properties.
const seed = 20261105

// verifiedLedger parses a toolLedger as a host does.
func verifiedLedger(t *testing.T, log []byte, msg string) Output {
	t.Helper()
	out := Parse(log, msg)
	if out.Reason != "" {
		t.Fatalf("fixture ledger refused: %s", out.Reason)
	}
	return out
}

func TestParse(t *testing.T) {
	shot := testPNG(t, 8, 8)
	log, msg := toolLedger(t, shot)
	out := verifiedLedger(t, log, msg)
	if len(out.Ledger.Entries) != 7 || len(out.Evidence) != 1 || out.Dropped != 0 {
		t.Fatalf("parsed %d entries, %d evidence, %d dropped", len(out.Ledger.Entries), len(out.Evidence), out.Dropped)
	}
	evLine := func(seq int64, sha string, data []byte) []byte {
		b, _ := json.Marshal(evidenceLine{V: toolwire.WireVersion, Seq: seq, SHA256: sha, Data: data})
		return append([]byte(toolwire.EvidencePrefix), append(b, '\n')...)
	}
	summaryAt := bytes.LastIndex(log, []byte(toolwire.EntryPrefix))
	for _, tc := range []struct {
		name    string
		log     []byte
		msg     string
		reason  v1alpha1.PreviewVerificationReason
		dropped int
	}{
		{"an extra evidence line whose bytes are not its sha256 drops alone",
			append(bytes.Clone(log), evLine(4, sha256Hex(shot), []byte("other"))...), msg, "", 1},
		{"malformed evidence drops alone",
			append(bytes.Clone(log), []byte(toolwire.EvidencePrefix+"{nope\n")...), msg, "", 1},
		{"a changed entry", bytes.Replace(log, []byte(`"seq":2`), []byte(`"seq":9`), 1), msg,
			v1alpha1.VerificationReasonLedgerMismatch, 0},
		{"another termination message", log, `{"count":7,"sha256":"` + strings.Repeat("0", 64) + `"}`,
			v1alpha1.VerificationReasonLedgerMismatch, 0},
		{"no summary", log[:summaryAt], msg, v1alpha1.VerificationReasonDeadlineNoSummary, 0},
		{"over the bound", make([]byte, toolwire.MaxLogBytes+1), msg, v1alpha1.VerificationReasonLedgerMismatch, 0},
	} {
		got := Parse(tc.log, tc.msg)
		if got.Reason != tc.reason || got.Dropped != tc.dropped {
			t.Errorf("%s: reason %q dropped %d, want %q %d", tc.name, got.Reason, got.Dropped, tc.reason, tc.dropped)
		}
	}
}

// The index and the readiness wait are read from the ledger alone.
func TestIndexAndReadiness(t *testing.T) {
	log, msg := toolLedger(t, testPNG(t, 8, 8))
	out := verifiedLedger(t, log, msg)
	idx := Index(out.Ledger.Entries)
	if len(idx) != 7 || idx[0].Seq != 1 || idx[3].Verb != v1alpha1.LedgerVerb(toolwire.VerbBrowser) ||
		len(idx[3].Artifacts) != 1 || idx[3].ResultSHA256 != out.Ledger.Entries[3].OutputSHA256 {
		t.Errorf("index %+v", idx)
	}
	waits := Readiness(out.Ledger.Entries)
	if len(waits) != 2 || waits[0].Target != "api" || waits[1].Target != "web" {
		t.Errorf("readiness %+v", waits)
	}
	if got := Readiness(out.Ledger.Entries[1:]); got != nil {
		t.Errorf("readiness of a ledger not opened by the relay's wait: %+v", got)
	}
	if got := Readiness(nil); got != nil {
		t.Errorf("readiness of no ledger: %+v", got)
	}
}

// What status holds of the ledger is read from the ledger alone: a
// confirmation whose check is over the saved bound, bound to no outcome
// under evaluation, or that failed, confirms nothing.
func TestConfirmedChecks(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var entries []toolwire.LedgerEntry
	add := func(e toolwire.LedgerEntry) {
		e.Seq, e.TS = int64(len(entries)+1), at
		entries = append(entries, e)
	}
	pass := []toolwire.Attempt{{Run: "0123456789abcdef", Result: toolwire.ResultPass}}
	propose := func(body string, confirmed toolwire.RunResult) string {
		c, err := checks.Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		d := strings.TrimPrefix(checks.Digest(c), "sha256:")
		args, _ := json.Marshal(toolwire.CheckRunRequest{Spec: checks.Canonical(c)})
		add(toolwire.LedgerEntry{Kind: toolwire.KindAgent, Verb: toolwire.VerbCheckRun, Args: args,
			CheckRun: &toolwire.CheckRunRecord{Digest: d, Mode: toolwire.ModeDryRun, Result: toolwire.RunPass,
				Attempts: pass}})
		attempts := pass
		if confirmed == toolwire.RunFail {
			attempts = []toolwire.Attempt{{Run: "a", Result: toolwire.ResultFail}, {Run: "b", Result: toolwire.ResultFail}}
		}
		add(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbConfirm, Args: json.RawMessage(`{}`),
			CheckRun: &toolwire.CheckRunRecord{Digest: d, Mode: toolwire.ModeConfirm, Result: confirmed,
				Attempts: attempts}})
		return d
	}
	good := propose(testCheckBody, toolwire.RunPass)
	propose(strings.ReplaceAll(testCheckBody, "items", "other"), toolwire.RunPass)
	propose(strings.Replace(testCheckBody, "items.body", "items.fails", 1), toolwire.RunFail)
	big := strings.Replace(testCheckBody, "/api/echo", "/api/"+strings.Repeat("a", v1alpha1.MaxSavedCheckBytes), 1)
	if _, err := checks.Parse([]byte(big)); err == nil {
		propose(strings.Replace(big, "items.body", "items.big", 1), toolwire.RunPass)
	}
	got := ConfirmedChecks(entries, testOutcomes())
	if len(got) != 1 || got[0].Digest != "sha256:"+good || got[0].ID != "items.body" {
		t.Errorf("confirmed %+v", got)
	}
	runs := ConfirmationRuns(entries)
	if len(runs) < 3 || runs[0].DryRunSeq != 1 || runs[0].Seq != 2 || runs[2].Result != v1alpha1.CheckRunFail {
		t.Errorf("confirmations %+v", runs)
	}
}

// boundedLedger is a verified ledger of n http entries, each output full.
func boundedLedger(t *testing.T, n int) toolwire.Ledger {
	t.Helper()
	var out bytes.Buffer
	w := toolwire.NewWriter(&out)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := range n {
		if _, err := w.Append(toolwire.LedgerEntry{TS: at, Kind: toolwire.KindAgent, Verb: toolwire.VerbHTTP,
			Args: json.RawMessage(`{}`), Output: strings.Repeat(fmt.Sprint(i%10), toolwire.MaxOutputBytes)}); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return verifiedLedger(t, out.Bytes(), sum.TerminationMessage()).Ledger
}

// The full ledger's data holds every entry's line within its bound: the
// oldest outputs go first, every seq and output digest stays.
func TestConfigMapDataBound(t *testing.T) {
	l := boundedLedger(t, 40)
	data, err := ConfigMapData(l)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for k, v := range data {
		size += len(k) + len(v)
	}
	if size > MaxConfigMapBytes {
		t.Errorf("ledger data holds %d bytes, over %d", size, MaxConfigMapBytes)
	}
	lines := strings.Split(strings.TrimSuffix(data[Key], "\n"), "\n")
	if len(lines) != 40 {
		t.Fatalf("%d lines, want every entry's", len(lines))
	}
	var first, last toolwire.LedgerEntry
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[39]), &last)
	if first.Output != DroppedOutput || first.OutputSHA256 != l.Entries[0].OutputSHA256 || first.Seq != 1 {
		t.Errorf("oldest entry %d kept its output or lost its digest", first.Seq)
	}
	if last.Output != l.Entries[39].Output {
		t.Error("the newest entry's output was dropped")
	}
	var sum toolwire.Summary
	if err := json.Unmarshal([]byte(data[SummaryKey]), &sum); err != nil || sum != l.Summary {
		t.Errorf("summary %s, want %+v: %v", data[SummaryKey], l.Summary, err)
	}
}

// TestConfigMapDataProperty: whatever the ledger's length, its data is
// within the bound, keeps one line per entry in order with its seq and
// output digest, and drops outputs oldest first: no entry keeps its output
// while an older one lost it.
func TestConfigMapDataProperty(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	for n := range 12 {
		count := r.Intn(80)
		l := boundedLedger(t, count)
		data, err := ConfigMapData(l)
		if err != nil {
			t.Fatalf("case %d (seed %d, %d entries): %v", n, seed, count, err)
		}
		size := 0
		for k, v := range data {
			size += len(k) + len(v)
		}
		if size > MaxConfigMapBytes {
			t.Fatalf("case %d (seed %d): %d bytes", n, seed, size)
		}
		body := strings.TrimSuffix(data[Key], "\n")
		var lines []string
		if body != "" {
			lines = strings.Split(body, "\n")
		}
		if len(lines) != count {
			t.Fatalf("case %d (seed %d): %d lines for %d entries", n, seed, len(lines), count)
		}
		kept := false
		for i, line := range lines {
			var e toolwire.LedgerEntry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatal(err)
			}
			if e.Seq != l.Entries[i].Seq || e.OutputSHA256 != l.Entries[i].OutputSHA256 {
				t.Fatalf("case %d (seed %d): line %d lost its seq or digest", n, seed, i)
			}
			dropped := e.Output == DroppedOutput
			if dropped && kept {
				t.Fatalf("case %d (seed %d): line %d dropped after a newer kept", n, seed, i)
			}
			kept = kept || !dropped
		}
	}
}
