// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPaths(t *testing.T) {
	want := map[Verb]string{
		VerbHTTP: "/v1/http", VerbBrowser: "/v1/browser", VerbExec: "/v1/exec", VerbLogs: "/v1/logs",
		VerbEvents: "/v1/events", VerbCheckRun: "/v1/check-run", VerbCheckResults: "/v1/check-results",
		VerbConfirm: "/v1/confirm", VerbFinish: "/v1/finish",
	}
	for v, p := range want {
		if got := Path(v); got != p {
			t.Errorf("Path(%s) = %q, want %q", v, got, p)
		}
	}
	if Path(VerbSession) != "" {
		t.Error("the relay's session has no endpoint")
	}
}

// requests is one valid request per endpoint.
func requests() map[Verb]Request {
	return map[Verb]Request{
		VerbHTTP:    &HTTPRequest{Method: "POST", Path: "/api/battles?x=1", Body: `{"name":"run-a"}`},
		VerbBrowser: &BrowserRequest{Verb: "click", Args: []string{"e12"}},
		VerbExec: &ExecRequest{Argv: []string{"go", "run", "main.go"},
			Files: []File{{Name: "main.go", Content: []byte("package main")}}},
		VerbLogs:         &LogsRequest{Component: "api"},
		VerbEvents:       &EventsRequest{},
		VerbCheckRun:     &CheckRunRequest{Spec: json.RawMessage(`{"id":"winner-rises"}`)},
		VerbCheckResults: &CheckResultsRequest{},
		VerbConfirm:      &ConfirmRequest{Digests: []string{strings.Repeat("a", 64)}},
		VerbFinish:       &FinishRequest{Evidence: []EvidenceRef{{Seq: 3, SHA256: strings.Repeat("b", 64)}}},
	}
}

func TestRequestsRoundTrip(t *testing.T) {
	for verb, req := range requests() {
		t.Run(string(verb), func(t *testing.T) {
			if err := req.Check(); err != nil {
				t.Fatalf("valid request refused: %v", err)
			}
			b, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			got := reflect.New(reflect.TypeOf(req).Elem()).Interface().(Request)
			if err := DecodeRequest(bytes.NewReader(b), got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, req) {
				t.Errorf("round trip: got %+v, want %+v", got, req)
			}
		})
	}
}

func TestDecodeRequestRefuses(t *testing.T) {
	cases := map[string]string{
		"unknown field": `{"method":"GET","path":"/","shell":"sh"}`,
		"trailing data": `{"method":"GET","path":"/"} {}`,
		"not json":      `GET /`,
		"too large":     `{"method":"GET","path":"/","body":"` + strings.Repeat("a", MaxRequestBytes) + `"}`,
		"invalid":       `{"method":"TRACE","path":"/"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := DecodeRequest(strings.NewReader(body), &HTTPRequest{}); !errors.Is(err, ErrRequest) {
				t.Errorf("DecodeRequest() = %v, want ErrRequest", err)
			}
		})
	}
}

func TestRequestCheckRefuses(t *testing.T) {
	long := strings.Repeat("a", 5000)
	cases := map[string]Request{
		"http method":        &HTTPRequest{Method: "CONNECT", Path: "/"},
		"http relative path": &HTTPRequest{Method: "GET", Path: "api"},
		"http absolute url":  &HTTPRequest{Method: "GET", Path: "//evil.example/"},
		"http control char":  &HTTPRequest{Method: "GET", Path: "/a\r\nHost: x"},
		"http body":          &HTTPRequest{Method: "POST", Path: "/", Body: strings.Repeat("a", MaxBodyBytes+1)},
		"browser no verb":    &BrowserRequest{},
		"browser verb shape": &BrowserRequest{Verb: "Run Code"},
		"browser arg":        &BrowserRequest{Verb: "fill", Args: []string{long}},
		"exec no argv":       &ExecRequest{},
		"exec empty argv0":   &ExecRequest{Argv: []string{""}},
		"exec file escape":   &ExecRequest{Argv: []string{"x"}, Files: []File{{Name: "../etc/passwd"}}},
		"exec file absolute": &ExecRequest{Argv: []string{"x"}, Files: []File{{Name: "/tmp/x"}}},
		"exec file twice":    &ExecRequest{Argv: []string{"x"}, Files: []File{{Name: "a"}, {Name: "a"}}},
		"exec files large": &ExecRequest{Argv: []string{"x"}, Files: []File{
			{Name: "a", Content: make([]byte, MaxProgramBytes/2+1)}, {Name: "b", Content: make([]byte, MaxProgramBytes/2)},
		}},
		"logs component":     &LogsRequest{Component: "API_1"},
		"check run empty":    &CheckRunRequest{},
		"check run large":    &CheckRunRequest{Spec: json.RawMessage(`"` + strings.Repeat("a", MaxCheckBytes) + `"`)},
		"confirm none":       &ConfirmRequest{},
		"confirm bad digest": &ConfirmRequest{Digests: []string{"abc"}},
		"confirm twice":      &ConfirmRequest{Digests: []string{strings.Repeat("a", 64), strings.Repeat("a", 64)}},
		"finish bad seq":     &FinishRequest{Evidence: []EvidenceRef{{Seq: 0, SHA256: strings.Repeat("a", 64)}}},
		"finish too much": &FinishRequest{Evidence: func() []EvidenceRef {
			rs := make([]EvidenceRef, 0, MaxEvidence+1)
			for i := range MaxEvidence + 1 {
				rs = append(rs, EvidenceRef{Seq: int64(i + 1), SHA256: strings.Repeat("a", 64)})
			}
			return rs
		}()},
		"finish bad evidence": &FinishRequest{Evidence: []EvidenceRef{{Seq: 1, SHA256: "x"}}},
		"confirm too many": &ConfirmRequest{Digests: func() []string {
			ds := make([]string, 0, MaxConfirm+1)
			for i := range MaxConfirm + 1 {
				ds = append(ds, sha256Hex([]byte{byte(i)}))
			}
			return ds
		}()},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if err := req.Check(); err == nil {
				t.Error("Check accepted it")
			}
		})
	}
}

func TestResponseRoundTrip(t *testing.T) {
	exit := 1
	rec := checkRunEntry(KindRelay, VerbConfirm, ModeConfirm, ResultFail, ResultPass).CheckRun
	r := Response{
		Seq: 7, Untrusted: "<html>", Artifacts: []Artifact{{Name: "s.png", SHA256: strings.Repeat("c", 64), Bytes: 3}},
		Observed: &Observed{Status: 200, LatencyMs: 12, Exit: &exit, CheckRuns: []CheckRunRecord{*rec},
			Summary: &Summary{Count: 7, SHA256: strings.Repeat("d", 64)}},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got Response
	if err := DecodeResponse(bytes.NewReader(b), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, r) {
		t.Errorf("round trip: got %+v, want %+v", got, r)
	}
	if err := DecodeResponse(strings.NewReader(`{"seq":1,"trusted":"x"}`), &got); err == nil {
		t.Error("DecodeResponse accepted an unknown field")
	}
}
