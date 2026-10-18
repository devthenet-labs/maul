// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakedex

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestAuthorizeAsDex holds /auth to what a Dex static client answers: a
// bare authorization request (no PKCE, no nonce) is its login page, a
// redirect URI the client does not register is refused with Dex's words,
// and a full request still signs the viewer in and redirects back.
func TestAuthorizeAsDex(t *testing.T) {
	d, err := Start("relay", "relay-secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	const callback = "https://preview-auth.acme.test/dex/callback"
	d.Register(callback)
	d.SignIn(map[string]any{"sub": "alice"})
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(q url.Values) (int, string, string) {
		t.Helper()
		resp, err := c.Get(d.URL + "/auth?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck // a test read; the status and body are what is judged
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Location"), string(body)
	}
	bare := url.Values{"client_id": {"relay"}, "redirect_uri": {callback}, "response_type": {"code"},
		"scope": {"openid"}, "state": {"s"}}
	if status, _, body := get(bare); status != http.StatusOK || !strings.Contains(body, LoginPage) {
		t.Errorf("a bare request: %d %q, want the login page", status, body)
	}
	other := url.Values{"client_id": {"relay"}, "redirect_uri": {"https://evil.test/cb"}, "response_type": {"code"}}
	if status, _, body := get(other); status != http.StatusBadRequest || !strings.Contains(body, "redirect_uri") {
		t.Errorf("an unregistered redirect URI: %d %q", status, body)
	}
	stranger := url.Values{"client_id": {"other"}, "redirect_uri": {callback}, "response_type": {"code"}}
	if status, _, _ := get(stranger); status != http.StatusBadRequest {
		t.Errorf("another client: %d, want 400", status)
	}
	full := url.Values{"client_id": {"relay"}, "redirect_uri": {callback}, "response_type": {"code"},
		"code_challenge": {"c"}, "code_challenge_method": {"S256"}, "nonce": {"n"}, "state": {"s"}}
	if status, location, _ := get(full); status != http.StatusSeeOther || !strings.HasPrefix(location, callback+"?") ||
		!strings.Contains(location, "code=") {
		t.Errorf("a full request: %d to %q, want a redirect back with a code", status, location)
	}
}
