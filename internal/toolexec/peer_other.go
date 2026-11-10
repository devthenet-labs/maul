// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !linux

package toolexec

import (
	"errors"
	"net"
)

// peerUID reads a peer's credentials on linux only, where the tool pod
// runs; elsewhere every side client's dial is refused.
func peerUID(net.Conn) (int, error) {
	return 0, errors.New("peer credentials are read on linux only")
}
