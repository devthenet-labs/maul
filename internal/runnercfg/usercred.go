// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnercfg

import (
	"fmt"
	"slices"

	"github.com/spf13/pflag"

	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/credgate"
	"github.com/bitwise-media-group/patchy/internal/model"
)

// RegisterUserCredentialsFlags adds --user-credentials and --account-url,
// shared by the controllers that hold a person's run until their credential
// is valid (remediation, intent): off by default.
func RegisterUserCredentialsFlags(f *pflag.FlagSet) {
	f.String(credgate.Flag, string(credgate.ModeOff),
		"per-user model credentials (the chart's egressBroker.userCredentials.mode): off and attribute create "+
			"every run as before; enforce holds a run attributed to a person until their ModelCredential is Valid "+
			"(needs --"+attribution.Flag+" attribute)")
	RegisterAccountURLFlag(f)
}

// RegisterAccountURLFlag adds --account-url alone.
func RegisterAccountURLFlag(f *pflag.FlagSet) {
	f.String(credgate.AccountURLFlag, "", credgate.AccountURLUsage)
}

// UserCredentials reads --user-credentials and --account-url. enforce is
// refused unless attribution stamps runs: without a stamp every run is the
// org's, so enforce would hold nothing while promising it does.
func UserCredentials(opts *cli.Options, attr attribution.Mode) (credgate.Mode, string, error) {
	mode, err := credgate.ParseMode(opts.String(credgate.Flag))
	if err != nil {
		return "", "", fmt.Errorf("--%s: %w", credgate.Flag, err)
	}
	if mode.Enforced() && !attr.On() {
		return "", "", fmt.Errorf("--%s %s needs --%s %s: whose credential pays is read from the run's attribution",
			credgate.Flag, mode, attribution.Flag, attribution.ModeAttribute)
	}
	u, err := AccountURL(opts)
	if err != nil {
		return "", "", err
	}
	return mode, u, nil
}

// AccountURL reads --account-url: empty, or a URL credgate.CheckAccountURL
// accepts.
func AccountURL(opts *cli.Options) (string, error) {
	raw := opts.String(credgate.AccountURLFlag)
	if raw == "" {
		return "", nil
	}
	if err := credgate.CheckAccountURL(raw); err != nil {
		return "", fmt.Errorf("--%s: %w", credgate.AccountURLFlag, err)
	}
	return raw, nil
}

// CodexUserCredentials refuses per-user credentials beside an enabled codex
// harness: they are Anthropic API keys and setup-tokens only for now, the
// broker refuses to start them beside its openai route, and a codex run
// must never be held on, or paid by, an Anthropic record. A temporary
// guard (codex broker design, group 2) that goes once per-user OpenAI keys
// exist. enabled is the resolved harness set.
func CodexUserCredentials(mode credgate.Mode, enabled []string) error {
	if mode == credgate.ModeOff || !slices.Contains(enabled, model.HarnessCodex) {
		return nil
	}
	return fmt.Errorf("--%s %s with the codex harness enabled: per-user credentials cover Anthropic only for "+
		"now, and the egress broker refuses them beside its openai route; set --%s %s, or disable codex "+
		"(--harnesses)", credgate.Flag, mode, credgate.Flag, credgate.ModeOff)
}
