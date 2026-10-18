// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/usagewire"
)

// Anthropic is the first-party route: inject the credential from a mounted
// Secret file and forward otherwise unchanged — the anthropic-version and
// anthropic-beta headers the CLI sends pass through, so prompt caching and
// beta features survive the hop. An API key rides x-api-key; bearer=true
// sends Authorization: Bearer instead, which is how a `claude setup-token`
// OAuth token authenticates. Either way the file is read per request, so a
// rotated Secret propagates without a restart.
//
// Under per-user credentials a request whose run is attributed to a person
// carries that person's credential in its context (resolved by the
// server), and the route sends it, by its own kind, instead of the org
// credential.
func Anthropic(target *url.URL, keyFile string, bearer bool) Upstream {
	kind := usagewire.CredentialAPIKey
	if bearer {
		kind = usagewire.CredentialOAuthToken
	}
	return Upstream{
		Target:         target,
		CredentialKind: kind,
		Credential: func(ctx context.Context, req *http.Request) error {
			if a := attachedFrom(ctx); a != nil {
				setAnthropicCredential(req.Header, a.kind == v1alpha1.ModelCredentialOAuthToken, a.credential)
				return nil
			}
			key, err := readKeyFile(keyFile)
			if err != nil {
				return err
			}
			setAnthropicCredential(req.Header, bearer, key)
			return nil
		},
		Ready: func(context.Context) error {
			_, err := readKeyFile(keyFile)
			return err
		},
	}
}

// setAnthropicCredential sets an API key as x-api-key or an OAuth token as
// an Authorization bearer.
func setAnthropicCredential(h http.Header, bearer bool, credential string) {
	if bearer {
		h.Set("Authorization", "Bearer "+credential)
	} else {
		h.Set("x-api-key", credential)
	}
}

// DefaultProbeModel is the model an oauth-token health probe asks for a
// single output token of: the cheapest current one, which the claude CLI's
// own helper turns use too.
const DefaultProbeModel = "claude-haiku-4-5"

// The health probe's fixed wire values.
const (
	anthropicVersion = "2023-06-01"
	// anthropicOAuthBeta is the beta an OAuth (setup-token) bearer needs
	// on the API.
	anthropicOAuthBeta = "oauth-2025-04-20"
	probeTimeout       = 30 * time.Second
)

// AnthropicProbe is the health probe of a person's Anthropic credential
// (design revision 1: health comes only from probes the broker issues
// itself). An API key lists one model (GET /v1/models?limit=1), which costs
// nothing; an OAuth token, which a subscription authorises for messages,
// asks model for one output token. A 2xx is Valid and a 401 Rejected;
// anything else (a 403, a 429, a 5xx, no answer) is not a verdict on the
// credential.
func AnthropicProbe(target *url.URL, model string, client *http.Client) ProbeFunc {
	return func(ctx context.Context, kind v1alpha1.ModelCredentialKind, credential string) ProbeResult {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		var req *http.Request
		var err error
		if kind == v1alpha1.ModelCredentialOAuthToken {
			body, _ := json.Marshal(map[string]any{
				"model": model, "max_tokens": 1,
				"messages": []map[string]string{{"role": "user", "content": "ping"}},
			})
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, target.JoinPath("v1", "messages").String(),
				bytes.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("anthropic-beta", anthropicOAuthBeta)
			}
		} else {
			u := target.JoinPath("v1", "models")
			u.RawQuery = "limit=1"
			req, err = http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		}
		if err != nil {
			return ProbeInconclusive
		}
		req.Header.Set("anthropic-version", anthropicVersion)
		setAnthropicCredential(req.Header, kind == v1alpha1.ModelCredentialOAuthToken, credential)
		resp, err := client.Do(req)
		if err != nil {
			return ProbeInconclusive
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode/100 == 2:
			return ProbeValid
		case resp.StatusCode == http.StatusUnauthorized:
			return ProbeRejected
		}
		return ProbeInconclusive
	}
}
