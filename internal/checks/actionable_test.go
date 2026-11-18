// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"errors"
	"strings"
	"testing"
)

// browserExpect is a browser check that opens the root and makes one
// expectation.
func browserExpect(expect string) string {
	return `{"id":"o-c","outcome":"o","form":"browser","browser":{"steps":[{"goto":"/"},{"expect":` + expect + `}]}}`
}

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
		// L6-B-BROWSER-1: the css-length and regex refusals say what to
		// write instead, as the selector-list and css-depth ones do.
		{"a css locator over the length bound", browserExpect(`{"assert":"count","locator":{"css":"` +
			strings.Repeat("a", MaxTextChars+1) + `"},"count":1}`), slotCtx, RuleGrammar,
			[]string{"201 characters, over 200", "an expect of its own", "located by role, label, text or testid",
				"within"}},
		// Gate 5's refused pattern, a text that does not say something.
		{"a text match with a lookahead", browserExpect(`{"assert":"text","locator":{"testid":"error"},` +
			`"text":{"matches":"^(?![\\s\\S]*Every must)"}}`), slotCtx, RuleGrammar,
			[]string{"not a regular expression", "no lookahead", "not what it is not", `^\s*$`}},
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

// TestRefusalHintsAreWritable (L6-B-BROWSER-1): what the css-length and
// regex refusals say to write instead is a check Validate accepts, so a hint
// never leads to a second refusal.
func TestRefusalHintsAreWritable(t *testing.T) {
	for name, expect := range map[string]string{
		"a text locator scoped with within": `{"assert":"visible","locator":{"text":"Fill kick",` +
			`"within":{"testid":"fills"}}}`,
		"a role locator scoped with hasText": `{"assert":"visible","locator":{"role":"button","hasText":"kick"}}`,
		"a label locator":                    `{"assert":"visible","locator":{"label":"Every"}}`,
		"an empty text by matches":           `{"assert":"text","locator":{"testid":"error"},"text":{"matches":"^\\s*$"}}`,
		"a text by contains":                 `{"assert":"text","locator":{"testid":"error"},"text":{"contains":"must"}}`,
		"a text by equals":                   `{"assert":"text","locator":{"testid":"error"},"text":{"equals":"must"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(mustParse(t, browserExpect(expect)), slotCtx); err != nil {
				t.Errorf("Validate refused what the hint says to write: %v", err)
			}
		})
	}
}
