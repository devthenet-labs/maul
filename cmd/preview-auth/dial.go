// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !e2edial

package main

import (
	"github.com/spf13/cobra"

	"github.com/devthenet-labs/maul/internal/cli"
	"github.com/devthenet-labs/maul/internal/previewauth/httpapi"
)

// testDialFlags adds nothing: only a binary built with the e2edial tag (the
// e2e suite's) can map slot Service names to other addresses
// (dial_e2e.go). A released binary always dials the name it routes to.
func testDialFlags(*cobra.Command) {}

// upstreamDial is the network dialer's: nil.
func upstreamDial(*cli.Options) (httpapi.DialFunc, error) { return nil, nil }
