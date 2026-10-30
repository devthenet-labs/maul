// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !e2edial

package main

import "github.com/devthenet-labs/maul/internal/runnercfg"

// resolvConf is the file the evaluator's strict sandbox probe target is
// read from: a released intent-controller's own /etc/resolv.conf, whatever
// its environment says (resolv_e2e.go is the e2e suite's seam).
func resolvConf(func(string) string) string { return runnercfg.ResolvConf }
