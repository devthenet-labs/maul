// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/usagewire"
)

// TestRouteCredentialKind: each route constructor declares the credential
// kind it attaches. The kind is what marks a run's cost notional (a
// `claude setup-token` OAuth token is subscription usage, never list-price
// spend), so a constructor reporting the wrong kind would misprice every run
// through it. Every route is built offline: the cloud ones resolve their
// credentials lazily, so no token is ever fetched here.
func TestRouteCredentialKind(t *testing.T) {
	isolateAWS(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", writeServiceAccount(t, "http://127.0.0.1:1/token"))
	key := filepath.Join(t.TempDir(), "key")

	must := func(up Upstream, err error) Upstream {
		t.Helper()
		if err != nil {
			t.Fatalf("build route = %v", err)
		}
		return up
	}
	anthropic := mustTarget(t, "https://api.anthropic.com")
	foundry := mustTarget(t, "https://foundry.example.com")
	routes := []struct {
		name string
		up   Upstream
		want string
	}{
		{"anthropic key", Anthropic(anthropic, key, false), usagewire.CredentialAPIKey},
		{"anthropic bearer", Anthropic(anthropic, key, true), usagewire.CredentialOAuthToken},
		{"foundry key", FoundryKey(foundry, key), usagewire.CredentialAPIKey},
		{"foundry entra", must(FoundryEntra(foundry)), usagewire.CredentialCloudIdentity},
		{"bedrock", must(Bedrock(context.Background(),
			mustTarget(t, "https://bedrock.example.com"), "eu-west-2")), usagewire.CredentialCloudIdentity},
		{"vertex", must(Vertex(context.Background(),
			mustTarget(t, "https://us-east5-aiplatform.googleapis.com"), "demo", "us-east5")),
			usagewire.CredentialCloudIdentity},
	}
	for _, r := range routes {
		if r.up.CredentialKind != r.want {
			t.Errorf("%s: CredentialKind = %q, want %q", r.name, r.up.CredentialKind, r.want)
		}
	}
}
