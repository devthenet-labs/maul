// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
)

// testPlanVerification is a verifying Project's three previewed components,
// as intent-controller hands them to a plan Job.
const testPlanVerification = `{"components":[{"key":"web","path":"/","readiness":"/healthz"},` +
	`{"key":"api","path":"/api","readiness":"/api/healthz"},{"key":"render","path":"/render"}]}`

// TestPlanPromptTeachesAcceptance: a plan Job handed its Project's
// verification (MAUL_PLAN_VERIFICATION) renders the plan prompt's acceptance
// section, naming every component a criterion can target, on claude and on
// codex alike; a plan Job handed none says nothing of acceptance, as before.
// Without this a verifying Project's planner was never asked for criteria,
// and verification never ran.
func TestPlanPromptTeachesAcceptance(t *testing.T) {
	for _, h := range []string{"claude", "codex"} {
		for _, tt := range []struct {
			name, handoff string
		}{{"verifying", testPlanVerification}, {"not verifying", ""}} {
			t.Run(h+"/"+tt.name, func(t *testing.T) {
				env := map[string]string{"MAUL_REPO": "acme/shop", "MAUL_FINDING": "target-1-plan-r1-a1",
					"MAUL_PHASE": string(PhasePlan), "MAUL_PLAN_VERIFICATION": tt.handoff}
				fromEnv, err := FromEnv(func(k string) string { return env[k] })
				if err != nil {
					t.Fatal(err)
				}
				var out, log bytes.Buffer
				var cfg Config
				var ws string
				if h == "codex" {
					cfg, ws = codexIntentConfig(t, PhasePlan, &out, &log)
				} else {
					cfg, ws = intentConfig(t, PhasePlan, &out)
					tokenFile := filepath.Join(t.TempDir(), "token")
					if err := os.WriteFile(tokenFile, []byte("caller-token\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					cfg.InvestigateHarness, cfg.BrokerTokenFile = "claude", tokenFile
				}
				cfg.PlanVerification = fromEnv.PlanVerification
				stream := streamSuccess
				if h == "codex" {
					stream = codexStream
				}
				fx := &fakeExec{steps: []step{{ws: ws, stdout: stream, writes: map[string]string{
					"reports/plan.md": goodPlan,
				}}}}
				if ev := onlyEvent(t, cfg, fx, &out); ev.Plan == nil || ev.Plan.Outcome != envelope.OutcomeOK {
					t.Fatalf("event = %+v, want an ok plan", ev)
				}
				prompt := strings.Join(fx.specs[0].Argv, "\n")
				taught := strings.Contains(prompt, "### Acceptance criteria")
				if taught != (tt.handoff != "") {
					t.Fatalf("prompt teaches acceptance = %v, want %v:\n%s", taught, tt.handoff != "", prompt)
				}
				if !taught {
					if strings.Contains(prompt, "acceptance") {
						t.Errorf("a plan Job handed no verification mentions acceptance:\n%s", prompt)
					}
					return
				}
				for _, want := range []string{
					"- `web`: served under `/`, ready when `/healthz` answers",
					"- `api`: served under `/api`, ready when `/api/healthz` answers",
					"- `render`: served under `/render`\n",
				} {
					if !strings.Contains(prompt, want) {
						t.Errorf("prompt lacks %q:\n%s", want, prompt)
					}
				}
			})
		}
	}
}

// TestPlanVerificationRefused: a handoff that does not decode is a
// configuration error naming it, not a plan silently asked for nothing.
func TestPlanVerificationRefused(t *testing.T) {
	for _, raw := range []string{"not json", `{"components":[]}`, `{"components":[{"key":"web"}]}`} {
		env := map[string]string{"MAUL_REPO": "acme/shop", "MAUL_PLAN_VERIFICATION": raw}
		if _, err := FromEnv(func(k string) string { return env[k] }); err == nil ||
			!strings.Contains(err.Error(), "MAUL_PLAN_VERIFICATION") {
			t.Errorf("FromEnv(%q) error = %v, want one naming MAUL_PLAN_VERIFICATION", raw, err)
		}
	}
}
