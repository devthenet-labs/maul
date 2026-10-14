// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpguard

import (
	"net"
	"net/http"
)

// SourceIP is the caller's address without the port: in-cluster there is no
// intermediary between an agent pod and a patchy proxy's Service, so this is
// the pod IP. No forwarding header is ever consulted, since the caller
// controls those. A RemoteAddr without a port is returned as it is.
func SourceIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
