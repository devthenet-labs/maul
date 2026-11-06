// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"testing"
	"testing/quick"
)

// TestBaseComparison (SPEC-S2 Q29, section 7 (c) and (d)): what a change
// outcome's check line says of its base runs: it discriminates only red by
// assertion at base, has no base comparison only for a cause that leaves
// none (or a base that did not build), and says nothing otherwise.
func TestBaseComparison(t *testing.T) {
	const c = "sha256:c"
	red := CheckRun{CheckDigest: c, Result: RunFail, Token: "t", AssertionStep: 1}
	compile := CheckRun{CheckDigest: c, Result: RunError, Token: "t", Cause: CauseCompile}
	tests := []struct {
		name  string
		base  *BaseRuns
		want  BaseComparison
		cause RunCause
	}{
		{"no base (a slot round)", nil, "", ""},
		{"red by assertion", &BaseRuns{Required: true, Runs: []CheckRun{red}}, BaseDiscriminates, ""},
		{"red beside a compile error", &BaseRuns{Required: true, Runs: []CheckRun{compile, red}},
			BaseDiscriminates, ""},
		{"a compile error", &BaseRuns{Required: true, Runs: []CheckRun{compile}}, BaseNoComparison, CauseCompile},
		{"a vendored tree", &BaseRuns{Required: true, Runs: []CheckRun{{CheckDigest: c, Result: RunError,
			Token: "t", Cause: CauseVendoredTree}}}, BaseNoComparison, CauseVendoredTree},
		{"an unbuildable base", &BaseRuns{Required: true, Unbuildable: true}, BaseNoComparison,
			CauseBaseUnbuildable},
		{"no run at a required base", &BaseRuns{Required: true}, "", ""},
		{"green at base", &BaseRuns{Required: true, Runs: []CheckRun{{CheckDigest: c, Result: RunPass,
			Token: "t"}}}, "", ""},
		{"an exit-only fail", &BaseRuns{Required: true, Runs: []CheckRun{{CheckDigest: c, Result: RunFail,
			Token: "t"}}}, "", ""},
		{"red beside a pass", &BaseRuns{Required: true, Runs: []CheckRun{red, {CheckDigest: c, Result: RunPass,
			Token: "t"}}}, "", ""},
		{"a timeout", &BaseRuns{Required: true, Runs: []CheckRun{{CheckDigest: c, Result: RunError,
			Token: "t"}}}, "", ""},
		{"a compile error beside a timeout", &BaseRuns{Required: true, Runs: []CheckRun{compile,
			{CheckDigest: c, Result: RunError, Token: "t"}}}, "", ""},
		{"another check's run", &BaseRuns{Required: true, Runs: []CheckRun{{CheckDigest: "sha256:d",
			Result: RunFail, Token: "t", AssertionStep: 1}}}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, cause := tt.base.Comparison(c)
			if got != tt.want || cause != tt.cause {
				t.Errorf("Comparison = %q (%q), want %q (%q)", got, cause, tt.want, tt.cause)
			}
		})
	}
}

// TestBaseComparisonAgreesWithDecide: a check line never says a change
// outcome's check discriminates, or has no base comparison, where Decide's
// rule 5 does not hold for it, and says it discriminates only on a fail by
// assertion at base (seeded property over random agents rounds).
func TestBaseComparisonAgreesWithDecide(t *testing.T) {
	var labelled int
	prop := func(g genAgentsRound) bool {
		d := decider{in: g.in, base: aggregate(g.in.Base.Runs)}
		for _, o := range g.in.Outcomes {
			if o.Keep {
				continue
			}
			for _, c := range d.bound(o.ID) {
				got, _ := g.in.Base.Comparison(c.Digest)
				if got == "" {
					continue
				}
				labelled++
				if !d.discriminates(c, o) {
					return false
				}
				if got == BaseDiscriminates && !d.base[c.Digest].asserted {
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Error(err)
	}
	if labelled < 100 {
		t.Errorf("only %d labelled checks: the generator rarely reaches the rule", labelled)
	}
}
