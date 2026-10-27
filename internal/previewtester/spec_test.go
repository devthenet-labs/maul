// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// validSpec is a spec preview-controller could render: one criterion per
// previewed component, each target its Service on port 80.
const validSpec = `{
  "round": 2, "attempt": 1, "run": "0a1b2c",
  "criteria": [
    {"id": "health", "kind": "http", "statement": "the api is healthy", "target": "api",
     "steps": [{"request": {"method": "GET", "path": "/api/healthz"}, "expect": {"status": [200]}}]},
    {"id": "home", "kind": "http", "statement": "the home page renders", "target": "web",
     "request": {"path": "/"}, "expect": {"bodyContains": "Beat Battle"}}
  ],
  "targets": {"api": "preview-bb-api:80", "web": "preview-bb:80"}
}`

func TestParseSpec(t *testing.T) {
	s, err := ParseSpec([]byte(validSpec))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if s.Round != 2 || s.Attempt != 1 || s.Run != "0a1b2c" || len(s.Criteria) != 2 {
		t.Fatalf("ParseSpec = %+v", s)
	}
	if got := s.RunTokens().Token(1); got != "v2a1t1-0a1b2c" {
		t.Errorf("RunTokens().Token(1) = %q, want v2a1t1-0a1b2c", got)
	}
}

// TestSpecEncodeRoundTrip: what preview-controller encodes, the tester
// decodes to the same spec.
func TestSpecEncodeRoundTrip(t *testing.T) {
	s, err := ParseSpec([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	back, err := ParseSpec(b)
	if err != nil {
		t.Fatalf("ParseSpec(Encode): %v", err)
	}
	if !reflect.DeepEqual(back, s) {
		t.Errorf("round trip changed the spec:\n got %+v\nwant %+v", back, s)
	}
}

// TestParseSpecRefusals: the tester runs nothing it was not built to run.
// Every refusal is fail-closed: an invalid spec observes nothing.
func TestParseSpecRefusals(t *testing.T) {
	edit := func(f func(m map[string]any)) string {
		var m map[string]any
		if err := json.Unmarshal([]byte(validSpec), &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	criterion := func(m map[string]any, i int) map[string]any {
		return m["criteria"].([]any)[i].(map[string]any)
	}
	cases := map[string]string{
		"empty":         ``,
		"not json":      `round: 1`,
		"trailing data": validSpec + `{}`,
		"unknown field": edit(func(m map[string]any) { m["image"] = "x" }),
		"oversize": edit(func(m map[string]any) {
			criterion(m, 0)["statement"] = strings.Repeat("x", MaxSpecBytes)
		}),
		"round zero":       edit(func(m map[string]any) { m["round"] = 0 }),
		"attempt zero":     edit(func(m map[string]any) { m["attempt"] = 0 }),
		"attempt four":     edit(func(m map[string]any) { m["attempt"] = 4 }),
		"run uppercase":    edit(func(m map[string]any) { m["run"] = "0A1B2C" }),
		"run short":        edit(func(m map[string]any) { m["run"] = "0a1b2" }),
		"run with a slash": edit(func(m map[string]any) { m["run"] = "0a/b2c" }),
		"no criteria":      edit(func(m map[string]any) { m["criteria"] = []any{} }),
		"manual criterion": edit(func(m map[string]any) {
			m["criteria"] = []any{map[string]any{"id": "x", "kind": "manual", "statement": "s", "reason": "r"}}
		}),
		"command criterion": edit(func(m map[string]any) {
			m["criteria"] = []any{map[string]any{"id": "x", "kind": "command", "statement": "s",
				"in": map[string]any{"component": "api"}, "argv": []any{"/bin/true"}}}
		}),
		"invalid criterion": edit(func(m map[string]any) {
			criterion(m, 0)["steps"].([]any)[0].(map[string]any)["request"].(map[string]any)["path"] = "//evil.example/"
		}),
		"duplicate ids": edit(func(m map[string]any) { criterion(m, 1)["id"] = "health" }),
		"target not in targets": edit(func(m map[string]any) {
			delete(m["targets"].(map[string]any), "web")
		}),
		"target an ip literal": edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "10.0.0.1:80" }),
		"target ipv6 literal":  edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "[::1]:80" }),
		"target a url":         edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "http://svc:80" }),
		"target with a path":   edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "svc:80/x" }),
		"target with userinfo": edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "u@svc:80" }),
		"target no port":       edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "svc" }),
		"target port zero":     edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "svc:0" }),
		"target port too big":  edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "svc:65536" }),
		"target uppercase":     edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "Svc:80" }),
		"target bad key":       edit(func(m map[string]any) { m["targets"].(map[string]any)["Bad_Key"] = "svc:80" }),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSpec([]byte(raw)); err == nil {
				t.Errorf("ParseSpec accepted %s", name)
			}
		})
	}
}

// TestEncodeRefusesInvalidSpec: the writer is held to the same rules.
func TestEncodeRefusesInvalidSpec(t *testing.T) {
	s, err := ParseSpec([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	s.Targets = map[string]string{"api": "10.0.0.1:80", "web": "preview-bb:80"}
	if _, err := s.Encode(); err == nil {
		t.Error("Encode accepted an IP literal target")
	}
}
