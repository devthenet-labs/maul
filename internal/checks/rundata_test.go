// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import "testing"

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

// TestExecAssertion (second review, 2B): the exec marker names the
// assertion that failed, "MAUL-ASSERTION-FAILED: <id>: <what>", at the start
// of a line; without an id it names none.
func TestExecAssertion(t *testing.T) {
	for _, tc := range []struct {
		out, id string
		ok      bool
	}{
		{"x\nMAUL-ASSERTION-FAILED: upset-stake: stake 1, want more\n", "upset-stake", true},
		{"MAUL-ASSERTION-FAILED: the stake was 1\n", "", false},
		{"MAUL-ASSERTION-FAILED: Bad_ID: x\n", "", false},
		{"echo MAUL-ASSERTION-FAILED: a: x\n", "", false},
		{"nothing\n", "", false},
	} {
		id, ok := ExecAssertion(tc.out)
		if id != tc.id || ok != tc.ok {
			t.Errorf("ExecAssertion(%q) = %q, %v; want %q, %v", tc.out, id, ok, tc.id, tc.ok)
		}
	}
}
