// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func statusServer(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code >= 400 {
			http.Error(w, "no", code)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
}

// acceptLoop serves connections with handle until the listener closes.
func acceptLoop(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
	return "http://" + l.Addr().String()
}

func TestProbeHTTP(t *testing.T) {
	ok, forbidden, broken := statusServer(200), statusServer(403), statusServer(502)
	defer ok.Close()
	defer forbidden.Close()
	defer broken.Close()
	// A guard that closes the connection at accept, as previewtools.GuardListener does.
	guard := acceptLoop(t, func(c net.Conn) { _ = c.Close() })
	// A listener that accepts and never answers.
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	hang := acceptLoop(t, func(c net.Conn) { <-hold; _ = c.Close() })
	gone, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	goneURL := "http://" + gone.Addr().String()
	_ = gone.Close()

	for _, tc := range []struct{ name, url, want string }{
		{"answered 200", ok.URL, outcomeReached},
		{"refused by status 403", forbidden.URL, "blocked"},
		{"closed at accept", guard, "blocked"},
		{"never answers", hang, "blocked"},
		{"a 502 proves nothing", broken.URL, outcomeInconclusive},
		{"nothing listens", goneURL, outcomeInconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeHTTP(httpSpec{name: "x", url: tc.url}, 300*time.Millisecond); got != tc.want {
				t.Errorf("probeHTTP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProbeHTTPSendsTheWrongKeyAndNoOrigin(t *testing.T) {
	var key string
	var hasOrigin bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("X-Maul-Cdp-Key")
		_, hasOrigin = r.Header["Origin"]
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	probeHTTP(httpSpec{name: "x", url: srv.URL, headers: map[string]string{"X-Maul-Cdp-Key": "wrong"}}, time.Second)
	if key != "wrong" || hasOrigin {
		t.Errorf("key = %q, origin sent = %v", key, hasOrigin)
	}
}

func TestSandboxSpecs(t *testing.T) {
	if _, ok := sandboxSpecsFrom(getenvOf(nil)); ok {
		t.Fatal("accepted a run without the pod's address")
	}
	if _, ok := sandboxSpecsFrom(getenvOf(map[string]string{"PROBE_POD_IP": "not-an-ip"})); ok {
		t.Fatal("accepted a name for the pod's address")
	}
	specs, ok := sandboxSpecsFrom(getenvOf(map[string]string{"PROBE_POD_IP": "10.42.0.9"}))
	if !ok {
		t.Fatal("not ok")
	}
	got := map[string]httpSpec{}
	for _, s := range specs {
		got[s.name] = s
	}
	want := map[string]string{
		"tool-api-loopback":  "http://127.0.0.1:8090/",
		"tool-api-pod-ip":    "http://10.42.0.9:8090/",
		"cdp-shim-no-key":    "http://127.0.0.1:9222/json/version",
		"cdp-shim-wrong-key": "http://127.0.0.1:9222/json/version",
	}
	if len(got) != len(want) {
		t.Fatalf("specs = %v", got)
	}
	for k, v := range want {
		if got[k].url != v {
			t.Errorf("%s = %q, want %q", k, got[k].url, v)
		}
	}
	if got["cdp-shim-wrong-key"].headers["X-Maul-Cdp-Key"] == "" {
		t.Error("the wrong-key probe sends no key")
	}
	if len(got["cdp-shim-no-key"].headers) != 0 {
		t.Error("the no-key probe sends a header")
	}
}

func TestRunSandbox(t *testing.T) {
	forbidden, open := statusServer(403), statusServer(200)
	defer forbidden.Close()
	defer open.Close()
	for _, tc := range []struct {
		name  string
		specs []httpSpec
		want  string
		code  int
	}{
		{"all refused", []httpSpec{{name: "a", url: forbidden.URL}}, outcomePass, 0},
		{"one answers", []httpSpec{{name: "a", url: forbidden.URL}, {name: "b", url: open.URL}}, outcomeSecurityFailure, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lines []result
			got, code := runSandbox(tc.specs, 300*time.Millisecond, func(r result) { lines = append(lines, r) })
			if got != tc.want || code != tc.code {
				t.Fatalf("runSandbox = %s, %d; want %s, %d", got, code, tc.want, tc.code)
			}
			if last := lines[len(lines)-1]; last.Name != "summary" || last.Outcome != tc.want {
				t.Errorf("last line %+v", last)
			}
		})
	}
}
