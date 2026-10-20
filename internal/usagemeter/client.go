// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagemeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/devthenet-labs/maul/internal/usagewire"
)

// Fetcher reads one Job's metered usage from the broker.
type Fetcher interface {
	Fetch(ctx context.Context, job string) (usagewire.Response, error)
}

// maxResponse bounds the answer read from the broker: one Job's pods hold
// at most a few model entries each.
const maxResponse = 1 << 20

// Client is the HTTP Fetcher: GET BaseURL + usagewire.PathPrefix + job with
// the projected token in TokenFile as a bearer, read fresh on every call
// (the kubelet rotates it in place).
type Client struct {
	BaseURL   string
	TokenFile string
	HTTP      *http.Client
}

// NewClient validates the broker's usage base URL and the token file path.
func NewClient(baseURL, tokenFile string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("usage broker URL %q is not an http(s) URL", baseURL)
	}
	if tokenFile == "" {
		return nil, errors.New("usage token file is required")
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), TokenFile: tokenFile,
		HTTP: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Fetch implements Fetcher.
func (c *Client) Fetch(ctx context.Context, job string) (usagewire.Response, error) {
	if !usagewire.ValidJobName(job) {
		return usagewire.Response{}, fmt.Errorf("usage: %q is not a job name", job)
	}
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return usagewire.Response{}, fmt.Errorf("usage: read the broker token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+usagewire.PathPrefix+job, nil)
	if err != nil {
		return usagewire.Response{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return usagewire.Response{}, fmt.Errorf("usage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return usagewire.Response{}, fmt.Errorf("usage: read the answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return usagewire.Response{}, fmt.Errorf("usage: broker answered %d", resp.StatusCode)
	}
	if len(body) > maxResponse {
		return usagewire.Response{}, errors.New("usage: broker answer too large")
	}
	var out usagewire.Response
	if err := json.Unmarshal(body, &out); err != nil {
		return usagewire.Response{}, fmt.Errorf("usage: decode the answer: %w", err)
	}
	return out, nil
}
