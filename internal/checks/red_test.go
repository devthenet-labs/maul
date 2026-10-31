// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"strings"
	"testing"
)

// TestValidateRed (third review, F2): a check saved to show a defect may
// list only success statuses: with an error status listed, a misspelled
// request gets its correct 400 and that body's JSON reads as a failed
// content assertion.
func TestValidateRed(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		{"success statuses only", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"n":"{{run}}"}},"expect":{"status":[200,201]}}]}}`, ""},
		{"an error status listed", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"n":"{{run}}"}},"expect":{"status":[201,400]}}]}}`,
			"status 400"},
		{"no status listed", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"n":"{{run}}"}},"expect":{"bodyContains":"x"}}]}}`, ""},
		{"a browser check", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
		 {"fill":{"locator":{"label":"N"},"text":"{{run}}"}},{"expect":{"assert":"visible","locator":{"text":"{{run}}"}}}]}}`,
			""},
	} {
		c, err := Parse([]byte(tc.src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		err = ValidateRed(c)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: ValidateRed = %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: ValidateRed = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}
