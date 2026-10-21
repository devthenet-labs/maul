// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagemeter

import (
	"fmt"
	"log/slog"

	"github.com/spf13/pflag"
)

// Flag names, shared by every controller that launches agent Jobs.
const (
	FlagMeasurement = "usage-measurement"
	FlagBrokerURL   = "usage-broker-url"
	FlagTokenFile   = "usage-token-file"
)

// DefaultTokenFile is where the chart mounts the controllers' projected
// token for usagewire.Audience.
const DefaultTokenFile = "/var/run/secrets/maul/broker-usage/token"

// RegisterFlags adds the --usage-* flags: measurement off by default.
func RegisterFlags(f *pflag.FlagSet) {
	f.Bool(FlagMeasurement, false,
		"record each settled run's usage as the egress broker metered it (status usage measured, source and "+
			"final); off records nothing")
	f.String(FlagBrokerURL, "",
		"base URL of the egress broker's usage endpoint (its own port); required with --"+FlagMeasurement)
	f.String(FlagTokenFile, DefaultTokenFile,
		"projected ServiceAccount token, bound to the broker's usage audience, presented to the usage endpoint")
}

// Config is the flags' values.
type Config struct {
	On        bool
	BrokerURL string
	TokenFile string
}

// New builds the Meter the flags configure: nil when off, an error when on
// without a usable broker URL.
func New(cfg Config, log *slog.Logger) (*Meter, error) {
	if !cfg.On {
		return nil, nil
	}
	c, err := NewClient(cfg.BrokerURL, cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", FlagMeasurement, err)
	}
	return &Meter{Fetcher: c, Log: log}, nil
}
