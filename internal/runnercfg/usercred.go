// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnercfg

import (
	"fmt"

	"github.com/spf13/pflag"

	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/credgate"
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
