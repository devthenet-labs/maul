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

// The index records each entry's tree in the ledger's own encoding: base
// for a base-side call, no side for the head however the head was spelled
// and for a verb that takes no side, so a base call is never read as
// evidence about the head (acceptance.ValidCite).
func TestIndexRecordsSide(t *testing.T) {
	sha := strings.Repeat("c", 64)
	entry := func(seq int64, verb toolwire.Verb, side toolwire.Side) toolwire.LedgerEntry {
		return toolwire.LedgerEntry{Seq: seq, Kind: toolwire.KindAgent, Verb: verb, Side: side, OutputSHA256: sha}
	}
	idx := Index([]toolwire.LedgerEntry{
		entry(1, toolwire.VerbExec, toolwire.SideBase),
		entry(2, toolwire.VerbExec, ""),
		entry(3, toolwire.VerbExec, toolwire.SideHead),
		entry(4, toolwire.VerbRun, toolwire.SideBase),
		entry(5, toolwire.VerbHTTP, ""),
	})
	want := []v1alpha1.TreeSide{v1alpha1.TreeBase, "", "", v1alpha1.TreeBase, ""}
	if len(idx) != len(want) {
		t.Fatalf("index has %d entries, want %d", len(idx), len(want))
	}
	for i, w := range want {
		if idx[i].Side != w {
			t.Errorf("entry %d side = %q, want %q", idx[i].Seq, idx[i].Side, w)
		}
	}
}

// TestIndexRecordsNoAnswer: the index marks the evaluator's call that got
// nothing back from what it exercised (noAnswer), which no claim can rest
// on (acceptance.ValidCite): an http request whose entry holds no answered
// exchange (no response: DNS, refused, timeout, TLS, reset), and a run
// whose program never ran (notRun). An answered exchange of any status, an
// exec or a run of any exit, a refused call and the relay's own readiness
// wait are not marked.
func TestIndexRecordsNoAnswer(t *testing.T) {
	sha := strings.Repeat("c", 64)
	entry := func(kind toolwire.Kind, verb toolwire.Verb, output string) toolwire.LedgerEntry {
		return toolwire.LedgerEntry{Kind: kind, Verb: verb, Output: output, OutputSHA256: sha}
	}
	notRun := entry(toolwire.KindAgent, toolwire.VerbRun, "# maulcheck\n./main.go:1: undefined: x\n")
	notRun.NotRun, notRun.Exit = true, 1
	ran := entry(toolwire.KindAgent, toolwire.VerbRun, "boom\n")
	ran.Exit = 1
	refused := entry(toolwire.KindAgent, toolwire.VerbHTTP, "")
	refused.Refused = "navigation"
	for _, tt := range []struct {
		name  string
		entry toolwire.LedgerEntry
		want  bool
	}{
		{"http with no response", entry(toolwire.KindAgent, toolwire.VerbHTTP, ""), true},
		{"http whose output is not an exchange", entry(toolwire.KindAgent, toolwire.VerbHTTP, "HTTP/1.1 200"), true},
		{"http answered 200", entry(toolwire.KindAgent, toolwire.VerbHTTP, "HTTP 200\n\nok"), false},
		{"http answered 503", entry(toolwire.KindAgent, toolwire.VerbHTTP, "HTTP 503\n\n"), false},
		{"refused http", refused, false},
		{"the relay's readiness wait", entry(toolwire.KindRelay, toolwire.VerbHTTP, `[]`), false},
		{"a run whose program did not compile", notRun, true},
		{"a run that ran and exited 1", ran, false},
		{"an exec with no output", entry(toolwire.KindAgent, toolwire.VerbExec, ""), false},
		{"a browser call", entry(toolwire.KindAgent, toolwire.VerbBrowser, ""), false},
	} {
		tt.entry.Seq = 1
		if got := Index([]toolwire.LedgerEntry{tt.entry})[0].NoAnswer; got != tt.want {
			t.Errorf("%s: noAnswer %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestIndexRecordsRanNothing (FD 1): only a refusal under a code the
// relay's gate alone writes, before it sends a call anywhere (no key, past
// maxToolCalls), shows that nothing ran. Every other refusal may have come
// after the call was sent, and an unrefused call ran.
func TestIndexRecordsRanNothing(t *testing.T) {
	for _, tt := range []struct {
		refused string
		want    bool
	}{
		{toolwire.RefusedKey, true},
		{toolwire.RefusedToolCalls, true},
		{toolwire.RefusedUnavailable, false},
		{toolwire.RefusedInvalid, false},
		{toolwire.RefusedVerb, false},
		{"", false},
	} {
		for _, verb := range []toolwire.Verb{toolwire.VerbExec, toolwire.VerbRun} {
			e := toolwire.LedgerEntry{Seq: 1, Kind: toolwire.KindAgent, Verb: verb, Refused: tt.refused}
			if got := Index([]toolwire.LedgerEntry{e})[0].RanNothing; got != tt.want {
				t.Errorf("%s refused %q: ranNothing %v, want %v", verb, tt.refused, got, tt.want)
			}
		}
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
