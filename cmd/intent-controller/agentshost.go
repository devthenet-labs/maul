// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"log/slog"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/devthenet-labs/maul/internal/controller/intent"
	"github.com/devthenet-labs/maul/internal/controller/intent/agentshost"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/runnerguard"
)

// agentsHostOn reports the agents host serving: the evaluator with
// --intent-evaluator-agents (Settings.agentsProject's switches).
func agentsHostOn(set intent.Settings) bool { return set.Evaluator && set.EvaluatorAgents }

// agentsHostSettings are the agents host's settings from intent-controller's.
func agentsHostSettings(set intent.Settings, harness string) agentshost.Settings {
	return agentshost.Settings{
		Namespace: set.Namespace, ToolsImage: set.ToolsImage, MaxToolPods: set.MaxToolPods,
		BuildTimeoutMax: set.ToolBuildTimeoutMax, Resources: set.ToolResources, PollInterval: set.PollInterval,
		Harness: harness, CodexHarness: set.Codex.ID, Attribution: set.Attribution,
	}
}

// setupAgentsHost registers the agents tool pod sweep, always (with the
// agents host off it deletes every tool Job, which none serves: a rollback
// leaves none behind), and, with the host on, its reconciler, the single
// writer of agents verification status (SPEC-S2 S2-13).
func setupAgentsHost(mgr ctrl.Manager, set intent.Settings, cs kubernetes.Interface, jc *jobs.Client,
	images runnerguard.Guard, harness string, log *slog.Logger) error {
	tools := agentshost.Kube{CS: cs, Reader: mgr.GetAPIReader(), Jobs: jc, Namespace: set.AgentNamespace}
	on := agentsHostOn(set)
	if err := mgr.Add(&agentshost.Sweeper{Client: mgr.GetClient(), Tools: tools, Namespace: set.Namespace,
		Enabled: on, Interval: set.PollInterval, Log: log}); err != nil {
		return err
	}
	if !on {
		return nil
	}
	return (&agentshost.Reconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Tools: tools, Images: images,
		Settings: agentsHostSettings(set, harness), Log: log,
	}).SetupWithManager(mgr)
}
