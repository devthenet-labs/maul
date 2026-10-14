// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSourceIP(t *testing.T) {
	cases := map[string]string{
		"10.0.0.7:41234":  "10.0.0.7",
		"[fd00::1]:8080":  "fd00::1",
		"10.0.0.7":        "10.0.0.7",
		"@unix-socket-id": "@unix-socket-id",
	}
	for remote, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if got := SourceIP(r); got != want {
			t.Errorf("SourceIP(%q) = %q, want %q", remote, got, want)
		}
	}
}

// TestSourceIPIgnoresForwardingHeaders: the caller controls those, so they
// are never a rate key.
func TestSourceIPIgnoresForwardingHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.7:41234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Real-Ip", "1.2.3.4")
	r.Header.Set("Forwarded", "for=1.2.3.4")
	if got := SourceIP(r); got != "10.0.0.7" {
		t.Fatalf("SourceIP = %q, want the RemoteAddr host", got)
	}
}
