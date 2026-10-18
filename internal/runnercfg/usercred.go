// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnercfg

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/pflag"

	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/credgate"
)

// maxAccountURL bounds --account-url: it is written into GitHub comments.
const maxAccountURL = 512

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

// RegisterAccountURLFlag adds --account-url alone, for a controller that
// only tells people where to add a credential (integration-controller's
// tracking issue projection).
func RegisterAccountURLFlag(f *pflag.FlagSet) {
	f.String(credgate.AccountURLFlag, "",
		"the status page's Account page, linked from every notice that a run waits for a person's credential "+
			"(empty: the notice names the page without a link)")
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

// AccountURL reads --account-url: empty, or an absolute http(s) URL with no
// whitespace, at most 512 bytes.
func AccountURL(opts *cli.Options) (string, error) {
	raw := opts.String(credgate.AccountURLFlag)
	if raw == "" {
		return "", nil
	}
	if err := checkAccountURL(raw); err != nil {
		return "", fmt.Errorf("--%s: %w", credgate.AccountURLFlag, err)
	}
	return raw, nil
}

func checkAccountURL(raw string) error {
	if len(raw) > maxAccountURL {
		return fmt.Errorf("longer than %d bytes", maxAccountURL)
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f || r > 0x7e }) {
		return fmt.Errorf("%q holds whitespace, control or non-ASCII characters", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return fmt.Errorf("%q is not an absolute http or https URL without credentials", raw)
	}
	return nil
}
