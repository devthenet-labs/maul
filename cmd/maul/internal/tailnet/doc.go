// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package tailnet finds the preview gateway's tailnet device as the
// caller's own Tailscale client sees it, for `maul setup previews
// --detect` and `maul check previews` (setup-previews-easy (b)).
//
// The Tailscale operator records the device it creates for an annotated
// ClusterIP Service only in the proxy's state Secret, beside the device's
// private keys, and the Kubernetes API hands a Secret out whole: reading
// one key alone is not possible. So the address is never read from the
// cluster. The caller's machine, when it is on the tailnet, already knows
// every peer's name, tags and addresses, and `tailscale status --json`
// says them without a credential and without changing anything.
//
// Two halves, kept apart so the judging is pure:
//
//   - ParseStatus and Find are pure (stdlib only, a test pins that):
//     ParseStatus reads the client's status into Devices, keeping only
//     what validates (a DNS name, tags, tailnet IPv4 addresses in
//     100.64.0.0/10), so nothing the client says reaches a terminal
//     unchecked; Find picks the one Running device that carries every
//     tag asked for under the hostname asked for, and says why when there
//     is none or more than one. Seeded properties hold it to that.
//   - Local runs the caller's own client (tailscale on PATH, else the
//     macOS app's bundled CLI) with a deadline and a bounded output: no
//     daemon socket is spoken to directly, nothing is written.
package tailnet
