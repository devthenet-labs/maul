// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import "encoding/json"

// CreatesRunData reports whether c acts with its own run token: an http
// request (its path, headers or body), a browser action (a goto, fill,
// select or key press), or an exec's argv or program files names
// RunPlaceholder. A check saved to show a defect must (review finding 3):
// its confirmation runs against whatever state the evaluator's exploration
// left behind, so what it asserts on has to be data its own run made. A
// token only in an expectation creates nothing.
func CreatesRunData(c Check) bool {
	switch {
	case c.HTTP != nil:
		for _, st := range c.HTTP.Steps {
			if marshalsRun(st.Request) {
				return true
			}
		}
	case c.Browser != nil:
		for _, st := range c.Browser.Steps {
			if st.Expect == nil && marshalsRun(st) {
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

// marshalsRun reports whether v's JSON names RunPlaceholder.
func marshalsRun(v any) bool {
	b, err := json.Marshal(v)
	return err == nil && hasRun(string(b))
}
