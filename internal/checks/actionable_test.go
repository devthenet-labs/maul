// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"errors"
	"strings"
	"testing"
)

// TestRefusalsSayHowToFixThem (defect gate 5, the evaluator's first drafts):
// the two refusals of a stdout assertion that the evaluator's drafts tripped
// on in every CLI round still refuse the same checks under the same rule,
// and now say what to write instead.
func TestRefusalsSayHowToFixThem(t *testing.T) {
	program := `"language":"go","files":[{"path":"main.go","content":"package main\n// {{bin}} {{marker}}\n"}],` +
		`"args":["{{run}}"],"timeout":"30s",`
	for _, tc := range []struct {
		name  string
		check string
		ctx   Context
		rule  Rule
		want  []string
	}{
		{"a newline in an exec's stdoutContains", `{"id":"o-c","outcome":"o","form":"exec","exec":{` +
			`"argv":["{{bin}}","p.json"],"files":[{"path":"p.json","content":"{}"}],"timeout":"30s",` +
			`"expect":{"exit":0,"stdoutContains":"kick 4\nsnare 2"}}}`, cliCtx, RuleGrammar,
			[]string{"one line", "stdoutJSON"}},
		{"a newline in a run's stdoutContains", runCheck(program + `"expect":{"stdoutContains":"kick 4\nsnare 2"}`),
			cliCtx, RuleGrammar, []string{"one line", "stdoutJSON"}},
		{"a newline in a slot exec's stdoutContains", `{"id":"o-c","outcome":"o","form":"exec","exec":{` +
			`"argv":["python3","c.py","{{run}}"],"files":[{"path":"c.py","content":"x"}],"timeout":"30s",` +
			`"expect":{"stdoutContains":"a\r\nb"}}}`, slotCtx, RuleGrammar, []string{"one line", "stdoutJSON"}},
		{"a run asserting only its exit code", runCheck(program + `"expect":{"exit":0}`), cliCtx, RuleNotSubstantive,
			[]string{"exit code alone", "stdoutContains or stdoutJSON", "what it observed"}},
		{"an exec asserting only its exit code", `{"id":"o-c","outcome":"o","form":"exec","exec":{` +
			`"argv":["{{bin}}","p.json"],"files":[{"path":"p.json","content":"{}"}],"timeout":"30s",` +
			`"expect":{"exit":0}}}`, cliCtx, RuleNotSubstantive,
			[]string{"exit code alone", "stdoutContains or stdoutJSON"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParse(t, tc.check)
			var r *Refusal
			if !errors.As(Validate(c, tc.ctx), &r) {
				t.Fatalf("Validate accepted %s", tc.check)
			}
			if r.Rule != tc.rule {
				t.Errorf("rule = %s, want %s (%s)", r.Rule, tc.rule, r.Reason)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.Reason, w) {
					t.Errorf("reason %q does not say %q", r.Reason, w)
				}
			}
		})
	}
}
