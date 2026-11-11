// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The cases below were exercised only through slice 1's judge and
// observation tests (internal/acceptance), deleted with the tester; the
// evaluator's checks still run this code, so it keeps its own tests.

// TestMatcherEdges: an observed value that is not one JSON value never
// matches, and objects compare by every key and value, never by order.
func TestMatcherEdges(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		observed string
		want     bool
	}{
		{"observed not JSON", `{"$contains":"a"}`, `{bad`, false},
		{"observed with trailing data", `1`, `1 2`, false},
		{"objects equal in another order", `{"a":1,"b":[1,2]}`, `{"b":[1,2],"a":1}`, true},
		{"object with a key more", `{"a":1}`, `{"a":1,"b":2}`, false},
		{"object against a list", `{"a":1}`, `[1]`, false},
		{"object with another key", `{"a":1}`, `{"b":1}`, false},
		{"object with another value", `{"a":1}`, `{"a":2}`, false},
		{"object with a nested difference", `{"a":{"b":1}}`, `{"a":{"b":"1"}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := parseMatcher(mustValue(t, c.expected))
			if err != nil {
				t.Fatal(err)
			}
			if m, err = m.substitute("r", nil); err != nil {
				t.Fatal(err)
			}
			if got := m.match(c.observed, true); got != c.want {
				t.Errorf("match(%s) against %s = %v, want %v", c.observed, c.expected, got, c.want)
			}
		})
	}
}

// TestMatchHeaderValue: a header is judged as observed text, exactly or by
// $contains under the run placeholder; absent, too large, a matcher a header
// cannot hold, or one whose placeholder does not substitute never matches.
func TestMatchHeaderValue(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		raw      string
		present  bool
		want     bool
	}{
		{"exact", `"application/json"`, "application/json", true, true},
		{"exact differs", `"application/json"`, "text/html", true, false},
		{"contains", `{"$contains":"json"}`, "application/json; charset=utf-8", true, true},
		{"contains the run", `{"$contains":"maul-{{run}}"}`, "id=maul-r7", true, true},
		{"contains another run", `{"$contains":"maul-{{run}}"}`, "id=maul-r8", true, false},
		{"absent", `"application/json"`, "", false, false},
		{"too large", `{"$contains":"x"}`, TooLarge, true, false},
		{"a matcher a header cannot hold", `{"$present":true}`, "x", true, false},
		{"not a matcher", `{"$nope":1}`, "x", true, false},
		{"an unknown capture", `{"$contains":"{{capture.id}}"}`, "x", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MatchHeaderValue(mustValue(t, c.expected), "r7", nil, c.raw, c.present); got != c.want {
				t.Errorf("MatchHeaderValue(%s, %q) = %v, want %v", c.expected, c.raw, got, c.want)
			}
		})
	}
}

// TestValidateMatcherPlaceholders: every text a matcher substitutes into,
// a $contains needle included, has its placeholders checked against the
// captures declared so far.
func TestValidateMatcherPlaceholders(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		captured map[string]bool
		wantErr  string
	}{
		{"contains the run", `{"$contains":"maul-{{run}}"}`, nil, ""},
		{"contains a declared capture", `{"$contains":"{{capture.id}}"}`, map[string]bool{"id": true}, ""},
		{"contains an undeclared capture", `{"$contains":"{{capture.id}}"}`, nil, "capture"},
		{"order with an undeclared capture", `{"$order":["{{capture.id}}"]}`, nil, "capture"},
		{"literal with an undeclared capture", `"{{capture.id}}"`, nil, "capture"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateMatcher(mustValue(t, c.expected), c.captured)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("ValidateMatcher(%s) = %v, want nil", c.expected, err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("ValidateMatcher(%s) = %v, want an error naming %q", c.expected, err, c.wantErr)
			}
		})
	}
}

// TestValueUnmarshalRefusals: a Value is one JSON value, whichever decoder
// reads it; YAML's unquoted timestamp, which JSON has no form for, is
// refused rather than checked as a time the approved text never showed.
func TestValueUnmarshalRefusals(t *testing.T) {
	var v Value
	for _, src := range []string{`{bad`, `1 2`} {
		if err := v.UnmarshalJSON([]byte(src)); err == nil {
			t.Errorf("UnmarshalJSON(%s) = nil, want a refusal", src)
		}
	}
	for _, src := range []string{"2026-11-10", "at: 2026-11-10T12:00:00Z"} {
		if err := yaml.Unmarshal([]byte(src), &v); err == nil || !strings.Contains(err.Error(), "timestamp") {
			t.Errorf("yaml %q = %v, want the timestamp refused", src, err)
		}
	}
	if err := yaml.Unmarshal([]byte(`"2026-11-10"`), &v); err != nil || v.String() != `"2026-11-10"` {
		t.Errorf("a quoted date = %s, %v; want the string", v.String(), err)
	}
}

// TestTargetWaitRecord: each failed attempt is counted under its transport
// class (refused, timeout and dns as themselves, anything else as reset),
// and a wait is reached exactly when one attempt more than its failures was
// made.
func TestTargetWaitRecord(t *testing.T) {
	var w TargetWait
	for _, tr := range []Transport{TransportRefused, TransportRefused, TransportTimeout, TransportDNS,
		TransportReset, TransportTLS, TransportTooLarge} {
		w.Record(tr)
	}
	if w.Refused != 2 || w.Timeout != 1 || w.DNS != 1 || w.Reset != 3 || w.Failures() != 7 {
		t.Fatalf("wait = %+v, want refused 2, timeout 1, dns 1, reset 3", w)
	}
	w.Attempts = 7
	if w.Reached() {
		t.Error("every attempt failed, yet the wait reads as reached")
	}
	w.Attempts = 8
	if !w.Reached() {
		t.Error("the last attempt connected, yet the wait reads as unreached")
	}
}
