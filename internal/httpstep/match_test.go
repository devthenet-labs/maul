// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import "testing"

func mustValue(t *testing.T, src string) Value {
	t.Helper()
	v, err := ParseValue([]byte(src))
	if err != nil {
		t.Fatalf("ParseValue(%s): %v", src, err)
	}
	return v
}

func TestMatchers(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		observed string // "" means absent
		present  bool
		want     bool
	}{
		{"literal equal", `{"a":1}`, `{"a":1}`, true, true},
		{"literal number forms", `1`, `1.0`, true, true},
		{"literal number exp", `1000`, `1e3`, true, true},
		{"literal big ints", `12345678901234567890`, `12345678901234567891`, true, false},
		{"literal differs", `"a"`, `"b"`, true, false},
		{"literal absent", `null`, "", false, false},
		{"literal too large", `"a"`, TooLarge, true, false},
		{"present", `{"$present":true}`, `null`, true, true},
		{"present too large", `{"$present":true}`, TooLarge, true, true},
		{"present absent", `{"$present":true}`, "", false, false},
		{"absent", `{"$absent":true}`, "", false, true},
		{"absent present", `{"$absent":true}`, `1`, true, false},
		{"contains", `{"$contains":"ell"}`, `"hello"`, true, true},
		{"contains no", `{"$contains":"x"}`, `"hello"`, true, false},
		{"contains number", `{"$contains":"1"}`, `1`, true, false},
		{"approx", `{"$approx":10,"$tolerance":1}`, `10.9`, true, true},
		{"approx out", `{"$approx":10,"$tolerance":1}`, `11.5`, true, false},
		{"approx list", `{"$approx":[0,500],"$tolerance":5}`, `[2,497]`, true, true},
		{"approx list len", `{"$approx":[0,500],"$tolerance":5}`, `[2]`, true, false},
		{"approx list vs scalar", `{"$approx":[0],"$tolerance":5}`, `0`, true, false},
		{"approx string", `{"$approx":1,"$tolerance":1}`, `"1"`, true, false},
		{"order", `{"$order":["b","a"]}`, `["b","a"]`, true, true},
		{"order reversed", `{"$order":["b","a"]}`, `["a","b"]`, true, false},
		{"order first occurrence", `{"$order":["b","a"]}`, `["a","b","a"]`, true, false},
		{"order duplicate later", `{"$order":["b","a"]}`, `["b","a","b"]`, true, true},
		{"order missing", `{"$order":["b","a"]}`, `["b"]`, true, false},
		{"order not list", `{"$order":["b"]}`, `"b"`, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := parseMatcher(mustValue(t, c.expected))
			if err != nil {
				t.Fatal(err)
			}
			ok, err := m.substitute("r", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := ok.match(c.observed, c.present); got != c.want {
				t.Errorf("match(%q) = %v, want %v", c.observed, got, c.want)
			}
		})
	}
}
