// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"os"
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
