// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	good := func() Config {
		c := baseConfig()
		c.Checks = []StoredCheck{storedCheck(t, scoreCheck)}
		return c
	}
	tests := []struct {
		name string
		mut  func(c *Config)
		raw  string
		ok   bool
	}{
		{"valid", func(*Config) {}, "", true},
		{"unknown field", nil, `{"components":[],"extra":1}`, false},
		{"no components", func(c *Config) { c.Components = nil }, "", false},
		{"a component named twice", func(c *Config) { c.Components[1].Key = "api" }, "", false},
		{"a component address by name", func(c *Config) { c.Components[0].Addr = "api.slot.svc:80" }, "", false},
		{"a component path not absolute", func(c *Config) { c.Components[0].Path = "api" }, "", false},
		{"a component key not a label", func(c *Config) { c.Components[0].Key = "API" }, "", false},
		{"too few tool calls", func(c *Config) { c.MaxToolCalls = 49 }, "", false},
		{"too many tool calls", func(c *Config) { c.MaxToolCalls = 301 }, "", false},
		{"no timeout", func(c *Config) { c.TimeoutSeconds = 0 }, "", false},
		{"a timeout over an hour", func(c *Config) { c.TimeoutSeconds = 3601 }, "", false},
		{"no viewer", func(c *Config) { c.Viewer = "" }, "", false},
		{"a host with a path", func(c *Config) { c.Host = "a.example/x" }, "", false},
		{"a check whose digest is another's", func(c *Config) {
			c.Checks[0].Digest = "sha256:" + strings.Repeat("0", 64)
		}, "", false},
		{"a check that does not parse", func(c *Config) { c.Checks[0].Body = "{" }, "", false},
		{"a check over 8 KiB", func(c *Config) { c.Checks[0].Body = strings.Repeat(" ", 8193) }, "", false},
		{"a check named twice", func(c *Config) { c.Checks = append(c.Checks, c.Checks[0]) }, "", false},
		{"a replay round with no checks", func(c *Config) { c.Checks, c.ReplayOnly = nil, true }, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			if tc.mut != nil {
				c := good()
				tc.mut(&c)
				var err error
				if raw, err = json.Marshal(c); err != nil {
					t.Fatal(err)
				}
			}
			_, err := ParseConfig(raw)
			if (err == nil) != tc.ok {
				t.Fatalf("ParseConfig = %v, want ok %v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrConfig) {
				t.Errorf("error %v is not ErrConfig", err)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("LoadConfig of a directory without the config succeeded")
	}
	raw, _ := json.Marshal(baseConfig())
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 10*time.Minute {
		t.Errorf("Timeout() = %s", c.Timeout())
	}
}

// fakeListener hands out pipes that report a chosen remote address.
type fakeListener struct {
	conns chan net.Conn
}

type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }

func (l *fakeListener) Accept() (net.Conn, error) {
	c, ok := <-l.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *fakeListener) Close() error   { return nil }
func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 8090} }

func TestGuardListenerRefusesLocalCallers(t *testing.T) {
	own := []net.IP{net.ParseIP("10.42.0.17"), net.ParseIP("fd00::17")}
	tests := []struct {
		from    string
		refused bool
	}{
		{"127.0.0.1", true},
		{"127.9.9.9", true},
		{"::1", true},
		{"10.42.0.17", true}, // the pod's own address: page JS, sandbox code
		{"fd00::17", true},
		{"::ffff:10.42.0.17", true},
		{"10.42.3.4", false}, // the evaluator
	}
	for _, tc := range tests {
		t.Run(tc.from, func(t *testing.T) {
			fl := &fakeListener{conns: make(chan net.Conn, 2)}
			g := GuardListener(fl, own)
			local, peer := net.Pipe()
			fl.conns <- addrConn{local, &net.TCPAddr{IP: net.ParseIP(tc.from), Port: 5555}}
			marker, markerPeer := net.Pipe()
			fl.conns <- addrConn{marker, &net.TCPAddr{IP: net.IPv4(10, 1, 1, 1), Port: 1}}
			close(fl.conns)
			got, err := g.Accept()
			if err != nil {
				t.Fatal(err)
			}
			from := (&net.TCPAddr{IP: net.ParseIP(tc.from), Port: 5555}).String()
			if refused := got.RemoteAddr().String() != from; refused != tc.refused {
				t.Errorf("accepted %s, refused %v, want refused %v", got.RemoteAddr(), refused, tc.refused)
			}
			if tc.refused {
				// The refused connection was closed.
				_ = peer.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := peer.Read(make([]byte, 1)); err == nil {
					t.Errorf("a refused connection stayed open")
				}
			}
			_ = got.Close()
			_ = markerPeer.Close()
			_ = peer.Close()
		})
	}
}
