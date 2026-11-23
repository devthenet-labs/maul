// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package maultool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckVerbsAreRetired: `check run` and `check results` are gone, a
// usage error that sends nothing, however well formed; the usage text no
// longer offers them.
func TestCheckVerbsAreRetired(t *testing.T) {
	scratch := t.TempDir()
	check := `{"id":"x.y","outcome":"x","form":"exec","exec":{"argv":["true"],"expect":{"exit":0}}}`
	if err := os.WriteFile(filepath.Join(scratch, "check.json"), []byte(check), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"check run":        {"check", "run", "check.json"},
		"check run --base": {"check", "run", "--base", "check.json"},
		"check results":    {"check", "results"},
	} {
		t.Run(name, func(t *testing.T) {
			r, srv := newRelay(t)
			res := run(t, srv.URL, scratch, args...)
			if res.code != ExitUsage || r.count() != 0 {
				t.Errorf("exit %d, %d calls reached the relay (stderr %q)", res.code, r.count(), res.stderr)
			}
		})
	}
	r, srv := newRelay(t)
	res := run(t, srv.URL, scratch)
	if r.count() != 0 || strings.Contains(res.stderr, "check") {
		t.Errorf("the usage text still offers the check verbs:\n%s", res.stderr)
	}
}
