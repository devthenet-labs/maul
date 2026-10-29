// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"strings"
	"testing"
)

func TestArtifactPath(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	if got, want := ArtifactPath(digest), "/v1/artifacts/"+digest; got != want {
		t.Errorf("ArtifactPath = %q, want %q", got, want)
	}
	for name, d := range map[string]string{
		"empty":     "",
		"short":     "abc",
		"uppercase": strings.Repeat("AB", 32),
		"traversal": "../" + strings.Repeat("a", 61),
	} {
		if got := ArtifactPath(d); got != "" {
			t.Errorf("%s: ArtifactPath(%q) = %q, want none", name, d, got)
		}
	}
}

func TestIsScreenshot(t *testing.T) {
	for name, want := range map[string]bool{
		"page.png":      true,
		"shot-1.PNG":    true,
		"snapshot.yml":  false,
		"png":           false,
		"page.png.yml":  false,
		"console.log":   false,
		"page-2026.png": true,
	} {
		if got := IsScreenshot(Artifact{Name: name}); got != want {
			t.Errorf("IsScreenshot(%q) = %v, want %v", name, got, want)
		}
	}
}
