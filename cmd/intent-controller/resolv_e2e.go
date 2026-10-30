// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package main

import "github.com/devthenet-labs/maul/internal/runnercfg"

// resolvConfEnv names the e2e suite's resolver file. The suite runs
// intent-controller on the host, whose /etc/resolv.conf often names a
// loopback stub (systemd-resolved's 127.0.0.53), which --intent-evaluator
// rightly refuses; the e2edial build reads the file named here instead.
// Never in a released binary (resolv.go).
const resolvConfEnv = "MAUL_E2E_RESOLV_CONF"

// resolvConf is resolvConfEnv when set, else /etc/resolv.conf.
func resolvConf(getenv func(string) string) string {
	if p := getenv(resolvConfEnv); p != "" {
		return p
	}
	return runnercfg.ResolvConf
}
