// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// seed is every property's seed, so the gate is deterministic.
const seed = 20261028

// written is a ledger as the tools container writes it.
type written struct {
	log  []byte
	term string
	sum  Summary
	ents []LedgerEntry
}

func writeLedger(t *testing.T, entries []LedgerEntry, evidence map[int64][]byte) written {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("node: some unrelated line\n")
	w := NewWriter(&buf)
	got := make([]LedgerEntry, 0, len(entries))
	for _, e := range entries {
		sealed, err := w.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, sealed)
	}
	for seq := int64(1); seq <= int64(len(entries)); seq++ {
		if b, ok := evidence[seq]; ok {
			if err := w.Evidence(seq, b); err != nil {
				t.Fatal(err)
			}
		}
	}
	s, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		got = nil // as Verify returns no entries
	}
	return written{log: buf.Bytes(), term: s.TerminationMessage(), sum: s, ents: got}
}

func sample(t *testing.T) written {
	return writeLedger(t, []LedgerEntry{
		checkRunEntry(KindRelay, VerbCheckRun, ModeReplay, ResultPass),
		browserEntry(),
		checkRunEntry(KindAgent, VerbCheckRun, ModeDryRun, ResultPass),
		{TS: ts, Kind: KindAgent, Verb: VerbBrowser, Args: []byte(`{"verb":"run-code"}`), Refused: "verb-not-allowed"},
	}, map[int64][]byte{2: pngBytes})
}

func TestVerifyRoundTrip(t *testing.T) {
	w := sample(t)
	l, err := Verify(w.log, w.term)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.Entries, w.ents) {
		t.Errorf("entries differ:\n got %+v\nwant %+v", l.Entries, w.ents)
	}
	if l.Summary != w.sum || l.Summary.Count != 4 {
		t.Errorf("summary = %+v, want %+v", l.Summary, w.sum)
	}
	if len(l.Evidence) != 1 || l.Evidence[0].Seq != 2 || !bytes.Equal(l.Evidence[0].Data, pngBytes) {
		t.Errorf("evidence = %+v", l.Evidence)
	}
	if len(w.term) >= MaxTerminationBytes {
		t.Errorf("termination message is %d bytes", len(w.term))
	}
}

func TestVerifyEmptyLedger(t *testing.T) {
	w := writeLedger(t, nil, nil)
	l, err := Verify(w.log, w.term)
	if err != nil || l.Summary.Count != 0 || len(l.Entries) != 0 {
		t.Fatalf("Verify(empty) = %+v, %v", l, err)
	}
}

// lines splits a log into its lines, each with its "\n".
func lines(log []byte) [][]byte {
	return bytes.SplitAfter(log, []byte("\n"))
}

func join(ls [][]byte) []byte { return bytes.Join(ls, nil) }

// entryLines are the indexes of the entry lines in ls.
func entryLines(ls [][]byte) []int {
	var out []int
	for i, l := range ls {
		if bytes.HasPrefix(l, []byte(EntryPrefix)) && !bytes.Contains(l, []byte(`"summary"`)) {
			out = append(out, i)
		}
	}
	return out
}

func TestVerifyRefuses(t *testing.T) {
	w := sample(t)
	ls := lines(w.log)
	idx := entryLines(ls)
	last := len(ls) - 2 // the summary line; the split leaves a trailing empty element
	cases := map[string]struct {
		log  func() []byte
		term string
		want error
	}{
		"tampered output": {log: func() []byte {
			c := bytes.Clone(w.log)
			return bytes.Replace(c, []byte(`button \"Play\"`), []byte(`button \"Vote\"`), 1)
		}, want: ErrInvalid},
		"tampered with a valid line": {log: func() []byte {
			forged := browserEntry()
			forged.Output = "- button \"Vote\" [ref=e12]\n"
			other := writeLedger(t, []LedgerEntry{browserEntry(), forged}, nil)
			ols := lines(other.log)
			c := append([][]byte(nil), ls...)
			c[idx[1]] = ols[entryLines(ols)[1]]
			return join(c)
		}, want: ErrMismatch},
		"reordered": {log: func() []byte {
			c := append([][]byte(nil), ls...)
			c[idx[0]], c[idx[1]] = c[idx[1]], c[idx[0]]
			return join(c)
		}, want: ErrInvalid},
		"truncated before the summary": {log: func() []byte {
			return join(ls[:idx[2]])
		}, want: ErrNoSummary},
		"an entry dropped": {log: func() []byte {
			c := append([][]byte(nil), ls[:idx[3]]...)
			return join(append(c, ls[idx[3]+1:]...))
		}, want: ErrMismatch},
		"an extra entry": {log: func() []byte {
			c := append([][]byte(nil), ls[:last]...)
			extra := writeLedger(t, []LedgerEntry{browserEntry(), browserEntry(), browserEntry(), browserEntry(),
				browserEntry()}, nil)
			els := lines(extra.log)
			c = append(c, els[entryLines(els)[4]])
			return join(append(c, ls[last:]...))
		}, want: ErrMismatch},
		"a line after the summary": {log: func() []byte {
			return append(bytes.Clone(w.log), ls[idx[0]]...)
		}, want: ErrInvalid},
		"two summaries": {log: func() []byte {
			return append(bytes.Clone(w.log), ls[last]...)
		}, want: ErrInvalid},
		"termination differs": {log: func() []byte { return w.log },
			term: `{"count":4,"sha256":"` + strings.Repeat("0", 64) + `"}`, want: ErrMismatch},
		"termination not json": {log: func() []byte { return w.log }, term: "oops", want: ErrInvalid},
		"termination too large": {log: func() []byte { return w.log },
			term: strings.Repeat(" ", MaxTerminationBytes) + w.term, want: ErrInvalid},
		"evidence bytes changed": {log: func() []byte {
			c := append([][]byte(nil), ls...)
			for i, l := range c {
				if bytes.HasPrefix(l, []byte(EvidencePrefix)) {
					data := base64.StdEncoding.EncodeToString(pngBytes)
					c[i] = bytes.Replace(l, []byte(data), []byte(base64.StdEncoding.EncodeToString([]byte("forged"))), 1)
				}
			}
			return join(c)
		}, want: ErrInvalid},
		"evidence citing a refused entry": {log: func() []byte {
			c := append([][]byte(nil), ls[:last]...)
			line := fmt.Sprintf("%s{\"v\":%d,\"seq\":4,\"sha256\":%q,\"data\":\"\"}\n", EvidencePrefix, WireVersion, sum(nil))
			c = append(c, []byte(line))
			return join(append(c, ls[last:]...))
		}, want: ErrInvalid},
		"another version": {log: func() []byte {
			return bytes.Replace(bytes.Clone(w.log), []byte(`{"v":2,`), []byte(`{"v":3,`), 1)
		}, want: ErrVersion},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			term := c.term
			if term == "" {
				term = w.term
			}
			_, err := Verify(c.log(), term)
			if !errors.Is(err, c.want) {
				t.Errorf("Verify() = %v, want %v", err, c.want)
			}
		})
	}
}

func TestParseSummaryBounds(t *testing.T) {
	for _, msg := range []string{
		`{"count":-1,"sha256":"` + strings.Repeat("a", 64) + `"}`,
		`{"count":1,"sha256":"` + strings.Repeat("A", 64) + `"}`,
		`{"count":1,"sha256":"` + strings.Repeat("a", 64) + `","extra":1}`,
		`{"count":1,"sha256":"abc"}`,
	} {
		if _, err := ParseSummary(msg); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseSummary(%s) = %v, want ErrInvalid", msg, err)
		}
	}
}

// genLedger is a random valid ledger: every kind, verb, refusal and check
// run shape, outputs with invalid UTF-8 and over the bounds, and evidence
// for some screenshots.
type genLedger struct {
	entries  []LedgerEntry
	evidence map[int64][]byte
}

func (genLedger) Generate(r *rand.Rand, _ int) reflect.Value {
	n := r.Intn(12)
	g := genLedger{evidence: map[int64][]byte{}}
	for i := range n {
		var e LedgerEntry
		switch r.Intn(4) {
		case 0:
			e = checkRunEntry(KindRelay, VerbCheckRun, ModeReplay, genAttempts(r)...)
		case 1:
			e = checkRunEntry(KindRelay, VerbConfirm, ModeConfirm, genAttempts(r)...)
		case 2:
			e = LedgerEntry{TS: ts, Kind: KindAgent, Verb: VerbBrowser, Args: []byte(`{}`),
				Refused: "verb-not-allowed"}
		default:
			data := randBytes(r, r.Intn(64))
			e = browserEntry()
			e.Artifacts = []Artifact{{Name: "s.png", SHA256: sum(data), Bytes: int64(len(data))}}
			e.Output = string(randBytes(r, r.Intn(200)))
			if r.Intn(10) == 0 {
				e.Output = strings.Repeat("z", MaxOutputBytes+1)
			}
			e.Exit = r.Intn(3)
			if len(g.evidence) < MaxEvidence && r.Intn(2) == 0 {
				g.evidence[int64(i+1)] = data
			}
		}
		e.TS = ts.Add(time.Duration(i) * time.Millisecond)
		g.entries = append(g.entries, e)
	}
	return reflect.ValueOf(g)
}

func genAttempts(r *rand.Rand) []AttemptResult {
	first := []AttemptResult{ResultPass, ResultFail, ResultError}[r.Intn(3)]
	if first == ResultPass || r.Intn(3) == 0 {
		return []AttemptResult{first}
	}
	return []AttemptResult{first, []AttemptResult{ResultPass, ResultFail, ResultError}[r.Intn(3)]}
}

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return b
}

// TestPropertyRoundTripAndTamper: every written ledger verifies to exactly
// what was written, and changing, dropping, duplicating or swapping any
// one entry line makes Verify fail.
func TestPropertyRoundTripAndTamper(t *testing.T) {
	prop := func(g genLedger, pick uint16, how uint8) bool {
		w := writeLedger(t, g.entries, g.evidence)
		l, err := Verify(w.log, w.term)
		if err != nil || !reflect.DeepEqual(l.Entries, w.ents) || len(l.Evidence) != len(g.evidence) {
			t.Logf("round trip: %v", err)
			return false
		}
		ls := lines(w.log)
		idx := entryLines(ls)
		if len(idx) == 0 {
			return true
		}
		i := idx[int(pick)%len(idx)]
		c := append([][]byte(nil), ls...)
		switch how % 4 {
		case 0: // flip one byte inside the JSON
			line := bytes.Clone(c[i])
			p := len(EntryPrefix) + int(pick)%(len(line)-len(EntryPrefix)-1)
			line[p] ^= 0x01
			c[i] = line
		case 1: // drop it
			c = append(c[:i], c[i+1:]...)
		case 2: // duplicate it
			c = append(c[:i+1], c[i:]...)
		case 3: // swap with the next entry line, or drop when it is the only one
			if len(idx) == 1 {
				c = append(c[:i], c[i+1:]...)
				break
			}
			j := idx[(int(pick)+1)%len(idx)]
			c[i], c[j] = c[j], c[i]
		}
		if _, err := Verify(join(c), w.term); err == nil {
			t.Logf("tamper %d on line %d accepted", how%4, i)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(seed))}); err != nil {
		t.Error(err)
	}
}
