// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"errors"
	"fmt"
	"strings"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// WritesRunData reports whether c writes data under its own run token: an
// http POST, PUT or PATCH whose path or body names RunPlaceholder, a browser
// fill of it, or an exec program run with it in its argv or files. A check
// saved to show a defect must (reviews, finding 3): its confirmation runs
// against whatever state the evaluator's exploration left behind, so what it
// asserts on has to be data its own run can have made. A token in a GET's
// query, a navigation or an expectation writes nothing.
func WritesRunData(c Check) bool {
	switch {
	case c.HTTP != nil:
		for _, st := range c.HTTP.Steps {
			switch strings.ToUpper(st.Request.Method) {
			case "POST", "PUT", "PATCH":
			default:
				continue
			}
			if hasRun(st.Request.Path) || hasRun(st.Request.Body) || marshalsRun(st.Request.JSON) {
				return true
			}
		}
	case c.Browser != nil:
		for _, st := range c.Browser.Steps {
			if st.Fill != nil && hasRun(st.Fill.Text) {
				return true
			}
		}
	case c.Exec != nil:
		for _, a := range c.Exec.Argv {
			if hasRun(a) {
				return true
			}
		}
		for _, f := range c.Exec.Files {
			if hasRun(f.Content) {
				return true
			}
		}
	}
	return false
}

// LayoutAssertion reports a browser assertion that measures the page's own
// layout or geometry: its size, a box relation, alignment, legibility,
// whether it receives a click or is visible. None reads data, so none can
// be wrong about rows the run did not make (S1 demo bug 9); text, count and
// url can.
func LayoutAssertion(k AssertKind) bool {
	switch k {
	case AssertSize, AssertLeftOf, AssertAbove, AssertNoOverlap, AssertEqualSize, AssertAligned, AssertLegible,
		AssertClickable, AssertVisible:
		return true
	case AssertText, AssertCount, AssertURL:
	}
	return false
}

// LayoutRead reports a browser check that writes no data of its own run
// (WritesRunData) and whose every assertion, at least one, is a
// LayoutAssertion: it reads the page's fixed markup, so it needs no data of
// its own to show a layout outcome (coverage design B4). A pass of one can
// pass an outcome once reviewed; a failure of one is still held for a
// person, as every check that writes no run data is.
func LayoutRead(c Check) bool {
	if c.Browser == nil || WritesRunData(c) {
		return false
	}
	asserts := 0
	for _, st := range c.Browser.Steps {
		if st.Expect == nil {
			continue
		}
		if !LayoutAssertion(st.Expect.Assert) {
			return false
		}
		asserts++
	}
	return asserts > 0
}

// FailsLayout reports a browser check whose step failing (0-based) is an
// expect of a LayoutAssertion.
func FailsLayout(c Check, failing int) bool {
	if c.Browser == nil || failing < 0 || failing >= len(c.Browser.Steps) {
		return false
	}
	a := c.Browser.Steps[failing].Expect
	return a != nil && LayoutAssertion(a.Assert)
}

// ValidateRed holds a check proposed to show a defect, whose step failing
// (0-based) failed its assertion, to the rules beyond the saved-check
// grammar: it writes data of its own run (WritesRunData), or it is a
// browser check failing a measured layout assertion (FailsLayout: a read of
// the page's own layout has no shared data to be wrong about; S1 demo bug
// 9, since a person confirms every defect a check saved red finds); and an
// http step lists only success statuses (third review, F2: with an error
// status listed, a misspelled request gets its correct 400 and that body's
// JSON reads as a failed content assertion).
func ValidateRed(c Check, failing int) error {
	if !WritesRunData(c) && !FailsLayout(c, failing) {
		return errors.New("a check that shows a defect writes data of its own run: {{run}} in a POST, PUT or " +
			"PATCH, a browser fill or its program; or, in the browser, fails a measured layout assertion (size, " +
			"left-of, above, no-overlap, equal-size, aligned, legible, clickable or visible)")
	}
	if c.HTTP == nil {
		return nil
	}
	for i, st := range c.HTTP.Steps {
		for _, code := range st.Expect.Status {
			if code < 200 || code > 299 {
				return fmt.Errorf("step %d expects status %d: a check that shows a defect expects only success "+
					"statuses", i+1, code)
			}
		}
	}
	return nil
}

// marshalsRun reports whether one of v's values names RunPlaceholder (fifth
// review, 2): never an object key, which substitution leaves as it is.
func marshalsRun(v *acceptance.Value) bool {
	return v != nil && v.AnyString(hasRun)
}
