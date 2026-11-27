// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"encoding/json"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// line014 is a <pv>-ledger line as v0.14.15's relay stored it: a runner's
// replay of a saved check, its checkRun record and all, beside the fields
// every entry carries.
const line014 = `{"seq":1,"ts":"2026-11-20T10:00:00Z","kind":"runner","verb":"check-run",` +
	`"args":{"digest":"` + "aaaa" + `","mode":"replay"},"exit":0,"output":"1 passed","outputSHA256":"` +
	"9a1d9a4cbad7d65e6b4ffc2bd2b1a5ac6e2db3a2c9e6b3cbbcd2c7e58bdeeb0f" + `","inline":"1 passed",` +
	`"checkRun":{"digest":"aaaa","checkID":"rate.name","mode":"replay","result":"pass","attempts":[{"run":` +
	`"v1a1t1-abc","result":"pass","durationMs":812,"steps":[{"index":0,"result":"pass"}]}]}}`

// TestReadStoredReadsAnEarlierReleasesLine: the display reader reads a
// v0.14.15 line, checkRun fields and all, and one whose checkRun has
// another shape, though this release's wire refuses both (its entry has no
// check run any more), since it decodes only what a display shows.
func TestReadStoredReadsAnEarlierReleasesLine(t *testing.T) {
	// The same line with a checkRun of another shape: the reader must not
	// rest on any check run type.
	reshaped := strings.Replace(line014, `"attempts":[`, `"attempts":{"old":[`, 1)
	reshaped = strings.Replace(reshaped, `]}]}}`, `]}]}}}`, 1)
	for name, line := range map[string]string{"as stored": line014, "reshaped": reshaped} {
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		var strict toolwire.LedgerEntry
		if err := dec.Decode(&strict); err == nil {
			t.Fatalf("%s: the line decodes strictly into toolwire.LedgerEntry: the case shows nothing", name)
		}
	}
	agent := `{"seq":2,"kind":"agent","verb":"browser","side":"base","args":{"verb":"eval","args":["() => 1"]},` +
		`"exit":0,"output":"### Result\n1","outputSHA256":"` + sha256Hex([]byte("### Result\n1")) + `"}`
	for name, data := range map[string]string{"as stored": line014, "reshaped": reshaped} {
		t.Run(name, func(t *testing.T) {
			got := ReadStored(data + "\n" + agent + "\n")
			one, ok := got[1]
			if !ok || one.Kind != "runner" || one.Verb != "check-run" || one.Output != "1 passed" ||
				!strings.Contains(string(one.Args), `"mode":"replay"`) {
				t.Errorf("line 1 = %+v, %v", one, ok)
			}
			two, ok := got[2]
			if !ok || two.Verb != "browser" || two.Side != "base" || !two.OutputKept() ||
				!strings.Contains(string(two.Args), "eval") {
				t.Errorf("line 2 = %+v, %v", two, ok)
			}
		})
	}
}

// TestReadStoredSkipsWhatItCannotRead: a line that is not JSON, numbers no
// entry, or numbers one past the ledger's bound is skipped; of two lines of
// one seq the first is kept, as the comment's reader keeps it.
func TestReadStoredSkipsWhatItCannotRead(t *testing.T) {
	data := strings.Join([]string{
		`not json`,
		`{"seq":0,"verb":"http"}`,
		`{"seq":-3,"verb":"http"}`,
		`{"seq":` + "9999999" + `,"verb":"http"}`,
		`{"seq":4,"verb":"http","output":"first"}`,
		`{"seq":4,"verb":"exec","output":"second"}`,
		``,
	}, "\n")
	got := ReadStored(data)
	if len(got) != 1 || got[4].Output != "first" {
		t.Errorf("ReadStored = %+v, want only seq 4's first line", got)
	}
}

// TestStoredLineIsTheIndexsCall: a line stands for an index entry only when
// it records the entry's result digest and verb, and its output is shown
// only when it hashes to that digest (a line cut to fit keeps its digest).
func TestStoredLineIsTheIndexsCall(t *testing.T) {
	out := "HTTP/1.1 200 OK"
	ix := v1alpha1.LedgerIndexEntry{Seq: 3, Verb: v1alpha1.LedgerVerbHTTP, ResultSHA256: sha256Hex([]byte(out))}
	line := StoredLine{Seq: 3, Verb: "http", Output: out, OutputSHA256: ix.ResultSHA256}
	for _, tc := range []struct {
		name      string
		mutate    func(*StoredLine)
		of, shown bool
	}{
		{"the call", func(*StoredLine) {}, true, true},
		{"another verb", func(l *StoredLine) { l.Verb = "exec" }, false, false},
		{"another digest", func(l *StoredLine) { l.OutputSHA256 = strings.Repeat("0", 64) }, false, false},
		{"output cut to fit", func(l *StoredLine) { l.Output = DroppedOutput }, true, false},
	} {
		l := line
		tc.mutate(&l)
		if got := l.Of(ix); got != tc.of {
			t.Errorf("%s: Of = %v, want %v", tc.name, got, tc.of)
		}
		if got := l.Of(ix) && l.OutputKept(); got != tc.shown {
			t.Errorf("%s: shown = %v, want %v", tc.name, got, tc.shown)
		}
	}
}
