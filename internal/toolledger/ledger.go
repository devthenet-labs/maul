// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

const (
	// Key holds the ledger's entries in its ConfigMap, one JSON line each;
	// SummaryKey the summary they were verified against.
	Key        = "ledger.jsonl"
	SummaryKey = "summary.json"
	// MaxConfigMapBytes bounds what a ledger or evidence ConfigMap holds
	// (SPEC Q7, Q8), under the API server's 1 MiB.
	MaxConfigMapBytes = 900 << 10
	// DroppedOutput replaces an output dropped to fit the ConfigMap; the
	// entry keeps its outputSHA256.
	DroppedOutput = "[maul: output dropped to fit; its sha256 is kept]"
	// droppedArgs replaces args dropped to fit, once every output is gone.
	droppedArgs = `{"maul":"args dropped to fit"}`
)

// Output is what Parse read: the verified ledger, the evidence lines that
// were well formed, how many were not, and the reason the log does not
// hold ("" when it does).
type Output struct {
	Ledger   toolwire.Ledger
	Evidence []toolwire.Evidence
	Dropped  int
	Reason   v1alpha1.PreviewVerificationReason
}

// evidenceLine is an evidence line's JSON (toolwire's wire, read here so
// that one bad image drops alone instead of failing the ledger).
type evidenceLine struct {
	V      int    `json:"v"`
	Seq    int64  `json:"seq"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}

// Parse reads the tools container's log against its termination message.
// Evidence lines are set apart first (they are not part of the summary's
// hash): one whose bytes do not hash to its sha256, of another version, or
// malformed, is dropped and counted. The rest must verify.
func Parse(log []byte, msg string) Output {
	if len(log) > toolwire.MaxLogBytes {
		return Output{Reason: v1alpha1.VerificationReasonLedgerMismatch}
	}
	var rest bytes.Buffer
	var out Output
	for _, line := range bytes.SplitAfter(log, []byte("\n")) {
		body, ok := bytes.CutPrefix(line, []byte(toolwire.EvidencePrefix))
		if !ok {
			rest.Write(line)
			continue
		}
		var ev evidenceLine
		if err := json.Unmarshal(bytes.TrimSuffix(body, []byte("\n")), &ev); err != nil ||
			ev.V != toolwire.WireVersion || len(ev.Data) > toolwire.MaxEvidenceBytes || ev.SHA256 != sha256Hex(ev.Data) {
			out.Dropped++
			continue
		}
		out.Evidence = append(out.Evidence, toolwire.Evidence{Seq: ev.Seq, SHA256: ev.SHA256, Data: ev.Data})
	}
	l, err := toolwire.Verify(rest.Bytes(), msg)
	switch {
	case errors.Is(err, toolwire.ErrNoSummary):
		out.Reason = v1alpha1.VerificationReasonDeadlineNoSummary
	case err != nil:
		out.Reason = v1alpha1.VerificationReasonLedgerMismatch
	}
	out.Ledger = l
	return out
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ClampInt32 is n within int32.
func ClampInt32[T int | int64](n T) int32 {
	return int32(max(min(int64(n), math.MaxInt32), math.MinInt32))
}

// Index indexes every entry, in order: its facts without its output, its
// side as the ledger records it (base, or none for the head), and whether
// it is one of the evaluator's calls that got no answer (noAnswer).
func Index(entries []toolwire.LedgerEntry) []v1alpha1.LedgerIndexEntry {
	out := make([]v1alpha1.LedgerIndexEntry, 0, len(entries))
	for _, e := range entries {
		ie := v1alpha1.LedgerIndexEntry{Seq: ClampInt32(e.Seq), Kind: v1alpha1.LedgerKind(e.Kind),
			Verb: v1alpha1.LedgerVerb(e.Verb), Exit: ClampInt32(e.Exit), ResultSHA256: e.OutputSHA256,
			Refused: e.Refused != "", Side: v1alpha1.TreeSide(e.Side.Recorded()), NoAnswer: noAnswer(e)}
		for _, a := range e.Artifacts {
			ie.Artifacts = append(ie.Artifacts, a.SHA256)
		}
		out = append(out, ie)
	}
	return out
}

// noAnswer reports one of the evaluator's unrefused calls that got nothing
// back from what it exercised: an http request whose output holds no
// answered exchange (it got no response), or a run whose program never ran.
func noAnswer(e toolwire.LedgerEntry) bool {
	if e.Kind != toolwire.KindAgent || e.Refused != "" {
		return false
	}
	switch e.Verb {
	case toolwire.VerbHTTP:
		return !toolwire.HTTPAnswered(e.Output)
	case toolwire.VerbRun:
		return e.NotRun
	}
	return false
}

// Readiness is the relay's readiness wait, its first entry: a relay http
// entry whose args name the readiness and whose output is the waits.
func Readiness(entries []toolwire.LedgerEntry) []httpstep.TargetWait {
	if len(entries) == 0 {
		return nil
	}
	e := entries[0]
	var args struct {
		Readiness json.RawMessage `json:"readiness"`
	}
	if e.Kind != toolwire.KindRelay || e.Verb != toolwire.VerbHTTP || json.Unmarshal(e.Args, &args) != nil ||
		args.Readiness == nil {
		return nil
	}
	var waits []httpstep.TargetWait
	if json.Unmarshal([]byte(e.Output), &waits) != nil {
		return nil
	}
	return waits
}

// ConfigMapData is the full ledger as a ConfigMap's data: each entry one
// JSON line, in order, under Key, and the summary under SummaryKey. Past
// MaxConfigMapBytes the oldest entries' outputs and inline text go first,
// then their args; every entry keeps its line, its seq and its
// outputSHA256.
func ConfigMapData(l toolwire.Ledger) (map[string]string, error) {
	entries := slices.Clone(l.Entries)
	sum, err := json.Marshal(l.Summary)
	if err != nil {
		return nil, err
	}
	lines := make([][]byte, len(entries))
	size := len(sum) + len(Key) + len(SummaryKey)
	for i := range entries {
		if lines[i], err = json.Marshal(entries[i]); err != nil {
			return nil, fmt.Errorf("encode ledger entry %d: %w", entries[i].Seq, err)
		}
		size += len(lines[i]) + 1
	}
	for _, drop := range []func(*toolwire.LedgerEntry){
		func(e *toolwire.LedgerEntry) { e.Output, e.Inline = DroppedOutput, "" },
		func(e *toolwire.LedgerEntry) { e.Args = json.RawMessage(droppedArgs) },
	} {
		for i := 0; i < len(entries) && size > MaxConfigMapBytes; i++ {
			drop(&entries[i])
			line, err := json.Marshal(entries[i])
			if err != nil {
				return nil, err
			}
			size += len(line) - len(lines[i])
			lines[i] = line
		}
	}
	if size > MaxConfigMapBytes {
		return nil, errors.New("the ledger's index lines alone are over the ConfigMap bound")
	}
	var b bytes.Buffer
	for _, line := range lines {
		b.Write(line)
		b.WriteByte('\n')
	}
	return map[string]string{Key: b.String(), SummaryKey: string(sum)}, nil
}
