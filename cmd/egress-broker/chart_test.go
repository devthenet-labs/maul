// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/bitwise-media-group/patchy/internal/cli"
)

// chartHelpers is the chart's helper library, which spells the broker's
// base-URL override keys out as literals.
const chartHelpers = "../../charts/patchy/templates/_helpers.tpl"

// TestChartOverrideKeysMatchFlags pins patchy.egressBrokerOverrideHosts to
// this binary's flags: the chart denies the host of every broker base-URL
// override (guard 13 at render time, PATCHY_DENY_HOSTS at redirect time), so
// it must read exactly the environment names of the *-base-url flags. A new
// provider override flag cannot ship without the chart denying its host.
func TestChartOverrideKeysMatchFlags(t *testing.T) {
	var want []string
	newServeCmd(cli.NewOptions()).Flags().VisitAll(func(f *pflag.Flag) {
		if strings.HasSuffix(f.Name, "-base-url") {
			want = append(want, "PATCHY_"+strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_")))
		}
	})
	if len(want) == 0 {
		t.Fatal("the serve command has no -base-url flags; the override surface moved")
	}
	slices.Sort(want)

	src, err := os.ReadFile(chartHelpers)
	if err != nil {
		t.Fatal(err)
	}
	def := regexp.MustCompile(`(?s)\{\{- define "patchy\.egressBrokerOverrideHosts" -\}\}\n(.*?)\n\{\{- end \}\}`).
		FindSubmatch(src)
	if def == nil {
		t.Fatal("_helpers.tpl does not define patchy.egressBrokerOverrideHosts")
	}
	keys := regexp.MustCompile(`"(PATCHY_[A-Z0-9_]+)"`).FindAllSubmatch(def[1], -1)
	got := make([]string, 0, len(keys))
	for _, k := range keys {
		got = append(got, string(k[1]))
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("patchy.egressBrokerOverrideHosts reads %v; the broker's -base-url flags are %v", got, want)
	}
}

// TestChartBrokerEnvAreFlags: every key the chart puts in the broker's
// ConfigMap, per-user credentials on, names one of this binary's flags, so
// a renamed flag cannot leave the chart setting nothing (viper ignores an
// environment name no flag reads).
func TestChartBrokerEnvAreFlags(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	out, err := exec.Command("helm", "template", "patchy", "../../charts/patchy", "--namespace", "patchy",
		"--set", "attribution.mode=attribute", "--set", "egressBroker.userCredentials.mode=enforce",
		"--set", "egressBroker.anthropicAuth=token",
		"--set", "egressBroker.userCredentials.orgCredentialIsPersonal=acknowledged",
		"--show-only", "templates/egress-broker.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, out)
	}
	flags := map[string]bool{}
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("egress-broker", "test", opts)
	serve := newServeCmd(opts)
	root.AddCommand(serve)
	for _, fs := range []*pflag.FlagSet{root.PersistentFlags(), serve.Flags()} {
		fs.VisitAll(func(f *pflag.Flag) {
			flags["PATCHY_"+strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))] = true
		})
	}
	keys := regexp.MustCompile(`(?m)^\s+(PATCHY_[A-Z0-9_]+):`).FindAllSubmatch(out, -1)
	seen := make([]string, 0, len(keys))
	for _, k := range keys {
		key := string(k[1])
		seen = append(seen, key)
		if !flags[key] {
			t.Errorf("the chart sets %s, which no broker flag reads", key)
		}
	}
	for _, want := range []string{"PATCHY_USER_CREDENTIALS", "PATCHY_CREDENTIALS_NAMESPACE",
		"PATCHY_CREDENTIAL_RECORDS_NAMESPACE", "PATCHY_ORG_CREDENTIAL_IS_PERSONAL"} {
		if !slices.Contains(seen, want) {
			t.Errorf("the chart does not set %s with per-user credentials on (keys %v)", want, seen)
		}
	}
}
