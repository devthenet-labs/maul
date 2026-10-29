// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// seenRequest is one request a fake component received.
type seenRequest struct {
	Method, RequestURI, Host string
	Header                   http.Header
	Body                     string
}

// fakeComponent is a previewed component: it records every request and
// answers with its handler.
type fakeComponent struct {
	mu      sync.Mutex
	reqs    []seenRequest
	handler func(w http.ResponseWriter, r *http.Request, n int)
	srv     *httptest.Server
}

func newFakeComponent(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) *fakeComponent {
	t.Helper()
	c := &fakeComponent{handler: h}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		n := len(c.reqs)
		c.reqs = append(c.reqs, seenRequest{r.Method, r.RequestURI, r.Host, r.Header.Clone(), string(body)})
		c.mu.Unlock()
		c.handler(w, r, n)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeComponent) requests() []seenRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]seenRequest(nil), c.reqs...)
}

// routes maps Service addresses to listeners, as the slot network would.
type routes map[string]string

func (m routes) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	to, ok := m[addr]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, to)
}

// counterTokens is a deterministic run-token source.
func counterTokens() func() string {
	var mu sync.Mutex
	n := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("tok%d", n)
	}
}

// fakeExec is the sandbox's exec shim.
type fakeExec struct {
	mu   sync.Mutex
	reqs []toolwire.ExecRequest
	run  func(req *toolwire.ExecRequest, n int) (toolexec.Result, error)
}

func (f *fakeExec) Exec(_ context.Context, req *toolwire.ExecRequest) (toolexec.Result, error) {
	f.mu.Lock()
	n := len(f.reqs)
	f.reqs = append(f.reqs, *req)
	f.mu.Unlock()
	return f.run(req, n)
}

// fakeArtifacts is an artifact store keyed by sha256.
type fakeArtifacts map[string][]byte

func (f fakeArtifacts) Read(sha string) ([]byte, error) {
	b, ok := f[sha]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

// harness is one relay under test, its ledger on out.
type harness struct {
	t     *testing.T
	relay *Relay
	out   *bytes.Buffer
	dir   string
	term  string
}

func baseConfig() Config {
	return Config{
		Components: []Component{
			{Key: "api", Path: "/api", Addr: "10.0.0.1:80"},
			{Key: "web", Path: "/", Addr: "10.0.0.2:80"},
		},
		Host:           "intent-1.preview.example",
		Viewer:         "maul-evaluator-pv1",
		MaxToolCalls:   50,
		TimeoutSeconds: 600,
	}
}

func newHarness(t *testing.T, cfg Config, rt routes, mut func(*Options)) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, out: &bytes.Buffer{}, dir: dir, term: filepath.Join(t.TempDir(), "termination-log")}
	o := Options{
		Config:          cfg,
		Dir:             dir,
		Out:             h.out,
		Client:          NewClient(ClientOptions{Dial: rt.dial, Timeout: 2 * time.Second}),
		Token:           counterTokens(),
		TerminationPath: h.term,
	}
	if mut != nil {
		mut(&o)
	}
	r, err := NewRelay(o)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	h.relay = r
	return h
}

// call posts body to the relay's endpoint for verb, from a remote caller,
// and decodes the response.
func (h *harness) call(verb toolwire.Verb, body string) (int, toolwire.Response) {
	h.t.Helper()
	return h.callPath(http.MethodPost, toolwire.Path(verb), body)
}

func (h *harness) callPath(method, path, body string) (int, toolwire.Response) {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "10.9.9.9:41000"
	rec := httptest.NewRecorder()
	h.relay.Handler().ServeHTTP(rec, req)
	var resp toolwire.Response
	if rec.Body.Len() > 0 {
		if err := toolwire.DecodeResponse(rec.Body, &resp); err != nil {
			h.t.Fatalf("%s %s: decode response %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code, resp
}

// finish closes the ledger through the runner's finish and verifies it.
func (h *harness) finish() toolwire.Ledger {
	h.t.Helper()
	code, resp := h.call(toolwire.VerbFinish, `{}`)
	if code != http.StatusOK || resp.Refused != "" {
		h.t.Fatalf("finish: %d %+v", code, resp)
	}
	return h.verify()
}

func (h *harness) verify() toolwire.Ledger {
	h.t.Helper()
	term, err := os.ReadFile(h.term)
	if err != nil {
		h.t.Fatalf("termination message: %v", err)
	}
	l, err := toolwire.Verify(h.out.Bytes(), string(term))
	if err != nil {
		h.t.Fatalf("Verify: %v\n%s", err, h.out.String())
	}
	return l
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// storedCheck is a check as the tool ConfigMap carries it.
func storedCheck(t *testing.T, src string) StoredCheck {
	t.Helper()
	c, err := checks.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := checks.Validate(c, checks.Preview{}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return StoredCheck{ID: c.ID, Digest: checks.Digest(c), Body: string(checks.Canonical(c))}
}

func bareDigest(t *testing.T, src string) string {
	t.Helper()
	return strings.TrimPrefix(storedCheck(t, src).Digest, "sha256:")
}

// entryOf is a ledger entry for record.
func entryOf(kind toolwire.Kind, verb toolwire.Verb, args string,
	opts ...func(*toolwire.LedgerEntry)) toolwire.LedgerEntry {
	e := toolwire.LedgerEntry{Kind: kind, Verb: verb, Args: json.RawMessage(args)}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func withArtifacts(as ...toolwire.Artifact) func(*toolwire.LedgerEntry) {
	return func(e *toolwire.LedgerEntry) { e.Artifacts = as }
}
