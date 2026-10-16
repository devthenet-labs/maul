// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package tlsdir

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"
)

// The file names of a kubernetes.io/tls Secret.
const (
	FileCert = "tls.crt"
	FileKey  = "tls.key"
)

// Store holds the certificates of every subdirectory of Dir.
type Store struct {
	// Dir is the directory whose subdirectories each hold one certificate.
	Dir string
	// Log reports a failed reload; nil logs nothing.
	Log *slog.Logger

	certs atomic.Pointer[[]tls.Certificate]
}

// Load reads every certificate under dir, refusing a directory with none or
// any subdirectory that does not hold a valid pair.
func Load(dir string) (*Store, error) {
	s := &Store{Dir: dir}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload re-reads the directory. On an error the previous certificates stay
// in use.
func (s *Store) Reload() error {
	certs, err := read(s.Dir)
	if err != nil {
		return err
	}
	s.certs.Store(&certs)
	return nil
}

// read loads each subdirectory's pair, in name order. A projected volume
// makes each one a symlink into its ..data directory, so entries are
// followed; the kubelet's own (..data and its timestamped targets) are
// skipped.
func read(dir string) ([]tls.Certificate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("gateway TLS directory: %w", err)
	}
	var certs []tls.Certificate
	for _, e := range entries {
		if e.Name()[0] == '.' {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || !fi.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		cert, err := tls.LoadX509KeyPair(filepath.Join(sub, FileCert), filepath.Join(sub, FileKey))
		if err != nil {
			return nil, fmt.Errorf("gateway TLS certificate %s: %w", e.Name(), err)
		}
		if cert.Leaf == nil {
			if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
				return nil, fmt.Errorf("gateway TLS certificate %s: %w", e.Name(), err)
			}
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("gateway TLS directory %s holds no certificate", dir)
	}
	return certs, nil
}

// GetCertificate is the tls.Config hook: the first certificate covering the
// requested server name, else the first one (a client that sends no SNI, or
// one for a name nothing covers, which its own verification then refuses).
func (s *Store) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	certs := s.certs.Load()
	if certs == nil || len(*certs) == 0 {
		return nil, errors.New("no gateway TLS certificate loaded")
	}
	for i := range *certs {
		c := &(*certs)[i]
		if hello.ServerName != "" && c.Leaf.VerifyHostname(hello.ServerName) == nil {
			return c, nil
		}
	}
	return &(*certs)[0], nil
}

// Covers reports whether some certificate is valid for host.
func (s *Store) Covers(host string) bool {
	certs := s.certs.Load()
	if certs == nil {
		return false
	}
	return slices.ContainsFunc(*certs, func(c tls.Certificate) bool { return c.Leaf.VerifyHostname(host) == nil })
}

// Run reloads every interval until ctx ends.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Reload(); err != nil && s.Log != nil {
				s.Log.LogAttrs(ctx, slog.LevelWarn, "gateway TLS reload failed; keeping the certificates in use",
					slog.Any("error", err))
			}
		}
	}
}
