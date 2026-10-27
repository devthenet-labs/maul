// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"errors"
	"strings"
	"testing"
)

func TestSubstitute(t *testing.T) {
	caps := map[string]string{"battle": "b-1", "a": "x{{run}}"}
	good := map[string]string{
		"":                                  "",
		"plain":                             "plain",
		"maul-{{run}}":                      "maul-v1a1t1-abcdef",
		"{{capture.battle}}/{{run}}":        "b-1/v1a1t1-abcdef",
		"{{capture.a}}":                     "x{{run}}", // substituted once, never re-expanded
		"{ {run}} {run} }}":                 "{ {run}} {run} }}",
		"{{run}}{{run}}":                    "v1a1t1-abcdefv1a1t1-abcdef",
		"/api/{{capture.battle}}/votes?x=1": "/api/b-1/votes?x=1",
	}
	for in, want := range good {
		got, err := Substitute(in, "v1a1t1-abcdef", caps)
		if err != nil || got != want {
			t.Errorf("Substitute(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"{{", "{{run", "{{ run }}", "{{RUN}}", "{{capture.}}", "{{capture.Bad}}", "{{env.HOME}}",
		"{{capture.battle.x}}", "{{capture}}", "{{}}",
	} {
		if _, err := Substitute(in, "r", caps); err == nil {
			t.Errorf("Substitute(%q) accepted", in)
		}
	}
	_, err := Substitute("{{capture.missing}}", "r", caps)
	if !errors.Is(err, ErrCaptureMissing) {
		t.Errorf("missing capture error = %v, want ErrCaptureMissing", err)
	}
}

func TestPlaceholders(t *testing.T) {
	got, err := placeholders("a {{run}} {{capture.x}} {{capture.y}} {{capture.x}}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "x,y,x" {
		t.Errorf("placeholders = %v", got)
	}
}

func TestBuildRequest(t *testing.T) {
	body := mustValue(t, `{"name":"maul-{{run}}","n":1,"tags":["{{capture.id}}"]}`)
	st := Step{Request: Request{
		Method: "POST", Path: "/api/{{capture.id}}/x?who={{capture.id}}&r={{run}}",
		Headers: map[string]string{"X-Run": "{{run}}"}, JSON: &body,
	}}
	// A capture is escaped into its context, so it can never add a segment
	// or a query parameter; an escaped slash is then refused by CheckPath.
	b, err := BuildRequest(st, "v1a1t1-abcdef", map[string]string{"id": "a b"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Method != "POST" || b.Path != "/api/a%20b/x" {
		t.Errorf("built %+v", b)
	}
	for _, id := range []string{"a/../b", "a/b", "..", "."} {
		if b, err := BuildRequest(st, "r", map[string]string{"id": id}); err == nil {
			t.Errorf("capture %q built %q", id, b.Target())
		}
	}
	b, err = BuildRequest(st, "v1a1t1-abcdef", map[string]string{"id": "42"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Path != "/api/42/x" || b.RawQuery != "who=42&r=v1a1t1-abcdef" ||
		b.Target() != "/api/42/x?who=42&r=v1a1t1-abcdef" {
		t.Errorf("built path %q query %q", b.Path, b.RawQuery)
	}
	if string(b.Body) != `{"n":1,"name":"maul-v1a1t1-abcdef","tags":["42"]}` {
		t.Errorf("body = %s", b.Body)
	}
	if b.Headers["X-Run"] != "v1a1t1-abcdef" || b.Headers["Content-Type"] != "application/json" {
		t.Errorf("headers = %v", b.Headers)
	}
}

// TestBuildRequestHostileCaptures: a value an application returned cannot
// reshape the request it is substituted into.
func TestBuildRequestHostileCaptures(t *testing.T) {
	st := Step{Request: Request{Method: "GET", Path: "/api/x?who={{capture.id}}"}}
	// A query capture is query-escaped: it cannot add a parameter.
	b, err := BuildRequest(st, "r", map[string]string{"id": "1&admin=1"})
	if err != nil || b.RawQuery != "who=1%26admin%3D1" {
		t.Errorf("query capture built %q, %v", b.RawQuery, err)
	}
	// A capture carrying a line break never reaches a header.
	hs := Step{Request: Request{Method: "GET", Path: "/", Headers: map[string]string{"X": "{{capture.id}}"}}}
	if _, err := BuildRequest(hs, "r", map[string]string{"id": "a\r\nHost: evil"}); err == nil {
		t.Error("a capture injected a header line")
	}
	// The body bound holds after substitution.
	big := Step{Request: Request{Method: "POST", Path: "/", Body: "{{capture.id}}"}}
	if _, err := BuildRequest(big, "r", map[string]string{"id": strings.Repeat("b", MaxBodyBytes+1)}); err == nil {
		t.Error("a body over the bound after substitution was accepted")
	}
	if _, err := BuildRequest(hs, "r", nil); !errors.Is(err, ErrCaptureMissing) {
		t.Errorf("missing capture = %v, want ErrCaptureMissing", err)
	}
	// A raw body keeps its own content type unless one is declared.
	raw := Step{Request: Request{Method: "PUT", Path: "/", Body: "k={{run}}"}}
	b, err = BuildRequest(raw, "r1", nil)
	if err != nil || string(b.Body) != "k=r1" || b.Headers["Content-Type"] != "" {
		t.Errorf("raw body built %+v, %v", b, err)
	}
}
