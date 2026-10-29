// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// reservedEntries is the room the ledger keeps past the agent's calls: the
// runner's confirm, one confirmation per check it may name, and the
// finish. An agent that spends its calls on refusals cannot crowd out the
// entries a pass rests on.
const reservedEntries = toolwire.MaxConfirm + 2

// ArtifactStore holds the bytes of the artifacts entries record (the
// browser relay's screenshots), by sha256. Only cited ones leave the tools
// container, as evidence lines at finish.
type ArtifactStore interface {
	Read(digest string) ([]byte, error)
}

// errNoRoom is an agent call the ledger has no room left to record.
var errNoRoom = errors.New("the ledger keeps its remaining room for confirmation")

// record appends e to the ledger, stamped now; the caller holds r.mu. It
// returns the entry's seq.
func (r *Relay) record(e toolwire.LedgerEntry) (int64, error) {
	if e.Kind == toolwire.KindAgent && r.entries >= toolwire.MaxEntries-reservedEntries {
		return 0, errNoRoom
	}
	e.TS = r.now()
	got, err := r.ledger.Append(e)
	if err != nil {
		return 0, err
	}
	r.entries++
	r.noteArtifacts(got.Seq, got)
	return got.Seq, nil
}

// closeLedger writes the cited evidence and the summary, then the summary
// again as the termination message. The caller holds r.mu and has
// recorded the finish entry.
func (r *Relay) closeLedger(evidence []evidenceBytes) (toolwire.Summary, error) {
	for _, ev := range evidence {
		if err := r.ledger.Evidence(ev.seq, ev.data); err != nil {
			return toolwire.Summary{}, fmt.Errorf("evidence of entry %d: %w", ev.seq, err)
		}
	}
	sum, err := r.ledger.Close()
	if err != nil {
		return toolwire.Summary{}, err
	}
	if r.termPath != "" {
		if err := os.WriteFile(r.termPath, []byte(sum.TerminationMessage()), 0o600); err != nil {
			return sum, fmt.Errorf("write termination message: %w", err)
		}
	}
	return sum, nil
}

// evidenceBytes is one cited image, read and checked.
type evidenceBytes struct {
	seq  int64
	data []byte
}

// gatherEvidence reads each cited image: an artifact the ledger recorded
// on that entry, whose bytes still hash to it. What it cannot emit it
// names in maul's words.
func (r *Relay) gatherEvidence(refs []toolwire.EvidenceRef) ([]evidenceBytes, []string) {
	var out []evidenceBytes
	var notes []string
	seen := map[string]bool{}
	for _, ref := range refs {
		key := fmt.Sprintf("%d/%s", ref.Seq, ref.SHA256)
		switch {
		case seen[key]:
			notes = append(notes, fmt.Sprintf("evidence %s: cited twice", key))
			continue
		case !r.artifacts[ref.Seq][ref.SHA256]:
			notes = append(notes, fmt.Sprintf("evidence %s: entry %d recorded no such artifact", key, ref.Seq))
			continue
		case r.store == nil:
			notes = append(notes, fmt.Sprintf("evidence %s: no artifact store", key))
			continue
		}
		data, err := r.store.Read(ref.SHA256)
		if err != nil || sha256Hex(data) != ref.SHA256 || len(data) > toolwire.MaxEvidenceBytes {
			notes = append(notes, fmt.Sprintf("evidence %s: bytes unavailable or changed", key))
			continue
		}
		seen[key] = true
		out = append(out, evidenceBytes{seq: ref.Seq, data: data})
	}
	return out, notes
}

// noteArtifacts remembers the artifacts an unrefused entry recorded, so
// finish emits only those.
func (r *Relay) noteArtifacts(seq int64, e toolwire.LedgerEntry) {
	if e.Refused != "" || len(e.Artifacts) == 0 {
		return
	}
	set := map[string]bool{}
	for _, a := range e.Artifacts {
		set[a.SHA256] = true
	}
	r.artifacts[seq] = set
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// bareDigestOf is a check digest's hex, as the ledger records it.
func bareDigestOf(digest string) string { return strings.TrimPrefix(digest, "sha256:") }
