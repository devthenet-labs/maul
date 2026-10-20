// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"slices"
	"testing"
)

func TestDNSRecords(t *testing.T) {
	got := DNSRecords("acme-previews.dev", "preview-auth.acme.dev",
		[]string{"100.101.12.58", "fd7a:115c:a1e0::3a", "not-an-ip", "::ffff:100.64.0.1"})
	want := []string{
		"*.acme-previews.dev. A 100.101.12.58",
		"*.acme-previews.dev. AAAA fd7a:115c:a1e0::3a",
		"*.acme-previews.dev. A 100.64.0.1",
		"preview-auth.acme.dev. A 100.101.12.58",
		"preview-auth.acme.dev. AAAA fd7a:115c:a1e0::3a",
		"preview-auth.acme.dev. A 100.64.0.1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("DNSRecords = %q", got)
	}
	if got := DNSRecords("", "", []string{"100.64.0.1"}); len(got) != 0 {
		t.Errorf("no names: %q", got)
	}
}
