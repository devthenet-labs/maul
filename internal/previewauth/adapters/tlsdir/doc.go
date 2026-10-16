// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package tlsdir serves the preview gateway's own TLS certificates, for an
// exposure with no TLS-terminating edge in front (a LoadBalancer Service,
// or a tunnel that passes TLS through). The chart projects each operator TLS
// Secret (kubernetes.io/tls: tls.crt and tls.key) into one subdirectory of a
// directory; Load reads every one, and GetCertificate picks by SNI the
// first whose certificate covers the requested name. Reload re-reads the
// directory (a renewed Secret, which the kubelet swaps in place) and keeps
// the last good set on a failure, so a half-written renewal never takes the
// listener down. No private key is ever logged.
package tlsdir
