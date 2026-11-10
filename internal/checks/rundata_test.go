// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"strings"
	"testing"
)

// TestWritesRunData (reviews, finding 3): a check saved to show a defect
// must WRITE data under its own run token (an http POST, PUT or PATCH whose
// path or body names it, a browser fill of it, or an exec program run with
// it), so what it asserts on can be data its run made. A token in a GET's
// query, a navigation or an expectation creates nothing.
func TestWritesRunData(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		{"an http POST body names the token", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"name":"{{run}}-x"}},"expect":{"status":201}}]}}`, true},
		{"an http PUT path names the token", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"PUT","path":"/p/{{run}}","json":{"n":1}},"expect":{"status":200}}]}}`, true},
		{"a GET query names the token", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"path":"/v?q={{run}}"},"expect":{"status":200}}]}}`, false},
		{"a DELETE names the token", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"DELETE","path":"/p/{{run}}"},"expect":{"status":204}}]}}`, false},
		{"the token in an expectation only", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/v"},"expect":{"status":200,"bodyContains":"{{run}}"}}]}}`, false},
		{"a browser fill names the token", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[
		 {"goto":"/"},{"fill":{"locator":{"label":"Name"},"text":"{{run}}"}},
		 {"expect":{"assert":"visible","locator":{"text":"{{run}}"}}}]}}`, true},
		{"a browser goto names the token", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[
		 {"goto":"/?q={{run}}"},{"expect":{"assert":"visible","locator":{"text":"{{run}}"}}}]}}`, false},
		// Fifth review, 2: substitution never touches an object key, so a
		// token used only as one is the same literal in every run.
		{"an http POST names the token only as a JSON key", `{"id":"a","outcome":"a","form":"http",
		 "http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"{{run}}":1}},"expect":{"status":201}}]}}`, false},
		{"an http POST names the token in a nested value", `{"id":"a","outcome":"a","form":"http",
		 "http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"k":["{{run}}"]}},"expect":{"status":201}}]}}`, true},
		{"an exec argv names the token", `{"id":"a","outcome":"a","form":"exec","exec":{"argv":["c","{{run}}"],
		 "expect":{"exit":0}}}`, true},
		{"an exec with no token", `{"id":"a","outcome":"a","form":"exec","exec":{"argv":["c"],"expect":{"exit":0}}}`,
			false},
	} {
		c, err := Parse([]byte(tc.src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := WritesRunData(c); got != tc.want {
			t.Errorf("%s: WritesRunData = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// testMarker is the line start one attempt's {{marker}} stands for.
var testMarker = ExecMarker("0123456789abcdef0123456789abcdef")

// longID is an assertion id of exactly MaxExecAssertionIDLen characters.
var longID = "a" + strings.Repeat("b-", (MaxExecAssertionIDLen-1)/2) + strings.Repeat("c", (MaxExecAssertionIDLen-1)%2)

// TestExecAssertion (second review, 2B; PR #320 F1, F6): the exec marker
// names the assertion that failed, "<marker> <id>: <what>", at the start of
// a line, the marker being this attempt's own; the first line that carries
// it with a valid id names it. A bare MAUL-ASSERTION-FAILED: (text the
// application can echo) or another attempt's marker names none.
func TestExecAssertion(t *testing.T) {
	other := ExecMarker("ffffffffffffffffffffffffffffffff")
	for _, tc := range []struct {
		out, id string
		ok      bool
	}{
		{"x\n" + testMarker + " upset-stake: stake 1, want more\n", "upset-stake", true},
		{testMarker + " the stake was 1\n", "", false},
		{testMarker + " Bad_ID: x\n", "", false},
		{testMarker + " no id\n" + testMarker + " upset-stake: x\n", "upset-stake", true},
		{"echo " + testMarker + " a: x\n", "", false},
		{"MAUL-ASSERTION-FAILED: upset-stake: from the application\n", "", false},
		{other + " upset-stake: another attempt's\n", "", false},
		{"nothing\n", "", false},
		// Gate D-B2: an id longer than 32 characters names its assertion,
		// up to MaxExecAssertionIDLen; past it, none.
		{testMarker + " first-json-is-usage-error-first-alone: x\n", "first-json-is-usage-error-first-alone", true},
		{testMarker + " " + longID + ": x\n", longID, true},
		{testMarker + " " + longID + "x: x\n", "", false},
	} {
		id, ok := ExecAssertion(tc.out, testMarker)
		if id != tc.id || ok != tc.ok {
			t.Errorf("ExecAssertion(%q) = %q, %v; want %q, %v", tc.out, id, ok, tc.id, tc.ok)
		}
	}
	if _, ok := ExecAssertion("MAUL-ASSERTION-FAILED-: a: x\n", ""); ok {
		t.Error("an empty marker matched")
	}
}

// TestExecMarked (S1 demo #57; PR #320 F1): a line that starts with this
// attempt's marker, a valid id or not, says the program found the outcome
// does not hold. A line the application could have written (the bare
// MAUL-ASSERTION-FAILED:, another marker) never does.
func TestExecMarked(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"x\n" + testMarker + " upset-stake: stake 1, want more\nDONE\n", true},
		{testMarker + " the stake was 1\r\n", true},
		{testMarker, true},
		{"echo " + testMarker + " a: x\n", false},
		{"MAUL-ASSERTION-FAILED: a: x\nDONE\n", false},
		{ExecMarker("ffffffffffffffffffffffffffffffff") + " a: x\n", false},
		{"nothing\nDONE\n", false},
		{"", false},
	} {
		if got := ExecMarked(tc.out, testMarker); got != tc.want {
			t.Errorf("ExecMarked(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
	if ExecMarked("MAUL-ASSERTION-FAILED-: a\n", "") || ExecMarked("x\n", "") {
		t.Error("an empty marker matched")
	}
}

// TestExecProgram (PR #320 F1): {{marker}} in a program file stands for
// the attempt's marker; nothing else is touched.
func TestExecProgram(t *testing.T) {
	got := ExecProgram("print('{{marker}} a: x', '{{run}}')\n", testMarker)
	if want := "print('" + testMarker + " a: x', '{{run}}')\n"; got != want {
		t.Errorf("ExecProgram = %q, want %q", got, want)
	}
}

// TestLayoutRead (coverage design B4): a browser check that writes no data
// of its own run and asserts only the page's layout reads the page's fixed
// markup, so it needs no data of its own; any data write, any text, count
// or url assertion, a locator by text, hasText or nth, an assertion only of
// visibility or clickability, another form or no assertion at all is not one.
func TestLayoutRead(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		{"size and aligned", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
		 {"expect":{"assert":"size","locator":{"testid":"pad"},"width":48,"height":48}},
		 {"expect":{"assert":"aligned","axis":"x","locator":{"testid":"a"},"other":{"testid":"b"}}}]}}`, true},
		{"a size at least", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
		 {"expect":{"assert":"size","locator":{"testid":"pad"},"minWidth":48,"maxHeight":64}}]}}`, true},
		{"visible beside a size", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
		 {"scroll":{"locator":{"testid":"row"},"dx":40,"dy":0}},
		 {"expect":{"assert":"visible","locator":{"testid":"pad"}}},
		 {"expect":{"assert":"size","locator":{"testid":"pad"},"width":48}}]}}`, true},
		{"visible only", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
		 {"scroll":{"locator":{"testid":"row"},"dx":40,"dy":0}},
		 {"expect":{"assert":"visible","locator":{"testid":"pad"}}}]}}`, false},
		{"clickable only", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
		 {"expect":{"assert":"clickable","locator":{"testid":"pad"}}}]}}`, false},
		{"a row by its text", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/board"},
		 {"expect":{"assert":"visible","locator":{"role":"row","hasText":"Alice"}}},
		 {"expect":{"assert":"size","locator":{"testid":"board"},"width":600}}]}}`, false},
		{"an nth row", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/board"},
		 {"expect":{"assert":"above","locator":{"role":"row","nth":0},"other":{"role":"row","nth":1}}}]}}`, false},
		{"text in a within", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/board"},
		 {"expect":{"assert":"size","locator":{"testid":"cell","within":{"text":"Alice"}},"width":40}}]}}`, false},
		{"text in the other", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/board"},
		 {"expect":{"assert":"left-of","locator":{"testid":"pad"},"other":{"text":"Alice"}}}]}}`, false},
		{"a text assertion", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
		 {"expect":{"assert":"size","locator":{"testid":"pad"},"width":48}},
		 {"expect":{"assert":"text","locator":{"testid":"top"},"text":{"contains":"x"}}}]}}`, false},
		{"a fill of its own run", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
		 {"fill":{"locator":{"label":"Name"},"text":"{{run}}"}},
		 {"expect":{"assert":"visible","locator":{"text":"{{run}}"}}}]}}`, false},
		{"no assertion", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"}]}}`, false},
		{"an http check", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"path":"/v"},"expect":{"json":{"/v":1}}}]}}`, false},
	} {
		c, err := Parse([]byte(tc.src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := LayoutRead(c); got != tc.want {
			t.Errorf("%s: LayoutRead = %v, want %v", tc.name, got, tc.want)
		}
	}
}
