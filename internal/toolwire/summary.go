// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
)

// MaxTerminationBytes bounds the termination message: the kubelet keeps
// 4096 bytes of it.
const MaxTerminationBytes = 4096

// Summary closes the ledger: how many entry lines came before it and the
// sha256 of those lines in order, each with its "\n". It is the last log
// line and the tools container's termination message, which the log
// cannot forge: only the container's own process writes it.
type Summary struct {
	Count  int64  `json:"count"`
	SHA256 string `json:"sha256"`
}

// TerminationMessage is the summary as the termination message.
func (s Summary) TerminationMessage() string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (s Summary) check() error {
	if s.Count < 0 || s.Count > MaxEntries || !isSHA256(s.SHA256) {
		return errors.New("summary out of bounds")
	}
	return nil
}

// Verify errors. A collector maps ErrNoSummary and every other error to
// a ledger integrity failure (Infra, DESIGN 4.3).
var (
	// ErrInvalid is a line, an entry, an evidence line or the termination
	// message that is not the ledger's grammar or breaks its bounds.
	ErrInvalid = errors.New("tools output is not a valid MAUL-TOOLS ledger")
	// ErrNoSummary is a log with no summary line: truncated, or the tools
	// container never finished.
	ErrNoSummary = errors.New("tools output has no summary")
	// ErrMismatch is entry lines that do not match the summary, or a
	// summary that is not the termination message.
	ErrMismatch = errors.New("tools output does not match its summary")
	// ErrVersion is a line of another WireVersion.
	ErrVersion = errors.New("tools output is another MAUL-TOOLS version")
)

// Ledger is a verified ledger.
type Ledger struct {
	Entries  []LedgerEntry
	Evidence []Evidence
	Summary  Summary
}

// ParseSummary reads a termination message.
func ParseSummary(msg string) (Summary, error) {
	var s Summary
	if len(msg) >= MaxTerminationBytes || decodeStrict([]byte(msg), &s) != nil || s.check() != nil {
		return Summary{}, ErrInvalid
	}
	return s, nil
}

// Verify reads the tools container's log against its termination message.
// Lines without a ledger prefix are ignored. The entry lines must be
// within their bounds, numbered 1, 2, 3, … in order, at most MaxEntries
// and MaxLedgerBytes; each evidence line must carry bytes whose sha256 is
// an artifact of an earlier, unrefused entry, at most MaxEvidence lines;
// exactly one summary line must follow them all, its count and sha256
// those of the entry lines, and equal to the termination message. Any
// change, reordering, truncation or extra line fails.
func Verify(log []byte, termination string) (Ledger, error) {
	term, err := ParseSummary(termination)
	if err != nil {
		return Ledger{}, fmt.Errorf("%w: termination message", ErrInvalid)
	}
	v := verifier{h: sha256.New(), artifacts: map[int64]map[string]bool{}, seen: map[string]bool{}}
	for _, line := range bytes.SplitAfter(log, []byte("\n")) {
		if err := v.line(bytes.TrimSuffix(line, []byte("\n"))); err != nil {
			return Ledger{}, err
		}
	}
	if !v.summed {
		return Ledger{}, ErrNoSummary
	}
	s := v.out.Summary
	if s.Count != int64(len(v.out.Entries)) || s.SHA256 != hex.EncodeToString(v.h.Sum(nil)) {
		return Ledger{}, fmt.Errorf("%w: entry lines", ErrMismatch)
	}
	if s != term {
		return Ledger{}, fmt.Errorf("%w: termination message", ErrMismatch)
	}
	return v.out, nil
}

type verifier struct {
	out       Ledger
	h         hash.Hash
	bytes     int
	summed    bool
	artifacts map[int64]map[string]bool
	seen      map[string]bool
}

func (v *verifier) line(line []byte) error {
	if body, ok := bytes.CutPrefix(line, []byte(EvidencePrefix)); ok {
		if v.summed {
			return fmt.Errorf("%w: a line after the summary", ErrInvalid)
		}
		return v.evidence(body)
	}
	body, ok := bytes.CutPrefix(line, []byte(EntryPrefix))
	if !ok {
		return nil
	}
	if v.summed {
		return fmt.Errorf("%w: a line after the summary", ErrInvalid)
	}
	if err := checkVersion(body); err != nil {
		return err
	}
	var l entryLine
	if decodeStrict(body, &l) != nil || (l.Entry == nil) == (l.Summary == nil) {
		return ErrInvalid
	}
	if l.Summary != nil {
		if l.Summary.check() != nil {
			return ErrInvalid
		}
		v.out.Summary, v.summed = *l.Summary, true
		return nil
	}
	return v.entry(*l.Entry, line)
}

func (v *verifier) entry(e LedgerEntry, line []byte) error {
	v.bytes += len(line) + 1
	switch {
	case len(v.out.Entries) >= MaxEntries || v.bytes > MaxLedgerBytes:
		return fmt.Errorf("%w: too many entries", ErrInvalid)
	case e.Seq != int64(len(v.out.Entries))+1:
		return fmt.Errorf("%w: entry %d out of order", ErrInvalid, e.Seq)
	}
	if err := e.Check(); err != nil {
		return fmt.Errorf("%w: entry %d: %v", ErrInvalid, e.Seq, err)
	}
	v.h.Write(line)
	v.h.Write([]byte("\n"))
	if e.Refused == "" && len(e.Artifacts) > 0 {
		set := map[string]bool{}
		for _, a := range e.Artifacts {
			set[a.SHA256] = true
		}
		v.artifacts[e.Seq] = set
	}
	v.out.Entries = append(v.out.Entries, e)
	return nil
}

func (v *verifier) evidence(body []byte) error {
	if err := checkVersion(body); err != nil {
		return err
	}
	var l evidenceLine
	if decodeStrict(body, &l) != nil {
		return ErrInvalid
	}
	ev := l.Evidence
	switch {
	case len(v.out.Evidence) >= MaxEvidence || len(ev.Data) > MaxEvidenceBytes:
		return fmt.Errorf("%w: evidence out of bounds", ErrInvalid)
	case ev.SHA256 != sha256Hex(ev.Data):
		return fmt.Errorf("%w: evidence does not match its sha256", ErrInvalid)
	case !v.artifacts[ev.Seq][ev.SHA256]:
		return fmt.Errorf("%w: evidence cites no artifact of entry %d", ErrInvalid, ev.Seq)
	case v.seen[evidenceKey(ev)]:
		return fmt.Errorf("%w: evidence repeated", ErrInvalid)
	}
	v.seen[evidenceKey(ev)] = true
	v.out.Evidence = append(v.out.Evidence, ev)
	return nil
}

// checkVersion refuses a line of another version as ErrVersion, before its
// grammar is judged.
func checkVersion(body []byte) error {
	var version struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(body, &version); err != nil {
		return ErrInvalid
	}
	if version.V != WireVersion {
		return fmt.Errorf("%w: v%d, this reader reads v%d", ErrVersion, version.V, WireVersion)
	}
	return nil
}

// decodeStrict decodes exactly one JSON value with no unknown fields.
func decodeStrict(b []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}
