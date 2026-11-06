// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"maps"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want map[string]string
	}{
		{"none", []string{"PATH=/usr/bin"}, nil},
		{"baked cache", []string{"PATH=/usr/local/go/bin", "GOMODCACHE=/opt/go/pkg/mod", "GOTOOLCHAIN=local"},
			map[string]string{"GOMODCACHE": "/opt/go/pkg/mod"}},
		{"all three", []string{"GOPATH=/go", "GOROOT=/usr/local/go", "GOMODCACHE=/go/pkg/mod"},
			map[string]string{"GOPATH": "/go", "GOROOT": "/usr/local/go", "GOMODCACHE": "/go/pkg/mod"}},
		{"last wins", []string{"GOMODCACHE=/a", "GOMODCACHE=/b"}, map[string]string{"GOMODCACHE": "/b"}},
		{"cleaned", []string{"GOMODCACHE=/opt/go/../go/pkg/mod/"}, map[string]string{"GOMODCACHE": "/opt/go/pkg/mod"}},
		{"relative dropped", []string{"GOMODCACHE=pkg/mod", "GOPATH=/go"}, map[string]string{"GOPATH": "/go"}},
		{"empty dropped", []string{"GOMODCACHE="}, nil},
		{"a list is not one directory", []string{"GOPATH=/go:/other"}, nil},
		{"control character", []string{"GOROOT=/usr/local/go\n"}, nil},
		{"root dropped", []string{"GOPATH=/"}, nil},
		{"other names ignored", []string{"GOFLAGS=-mod=mod", "GOPROXY=off", "HOME=/root"}, nil},
		{"no value", []string{"GOMODCACHE"}, nil},
		{"over the bound", []string{"GOMODCACHE=/" + strings.Repeat("a", MaxGoEnvValue)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GoEnv(tc.env); !maps.Equal(got, tc.want) {
				t.Fatalf("GoEnv(%q) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestGoEnvProperty: whatever an image config says, GoEnv keeps only the
// allowlisted names, each a clean absolute directory other than / within
// the bound, with no list separator or control character.
func TestGoEnvProperty(t *testing.T) {
	r := rand.New(rand.NewSource(13))
	names := []string{"GOMODCACHE", "GOPATH", "GOROOT", "GOFLAGS", "PATH", "gomodcache", ""}
	parts := []string{"/", "a", "..", ".", ":", "\x00", "\n", "go", "pkg", "mod", " ", " "}
	for i := range 5000 {
		var env []string
		for range r.Intn(5) {
			var v strings.Builder
			for range r.Intn(8) {
				v.WriteString(parts[r.Intn(len(parts))])
			}
			env = append(env, names[r.Intn(len(names))]+"="+v.String())
		}
		for k, v := range GoEnv(env) {
			if !GoEnvName(k) || !filepath.IsAbs(v) || filepath.Clean(v) != v || v == "/" ||
				strings.ContainsAny(v, ":\x00\n") || len(v) > MaxGoEnvValue {
				t.Fatalf("case %d: GoEnv(%q) kept %s=%q", i, env, k, v)
			}
		}
	}
}
