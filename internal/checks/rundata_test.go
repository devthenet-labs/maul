// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import "testing"

// TestCreatesRunData (review finding 3): a check saved to show a defect
// must act with its own run token (in a request, a browser action or its
// program), so what it asserts on is data its run made, not state the
// exploration left behind. A token only in an expectation creates nothing.
func TestCreatesRunData(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		{"an http request names the token", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"method":"POST","path":"/p","json":{"name":"{{run}}-x"}},"expect":{"status":201}}]}}`, true},
		{"an http path names the token", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"path":"/v?q={{run}}"},"expect":{"status":200}}]}}`, true},
		{"the token in an expectation only", `{"id":"a","outcome":"a","form":"http","http":{"target":"api","steps":[
		 {"request":{"path":"/v"},"expect":{"status":200,"bodyContains":"{{run}}"}}]}}`, false},
		{"a browser fill names the token", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[
		 {"goto":"/"},{"fill":{"locator":{"label":"Name"},"text":"{{run}}"}},
		 {"expect":{"assert":"visible","locator":{"text":"{{run}}"}}}]}}`, true},
		{"a browser check acting on nothing of its own", `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[
		 {"goto":"/"},{"expect":{"assert":"visible","locator":{"text":"{{run}}"}}}]}}`, false},
		{"an exec argv names the token", `{"id":"a","outcome":"a","form":"exec","exec":{"argv":["c","{{run}}"],
		 "expect":{"exit":0}}}`, true},
		{"an exec with no token", `{"id":"a","outcome":"a","form":"exec","exec":{"argv":["c"],"expect":{"exit":0}}}`,
			false},
	} {
		c, err := Parse([]byte(tc.src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := CreatesRunData(c); got != tc.want {
			t.Errorf("%s: CreatesRunData = %v, want %v", tc.name, got, tc.want)
		}
	}
}
