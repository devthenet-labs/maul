// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/egressproxy"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

const chartDir = "../../charts/patchy"

// chartTunnelFlags maps each egressProxy.tunnel value to the flag it stamps
// (PATCHY_TUNNEL_<FLAG> in the proxy's ConfigMap). denyCIDRs and
// sourceBudgetIdle are stamped from more than the value alone, so they are
// checked through the render below instead.
var chartTunnelFlags = map[string]string{
	"maxConcurrent":          "tunnel-max-concurrent",
	"maxConcurrentPerSource": "tunnel-max-concurrent-per-source",
	"ratePerSource":          "tunnel-rate-per-source",
	"burstPerSource":         "tunnel-burst-per-source",
	"limitWait":              "tunnel-limit-wait",
	"dialTimeout":            "tunnel-dial-timeout",
	"helloTimeout":           "tunnel-hello-timeout",
	"idleTimeout":            "tunnel-idle-timeout",
	"maxDuration":            "tunnel-max-duration",
	"maxUploadBytes":         "tunnel-max-upload-bytes",
	"maxDownloadBytes":       "tunnel-max-download-bytes",
	"sourceUploadBudget":     "tunnel-source-upload-budget",
	"sourceDownloadBudget":   "tunnel-source-download-budget",
	"maxSources":             "tunnel-max-sources",
	"progressInterval":       "tunnel-progress-interval",
}

// TestChartTunnelDefaultsMatchFlags: every egressProxy.tunnel default in the
// chart's values.yaml is this binary's flag default (equal as a number or a
// duration), so an operator reading values.yaml reads what the proxy does,
// and every value has a flag.
func TestChartTunnelDefaultsMatchFlags(t *testing.T) {
	raw, err := os.ReadFile(chartDir + "/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		EgressProxy struct {
			Tunnel map[string]any `json:"tunnel"`
		} `json:"egressProxy"`
	}
	if err := sigsyaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	flags := newServeCmd(cli.NewOptions()).Flags()
	seen := map[string]bool{}
	for key, v := range values.EgressProxy.Tunnel {
		seen[key] = true
		if key == "denyCIDRs" || key == "sourceBudgetIdle" {
			continue
		}
		name, ok := chartTunnelFlags[key]
		if !ok {
			t.Errorf("egressProxy.tunnel.%s maps to no flag", key)
			continue
		}
		f := flags.Lookup(name)
		if f == nil {
			t.Errorf("egressProxy.tunnel.%s: the binary has no --%s", key, name)
			continue
		}
		if !sameValue(t, f.DefValue, v) {
			t.Errorf("egressProxy.tunnel.%s = %v, but --%s defaults to %s", key, v, name, f.DefValue)
		}
	}
	for key := range chartTunnelFlags {
		if !seen[key] {
			t.Errorf("values.yaml has no egressProxy.tunnel.%s", key)
		}
	}
	for _, key := range []string{"denyCIDRs", "sourceBudgetIdle"} {
		if !seen[key] {
			t.Errorf("values.yaml has no egressProxy.tunnel.%s", key)
		}
	}
}

// sameValue reports whether a flag default and a values.yaml value mean
// the same: equal durations, or equal numbers.
func sameValue(t *testing.T, def string, v any) bool {
	t.Helper()
	if s, ok := v.(string); ok {
		a, errA := time.ParseDuration(def)
		b, errB := time.ParseDuration(s)
		return errA == nil && errB == nil && a == b
	}
	n, ok := v.(float64)
	if !ok {
		return false
	}
	d, err := strconv.ParseFloat(def, 64)
	return err == nil && d == n
}

// TestChartTunnelRenderStarts: the proxy ConfigMap the chart renders for the
// tunnel fixture, every PATCHY_TUNNEL_* key and the profile set as the
// environment, starts this binary with the flag defaults, the budget floor
// at the longest Job deadline (the intent controller's 90m) and the API
// server's address beside tunnel.denyCIDRs on the tunnel's deny list.
func TestChartTunnelRenderStarts(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	fixtures := "../../hack/testdata/chart-render/"
	out, err := exec.CommandContext(t.Context(), "helm", "template", "patchy", chartDir, "--namespace", "patchy",
		"-f", fixtures+"repository-images.yaml", "-f", fixtures+"intent-controller.yaml",
		"-f", fixtures+"egress-tunnel.yaml").Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(out)), 4096)
	var data map[string]string
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		if u.GetKind() == "ConfigMap" && u.GetName() == "patchy-egress-proxy-config" {
			data, _, _ = unstructured.NestedStringMap(u.Object, "data")
		}
	}
	tunnelKeys := 0
	for k, v := range data {
		if strings.HasPrefix(k, "PATCHY_TUNNEL_") {
			tunnelKeys++
			t.Setenv(k, v)
		}
	}
	if tunnelKeys == 0 {
		t.Fatal("the tunnel fixture rendered no PATCHY_TUNNEL_* key")
	}
	s, err := configure(serveOpts(t, "--network-profiles", data[netprofile.EnvProfiles]))
	if err != nil {
		t.Fatalf("the chart's tunnel render does not start the proxy: %v", err)
	}
	want := egressproxy.TunnelLimits{
		MaxConcurrent:        egressproxy.DefaultTunnelMaxConcurrent,
		PerSourceConcurrent:  egressproxy.DefaultTunnelPerSourceConcurrent,
		PerSourceRate:        egressproxy.DefaultTunnelPerSourceRate,
		PerSourceBurst:       egressproxy.DefaultTunnelPerSourceBurst,
		Wait:                 egressproxy.DefaultTunnelLimitWait,
		DialTimeout:          egressproxy.DefaultTunnelDialTimeout,
		HelloTimeout:         egressproxy.DefaultTunnelHelloTimeout,
		IdleTimeout:          egressproxy.DefaultTunnelIdleTimeout,
		MaxDuration:          egressproxy.DefaultTunnelMaxDuration,
		MaxUploadBytes:       egressproxy.DefaultTunnelMaxUploadBytes,
		MaxDownloadBytes:     egressproxy.DefaultTunnelMaxDownloadBytes,
		SourceUploadBudget:   egressproxy.DefaultTunnelSourceUploadBudget,
		SourceDownloadBudget: egressproxy.DefaultTunnelSourceDownloadBudget,
		SourceBudgetIdle:     egressproxy.DefaultTunnelSourceBudgetIdle,
		MinBudgetIdle:        90 * time.Minute,
		MaxSources:           egressproxy.DefaultTunnelMaxSources,
		ProgressInterval:     egressproxy.DefaultTunnelProgressInterval,
		Deny:                 []netip.Prefix{netip.MustParsePrefix("10.0.1.10/32"), netip.MustParsePrefix("203.0.113.10/32")},
	}
	if !reflect.DeepEqual(s.engine.TunnelLimits, want) {
		t.Errorf("tunnel limits from the chart = %+v\nwant %+v", s.engine.TunnelLimits, want)
	}
	if _, err := egressproxy.New(s.engine, nil); err != nil {
		t.Errorf("the chart's tunnel profiles did not start the engine: %v", err)
	}
}
