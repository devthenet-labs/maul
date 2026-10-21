// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"maps"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/controller/preview"
)

// previewRuntimeArgs renders the preview foundation with the controller on.
var previewRuntimeArgs = []string{
	"-f", "../../hack/testdata/chart-render/preview-foundation.yaml",
	"-f", "../../hack/testdata/chart-render/intent-controller.yaml",
	"-f", "../../hack/testdata/chart-render/preview-controller.yaml",
}

// noLegacyNodes clears the fixture's legacy NodePool and NodeClass.
var noLegacyNodes = []string{"--set", "preview.nodeIsolation.nodePool=", "--set", "preview.nodeIsolation.nodeClass="}

// TestChartNodeIsolationIsTheControllers: every node isolation shape the
// chart renders is one preview-controller starts with (Settings.Validate, as
// cmd/preview-controller builds it from the ConfigMap), and it selects the
// nodes the chart meant.
func TestChartNodeIsolationIsTheControllers(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	eks := map[string]string{"karpenter.sh/nodepool": "maul-preview", "eks.amazonaws.com/nodeclass": "maul-preview"}
	generic := map[string]string{"maul.example.com/previews": "true"}
	for _, tc := range []struct {
		name   string
		args   []string
		want   map[string]string
		isNone bool
	}{
		{name: "legacy EKS names", want: eks},
		{name: "eksAutoMode", args: append(append([]string{}, noLegacyNodes...),
			"--set", "preview.nodeIsolation.eksAutoMode.nodePool=maul-preview",
			"--set", "preview.nodeIsolation.eksAutoMode.nodeClass=maul-preview"), want: eks},
		{name: "generic selector", args: append(append([]string{}, noLegacyNodes...),
			"--set-json", `preview.nodeIsolation.selector={"maul.example.com/previews":"true"}`), want: generic},
		{name: "EKS and generic", args: []string{
			"--set-json", `preview.nodeIsolation.selector={"maul.example.com/previews":"true"}`},
			want: map[string]string{"karpenter.sh/nodepool": "maul-preview",
				"eks.amazonaws.com/nodeclass": "maul-preview", "maul.example.com/previews": "true"}},
		{name: "none", args: append(append([]string{}, noLegacyNodes...),
			"--set", "preview.nodeIsolation.taintKey=", "--set", "preview.nodeIsolation.mode=none"), isNone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, previewRuntimeArgs...), tc.args...)
			cm := renderConfigMaps(t, args)["maul-preview-controller-config"]
			s := settingsFrom(t, cm)
			if err := s.Validate(); err != nil {
				t.Fatalf("preview-controller refuses the chart's settings: %v (ConfigMap %v)", err, cm)
			}
			got := maps.Clone(s.NodeSelector)
			if got == nil {
				got = map[string]string{}
			}
			if s.NodePool != "" {
				got["karpenter.sh/nodepool"] = s.NodePool
			}
			if s.NodeClass != "" {
				got["eks.amazonaws.com/nodeclass"] = s.NodeClass
			}
			if tc.isNone {
				if len(got) != 0 || s.TaintKey != "" || s.NodeIsolation != preview.NodeIsolationNone {
					t.Fatalf("mode none renders selector %v, taint %q, isolation %q", got, s.TaintKey, s.NodeIsolation)
				}
				return
			}
			if !maps.Equal(got, tc.want) || s.TaintKey != previewTaintKey {
				t.Fatalf("selector %v, taint %q; want %v, %q", got, s.TaintKey, tc.want, previewTaintKey)
			}
		})
	}
}

// settingsFrom builds preview-controller's Settings from its ConfigMap, as
// cmd/preview-controller does from the flags the keys bind.
func settingsFrom(t *testing.T, cm map[string]string) preview.Settings {
	t.Helper()
	atoi := func(k string) int {
		n, err := strconv.Atoi(cm[k])
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		return n
	}
	dur := func(k string) time.Duration {
		d, err := time.ParseDuration(cm[k])
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		return d
	}
	s := preview.Settings{
		Namespace: "maul", SlotCount: atoi("MAUL_PREVIEW_SLOT_COUNT"),
		ImagePrefix: cm["MAUL_PREVIEW_IMAGE_PREFIX"], HostSuffix: cm["MAUL_PREVIEW_HOST_SUFFIX"],
		NodePool: cm["MAUL_PREVIEW_NODE_POOL"], NodeClass: cm["MAUL_PREVIEW_NODE_CLASS"],
		TaintKey: cm["MAUL_PREVIEW_TAINT_KEY"], NodeIsolation: cm["MAUL_PREVIEW_NODE_ISOLATION"],
		RolloutTimeout: dur("MAUL_PREVIEW_ROLLOUT_TIMEOUT"), PollInterval: dur("MAUL_PREVIEW_POLL_INTERVAL"),
		MaxRetries: int32(atoi("MAUL_PREVIEW_MAX_RETRIES")),
	}
	if s.NodeIsolation == "" {
		s.NodeIsolation = preview.NodeIsolationRequired
	}
	selector, err := preview.ParseNodeSelector(cm["MAUL_PREVIEW_NODE_SELECTOR"])
	if err != nil {
		t.Fatal(err)
	}
	s.NodeSelector = selector
	return s
}
