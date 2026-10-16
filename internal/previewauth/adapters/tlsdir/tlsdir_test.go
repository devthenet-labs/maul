// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package tlsdir

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePair writes a self-signed certificate for names into dir/sub, as a
// projected kubernetes.io/tls Secret lays it out.
func writePair(t *testing.T, dir, sub string, names ...string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
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
	for name, block := range map[string]*pem.Block{
		FileCert: {Type: "CERTIFICATE", Bytes: der}, FileKey: {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(filepath.Join(dir, sub, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func served(t *testing.T, s *Store, sni string) string {
	t.Helper()
	c, err := s.GetCertificate(&tls.ClientHelloInfo{ServerName: sni})
	if err != nil {
		t.Fatal(err)
	}
	return c.Leaf.DNSNames[0]
}

func TestLoadSelectsBySNI(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "0", "*.preview.example.com")
	writePair(t, dir, "1", "signin.example.com")
	// The kubelet's own entries are not certificates.
	if err := os.MkdirAll(filepath.Join(dir, "..data"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for sni, want := range map[string]string{
		"demo-1.preview.example.com": "*.preview.example.com",
		"signin.example.com":         "signin.example.com",
		"":                           "*.preview.example.com", // no SNI: the first
		"other.example.org":          "*.preview.example.com", // nothing covers it: the first
	} {
		if got := served(t, s, sni); got != want {
			t.Errorf("SNI %q served %q, want %q", sni, got, want)
		}
	}
	if !s.Covers("demo-1.preview.example.com") || !s.Covers("signin.example.com") || s.Covers("a.b.preview.example.com") {
		t.Fatal("Covers disagrees with the certificates")
	}
}

func TestLoadFollowsProjectedSymlinks(t *testing.T) {
	dir := t.TempDir()
	writePair(t, filepath.Join(dir, "..2026_10_16"), "0", "*.preview.example.com")
	if err := os.Symlink("..2026_10_16", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "0"), filepath.Join(dir, "0")); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Covers("x.preview.example.com") {
		t.Fatal("the projected certificate is not loaded")
	}
}

func TestLoadRefuses(t *testing.T) {
	empty := t.TempDir()
	if _, err := Load(empty); err == nil || !strings.Contains(err.Error(), "holds no certificate") {
		t.Errorf("empty directory: %v", err)
	}
	if _, err := Load(filepath.Join(empty, "missing")); err == nil {
		t.Error("missing directory loaded")
	}
	half := t.TempDir()
	writePair(t, half, "0", "a.example.com")
	if err := os.Remove(filepath.Join(half, "0", FileKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(half); err == nil {
		t.Error("a certificate without its key loaded")
	}
}

func TestReloadKeepsTheLastGoodSet(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "0", "a.example.com")
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A renewal swaps the pair in.
	writePair(t, dir, "0", "b.example.com")
	if err := s.Reload(); err != nil || !s.Covers("b.example.com") || s.Covers("a.example.com") {
		t.Fatalf("renewal not picked up: %v", err)
	}
	// A half-written one is refused and the renewed pair stays.
	if err := os.WriteFile(filepath.Join(dir, "0", FileKey), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err == nil {
		t.Fatal("a broken pair reloaded")
	}
	if !s.Covers("b.example.com") {
		t.Fatal("a failed reload dropped the certificates in use")
	}
}

func TestRunReloads(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "0", "a.example.com")
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	go s.Run(t.Context(), 10*time.Millisecond)
	writePair(t, dir, "0", "b.example.com")
	deadline := time.Now().Add(5 * time.Second)
	for !s.Covers("b.example.com") {
		if time.Now().After(deadline) {
			t.Fatal("Run never picked up the renewal")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
