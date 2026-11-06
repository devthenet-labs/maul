// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/controller/intent"
	"github.com/devthenet-labs/maul/internal/controller/intent/agentshost"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// TestAgentsHostSettings: the agents host serves exactly under the
// evaluator with --intent-evaluator-agents, on intent-controller's own S2
// flags.
func TestAgentsHostSettings(t *testing.T) {
	set := intent.Settings{Namespace: "maul", AgentNamespace: "maul-agents", Evaluator: true, EvaluatorAgents: true,
		ToolsImage: "ghcr.io/x/preview-tools@sha256:" + "ab", MaxToolPods: 3, ToolBuildTimeoutMax: 15 * time.Minute,
		ToolResources: jobs.ToolPodResources{Sandbox: jobs.ToolResources{CPU: "2"}}, PollInterval: time.Minute,
		Codex: intent.RunHarness{ID: "codex"}, Attribution: attribution.ModeAttribute}
	got := agentsHostSettings(set, "claude")
	want := agentshost.Settings{Namespace: "maul", ToolsImage: set.ToolsImage, MaxToolPods: 3,
		BuildTimeoutMax: 15 * time.Minute, Resources: set.ToolResources, PollInterval: time.Minute,
		Harness: "claude", CodexHarness: "codex", Attribution: attribution.ModeAttribute}
	if got != want {
		t.Errorf("agentsHostSettings = %+v, want %+v", got, want)
	}
	for _, tc := range []struct {
		evaluator, agents, on bool
	}{{true, true, true}, {true, false, false}, {false, true, false}, {false, false, false}} {
		set.Evaluator, set.EvaluatorAgents = tc.evaluator, tc.agents
		if agentsHostOn(set) != tc.on {
			t.Errorf("evaluator %v agents %v: on = %v, want %v", tc.evaluator, tc.agents, !tc.on, tc.on)
		}
	}
}
