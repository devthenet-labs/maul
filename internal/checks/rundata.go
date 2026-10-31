// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"encoding/json"
	"strings"
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

// marshalsRun reports whether v's JSON names RunPlaceholder.
func marshalsRun(v any) bool {
	b, err := json.Marshal(v)
	return err == nil && hasRun(string(b))
}
