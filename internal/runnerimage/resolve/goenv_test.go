// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package resolve

import (
	"context"
	"maps"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// TestResolveGoEnv: the Go toolchain directories a config bakes are
// recorded beside the search path (SPEC-S2 S2-13 step 0); an index whose
// runnable children bake different ones records none, and is still
// accepted.
func TestResolveGoEnv(t *testing.T) {
	baked := []string{"PATH=/usr/local/go/bin:/usr/bin", "GOMODCACHE=/opt/go/pkg/mod", "GOFLAGS=-mod=mod"}
	other := []string{"PATH=/usr/local/go/bin:/usr/bin", "GOMODCACHE=/other/mod"}
	cfg := func(arch string, env []string) *v1.ConfigFile {
		return &v1.ConfigFile{Architecture: arch, Config: v1.Config{Env: env}}
	}
	for _, tc := range []struct {
		name     string
		children []indexChild
		want     map[string]string
	}{
		{"one manifest", []indexChild{{image(t, cfg("", baked), []byte("a")), linux("amd64")}},
			map[string]string{"GOMODCACHE": "/opt/go/pkg/mod"}},
		{"children agree", []indexChild{
			{image(t, cfg("", baked), []byte("a")), linux("amd64")},
			{image(t, cfg("arm64", baked), []byte("b")), linux("arm64")}},
			map[string]string{"GOMODCACHE": "/opt/go/pkg/mod"}},
		{"children disagree", []indexChild{
			{image(t, cfg("", baked), []byte("a")), linux("amd64")},
			{image(t, cfg("arm64", other), []byte("b")), linux("arm64")}}, nil},
		{"none baked", []indexChild{{image(t, cfg("", baked[:1]), []byte("a")), linux("amd64")}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRegistry(t, false, nil)
			pushIndex(t, repo.Tag("v1"), tc.children...)
			got, err := newResolver(t, Config{}).Resolve(context.Background(), declared(t, repo.String()+":v1"))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !maps.Equal(got.GoEnv, tc.want) {
				t.Errorf("GoEnv = %v, want %v", got.GoEnv, tc.want)
			}
		})
	}
}
