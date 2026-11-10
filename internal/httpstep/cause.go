// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

// RunCause is why a run errored, recorded by the tools relay only when the
// error shows the tree lacks what the check needs rather than that the run
// was cut short or disturbed (slice S2, the agents placement).
type RunCause string

// Run causes: the only base errors that count as "no base comparison"
// (n/a, SPEC-S2 S2-20, decided 2026-11-05: zero false passes outranks the
// confirmed rate). Every other error at base (a timeout, processes left
// behind, output cut, a bind or connection error, a refused or unanswered
// call) carries no cause: it gives no base evidence.
const (
	// CauseCompile: the check's program did not compile against the tree
	// (the compiler ran to its end and refused it).
	CauseCompile RunCause = "compile"
	// CauseVendoredTree, CauseReplaceOutsideTree, CauseNoModule: a tree a
	// run program cannot be built against (SPEC Q25).
	CauseVendoredTree       RunCause = "vendored-tree"
	CauseReplaceOutsideTree RunCause = "replace-outside-tree"
	CauseNoModule           RunCause = "no-module"
	// CauseBaseUnbuildable: the base tree did not build, so nothing ran
	// there.
	CauseBaseUnbuildable RunCause = "base-unbuildable"
)

// NoComparison reports a cause that leaves a check with no base
// comparison: n/a for rule 5, never a discrimination of its own.
func (c RunCause) NoComparison() bool {
	switch c {
	case CauseCompile, CauseVendoredTree, CauseReplaceOutsideTree, CauseNoModule, CauseBaseUnbuildable:
		return true
	}
	return false
}
