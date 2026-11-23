// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
	"github.com/devthenet-labs/maul/internal/web/authz"
)

// agentsEvaluation is an agents round: no Preview, both trees built, a
// call at the head and one at the base.
func agentsEvaluation() *v1alpha1.PreviewVerification {
	pv := &v1alpha1.PreviewVerification{}
	pv.Name = evalPV
	pv.Spec.Placement = v1alpha1.PlacementAgents
	pv.Status.Ledger = &v1alpha1.LedgerSummary{Count: 4, Builds: []v1alpha1.LedgerBuild{
		{Side: v1alpha1.TreeHead, Result: v1alpha1.BuildBuilt, Seq: 1, DurationMs: 900},
		{Side: v1alpha1.TreeBase, Result: v1alpha1.BuildFailed, Seq: 2, ExitCode: 2},
	}, Entries: []v1alpha1.LedgerIndexEntry{
		{Seq: 1, Kind: v1alpha1.LedgerKindRunner, Verb: v1alpha1.LedgerVerbBuild},
		{Seq: 2, Kind: v1alpha1.LedgerKindRunner, Verb: v1alpha1.LedgerVerbBuild, Side: v1alpha1.TreeBase},
		{Seq: 3, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbExec},
		{Seq: 4, Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerbExec, Side: v1alpha1.TreeBase},
	}}
	return pv
}

// TestRunEvaluationAgentsPlacement: an agents round names its placement and
// its tree builds, and each ledger entry its side, so a call at the base,
// which never counts, reads as one.
func TestRunEvaluationAgentsPlacement(t *testing.T) {
	ev := buildRunEvaluation(&v1alpha1.Intent{}, &v1alpha1.IntentRun{}, agentsEvaluation(), authz.TierIntents)
	if ev.Placement != "agents" {
		t.Errorf("placement %q", ev.Placement)
	}
	if len(ev.Builds) != 2 || ev.Builds[0].Tree != "head" || ev.Builds[1].Tree != "base" ||
		ev.Builds[1].Result != "failed" || ev.Builds[1].ExitCode != 2 || ev.Builds[0].DurationMs != 900 {
		t.Errorf("builds = %+v", ev.Builds)
	}
	sides := make([]string, 0, len(ev.Ledger.Entries))
	for _, e := range ev.Ledger.Entries {
		sides = append(sides, e.Side)
	}
	if strings.Join(sides, ",") != ",base,,base" {
		t.Errorf("sides = %q, want the index's", sides)
	}
}

func TestRunEvaluationSlotHasNoAgentsFields(t *testing.T) {
	pv := agentsEvaluation()
	pv.Spec.Placement = ""
	pv.Status.Ledger.Builds = nil
	for i := range pv.Status.Ledger.Entries {
		pv.Status.Ledger.Entries[i].Side = ""
	}
	b, err := json.Marshal(buildRunEvaluation(&v1alpha1.Intent{}, &v1alpha1.IntentRun{}, pv, authz.TierIntents))
	if err != nil {
		t.Fatal(err)
	}
	// A slot round has no trees: its entries carry no side, so the S1 view
	// is unchanged.
	for _, k := range []string{"placement", "builds", `"side"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("slot view carries %s: %s", k, b)
		}
	}
}

// TestRunEvaluationAgentsOwnLabel: at the transcripts tier an agents
// round's exec is the evaluator's own program unless it ran exactly the
// built binary, and a run always is (toolledger.OwnCall, the comment's
// rule); a slot round's exec always is.
func TestRunEvaluationAgentsOwnLabel(t *testing.T) {
	calls := []struct {
		verb toolwire.Verb
		args string
	}{
		{toolwire.VerbExec, `{"argv":["{{bin}}","list"]}`},
		{toolwire.VerbExec, `{"argv":["battle","list"]}`},
		{toolwire.VerbExec, `{"argv":["sh","-c","battle list"]}`},
		{toolwire.VerbExec, `{"argv":["{{bin}}"],"files":[{"path":"in.txt","content":"x"}]}`},
		{toolwire.VerbRun, `{"files":[{"path":"main.go","content":"package main"}]}`},
	}
	for _, tc := range []struct {
		name   string
		agents bool
		want   []bool
	}{
		{"agents", true, []bool{false, false, true, true, true}},
		{"slot", false, []bool{true, true, true, true, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := evaluationFixture(t)
			pv := findPV(objs)
			if tc.agents {
				pv.Spec.Placement = v1alpha1.PlacementAgents
				pv.Spec.Workspace = &v1alpha1.VerificationWorkspace{Surface: v1alpha1.WorkspaceSurface{
					CLI: &v1alpha1.WorkspaceCLI{Binary: "battle"}}}
			}
			lines := make([]string, 0, len(calls))
			pv.Status.Ledger.Entries, pv.Status.Ledger.Count = nil, int32(len(calls))
			for i, c := range calls {
				out := "ok " + c.args
				pv.Status.Ledger.Entries = append(pv.Status.Ledger.Entries, v1alpha1.LedgerIndexEntry{
					Seq: int32(i + 1), Kind: v1alpha1.LedgerKindAgent, Verb: v1alpha1.LedgerVerb(c.verb),
					ResultSHA256: sha256Hex([]byte(out))})
				b, err := json.Marshal(toolwire.LedgerEntry{Seq: int64(i + 1), Kind: toolwire.KindAgent,
					Verb: c.verb, Args: json.RawMessage(c.args), Output: out, OutputSHA256: sha256Hex([]byte(out))})
				if err != nil {
					t.Fatal(err)
				}
				lines = append(lines, string(b))
			}
			findConfigMap(objs, evalPV+"-ledger").Data[toolledger.Key] = strings.Join(lines, "\n")
			s, _ := intentsServer(t, nil, objs...)
			code, body := get(t, as(t, s, readerAlpha), evaluationPath(evalRun))
			if code != http.StatusOK {
				t.Fatalf("GET evaluation = %d %s", code, body)
			}
			ev := decode[RunEvaluation](t, body)
			for i, e := range ev.Ledger.Entries {
				if e.Own != tc.want[i] || e.Args != calls[i].args {
					t.Errorf("call %d %s: own %v, want %v (%+v)", i+1, calls[i].args, e.Own, tc.want[i], e)
				}
			}
		})
	}
}
