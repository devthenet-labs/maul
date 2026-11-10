// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStatusListScalarOrList(t *testing.T) {
	for src, want := range map[string]StatusList{
		`201`:        {201},
		`[200, 204]`: {200, 204},
	} {
		var y StatusList
		if err := yaml.Unmarshal([]byte(src), &y); err != nil || !reflect.DeepEqual(y, want) {
			t.Errorf("yaml %s = %v, %v; want %v", src, y, err, want)
		}
		var j StatusList
		if err := json.Unmarshal([]byte(src), &j); err != nil || !reflect.DeepEqual(j, want) {
			t.Errorf("json %s = %v, %v; want %v", src, j, err, want)
		}
		// Always marshalled as a list, so a CRD mirror can be a plain []int32.
		out, _ := json.Marshal(j)
		if out[0] != '[' {
			t.Errorf("marshal %v = %s, want a list", j, out)
		}
	}
	for _, bad := range []string{`"201"`, `[200, "x"]`, `2.5`, `{}`, `[[200]]`} {
		var j StatusList
		if err := json.Unmarshal([]byte(bad), &j); err == nil {
			t.Errorf("json %s accepted as %v", bad, j)
		}
		var y StatusList
		if err := yaml.Unmarshal([]byte(bad), &y); err == nil {
			t.Errorf("yaml %s accepted as %v", bad, y)
		}
	}
}

func TestValueCanonical(t *testing.T) {
	var a, b Value
	if err := json.Unmarshal([]byte(`{ "b": 1, "a": [true, null, "x"] }`), &a); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte("{a: [true, null, x], b: 1}"), &b); err != nil {
		t.Fatal(err)
	}
	want := `{"a":[true,null,"x"],"b":1}`
	if a.String() != want || b.String() != want {
		t.Errorf("canonical forms %s / %s, want %s", a, b, want)
	}
	out, err := json.Marshal(map[string]Value{"/x": a})
	if err != nil || !bytes.Equal(out, []byte(`{"/x":`+want+`}`)) {
		t.Errorf("marshal = %s, %v", out, err)
	}
	var zero Value
	if out, _ := json.Marshal(zero); string(out) != "null" {
		t.Errorf("zero Value marshals as %s, want null", out)
	}
	// A quoted date is the text it shows.
	var ts Value
	if err := yaml.Unmarshal([]byte(`"2026-10-26"`), &ts); err != nil || ts.String() != `"2026-10-26"` {
		t.Errorf("quoted date = %s, %v", ts, err)
	}
	// An unquoted one would be checked as a timestamp the approver never
	// saw written, so it is refused.
	for _, bad := range []string{"{1: a}", ".nan", ".inf", "2026-10-26", "{a: [2026-10-26T10:00:00Z]}"} {
		var v Value
		if err := yaml.Unmarshal([]byte(bad), &v); err == nil {
			t.Errorf("yaml %q accepted as %s", bad, v)
		}
	}
}
