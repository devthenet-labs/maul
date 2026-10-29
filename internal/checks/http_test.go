// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

func value(t *testing.T, s string) acceptance.Value {
	t.Helper()
	v, err := acceptance.ParseValue([]byte(s))
	if err != nil {
		t.Fatalf("ParseValue(%s): %v", s, err)
	}
	return v
}

func TestMatchJSON(t *testing.T) {
	caps := map[string]string{"id": "7"}
	for _, tc := range []struct {
		expected, raw string
		present, want bool
	}{
		// The new matchers.
		{`{"$range":[1,3]}`, `2`, true, true},
		{`{"$range":[1,3]}`, `3`, true, true},
		{`{"$range":[1,3]}`, `3.5`, true, false},
		{`{"$range":[1,3]}`, `"2"`, true, false},
		{`{"$range":[1,3]}`, ``, false, false},
		{`{"$range":[-1.5,-1.5]}`, `-1.5`, true, true},
		{`{"$includes":"{{run}}-a"}`, `["x","R-a"]`, true, true},
		{`{"$includes":"{{run}}-a"}`, `["x","y"]`, true, false},
		{`{"$includes":"{{run}}-a"}`, `"R-a"`, true, false},
		{`{"$includes":{"name":"{{run}}-a"}}`, `[{"name":"R-a","rating":1012}]`, true, true},
		{`{"$includes":{"name":"{{run}}-a","p":{"id":"{{capture.id}}"}}}`,
			`[{"name":"R-a","p":{"id":"7","x":1}}]`, true, true},
		{`{"$includes":{"name":"{{run}}-a"}}`, `[{"name":"R-b"}]`, true, false},
		{`{"$includes":{"rating":1012}}`, `[{"rating":1012.0}]`, true, true},
		{`{"$includes":[1,2]}`, `[[1,2],[3]]`, true, true},
		{`{"$includes":[1]}`, `[[1,2]]`, true, false},
		// The slice-1 matchers, delegated unchanged.
		{`{"$order":["{{run}}-a","{{run}}-b"]}`, `["R-a","x","R-b"]`, true, true},
		{`{"$order":["{{run}}-a","{{run}}-b"]}`, `["R-b","R-a"]`, true, false},
		{`{"$present":true}`, `null`, true, true},
		{`{"$approx":10,"$tolerance":1}`, `10.5`, true, true},
		{`"{{run}}"`, `"R"`, true, true},
	} {
		got := MatchJSON(value(t, tc.expected), "R", caps, tc.raw, tc.present)
		if got != tc.want {
			t.Errorf("MatchJSON(%s, %q, %v) = %v, want %v", tc.expected, tc.raw, tc.present, got, tc.want)
		}
	}
}

func TestMatchHeader(t *testing.T) {
	for _, tc := range []struct {
		expected, raw string
		present, want bool
	}{
		{`{"$present":true}`, `30`, true, true},
		{`{"$present":true}`, ``, false, false},
		{`{"$absent":true}`, ``, false, true},
		{`{"$absent":true}`, `x`, true, false},
		{`{"$contains":"max-age"}`, `public, max-age=60`, true, true},
		{`"60"`, `60`, true, true},
		{`"60"`, `61`, true, false},
	} {
		got := MatchHeader(value(t, tc.expected), "R", nil, tc.raw, tc.present)
		if got != tc.want {
			t.Errorf("MatchHeader(%s, %q, %v) = %v, want %v", tc.expected, tc.raw, tc.present, got, tc.want)
		}
	}
}
