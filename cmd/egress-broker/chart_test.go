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

	"github.com/devthenet-labs/maul/internal/cli"
)

// chartHelpers is the chart's helper library, which spells the broker's
// base-URL override keys out as literals.
const chartHelpers = "../../charts/maul/templates/_helpers.tpl"

// TestChartOverrideKeysMatchFlags pins maul.egressBrokerOverrideHosts to
// this binary's flags: the chart denies the host of every broker base-URL
// override (guard 13 at render time, MAUL_DENY_HOSTS at redirect time), so
// it must read exactly the environment names of the *-base-url flags. A new
// provider override flag cannot ship without the chart denying its host.
func TestChartOverrideKeysMatchFlags(t *testing.T) {
	var want []string
	newServeCmd(cli.NewOptions()).Flags().VisitAll(func(f *pflag.Flag) {
		if strings.HasSuffix(f.Name, "-base-url") {
			want = append(want, "MAUL_"+strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_")))
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
	def := regexp.MustCompile(`(?s)\{\{- define "maul\.egressBrokerOverrideHosts" -\}\}\n(.*?)\n\{\{- end \}\}`).
		FindSubmatch(src)
	if def == nil {
		t.Fatal("_helpers.tpl does not define maul.egressBrokerOverrideHosts")
	}
	keys := regexp.MustCompile(`"(MAUL_[A-Z0-9_]+)"`).FindAllSubmatch(def[1], -1)
	got := make([]string, 0, len(keys))
	for _, k := range keys {
		got = append(got, string(k[1]))
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("maul.egressBrokerOverrideHosts reads %v; the broker's -base-url flags are %v", got, want)
	}
}

// TestChartBrokerEnvAreFlags: every key the chart puts in the broker's
// ConfigMap, per-user credentials on or the openai route on, names one of
// this binary's flags, so a renamed flag cannot leave the chart setting
// nothing (viper ignores an environment name no flag reads).
func TestChartBrokerEnvAreFlags(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	flags := map[string]bool{}
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("egress-broker", "test", opts)
	serve := newServeCmd(opts)
	root.AddCommand(serve)
	for _, fs := range []*pflag.FlagSet{root.PersistentFlags(), serve.Flags()} {
		fs.VisitAll(func(f *pflag.Flag) {
			flags["MAUL_"+strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))] = true
		})
	}
	for _, c := range []struct {
		name string
		sets []string
		want []string
	}{
		{"user credentials", []string{"attribution.mode=attribute", "egressBroker.userCredentials.mode=enforce",
			"egressBroker.anthropicAuth=token", "egressBroker.userCredentials.orgCredentialIsPersonal=acknowledged"},
			[]string{"MAUL_USER_CREDENTIALS", "MAUL_CREDENTIALS_NAMESPACE",
				"MAUL_CREDENTIAL_RECORDS_NAMESPACE", "MAUL_ORG_CREDENTIAL_IS_PERSONAL"}},
		{"openai route", []string{"egressBroker.openai.apiKey.secret=maul-openai"},
			[]string{"MAUL_OPENAI_API_KEY_FILE", "MAUL_ANTHROPIC_API_KEY_FILE"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := make([]string, 0, 7+2*len(c.sets))
			args = append(args, "template", "maul", "../../charts/maul", "--namespace", "maul",
				"--show-only", "templates/egress-broker.yaml")
			for _, set := range c.sets {
				args = append(args, "--set", set)
			}
			out, err := exec.Command("helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v: %s", err, out)
			}
			keys := regexp.MustCompile(`(?m)^\s+(MAUL_[A-Z0-9_]+):`).FindAllSubmatch(out, -1)
			seen := make([]string, 0, len(keys))
			for _, k := range keys {
				key := string(k[1])
				seen = append(seen, key)
				if !flags[key] {
					t.Errorf("the chart sets %s, which no broker flag reads", key)
				}
			}
			for _, want := range c.want {
				if !slices.Contains(seen, want) {
					t.Errorf("the chart does not set %s (keys %v)", want, seen)
				}
			}
		})
	}
}
