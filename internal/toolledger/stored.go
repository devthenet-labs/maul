// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"encoding/json"
	"strings"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// StoredLine is one line of a verification's <pv>-ledger ConfigMap as a
// display reads it: what the call sent and what came back, nothing more.
// It is read leniently, any other field ignored, so a line an earlier
// release stored (a check run's record, say) still reads once that wire
// is gone. It is never a fact maul judges by: the verified index on the
// verification's status is, and Of holds a line to it.
type StoredLine struct {
	Seq  int64  `json:"seq"`
	Kind string `json:"kind"`
	Verb string `json:"verb"`
	// Side is "base" for a call at an agents round's base tree, else "".
	Side string `json:"side,omitempty"`
	// Args is the request as the caller sent it, JSON.
	Args         json.RawMessage `json:"args"`
	Output       string          `json:"output"`
	OutputSHA256 string          `json:"outputSHA256"`
}

// ReadStored reads a <pv>-ledger ConfigMap's lines (its Key) by seq, for
// display only. A line that does not decode, or whose seq is outside
// 1..toolwire.MaxEntries, is skipped; of two lines of one seq the first is
// kept.
func ReadStored(data string) map[int64]StoredLine {
	out := map[int64]StoredLine{}
	for line := range strings.SplitSeq(data, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var l StoredLine
		if json.Unmarshal([]byte(line), &l) != nil || l.Seq < 1 || l.Seq > toolwire.MaxEntries {
			continue
		}
		if _, dup := out[l.Seq]; !dup {
			out[l.Seq] = l
		}
	}
	return out
}

// Of reports the line as the call the verified index entry ix records:
// the same result digest and verb. Only then is what it sent shown as that
// call's.
func (l StoredLine) Of(ix v1alpha1.LedgerIndexEntry) bool {
	return l.OutputSHA256 == ix.ResultSHA256 && l.Verb == string(ix.Verb)
}

// OutputKept reports the line's output as the one its digest names: an
// output dropped to fit the ConfigMap keeps only its digest.
func (l StoredLine) OutputKept() bool { return sha256Hex([]byte(l.Output)) == l.OutputSHA256 }
