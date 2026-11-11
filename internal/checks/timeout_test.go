// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"errors"
	"testing"
	"time"
)

// TestExecTimeoutBound: an exec or run check may claim at most the exec
// shim's own bound (MaxExecTimeout, pinned to toolexec.Timeout in
// previewtools), so no check is admitted asking for time the sandbox cuts
// it short of. One past the bound is refused by its own rule
// (RuleTimeout); a value that is no positive duration stays a grammar
// refusal.
func TestExecTimeoutBound(t *testing.T) {
	if MaxExecTimeout != time.Minute {
		t.Fatalf("MaxExecTimeout = %s, want the exec shim's 1m", MaxExecTimeout)
	}
	forms := []struct {
		name string
		body func(timeout string) string
		ctx  Context
	}{
		{"slot exec", func(d string) string {
			return execCheck(execBase + `,"timeout":"` + d + `"` + execRunOK)
		}, slotCtx},
		{"cli exec", func(d string) string {
			return execCheck(`"argv":["{{bin}}"],"timeout":"` + d + `",` + okOut)
		}, cliCtx},
		{"library run", func(d string) string {
			return runCheck(`"language":"go",` + libMain + `,"timeout":"` + d + `",` + okOut)
		}, libCtx},
	}
	for _, f := range forms {
		for _, tc := range []struct {
			timeout string
			want    Rule // "" admitted
		}{
			{"1ms", ""},
			{"30s", ""},
			{"60s", ""},
			{"1m", ""},
			{"60001ms", RuleTimeout},
			{"61s", RuleTimeout},
			{"2m", RuleTimeout},
			{"5m", RuleTimeout},
			{"1h", RuleTimeout},
			{"0s", RuleGrammar},
			{"-1s", RuleGrammar},
			{"soon", RuleGrammar},
		} {
			t.Run(f.name+" "+tc.timeout, func(t *testing.T) {
				c := mustParse(t, f.body(tc.timeout))
				err := Validate(c, f.ctx)
				if tc.want == "" {
					if err != nil {
						t.Fatalf("Validate = %v, want admitted", err)
					}
					return
				}
				var r *Refusal
				if !errors.As(err, &r) || r.Rule != tc.want {
					t.Fatalf("Validate = %v, want rule %s", err, tc.want)
				}
			})
		}
	}
}
