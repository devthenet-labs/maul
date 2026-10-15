// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// testPKI is a throwaway CA that issues the TLS registries' certificates,
// so a registry is reached by its own host name, as in production.
type testPKI struct {
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "egress-proxy test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &testPKI{ca: ca, key: key, pool: pool}
}

// leaf issues a server certificate for host.
func (p *testPKI) leaf(t *testing.T, host string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// world is a set of TLS hosts on loopback, each reached as host:443 by
// the engine's registry transports.
type world struct {
	t     *testing.T
	pki   *testPKI
	mu    sync.Mutex
	addrs map[string]string // "host:443" -> loopback listener
}

func newWorld(t *testing.T) *world {
	return &world{t: t, pki: newTestPKI(t), addrs: map[string]string{}}
}

// host serves h over TLS as https://<name>/ and records every request.
func (w *world) host(name string, h http.HandlerFunc) *recordingUpstream {
	w.t.Helper()
	u := &recordingUpstream{}
	u.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits = append(u.hits, upstreamHit{method: r.Method, requestURI: r.RequestURI, header: r.Header.Clone()})
		u.mu.Unlock()
		if h != nil {
			h(rw, r)
		}
	}))
	u.TLS = &tls.Config{Certificates: []tls.Certificate{w.pki.leaf(w.t, name)}}
	u.StartTLS()
	w.t.Cleanup(u.Close)
	w.mu.Lock()
	w.addrs[net.JoinHostPort(name, "443")] = u.Listener.Addr().String()
	w.mu.Unlock()
	return u
}

// rewrite maps a registry dial to its listener; an unknown host keeps its
// address (and fails to connect or is refused by the policy).
func (w *world) rewrite(addr string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if a, ok := w.addrs[addr]; ok {
		return a
	}
	return addr
}

// fakeCreds is a CredentialSource holding fixed credentials.
type fakeCreds struct {
	mu          sync.Mutex
	creds       map[string]Credential
	invalidated map[string]int
}

func newFakeCreds() *fakeCreds {
	return &fakeCreds{creds: map[string]Credential{}, invalidated: map[string]int{}}
}

func (f *fakeCreds) set(name string, c Credential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creds[name] = c
}

func (f *fakeCreds) Get(name string) (Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.creds[name]
	if !ok {
		return Credential{}, ErrCredentialUnavailable
	}
	return c, nil
}

func (f *fakeCreds) Invalidate(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated[name]++
}

func (f *fakeCreds) invalidations(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invalidated[name]
}

// mustRegistries validates registries for a test.
func mustRegistries(t *testing.T, regs ...netprofile.Registry) netprofile.RegistrySet {
	t.Helper()
	set, err := netprofile.NewRegistrySet(regs, netprofile.RegistryContext{})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// grantOf is a grant of r carrying what the chart copies from it, edited by
// mod.
func grantOf(r netprofile.Registry, mod func(*netprofile.Grant)) netprofile.Grant {
	g := netprofile.Grant{Name: r.Name, Ecosystem: r.Ecosystem, Hosts: r.Hosts(),
		PackagePolicy: netprofile.PackagePolicyRegistry}
	if r.PrivateModules != nil {
		g.PrivateModules = append([]string(nil), *r.PrivateModules...)
	}
	if mod != nil {
		mod(&g)
	}
	return g
}

// mustProfiles validates profiles for a test.
func mustProfiles(t *testing.T, ps ...netprofile.Profile) netprofile.Set {
	t.Helper()
	for i := range ps {
		if ps[i].Host == "" {
			ps[i].Host = "github.com"
		}
		if len(ps[i].Repositories) == 0 {
			ps[i].Repositories = []string{"devthenet-labs/" + ps[i].Name}
		}
	}
	set, err := netprofile.NewSet(ps)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func boolp(b bool) *bool { return &b }

func strsp(s ...string) *[]string { return &s }

// regHarness is an engine with registries, TLS upstreams in a world, the
// fake credentials and the profile listeners.
type regHarness struct {
	*harness
	world *world
	creds *fakeCreds
}

// newRegHarness starts the engine; cfg carries Profiles and Registries.
// The step-1 upstreams are plain loopback servers, as in step 1's tests.
func newRegHarness(t *testing.T, w *world, creds *fakeCreds, cfg Config, goUp, sumUp *recordingUpstream) *regHarness {
	t.Helper()
	if goUp == nil {
		goUp = failingUpstream(t)
	}
	if sumUp == nil {
		sumUp = failingUpstream(t)
	}
	cfg.Credentials = creds
	cfg.rootCAs = w.pki.pool
	cfg.dialRewrite = w.rewrite
	h := newHarness(t, cfg, goUp, sumUp)
	return &regHarness{harness: h, world: w, creds: creds}
}

// lastLine is the last audit line.
func (h *harness) lastLine(t *testing.T) map[string]any {
	t.Helper()
	lines := h.log.lines(t)
	if len(lines) == 0 {
		t.Fatal("no audit line")
	}
	return lines[len(lines)-1]
}

// jsonError decodes an npm refusal body.
func jsonError(t *testing.T, body string) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body %q is not npm's JSON error: %v", body, err)
	}
	return m["error"]
}

// discardLog is a logger for engines whose lines a test does not read.
func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
