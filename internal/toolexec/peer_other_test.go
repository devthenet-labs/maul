// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !linux

package toolexec

import (
	"context"
	"os"
	"testing"
)

// TestSideClientRefusesOffLinux: peer credentials are read on linux only,
// so elsewhere a side client refuses even its own side's socket: no call
// ever reaches a shim it could not check.
func TestSideClientRefusesOffLinux(t *testing.T) {
	_, sock := serve(t, newServer(t))
	c := NewSideClient(sock, os.Getuid())
	ctx := context.Background()
	if err := c.Ready(ctx); err == nil {
		t.Error("Ready reached a shim whose peer was never checked")
	}
	if res, err := c.Do(ctx, &Request{Argv: []string{"true"}}); err == nil || res != (Result{}) {
		t.Errorf("Do %+v, %v; want no result", res, err)
	}
}
