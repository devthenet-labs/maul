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
		// failing is the index of the step whose assertion failed.
		failing int
	}{
		{"success statuses only", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"n":"{{run}}"}},"expect":{"status":[200,201]}}]}}`, "", 0},
		{"an error status listed", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"n":"{{run}}"}},"expect":{"status":[201,400]}}]}}`,
			"status 400", 0},
		{"no status listed", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"n":"{{run}}"}},"expect":{"bodyContains":"x"}}]}}`, "", 0},
		{"a browser check", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
		 {"fill":{"locator":{"label":"N"},"text":"{{run}}"}},{"expect":{"assert":"visible","locator":{"text":"{{run}}"}}}]}}`,
			"", 2},
		// S1 demo bug 9: a browser check that writes nothing may show a
		// defect only by a measured layout or geometry assertion.
		{"a layout read failing its size", layoutRead(`{"assert":"size","locator":{"testid":"pad"},"height":48}`), "", 2},
		{"a layout read failing aligned", layoutRead(`{"assert":"aligned","locator":{"testid":"a"},"other":{"testid":"b"},
		 "axis":"x"}`), "", 2},
		{"a layout read failing a text assertion", layoutRead(`{"assert":"text","locator":{"testid":"t"},
		 "text":{"contains":"{{run}}"}}`), "writes data of its own run", 2},
		{"a layout read failing a count", layoutRead(`{"assert":"count","locator":{"testid":"t"},"count":2}`),
			"writes data of its own run", 2},
		{"a layout read failing a step that is no expect", layoutRead(`{"assert":"size","locator":{"testid":"pad"},
		 "height":48}`), "writes data of its own run", 1},
		{"an http read that writes nothing", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"path":"/p"},"expect":{"json":{"/h":48}}}]}}`, "writes data of its own run", 0},
	} {
		c, err := Parse([]byte(tc.src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		err = ValidateRed(c, tc.failing)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: ValidateRed = %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: ValidateRed = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}

// layoutRead is a browser check that writes nothing: it opens the page,
// scrolls, and asserts expect at its third step.
func layoutRead(expect string) string {
	return `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
	 {"scroll":{"locator":{"testid":"row"},"dx":100,"dy":0}},{"expect":` + expect + `}]}}`
}
