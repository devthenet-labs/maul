// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cdpshim

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

// TestLiveCDPShim drives a real Chromium through the shim (`mise run
// live-browser`; MAUL_LIVE_BROWSER=1, the binary from MAUL_LIVE_CHROMIUM,
// default /usr/bin/chromium): /json/version, a WebSocket with the key and
// no Origin, the browser's version, a page that evaluates script, refusals
// without the key or with an Origin, and every Chromium process gone once
// the client disconnects. Opt-in (it needs Chromium), never in CI; run it
// before a shim change or a Chromium bump in Dockerfile.preview-browser
// merges.
func TestLiveCDPShim(t *testing.T) {
	if os.Getenv("MAUL_LIVE_BROWSER") != "1" {
		t.Skip("set MAUL_LIVE_BROWSER=1 (mise run live-browser) to run against a real Chromium")
	}
	bin := os.Getenv("MAUL_LIVE_CHROMIUM")
	if bin == "" {
		bin = "/usr/bin/chromium"
	}
	tmp := t.TempDir()
	logs := &syncBuffer{}
	s, err := New(Options{
		Key:    testKey,
		Launch: Chromium{Path: bin, TempDir: tmp}.Launch,
		Log:    slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		s.Close()
		srv.Close()
		t.Logf("shim log:\n%s", logs.String())
	})

	resp := get(t, srv, http.MethodGet, "/json/version", http.Header{KeyHeader: {string(testKey)}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/json/version: %d", resp.StatusCode)
	}
	if _, err := dial(t, srv, testKey, true); err == nil {
		t.Fatal("WebSocket with an Origin admitted")
	}
	if _, err := dial(t, srv, []byte(strings.Repeat("w", len(testKey))), false); err == nil {
		t.Fatal("WebSocket with a wrong key admitted")
	}

	ws, err := dial(t, srv, testKey, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &cdpClient{t: t, ws: ws}
	var version struct {
		Product string `json:"product"`
	}
	c.call("", "Browser.getVersion", nil, &version)
	if !strings.Contains(version.Product, "Chrome") {
		t.Errorf("product %q", version.Product)
	}
	t.Logf("browser: %s", version.Product)
	var target struct {
		TargetID string `json:"targetId"`
	}
	c.call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	c.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached)
	var eval struct {
		Result struct {
			Value int `json:"value"`
		} `json:"result"`
	}
	c.call(attached.SessionID, "Runtime.evaluate", map[string]any{"expression": "6*7", "returnByValue": true}, &eval)
	if eval.Result.Value != 42 {
		t.Errorf("evaluate = %d, want 42", eval.Result.Value)
	}
	if n := chromiumProcesses(t, tmp); n == 0 {
		t.Error("no Chromium process found with a profile under the shim's directory")
	}

	_ = ws.Close()
	eventually(t, "every Chromium process gone", func() bool { return s.Live() == 0 && chromiumProcesses(t, tmp) == 0 })
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("profiles left behind: %v", left)
	}
}

// cdpClient is the smallest CDP client: one call at a time, events skipped.
type cdpClient struct {
	t  *testing.T
	ws *websocket.Conn
	id int
}

func (c *cdpClient) call(session, method string, params map[string]any, out any) {
	c.t.Helper()
	c.id++
	req := map[string]any{"id": c.id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if session != "" {
		req["sessionId"] = session
	}
	if err := websocket.JSON.Send(c.ws, req); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	for {
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := websocket.JSON.Receive(c.ws, &resp); err != nil {
			c.t.Fatalf("%s: %v", method, err)
		}
		if resp.ID != c.id {
			continue
		}
		if len(resp.Error) > 0 {
			c.t.Fatalf("%s: %s", method, resp.Error)
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			c.t.Fatalf("%s: %v", method, err)
		}
		return
	}
}

// chromiumProcesses counts live processes whose command line names a
// profile directory under dir.
func chromiumProcesses(t *testing.T, dir string) int {
	t.Helper()
	cmdlines, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range cmdlines {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), "--user-data-dir="+dir+"/") {
			n++
		}
	}
	return n
}
