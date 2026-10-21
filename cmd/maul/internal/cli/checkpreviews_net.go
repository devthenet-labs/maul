// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !e2edial

package cli

import "github.com/spf13/cobra"

// checkPreviewsTestFlags adds nothing: only a binary built with the e2edial
// tag (the e2e suite's) can point check previews at other addresses than
// the names it checks (checkpreviews_e2e.go). A released binary always asks
// the caller's DNS and the system's roots.
func checkPreviewsTestFlags(*cobra.Command) {}

// checkPreviewsNetwork is the caller's network.
func checkPreviewsNetwork(*cobra.Command) (checkPreviewsDeps, error) {
	return networkCheckPreviewsDeps(), nil
}
