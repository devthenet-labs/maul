// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !e2edial

package main

import "github.com/devthenet-labs/maul/internal/previewtester"

// testDial is the network dialer's: nil. Only a binary built with the
// e2edial tag (the e2e suite's) can map Service names to other addresses
// (dial_e2e.go); a released tester always dials the name it was given.
func testDial(func(string) string) (previewtester.DialFunc, error) { return nil, nil }

// terminationPath is always the container's termination message file.
func terminationPath(func(string) string) string { return previewtester.TerminationPath }
