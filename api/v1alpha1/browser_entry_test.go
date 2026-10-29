// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"strings"
	"testing"
)

func TestValidBrowserEntry(t *testing.T) {
	for entry, want := range map[string]bool{
		"/": true, "/battle": true, "/a/b?x=1&y=2#z": true, "/" + strings.Repeat("a", 255): true,
		"": false, "battle": false, "//evil.example": false, "http://evil.example": false, "/\\evil": false,
		"/a b": false, "/a`b": false, "/a\nb": false, "/a\x00": false, "/" + strings.Repeat("a", 256): false, "/é": false,
	} {
		if got := ValidBrowserEntry(entry); got != want {
			t.Errorf("ValidBrowserEntry(%q) = %v, want %v", entry, got, want)
		}
	}
}
