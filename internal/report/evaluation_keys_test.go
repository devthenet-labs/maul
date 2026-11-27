// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// liveShapedEvaluation is a report shaped like the ones every live
// evaluator wrote on 0.13.7 (S1 bug 5), less the check keys the observed
// evaluator retired: each verdict names its outcome with `outcome:`.
const liveShapedEvaluation = `---
verdicts:
  - outcome: "rate-battle"
    verdict: pass
    ledger: [12, 14]
    note: "The rating moved by 16 points."
  - outcome: "leaderboard"
    verdict: unsure
    ledger: []
anomalies: []
---
Rated a battle.
`

// TestParseEvaluationLiveShape (S1 bug 5): the live report is accepted, its
// verdicts keyed by their outcomes.
func TestParseEvaluationLiveShape(t *testing.T) {
	e, err := ParseEvaluation([]byte(liveShapedEvaluation))
	if err != nil {
		t.Fatalf("ParseEvaluation() error = %v", err)
	}
	if len(e.Verdicts) != 2 || e.Verdicts[0].ID != "rate-battle" || e.Verdicts[0].Outcome != "rate-battle" ||
		e.Verdicts[1].ID != "leaderboard" {
		t.Errorf("verdicts = %+v, want each keyed by its outcome", e.Verdicts)
	}
}

// TestParseEvaluationVerdictKeys: a verdict names its outcome as outcome
// or, as before, id; both are accepted when they agree, refused when they
// do not; every other unknown key is still refused.
func TestParseEvaluationVerdictKeys(t *testing.T) {
	verdict := func(keys string) string {
		return "---\nverdicts:\n  - " + keys + "\n    verdict: unsure\n---\nbody\n"
	}
	for _, tt := range []struct {
		name, keys, want string
	}{
		{"outcome", `outcome: "rate-battle"`, ""},
		{"id", `id: "rate-battle"`, ""},
		{"both agreeing", "id: rate-battle\n    outcome: rate-battle", ""},
		{"both disagreeing", "id: rate-battle\n    outcome: leaderboard", "names its outcome twice"},
		{"neither", `note: "x"`, "verdicts[0].outcome"},
		{"an unknown key", "outcome: rate-battle\n    evidence: [3]", "field evidence not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, err := ParseEvaluation([]byte(verdict(tt.keys)))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ParseEvaluation() error = %v", err)
				}
				if v := e.Verdicts[0]; v.ID != "rate-battle" || v.Outcome != "rate-battle" {
					t.Errorf("verdict = %+v, want rate-battle as both", v)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseEvaluation() error = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestFrontmatterKeyPaths: a refused report's frontmatter is described by
// its key paths, never its values, list items folded to [], each path once
// in order, bounded, and a key that is not plain text shown as ?.
func TestFrontmatterKeyPaths(t *testing.T) {
	got := FrontmatterKeyPaths([]byte("---\nverdicts:\n  - outcome: \"secret value\"\n    evidence: [3]\n" +
		"  - outcome: b\n    verdict: pass\nproposed:\n  - file: x\n    \"we\\u202eird\": 1\nnested:\n  a:\n    b: 1\n" +
		"---\nbody\n"))
	want := []string{"verdicts", "verdicts[].outcome", "verdicts[].evidence", "verdicts[].verdict", "proposed",
		"proposed[].file", "proposed[].?", "nested", "nested.a", "nested.a.b"}
	if !slices.Equal(got, want) {
		t.Errorf("FrontmatterKeyPaths() = %q, want %q", got, want)
	}
	for _, p := range got {
		if strings.Contains(p, "secret") {
			t.Errorf("a value leaked into %q", p)
		}
	}
	if got := FrontmatterKeyPaths([]byte("no frontmatter")); got != nil {
		t.Errorf("FrontmatterKeyPaths(no frontmatter) = %q, want none", got)
	}
	if got := FrontmatterKeyPaths([]byte("---\n: [\n---\n")); got != nil {
		t.Errorf("FrontmatterKeyPaths(not YAML) = %q, want none", got)
	}
	var many strings.Builder
	many.WriteString("---\n")
	for i := range 300 {
		fmt.Fprintf(&many, "key-%03d-%s: 1\n", i, strings.Repeat("x", 30))
	}
	many.WriteString("---\n")
	got = FrontmatterKeyPaths([]byte(many.String()))
	if len(got) == 0 || len(got) > FrontmatterKeyPathsMax || len(strings.Join(got, ", ")) > FrontmatterKeyPathsMaxBytes {
		t.Errorf("FrontmatterKeyPaths() = %d paths, %d bytes; want some, at most %d and %d", len(got),
			len(strings.Join(got, ", ")), FrontmatterKeyPathsMax, FrontmatterKeyPathsMaxBytes)
	}
}
