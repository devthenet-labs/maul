// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

const (
	digestA = "sha256:" + "aa00000000000000000000000000000000000000000000000000000000000000"
	digestB = "sha256:" + "bb00000000000000000000000000000000000000000000000000000000000000"
	digestC = "sha256:" + "cc00000000000000000000000000000000000000000000000000000000000000"
)

const evaluationReport = `---
verdicts:
  - id: rate-battle
    verdict: pass
    ledger: [12, 14]
    note: "  The rating moved by 16 points: as the outcome asks.  "
  - id: leaderboard
    verdict: unsure
    note: "Replay failed on shared data; disputed."
disputes:
  - check_digest: "` + digestA + `"
    reason: "The check counted rows other runs created."
    replacement: leaderboard-own-rows
    ledger: [3]
proposed:
  - id: leaderboard-own-rows
    outcome: leaderboard
    digest: "` + digestB + `"
retire:
  - check_digest: "` + digestC + `"
    reason: "Superseded by leaderboard-own-rows."
anomalies:
  - summary: "Console error on /battles: TypeError"
    route: /battles
    ledger: [20]
---
## What I did

Rated two battles and read the leaderboard.
`

func TestParseEvaluation(t *testing.T) {
	e, err := ParseEvaluation([]byte(evaluationReport))
	if err != nil {
		t.Fatalf("ParseEvaluation() error = %v", err)
	}
	if len(e.Verdicts) != 2 {
		t.Fatalf("Verdicts = %+v, want 2", e.Verdicts)
	}
	v := e.Verdicts[0]
	if v.ID != "rate-battle" || v.Verdict != VerdictPass || !slices.Equal(v.Ledger, []int32{12, 14}) ||
		v.Note != "The rating moved by 16 points: as the outcome asks." {
		t.Errorf("Verdicts[0] = %+v, want the pass trimmed", v)
	}
	if u := e.Verdicts[1]; u.Verdict != VerdictUnsure || len(u.Ledger) != 0 {
		t.Errorf("Verdicts[1] = %+v, want unsure without refs", u)
	}
	if !strings.HasPrefix(e.Body, "## What I did") {
		t.Errorf("Body = %q", e.Body)
	}
	if got := e.LedgerRefs(); !slices.Equal(got, []int32{3, 12, 14, 20}) {
		t.Errorf("LedgerRefs() = %v, want every cited seq, sorted and unique", got)
	}
}

func TestParseEvaluationLists(t *testing.T) {
	e, err := ParseEvaluation([]byte(evaluationReport))
	if err != nil {
		t.Fatalf("ParseEvaluation() error = %v", err)
	}
	if len(e.Disputes) != 1 || e.Disputes[0].CheckDigest != digestA ||
		e.Disputes[0].Replacement != "leaderboard-own-rows" || !slices.Equal(e.Disputes[0].Ledger, []int32{3}) {
		t.Errorf("Disputes = %+v", e.Disputes)
	}
	if len(e.Proposed) != 1 || e.Proposed[0] != (ProposedCheck{ID: "leaderboard-own-rows", Outcome: "leaderboard",
		Digest: digestB}) {
		t.Errorf("Proposed = %+v", e.Proposed)
	}
	if len(e.Retire) != 1 || e.Retire[0].CheckDigest != digestC || e.Retire[0].Reason == "" {
		t.Errorf("Retire = %+v", e.Retire)
	}
	if len(e.Anomalies) != 1 || e.Anomalies[0].Route != "/battles" || !slices.Equal(e.Anomalies[0].Ledger, []int32{20}) {
		t.Errorf("Anomalies = %+v", e.Anomalies)
	}
}

// TestParseEvaluationRedProposal: a check proposed to show a defect names
// the outcome it disproves, which the report says fails.
func TestParseEvaluationRedProposal(t *testing.T) {
	doc := "---\nverdicts:\n  - outcome: upset-pays-more\n    verdict: fail\n    ledger: [7, 9]\n" +
		"proposed:\n  - id: upset-pays-more\n    outcome: upset-pays-more\n    file: \"checks/upset.json\"\n" +
		"    expect: fail\n  - id: upset-pays-more.green\n    outcome: upset-pays-more\n    file: \"checks/g.json\"\n" +
		"    expect: pass\n---\n"
	e, err := ParseEvaluation([]byte(doc))
	if err != nil {
		t.Fatalf("ParseEvaluation() error = %v", err)
	}
	if len(e.Proposed) != 2 || e.Proposed[0].Expect != ExpectFail || e.Proposed[1].Expect != ExpectPass ||
		!e.Proposed[0].ExpectsFail() || e.Proposed[1].ExpectsFail() {
		t.Errorf("Proposed = %+v, want the first expected to fail", e.Proposed)
	}
}

// minimalEvaluation is the smallest valid report: one unsure verdict.
const minimalEvaluation = "---\nverdicts:\n  - id: rate-battle\n    verdict: unsure\n---\n"

func TestParseEvaluationMinimal(t *testing.T) {
	e, err := ParseEvaluation([]byte(minimalEvaluation))
	if err != nil {
		t.Fatalf("ParseEvaluation() error = %v", err)
	}
	if len(e.Verdicts) != 1 || e.Disputes != nil || e.Proposed != nil || e.Retire != nil || e.Anomalies != nil {
		t.Errorf("ParseEvaluation() = %+v", e)
	}
}

func TestParseEvaluationRefusals(t *testing.T) {
	verdict := func(body string) string { return "---\nverdicts:\n" + body + "---\n" }
	withVerdict := func(rest string) string {
		return "---\nverdicts:\n  - id: rate-battle\n    verdict: unsure\n" + rest + "---\n"
	}
	tests := []struct {
		name, doc, want string
	}{
		{"no frontmatter", "## report\n", "missing frontmatter"},
		{"no verdicts", "---\nanomalies: []\n---\n", "verdicts is required"},
		{"unknown key", withVerdict("score: 10\n"), "field score not found"},
		{"unknown verdict key", verdict("  - id: a\n    verdict: unsure\n    confidence: 1\n"), "confidence"},
		{"bad verdict", verdict("  - id: a\n    verdict: maybe\n"), `verdicts[0].verdict "maybe"`},
		{"bad id", verdict("  - id: Rate_Battle\n    verdict: unsure\n"), "verdicts[0].id"},
		{"duplicate id", verdict("  - id: a\n    verdict: unsure\n  - id: a\n    verdict: unsure\n"),
			"verdicts[1].id \"a\" is listed twice"},
		// D11: no evidence means unsure.
		{"pass without refs", verdict("  - id: a\n    verdict: pass\n"), "verdicts[0] is pass with no ledger"},
		{"fail without refs", verdict("  - id: a\n    verdict: fail\n    ledger: []\n"),
			"verdicts[0] is fail with no ledger"},
		{"negative ref", verdict("  - id: a\n    verdict: pass\n    ledger: [-1]\n"), "verdicts[0].ledger[0] is -1"},
		{"repeated ref", verdict("  - id: a\n    verdict: pass\n    ledger: [4, 4]\n"),
			"verdicts[0].ledger[1] repeats 4"},
		{"multi-line note", verdict("  - id: a\n    verdict: unsure\n    note: \"one\\ntwo\"\n"), "verdicts[0].note"},
		{"long note", verdict("  - id: a\n    verdict: unsure\n    note: \"" + strings.Repeat("n", NoteMaxChars+1) +
			"\"\n"), "over 600"},
		{"dispute bad digest", withVerdict("disputes:\n  - check_digest: abc\n    reason: r\n    ledger: [1]\n"),
			"disputes[0].check_digest"},
		{"dispute no reason", withVerdict("disputes:\n  - check_digest: \"" + digestA + "\"\n    ledger: [1]\n"),
			"disputes[0].reason is required"},
		{"dispute no refs", withVerdict("disputes:\n  - check_digest: \"" + digestA + "\"\n    reason: r\n"),
			"disputes[0] cites no ledger"},
		{"dispute unknown replacement", withVerdict("disputes:\n  - check_digest: \"" + digestA +
			"\"\n    reason: r\n    replacement: other\n    ledger: [1]\n"),
			`disputes[0].replacement "other" names no proposed check`},
		{"dispute twice", withVerdict("disputes:\n  - check_digest: \"" + digestA + "\"\n    reason: r\n" +
			"    ledger: [1]\n  - check_digest: \"" + digestA + "\"\n    reason: r\n    ledger: [2]\n"),
			"disputes[1].check_digest is listed twice"},
		{"proposed for no verdict", withVerdict("proposed:\n  - id: c\n    outcome: other\n    digest: \"" +
			digestB + "\"\n"), `proposed[0].outcome "other" names no verdict`},
		{"proposed bad digest", withVerdict("proposed:\n  - id: c\n    outcome: rate-battle\n    digest: x\n"),
			"proposed[0].digest"},
		{"proposed to fail an outcome not reported fail", withVerdict("proposed:\n  - id: c\n    outcome: rate-battle\n" +
			"    digest: \"" + digestB + "\"\n    expect: fail\n"),
			`proposed[0] expects to fail, but its outcome "rate-battle" is not reported fail`},
		{"proposed with another expectation", withVerdict("proposed:\n  - id: c\n    outcome: rate-battle\n" +
			"    digest: \"" + digestB + "\"\n    expect: maybe\n"), `proposed[0].expect "maybe" is not pass or fail`},
		{"proposed body", withVerdict("proposed:\n  - id: c\n    outcome: rate-battle\n    digest: \"" + digestB +
			"\"\n    steps: []\n"), "steps"},
		{"proposed duplicate id", withVerdict("proposed:\n  - id: c\n    outcome: rate-battle\n    digest: \"" +
			digestB + "\"\n  - id: c\n    outcome: rate-battle\n    digest: \"" + digestC + "\"\n"),
			`proposed[1].id "c" is listed twice`},
		{"proposed duplicate digest", withVerdict("proposed:\n  - id: c\n    outcome: rate-battle\n    digest: \"" +
			digestB + "\"\n  - id: d\n    outcome: rate-battle\n    digest: \"" + digestB + "\"\n"),
			"proposed[1].digest is listed twice"},
		{"retire no reason", withVerdict("retire:\n  - check_digest: \"" + digestC + "\"\n"),
			"retire[0].reason is required"},
		{"retired and disputed", withVerdict("disputes:\n  - check_digest: \"" + digestA + "\"\n    reason: r\n" +
			"    ledger: [1]\nretire:\n  - check_digest: \"" + digestA + "\"\n    reason: r\n"),
			"retire[0].check_digest is also disputed"},
		{"anomaly no refs", withVerdict("anomalies:\n  - summary: s\n    route: /\n"), "anomalies[0] cites no ledger"},
		{"anomaly no summary", withVerdict("anomalies:\n  - route: /\n    ledger: [1]\n"),
			"anomalies[0].summary is required"},
		{"anomaly no route", withVerdict("anomalies:\n  - summary: s\n    ledger: [1]\n"),
			"anomalies[0].route is required"},
		{"tagged value", verdict("  - id: a\n    verdict: !!str unsure\n"), "tagged"},
		{"hidden character", minimalEvaluation + "Pass" + string(rune(0x200b)) + "\n", "U+200B"},
		{"over the document bound", minimalEvaluation + strings.Repeat("x\n", ReportMaxBytes/2), "bound"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseEvaluation([]byte(tt.doc))
			if err == nil {
				t.Fatalf("ParseEvaluation() error = nil, want %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseEvaluation() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestParseEvaluationBounds(t *testing.T) {
	list := func(n int, item func(i int) string) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(item(i))
		}
		return b.String()
	}
	digest := func(i int) string { return fmt.Sprintf("\"sha256:%064x\"", i) }
	verdicts := "verdicts:\n  - id: a\n    verdict: unsure\n"
	tests := []struct {
		name, frontmatter, want string
	}{
		{"verdicts", "verdicts:\n" + list(EvaluationMaxVerdicts+1, func(i int) string {
			return fmt.Sprintf("  - id: o%d\n    verdict: unsure\n", i)
		}), "verdicts has 21 items, over 20"},
		{"proposed", verdicts + "proposed:\n" + list(EvaluationMaxProposed+1, func(i int) string {
			return fmt.Sprintf("  - id: c%d\n    outcome: a\n    digest: %s\n", i, digest(i))
		}), "proposed has 41 items, over 40"},
		{"anomalies", verdicts + "anomalies:\n" + list(EvaluationMaxAnomalies+1, func(i int) string {
			return fmt.Sprintf("  - summary: s\n    route: /\n    ledger: [%d]\n", i)
		}), "anomalies has 11 items, over 10"},
		{"disputes", verdicts + "disputes:\n" + list(EvaluationMaxDisputes+1, func(i int) string {
			return fmt.Sprintf("  - check_digest: %s\n    reason: r\n    ledger: [1]\n", digest(i))
		}), "disputes has 41 items, over 40"},
		{"retire", verdicts + "retire:\n" + list(EvaluationMaxRetire+1, func(i int) string {
			return fmt.Sprintf("  - check_digest: %s\n    reason: r\n", digest(i))
		}), "retire has 41 items, over 40"},
		{"ledger refs", verdicts + "anomalies:\n" + list(EvaluationMaxAnomalies, func(i int) string {
			return fmt.Sprintf("  - summary: s\n    route: /\n    ledger: [%s]\n", list(31, func(j int) string {
				return fmt.Sprintf("%d, ", i*100+j)
			})+"9999")
		}), "320 ledger refs, over 300"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseEvaluation([]byte("---\n" + tt.frontmatter + "---\n"))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseEvaluation() error = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestParseEvaluationNoEvidenceNoVerdict is D11 as a seeded property: a
// report ParseEvaluation accepts never carries a pass or a fail without a
// ledger ref, whatever mix of verdicts and refs it was written with.
func TestParseEvaluationNoEvidenceNoVerdict(t *testing.T) {
	r := rand.New(rand.NewSource(propertySeed))
	verdicts := []Verdict{VerdictPass, VerdictFail, VerdictUnsure}
	accepted, refused := 0, 0
	for range propertyCount {
		var b strings.Builder
		b.WriteString("---\nverdicts:\n")
		n := 1 + r.Intn(5)
		for i := range n {
			fmt.Fprintf(&b, "  - id: o%d\n    verdict: %s\n", i, verdicts[r.Intn(len(verdicts))])
			switch r.Intn(3) {
			case 0:
				// no ledger key at all
			case 1:
				b.WriteString("    ledger: []\n")
			default:
				fmt.Fprintf(&b, "    ledger: [%d]\n", r.Intn(1000))
			}
		}
		b.WriteString("---\n")
		e, err := ParseEvaluation([]byte(b.String()))
		if err != nil {
			refused++
			continue
		}
		accepted++
		for _, v := range e.Verdicts {
			if v.Verdict != VerdictUnsure && len(v.Ledger) == 0 {
				t.Fatalf("accepted %s with no ledger refs:\n%s", v.Verdict, b.String())
			}
		}
	}
	if accepted == 0 || refused == 0 {
		t.Errorf("accepted %d, refused %d: the generator must exercise both sides", accepted, refused)
	}
}
