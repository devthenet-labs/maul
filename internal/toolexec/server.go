// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The shim's endpoints.
const (
	// ExecPath runs one program: POST, a toolwire.ExecRequest in, a Result
	// out (status 400 with a refused Result for a request of no grammar).
	ExecPath = "/v1/exec"
	// ReadyPath answers GET with 200 once the shim serves.
	ReadyPath = "/v1/ready"
	// maxResultBytes bounds a Result a client reads: the output, escaped
	// at worst six bytes a byte, with room.
	maxResultBytes = 6*MaxOutputBytes + 16<<10
)

// Listen listens on a unix socket at path, replacing a stale one, with
// mode 0666 so the tools container, another UID of the pod, can connect.
// Only tools and sandbox mount the socket's directory.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("toolexec: remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("toolexec: listen: %w", err)
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // the socket must be reachable by the tools UID
		_ = ln.Close()
		return nil, fmt.Errorf("toolexec: socket mode: %w", err)
	}
	return ln, nil
}

// Handler serves ExecPath and ReadyPath. A caller that goes away kills its
// program.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ExecPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req toolwire.ExecRequest
		if err := toolwire.DecodeRequest(r.Body, &req); err != nil {
			writeResult(w, http.StatusBadRequest, refused(toolwire.RefusedInvalid, err.Error()))
			return
		}
		writeResult(w, http.StatusOK, s.Run(r.Context(), &req))
	})
	mux.HandleFunc(ReadyPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

func writeResult(w http.ResponseWriter, status int, res Result) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// Client is the tools container's side of the shim.
type Client struct {
	http *http.Client
}

// NewClient returns a client of the shim listening at socket.
func NewClient(socket string) *Client {
	return &Client{http: unixHTTP(socket)}
}

func unixHTTP(socket string) *http.Client {
	var d net.Dialer
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
}

// Exec runs req in the sandbox. A refusal is a Result with Refused set; an
// error means the shim gave no Result. ctx bounds the wait and the run.
func (c *Client) Exec(ctx context.Context, req *toolwire.ExecRequest) (Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, fmt.Errorf("toolexec: encode request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://sandbox"+ExecPath, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("toolexec: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hreq)
	if err != nil {
		return Result{}, fmt.Errorf("toolexec: exec: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		return Result{}, fmt.Errorf("toolexec: exec: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResultBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("toolexec: read result: %w", err)
	}
	if len(raw) > maxResultBytes {
		return Result{}, errors.New("toolexec: result too large")
	}
	var res Result
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return Result{}, fmt.Errorf("toolexec: decode result: %w", err)
	}
	if resp.StatusCode == http.StatusBadRequest && res.Refused == "" {
		return Result{}, errors.New("toolexec: exec: status 400 without a refusal")
	}
	return res, nil
}

// Ready reports whether the shim serves.
func (c *Client) Ready(ctx context.Context) error {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sandbox"+ReadyPath, nil)
	if err != nil {
		return fmt.Errorf("toolexec: %w", err)
	}
	resp, err := c.http.Do(hreq)
	if err != nil {
		return fmt.Errorf("toolexec: ready: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("toolexec: ready: status %d", resp.StatusCode)
	}
	return nil
}
