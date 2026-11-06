// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

// BaseComparison is what a change outcome's check line says of the check's
// runs at base (rule 5): that it discriminates, that it has no base
// comparison, or "" for nothing (SPEC-S2 Q29, section 7 (c) and (d)).
type BaseComparison string

// Base comparisons.
const (
	// BaseDiscriminates: red by assertion at base, and nothing there
	// that spoils it.
	BaseDiscriminates BaseComparison = "discriminates"
	// BaseNoComparison: every run at base errored for a cause that
	// leaves no base comparison (RunCause.NoComparison), or the base did
	// not build and nothing ran there.
	BaseNoComparison BaseComparison = "no-comparison"
)

// Comparison is how check digest of a change outcome stands at b, with the
// cause when it has no base comparison: only what rule 5 accepts for a
// change outcome is labelled (discriminates), so a label never claims more
// than Decide; any run that gives no base evidence (a pass, an exit-only
// fail, a timeout, a refusal) leaves it unlabelled. "" for a nil b (the
// slot placement, which has no base).
func (b *BaseRuns) Comparison(digest string) (BaseComparison, RunCause) {
	if b == nil {
		return "", ""
	}
	var mine []CheckRun
	for _, r := range b.Runs {
		if r.CheckDigest == digest {
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		if b.Unbuildable {
			return BaseNoComparison, CauseBaseUnbuildable
		}
		return "", ""
	}
	a := aggregate(mine)[digest]
	switch {
	case a.pass || a.bare || a.other || a.quarantined || a.untrusted:
		return "", ""
	case a.asserted:
		return BaseDiscriminates, ""
	}
	for _, r := range mine {
		if r.Result == RunError && r.Cause.NoComparison() {
			return BaseNoComparison, r.Cause
		}
	}
	return "", ""
}
