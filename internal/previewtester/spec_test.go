// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// validSpec is a spec preview-controller could render: one criterion per
// previewed component, each target its Service on port 80.
const validSpec = `{
  "round": 2, "attempt": 1, "run": "0a1b2c", "budgetSeconds": 45,
  "criteria": [
    {"id": "health", "kind": "http", "statement": "the api is healthy", "target": "api",
     "steps": [{"request": {"method": "GET", "path": "/api/healthz"}, "expect": {"status": [200]}}]},
    {"id": "home", "kind": "http", "statement": "the home page renders", "target": "web",
     "request": {"path": "/"}, "expect": {"bodyContains": "Beat Battle"}}
  ],
  "targets": {"api": "10.96.0.10:80", "web": "10.96.0.11:80"}
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
		// The tester never resolves a name: a target is a Service's
		// ClusterIP, which preview-controller reads and renders.
		"target a Service name": edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "svc:80" }),
		"target a qualified name": edit(func(m map[string]any) {
			m["targets"].(map[string]any)["api"] = "preview-bb-api.maul-preview-0.svc:80"
		}),
		"target unspecified":   edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "0.0.0.0:80" }),
		"target link-local":    edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "169.254.169.254:80" }),
		"target multicast":     edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "224.0.0.1:80" }),
		"target a zone":        edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "[fe80::1%eth0]:80" }),
		"target a url":         edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "http://10.0.0.1:80" }),
		"target with a path":   edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "10.0.0.1:80/x" }),
		"target with userinfo": edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "u@10.0.0.1:80" }),
		"target no port":       edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "10.0.0.1" }),
		"target port zero":     edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "10.0.0.1:0" }),
		"target port too big":  edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "10.0.0.1:65536" }),
		"target port padded":   edit(func(m map[string]any) { m["targets"].(map[string]any)["api"] = "10.0.0.1:080" }),
		"target bad key":       edit(func(m map[string]any) { m["targets"].(map[string]any)["Bad_Key"] = "10.0.0.1:80" }),
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
	s.Targets = map[string]string{"api": "preview-bb-api:80", "web": "10.96.0.11:80"}
	if _, err := s.Encode(); err == nil {
		t.Error("Encode accepted a name the tester would resolve")
	}
}

// TestBudgetFor: the tester's budget leaves room below the Job's deadline
// for the pod to start and the summary to be written, and is a budget
// ParseSpec admits for every deadline the tester Job can carry.
func TestBudgetFor(t *testing.T) {
	cases := []struct {
		deadline time.Duration
		want     int32
	}{
		{time.Minute, 45},
		{10 * time.Minute, 480},
		{time.Hour, 3480},
	}
	for _, c := range cases {
		if got := BudgetFor(c.deadline); got != c.want {
			t.Errorf("BudgetFor(%s) = %d, want %d", c.deadline, got, c.want)
		}
	}
	for d := time.Minute; d <= time.Hour; d += 7 * time.Second {
		b := BudgetFor(d)
		if b < 1 || b > maxBudgetSeconds || time.Duration(b)*time.Second > d-15*time.Second {
			t.Fatalf("BudgetFor(%s) = %ds leaves too little room", d, b)
		}
	}
}

// TestSpecTargetsAreClusterIPs: IPv4 and IPv6 ClusterIPs are targets.
func TestSpecTargetsAreClusterIPs(t *testing.T) {
	s, err := ParseSpec([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"10.96.0.10:80", "[fd00:10:96::a]:80", "172.20.4.7:8080"} {
		s.Targets["api"] = addr
		if err := s.Validate(); err != nil {
			t.Errorf("%s refused: %v", addr, err)
		}
	}
}
