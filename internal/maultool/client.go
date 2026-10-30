// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package maultool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// CallTimeout bounds one call: a check run's two attempts in a fresh
// browser each, with room.
const CallTimeout = 10 * time.Minute

// Client calls one tools container over the toolwire contract.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient returns a client of the tools container at raw: an http or
// https origin with no user info, path, query or fragment. It reads no
// proxy variable and follows no redirect.
func NewClient(raw string) (*Client, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s is not set", EnvToolsURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not a URL", EnvToolsURL)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("%s is not an http or https URL", EnvToolsURL)
	case u.Host == "" || u.Hostname() == "":
		return nil, fmt.Errorf("%s names no host", EnvToolsURL)
	case u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return nil, fmt.Errorf("%s is not a bare origin", EnvToolsURL)
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: CallTimeout,
		MaxIdleConns:          1,
	}
	return &Client{
		base: u.Scheme + "://" + u.Host,
		hc: &http.Client{
			Transport:     transport,
			Timeout:       CallTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// errRefused marks a response whose call the tools container refused.
var errRefused = errors.New("refused")

// Call sends req to verb's endpoint and returns the response. A refused
// call returns the response and an error wrapping errRefused; any other
// error is a tool error, the response nil.
func (c *Client) Call(ctx context.Context, verb toolwire.Verb, req toolwire.Request) (*toolwire.Response, error) {
	path := toolwire.Path(verb)
	if path == "" {
		return nil, fmt.Errorf("verb %q has no endpoint", verb)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hresp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("the tools container did not answer: %w", err)
	}
	defer func() { _ = hresp.Body.Close() }()
	var resp toolwire.Response
	if err := toolwire.DecodeResponse(hresp.Body, &resp); err != nil {
		return nil, fmt.Errorf("the tools container answered %d with no tool response", hresp.StatusCode)
	}
	if resp.Seq < 1 {
		return nil, fmt.Errorf("the tools container answered %d with no ledger entry", hresp.StatusCode)
	}
	if resp.Refused != "" {
		return &resp, errRefused
	}
	if hresp.StatusCode < 200 || hresp.StatusCode > 299 {
		return nil, fmt.Errorf("the tools container answered %d", hresp.StatusCode)
	}
	return &resp, nil
}

// Artifact fetches a's bytes by digest and holds them to a's sha256 and
// size.
func (c *Client) Artifact(ctx context.Context, a toolwire.Artifact) ([]byte, error) {
	path := toolwire.ArtifactPath(a.SHA256)
	if path == "" || a.Bytes < 0 || a.Bytes > toolwire.MaxArtifactBytes {
		return nil, errors.New("the artifact's digest or size is out of bounds")
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	hresp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("the tools container did not answer: %w", err)
	}
	defer func() { _ = hresp.Body.Close() }()
	if hresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the tools container answered %d", hresp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(hresp.Body, a.Bytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the artifact: %w", err)
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != a.Bytes || hex.EncodeToString(sum[:]) != a.SHA256 {
		return nil, errors.New("the artifact does not match its ledger entry")
	}
	return data, nil
}
