// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"encoding/json"
	"os"
	"testing"
)

// SavedFixture is one check as an Intent's check store keeps it.
type savedFixture struct {
	ID     string `json:"id"`
	Body   string `json:"body"`
	Digest string `json:"digest"`
}

// loadSaved reads testdata/saved-gate4.json: three checks saved by defect
// gate 4 (beatbattle-101's fresh-no-line-ui and line-not-covering-ui,
// beatbattle-103's tempo-loaded-beat-page) as their immutable check store
// ConfigMaps hold them, written and proven before a label or role name
// could be exact.
func loadSaved(t *testing.T) []savedFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/saved-gate4.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []savedFixture
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSavedChecksKeepTheirForm: a check saved before exact matching parses
// to the very bytes and digest it was saved with, so the check store, the
// PreviewVerification and the tools config still accept it, and none of
// its locators reads as exact: it replays as it was proven.
func TestSavedChecksKeepTheirForm(t *testing.T) {
	for _, s := range loadSaved(t) {
		t.Run(s.ID, func(t *testing.T) {
			c, err := Parse([]byte(s.Body))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(Canonical(c)); got != s.Body {
				t.Errorf("Canonical changed the saved body:\n got %s\nwant %s", got, s.Body)
			}
			if got := Digest(c); got != s.Digest {
				t.Errorf("Digest = %s, saved %s", got, s.Digest)
			}
			if err := Validate(c, slotCtx); err != nil {
				t.Errorf("the saved check no longer validates: %v", err)
			}
			if err := Substantive(c); err != nil {
				t.Errorf("the saved check is no longer substantive: %v", err)
			}
		})
	}
}
