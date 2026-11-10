// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build linux

package toolexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSideClientRefusesCrossing (review SEC-1): a side client reaches only
// a socket that is no link and is served by its side's UID. A sandbox can
// swap its own socket for a symlink to the other side's, and a foreign
// peer never answers as the side's shim; neither is ever a result.
func TestSideClientRefusesCrossing(t *testing.T) {
	_, sock := serve(t, newServer(t))
	link := filepath.Join(filepath.Dir(sock), "link")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		socket  string
		uid     int
		ok      bool
		foreign bool
	}{
		{"its own socket, its UID", sock, uid, true, false},
		{"a symlink to a socket of its UID", link, uid, false, false},
		{"a socket of another UID", sock, uid + 1, false, true},
	} {
		c := NewSideClient(tc.socket, tc.uid)
		err := c.Ready(ctx)
		if (err == nil) != tc.ok {
			t.Errorf("%s: Ready %v, want ok=%v", tc.name, err, tc.ok)
		}
		if got := errors.Is(err, ErrForeignPeer); got != tc.foreign {
			t.Errorf("%s: ErrForeignPeer %v, want %v (%v)", tc.name, got, tc.foreign, err)
		}
		res, err := c.Do(ctx, &Request{Argv: []string{"true"}})
		if !tc.ok && (err == nil || res != (Result{})) {
			t.Errorf("%s: Do %+v, %v; want no result", tc.name, res, err)
		}
	}
}
