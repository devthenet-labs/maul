// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/web/authz"
)

// agentsEvaluation is an agents round: no Preview, a head and a base run of
// one saved check, both trees built.
func agentsEvaluation(unbuildable bool) *v1alpha1.PreviewVerification {
	digest := "sha256:" + evalShaA
	pv := &v1alpha1.PreviewVerification{}
	pv.Name = evalPV
	pv.Spec.Placement = v1alpha1.PlacementAgents
	pv.Spec.Checks = []v1alpha1.SavedCheck{{ID: "c1", Outcome: "o1", Form: v1alpha1.CheckFormExec,
		Digest: digest, Body: `{"exec":["x"]}`}}
	pv.Status.Replay = []v1alpha1.CheckRun{{CheckDigest: digest, Result: v1alpha1.CheckRunPass, Token: "t1", Seq: 3}}
	pv.Status.Base = &v1alpha1.BaseStatus{Unbuildable: unbuildable}
	if !unbuildable {
		pv.Status.Base.Runs = []v1alpha1.CheckRun{{CheckDigest: digest, Result: v1alpha1.CheckRunFail,
			Token: "t2", Seq: 4, Tree: v1alpha1.TreeBase}}
	}
	pv.Status.Ledger = &v1alpha1.LedgerSummary{Count: 4, Builds: []v1alpha1.LedgerBuild{
		{Side: v1alpha1.TreeHead, Result: v1alpha1.BuildBuilt, Seq: 1, DurationMs: 900},
		{Side: v1alpha1.TreeBase, Result: v1alpha1.BuildFailed, Seq: 2, ExitCode: 2},
	}}
	return pv
}

func TestRunEvaluationAgentsPlacement(t *testing.T) {
	in, run := &v1alpha1.Intent{}, &v1alpha1.IntentRun{}
	for _, unbuildable := range []bool{false, true} {
		ev := buildRunEvaluation(in, run, agentsEvaluation(unbuildable), authz.TierIntents)
		if ev.Placement != "agents" || ev.NoBaseComparison != unbuildable {
			t.Errorf("unbuildable=%v: placement %q, noBaseComparison %v", unbuildable, ev.Placement,
				ev.NoBaseComparison)
		}
		if len(ev.Builds) != 2 || ev.Builds[0].Tree != "head" || ev.Builds[1].Tree != "base" ||
			ev.Builds[1].Result != "failed" || ev.Builds[1].ExitCode != 2 || ev.Builds[0].DurationMs != 900 {
			t.Errorf("builds = %+v", ev.Builds)
		}
		c := ev.Checks[0]
		if c.Replay == nil || c.Replay.Tree != "head" {
			t.Errorf("replay = %+v", c.Replay)
		}
		if unbuildable != (c.Base == nil) || (c.Base != nil && (c.Base.Tree != "base" || c.Base.Seq != 4)) {
			t.Errorf("unbuildable=%v: base = %+v", unbuildable, c.Base)
		}
	}
}

func TestRunEvaluationSlotHasNoAgentsFields(t *testing.T) {
	pv := agentsEvaluation(false)
	pv.Spec.Placement = ""
	pv.Status.Base, pv.Status.Ledger.Builds = nil, nil
	b, err := json.Marshal(buildRunEvaluation(&v1alpha1.Intent{}, &v1alpha1.IntentRun{}, pv, authz.TierIntents))
	if err != nil {
		t.Fatal(err)
	}
	// A slot round has no trees: its runs carry no tree, so the S1 view is
	// unchanged.
	for _, k := range []string{"placement", "noBaseComparison", "builds", `"base"`, `"tree"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("slot view carries %s: %s", k, b)
		}
	}
}
