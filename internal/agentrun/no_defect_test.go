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

// noDefectBuild is an acceptance-fix round's report that found no defect.
const noDefectBuild = "---\nsuccess: false\nno_defect: true\nsummary: \"No defect found\"\ntests:\n" +
	"  ran: true\n  passed: true\n  command: \"go test ./...\"\n" +
	"reason: \"each failing criterion passes when replayed against this code\"\n---\nReplayed each criterion.\n"

// TestBuildNoDefect: on an acceptance-fix round (MAUL_ACCEPTANCE_FIX) a
// report concluding no defect is an ok stage with no changeset and no
// commit.sh; on any other build the same report is refused, so only the
// round that may conclude it does. A build that changed nothing without
// saying so stays a failure on either.
func TestBuildNoDefect(t *testing.T) {
	tests := []struct {
		name   string
		fix    bool
		report string
		want   envelope.Outcome
		detail string
	}{
		{"no defect on an acceptance-fix round", true, noDefectBuild, envelope.OutcomeOK, ""},
		{"no defect on any other build", false, noDefectBuild, envelope.OutcomeReportInvalid,
			"no_defect is only for an acceptance-fix round"},
		{"no change without saying so on an acceptance-fix round", true, goodBuild, envelope.OutcomeCommitFailed,
			"commit.sh missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := intentConfig(t, PhaseBuild, &out)
			cfg.AcceptanceFix = tt.fix
			rem := onlyEvent(t, cfg, &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess,
				writes: map[string]string{"reports/build.md": tt.report}}}}, &out).Remediation
			if rem == nil || rem.Outcome != tt.want || rem.Success || rem.Changeset != nil {
				t.Fatalf("build = %+v, want outcome %q with no success and no changeset", rem, tt.want)
			}
			if !strings.Contains(rem.Detail, tt.detail) {
				t.Errorf("detail = %q, want it to mention %q", rem.Detail, tt.detail)
			}
			if tt.want == envelope.OutcomeOK && rem.ReportMarkdown != tt.report {
				t.Errorf("ReportMarkdown = %q, want the report", rem.ReportMarkdown)
			}
		})
	}
}

// TestBuildPromptOffersNoDefect: only an acceptance-fix round's prompt
// tells the agent it may conclude no defect, and how to say so.
func TestBuildPromptOffersNoDefect(t *testing.T) {
	for _, fix := range []bool{true, false} {
		var out bytes.Buffer
		cfg, ws := intentConfig(t, PhaseBuild, &out)
		cfg.AcceptanceFix = fix
		cfg.RemediateHarness = "claude"
		cfg.BrokerTokenFile = filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(cfg.BrokerTokenFile, []byte("caller-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess,
			writes: map[string]string{"reports/build.md": noDefectBuild}}}}
		onlyEvent(t, cfg, fx, &out)
		var argv []string
		for _, spec := range fx.specs {
			argv = append(argv, spec.Argv...)
		}
		prompt := strings.Join(argv, "\n")
		if got := strings.Contains(prompt, "no_defect: true"); got != fix {
			t.Errorf("acceptance fix %v: prompt offers no_defect = %v:\n%s", fix, got, prompt)
		}
	}
}

// TestFromEnvAcceptanceFix: MAUL_ACCEPTANCE_FIX is "true" or unset; any
// other value is refused by name, never read as either.
func TestFromEnvAcceptanceFix(t *testing.T) {
	for value, want := range map[string]bool{"": false, "true": true} {
		env := map[string]string{"MAUL_REPO": "acme/shop", "MAUL_FINDING": "run-1", "MAUL_PHASE": "build",
			"MAUL_ACCEPTANCE_FIX": value}
		cfg, err := FromEnv(func(k string) string { return env[k] })
		if err != nil || cfg.AcceptanceFix != want {
			t.Errorf("FromEnv(%q) = %v, %v; want %v", value, cfg.AcceptanceFix, err, want)
		}
	}
	env := map[string]string{"MAUL_REPO": "acme/shop", "MAUL_FINDING": "run-1", "MAUL_PHASE": "build",
		"MAUL_ACCEPTANCE_FIX": "yes"}
	if _, err := FromEnv(func(k string) string { return env[k] }); err == nil ||
		!strings.Contains(err.Error(), `MAUL_ACCEPTANCE_FIX="yes"`) {
		t.Errorf("FromEnv(yes) error = %v, want it refused by name", err)
	}
}
