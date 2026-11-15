// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import "testing"

// TestCarriedFieldExpectationsAreTyped pins what the slot evaluate prompt
// tells a check of a carried-over field (gate 4, the #414 caveat): `null`,
// `""`, `0` and `[]` are literal expectations the grammar admits, each
// matching only its own JSON type; `{"$absent": true}` matches only a key
// left out, never a null; and `{"$present": true}` reads null, empty and a
// value alike, so it never shows a value was carried over. A pointer that
// resolves to JSON null is present, its raw value "null".
func TestCarriedFieldExpectationsAreTyped(t *testing.T) {
	type observed struct {
		raw     string
		present bool
	}
	var (
		null    = observed{"null", true}
		empty   = observed{`""`, true}
		zero    = observed{"0", true}
		list    = observed{"[]", true}
		absent  = observed{"", false}
		aString = observed{`"7"`, true}
	)
	all := []observed{null, empty, zero, list, absent, aString}
	for _, tt := range []struct {
		expected string
		matches  observed
	}{
		{"null", null},
		{`""`, empty},
		{"0", zero},
		{"[]", list},
		{`{"$absent": true}`, absent},
	} {
		v, err := ParseValue([]byte(tt.expected))
		if err != nil {
			t.Fatalf("ParseValue(%s): %v", tt.expected, err)
		}
		if err := ValidateMatcher(v, nil); err != nil {
			t.Fatalf("ValidateMatcher(%s) = %v, want admitted", tt.expected, err)
		}
		for _, o := range all {
			if got, want := MatchValue(v, "", nil, o.raw, o.present), o == tt.matches; got != want {
				t.Errorf("%s against %+v = %v, want %v", tt.expected, o, got, want)
			}
		}
	}
	present, err := ParseValue([]byte(`{"$present": true}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []observed{null, empty, aString} {
		if !MatchValue(present, "", nil, o.raw, o.present) {
			t.Errorf("$present against %+v = false, want true: it reads them alike", o)
		}
	}
}
