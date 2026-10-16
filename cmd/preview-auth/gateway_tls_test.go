// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/bitwise-media-group/patchy/internal/previewauth/adapters/tlsdir"
)

// tlsPair writes a self-signed pair for names into dir/sub, as the chart
// projects a kubernetes.io/tls Secret, and returns its certificate.
func tlsPair(t *testing.T, dir, sub string, names ...string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	writeFile(t, filepath.Join(dir, sub), tlsdir.FileCert, string(certPEM))
	writeFile(t, filepath.Join(dir, sub), tlsdir.FileKey, string(keyPEM))
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestLoadGatewayTLS(t *testing.T) {
	dir := t.TempDir()
	tlsPair(t, dir, "0", "*.previews.example.com")
	tlsPair(t, dir, "1", "auth.example.com")
	st, err := loadStatic(serveOpts(t, gatewayArgs(t, "--preview-auth-gateway-tls-dir", dir)...))
	if err != nil {
		t.Fatal(err)
	}
	if st.gateway.tls == nil || st.gateway.tlsAddr != ":8443" {
		t.Fatalf("gateway TLS = %v at %q", st.gateway.tls, st.gateway.tlsAddr)
	}
	st, err = loadStatic(serveOpts(t, gatewayArgs(t, "--preview-auth-gateway-tls-dir", dir,
		"--preview-auth-gateway-tls-listen-addr", ":9443")...))
	if err != nil || st.gateway.tlsAddr != ":9443" {
		t.Fatalf("TLS listen address: %v, %+v", err, st.gateway)
	}
	// Without the directory the gateway serves plain HTTP alone, as before.
	st, err = loadStatic(serveOpts(t, gatewayArgs(t)...))
	if err != nil || st.gateway.tls != nil {
		t.Fatalf("TLS loaded without a directory: %v, %+v", err, st.gateway)
	}
}

func TestLoadGatewayTLSRefuses(t *testing.T) {
	previewsOnly := t.TempDir()
	tlsPair(t, previewsOnly, "0", "*.previews.example.com")
	signInOnly := t.TempDir()
	tlsPair(t, signInOnly, "0", "auth.example.com")
	for name, tc := range map[string]struct{ dir, want string }{
		"sign-in host uncovered":   {previewsOnly, "no certificate covers auth.example.com"},
		"preview hosts uncovered":  {signInOnly, "no certificate covers preview-label.previews.example.com"},
		"no certificate":           {t.TempDir(), "holds no certificate"},
		"directory does not exist": {filepath.Join(t.TempDir(), "missing"), "tls-dir"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadStatic(serveOpts(t, gatewayArgs(t, "--preview-auth-gateway-tls-dir", tc.dir)...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadStatic error = %v, want %q", err, tc.want)
			}
		})
	}
}

// addedRunnables records what addTLSListener adds to the manager.
type addedRunnables []manager.Runnable

func (a *addedRunnables) Add(r manager.Runnable) error { *a = append(*a, r); return nil }

// TestTLSListenerServesBySNI: the TLS listener serves the gateway's handler
// with the certificate covering each requested name, on every replica, and
// its certificates are kept current.
func TestTLSListenerServesBySNI(t *testing.T) {
	dir := t.TempDir()
	previews := tlsPair(t, dir, "0", "*.previews.example.com")
	signIn := tlsPair(t, dir, "1", "auth.example.com")
	store, err := tlsdir.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var added addedRunnables
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Host) })
	if err := addTLSListener(&added, &gatewayStatic{tls: store, tlsAddr: "127.0.0.1:0"}, handler, nil); err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("added %d runnables, want the TLS listener and its reloader", len(added))
	}
	l, ok := added[0].(listener)
	if !ok || !l.tls || l.srv.TLSConfig == nil || l.srv.TLSConfig.MinVersion != tls.VersionTLS12 ||
		l.srv.ReadTimeout != 0 || l.srv.WriteTimeout != 0 {
		t.Fatalf("TLS listener = %+v", added[0])
	}
	if r, ok := added[1].(tlsReloader); !ok || r.NeedLeaderElection() || l.NeedLeaderElection() {
		t.Fatal("the TLS listener and reloader must run on every replica")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = l.srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = l.srv.Close() })
	certs := map[string]*x509.Certificate{"demo-1.previews.example.com": previews, "auth.example.com": signIn}
	for host, want := range certs {
		pool := x509.NewCertPool()
		pool.AddCert(want)
		client := &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
			},
		}}
		resp, err := client.Get("https://" + host + "/")
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != host {
			t.Errorf("%s: handler saw host %q", host, body)
		}
	}
	// TLS 1.1 is refused.
	//nolint:gosec // an old, unverified handshake is the refusal under test
	old := &tls.Config{ServerName: "auth.example.com", InsecureSkipVerify: true,
		MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}
	conn, err := tls.Dial("tcp", ln.Addr().String(), old)
	if err == nil {
		_ = conn.Close()
		t.Error("a TLS 1.1 handshake succeeded")
	}
}

func TestTLSListenerStops(t *testing.T) {
	dir := t.TempDir()
	tlsPair(t, dir, "0", "*.previews.example.com")
	store, err := tlsdir.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := listener{srv: newTLSServer("127.0.0.1:0", http.NotFoundHandler(), store), tls: true}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.Start(ctx); err != nil {
		t.Errorf("Start after cancel = %v, want a clean shutdown", err)
	}
	r := tlsReloader{store}
	if err := r.Start(ctx); err != nil {
		t.Errorf("reloader Start after cancel = %v", err)
	}
}
