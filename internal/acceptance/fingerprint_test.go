// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"strings"
	"testing"
)

// TestOutcomeFingerprintGoldens pins fingerprint/2 to its documented
// canonical bytes, sha256 taken outside Go: {"o":[{"id":…,"d":…},…]},
// sorted by id, duplicates dropped.
func TestOutcomeFingerprintGoldens(t *testing.T) {
	elo := OutcomeRef{ID: "elo", Digest: "sha256:aa", Manual: true}
	rank := OutcomeRef{ID: "rank", Digest: "sha256:bb", Keep: true}
	for _, tt := range []struct {
		name string
		got  string
		want string
	}{
		{"two outcomes", OutcomeFingerprint([]OutcomeRef{rank, elo, rank}),
			"maul-evaluation-fingerprint/2:b8a4f4e6bbfd94c05ed122d523a98542fde8fccc8da91209ad70dc7cf55c0bc1"},
		{"one outcome", OutcomeFingerprint([]OutcomeRef{elo}),
			"maul-evaluation-fingerprint/2:da5ba5a2a329079309a6378b26abaa38247254cf7b7f710e5fb83c6d4be3372b"},
		{"none", OutcomeFingerprint(nil),
			"maul-evaluation-fingerprint/2:af4f11051ab9c47a5ff25fad91f666380c23ddf997fa8aa71e30a14ee6458d01"},
		{"piece", OutcomeFingerprintPiece(elo),
			"o2:elo maul-evaluation-fingerprint/2:da5ba5a2a329079309a6378b26abaa38247254cf7b7f710e5fb83c6d4be3372b"},
	} {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// TestOutcomeFingerprint: fingerprint/2 names the failing outcomes by id and
// digest in any order, and is apart from an earlier release's
// fingerprint/1, as a v2 piece is from a v1 one ("o:").
func TestOutcomeFingerprint(t *testing.T) {
	a, b := OutcomeRef{ID: "elo", Digest: "sha256:aa"}, OutcomeRef{ID: "rank", Digest: "sha256:bb"}
	base := OutcomeFingerprint([]OutcomeRef{a, b})
	const v1 = "maul-evaluation-fingerprint/1"
	if !strings.HasPrefix(base, OutcomeFingerprintVersion+":") || OutcomeFingerprintVersion == v1 {
		t.Fatalf("fingerprint %q is not apart from %s", base, v1)
	}
	for _, tt := range []struct {
		name string
		refs []OutcomeRef
	}{
		{"order", []OutcomeRef{b, a}},
		{"duplicates", []OutcomeRef{a, b, a}},
		{"keep and manual flags", []OutcomeRef{{ID: "elo", Digest: "sha256:aa", Keep: true, Manual: true}, b}},
	} {
		if got := OutcomeFingerprint(tt.refs); got != base {
			t.Errorf("%s changed the fingerprint", tt.name)
		}
	}
	for _, tt := range []struct {
		name string
		refs []OutcomeRef
	}{
		{"another outcome set", []OutcomeRef{a}},
		{"a changed digest", []OutcomeRef{a, {ID: "rank", Digest: "sha256:cc"}}},
		{"digests swapped between outcomes",
			[]OutcomeRef{{ID: "elo", Digest: "sha256:bb"}, {ID: "rank", Digest: "sha256:aa"}}},
		{"an id running into its digest", []OutcomeRef{{ID: "elo sha256:aa", Digest: ""}, b}},
	} {
		if got := OutcomeFingerprint(tt.refs); got == base {
			t.Errorf("%s left the fingerprint unchanged", tt.name)
		}
	}
	if p := OutcomeFingerprintPiece(a); strings.HasPrefix(p, "o:") || !strings.HasPrefix(p, OutcomePiecePrefix+a.ID+" ") {
		t.Errorf("piece %q is not a v2 piece apart from v1's", p)
	}
}
