// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Digest binds a criteria list: "sha256:" and the hex sha256 of the
// canonical JSON of the FULL list (manual criteria included, plan order
// kept), normalised first, so the shorthand and its one-step form, and
// expected values however they were spelled, digest alike. An empty list
// digests "[]" (v1alpha1.EmptyListDigest). intent-controller records it as
// IntentAcceptance.Digest: the empty list's for every plan the evaluator
// records, a criteria list's only for a plan nothing verifies.
func Digest(cs []Criterion) string {
	n := Normalize(cs)
	if n == nil {
		n = []Criterion{}
	}
	b, err := json.Marshal(n)
	if err != nil {
		// Unreachable: every field is a plain value or a canonical Value.
		b = []byte("!unencodable")
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
