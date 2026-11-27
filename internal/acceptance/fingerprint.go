// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
)

// OutcomeFingerprintVersion prefixes every observed-evaluator fingerprint
// (Judge's). An earlier release's were maul-evaluation-fingerprint/1, over
// saved checks and the environment too: a fingerprint, or a piece,
// recorded before the observed evaluator never matches one taken after it,
// so the first failure after the upgrade is never a repeat.
const OutcomeFingerprintVersion = "maul-evaluation-fingerprint/2"

// OutcomePiecePrefix starts an outcome's fingerprint piece
// (OutcomeFingerprintPiece); slice-era pieces start "o:".
const OutcomePiecePrefix = "o2:"

// OutcomeFingerprint names an observed round's failure for RepeatedFailure
// detection: sha256 over the failing outcomes' ids, each with its outcome
// digest, sorted, duplicates dropped. Nothing the evaluator chose (its
// citations, note or call arguments) goes in, and no environment: a fix
// is asked only of a clean environment.
func OutcomeFingerprint(failing []OutcomeRef) string {
	type piece struct {
		ID     string `json:"id"`
		Digest string `json:"d"`
	}
	pieces := make([]piece, 0, len(failing))
	for _, o := range failing {
		pieces = append(pieces, piece{ID: o.ID, Digest: o.Digest})
	}
	sort.Slice(pieces, func(i, j int) bool {
		if pieces[i].ID != pieces[j].ID {
			return pieces[i].ID < pieces[j].ID
		}
		return pieces[i].Digest < pieces[j].Digest
	})
	doc := struct {
		Outcomes []piece `json:"o"`
	}{slices.Compact(pieces)}
	b, _ := json.Marshal(doc) // plain structs: cannot fail
	sum := sha256.Sum256(b)
	return OutcomeFingerprintVersion + ":" + hex.EncodeToString(sum[:])
}

// OutcomeFingerprintPiece is one failing outcome's piece of a fix round's
// recorded failure: OutcomePiecePrefix, its id, a space and the
// OutcomeFingerprint of it alone.
func OutcomeFingerprintPiece(o OutcomeRef) string {
	return OutcomePiecePrefix + o.ID + " " + OutcomeFingerprint([]OutcomeRef{o})
}
