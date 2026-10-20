// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/provider"
	"github.com/devthenet-labs/maul/internal/usagewire"
)

// openaiUsageObject is a Responses usage object.
func openaiUsageObject(input, cached, output int64) string {
	return fmt.Sprintf(`{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d},"output_tokens":%d,`+
		`"output_tokens_details":{"reasoning_tokens":%d},"total_tokens":%d}`, input, cached, output, output/2,
		input+output)
}

// openaiStream is a Responses event stream in the shape the API sends: the
// created and in-progress events (usage null), output deltas, then the
// terminal event carrying the whole response, whose encrypted reasoning
// item is padded by pad bytes (a real one is routinely far over 64 KiB).
// terminal "" ends the stream before any terminal event, as a cut does.
func openaiStream(pad int, terminal, usageJSON string) string {
	var b strings.Builder
	event := func(typ, data string) {
		b.WriteString("event: " + typ + "\n" + "data: " + data + "\n\n")
	}
	head := `{"id":"resp_1","object":"response","status":"in_progress","model":"gpt-5.3-codex-2026-02-05",` +
		`"output":[],"usage":null}`
	event("response.created", `{"type":"response.created","sequence_number":0,"response":`+head+`}`)
	event("response.in_progress", `{"type":"response.in_progress","sequence_number":1,"response":`+head+`}`)
	event("response.output_item.added", `{"type":"response.output_item.added","sequence_number":2,"output_index":0,`+
		`"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}`)
	for i, d := range []string{"Hel", "lo ", `\"usage\": not this`} {
		event("response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":%d,`+
			`"item_id":"msg_1","output_index":0,"content_index":0,"delta":"%s"}`, 3+i, d))
	}
	if terminal == "" {
		return b.String()
	}
	status := strings.TrimPrefix(terminal, "response.")
	event(terminal, `{"type":"`+terminal+`","sequence_number":9,"response":{"id":"resp_1","object":"response",`+
		`"status":"`+status+`","model":"gpt-5.3-codex-2026-02-05","output":[{"type":"reasoning","id":"rs_1",`+
		`"encrypted_content":"`+strings.Repeat("A", pad)+`","summary":[]},{"type":"message","id":"msg_1",`+
		`"role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":`+usageJSON+`}}`)
	return b.String()
}

func scanOpenAI(contentType string, chunks ...string) (usage, bool) {
	s := &usageScanner{dialect: openaiUsage{}}
	s.start(contentType)
	var total usage
	for _, c := range chunks {
		total = total.mergeAdd(s.write([]byte(c)))
	}
	total = total.mergeAdd(s.finish())
	return total, s.complete
}

func TestOpenAIUsageScanner(t *testing.T) {
	u := openaiUsageObject(12000, 8000, 900)
	want := usage{input: 4000, cacheRead: 8000, output: 900}
	big := 200 << 10 // a terminal event far over the Anthropic routes' 64 KiB
	tests := []struct {
		name        string
		contentType string
		body        string
		want        usage
		complete    bool
	}{
		{"completed over 64 KiB", "text/event-stream", openaiStream(big, "response.completed", u), want, true},
		{"completed small", "text/event-stream; charset=utf-8", openaiStream(10, "response.completed", u), want, true},
		{"incomplete", "text/event-stream", openaiStream(big, "response.incomplete", u), want, true},
		{"failed", "text/event-stream", openaiStream(10, "response.failed", u), want, true},
		{"terminal without usage", "text/event-stream", openaiStream(10, "response.completed", "null"), usage{}, false},
		{"cut before the terminal event", "text/event-stream", openaiStream(0, "", ""), usage{}, false},
		{"terminal over the route cap", "text/event-stream",
			openaiStream(maxOpenAIEventLine, "response.completed", u), usage{}, false},
		{"json body", "application/json", `{"id":"resp_1","object":"response","output":[],"usage":` + u + `}`,
			want, true},
		{"json body without usage", "application/json", `{"id":"resp_1","usage":null}`, usage{}, false},
		{"json body over the route cap", "application/json",
			`{"pad":"` + strings.Repeat("x", maxOpenAIJSONBody) + `","usage":` + u + `}`, usage{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, complete := scanOpenAI(tt.contentType, tt.body)
			if got != tt.want || complete != tt.complete {
				t.Fatalf("usage = %+v complete = %v, want %+v/%v", got, complete, tt.want, tt.complete)
			}
		})
	}
	// The Anthropic routes' bound still skips the same event: the OpenAI
	// route's own bound is what reads it.
	if got, complete := scanAll("text/event-stream", openaiStream(big, "response.completed", u)); complete ||
		got.total() != 0 {
		t.Errorf("the anthropic dialect read a Responses stream: %+v %v", got, complete)
	}
}

// TestOpenAIUsageScannerChunkingProperty (seeded): however a Responses
// stream with a >64 KiB terminal event is split into writes, the charge and
// completion are those of the whole stream.
func TestOpenAIUsageScannerChunkingProperty(t *testing.T) {
	stream := openaiStream(100<<10, "response.completed", openaiUsageObject(5000, 1200, 300))
	want := usage{input: 3800, cacheRead: 1200, output: 300}
	prop := func(cuts []uint32) bool {
		points := make([]int, 0, len(cuts))
		for _, c := range cuts {
			points = append(points, int(c)%len(stream))
		}
		sort.Ints(points)
		chunks := make([]string, 0, len(points)+1)
		prev := 0
		for _, p := range points {
			chunks = append(chunks, stream[prev:p])
			prev = p
		}
		chunks = append(chunks, stream[prev:])
		got, complete := scanOpenAI("text/event-stream", chunks...)
		return complete && got == want
	}
	if err := quick.Check(prop, quickConfig()); err != nil {
		t.Fatal(err)
	}
}

// TestOpenAIUsageConservationProperty (seeded): the Responses mapping
// conserves the reported totals. Fresh input plus cache reads is exactly
// input_tokens, output is output_tokens, no cache write is ever invented,
// and the total every limit counts is input_tokens + output_tokens, for
// any figures the upstream reports, malformed ones included (a cached
// count over the input, a negative one).
func TestOpenAIUsageConservationProperty(t *testing.T) {
	cfg := quickConfig()
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		in := r.Int63n(1 << 40)
		cached := r.Int63n(in + 1)
		switch r.Intn(6) {
		case 0:
			cached = in + 1 + r.Int63n(1000) // more cached than input
		case 1:
			cached = -r.Int63n(1000)
		}
		args[0] = reflect.ValueOf(in)
		args[1] = reflect.ValueOf(cached)
		args[2] = reflect.ValueOf(r.Int63n(1 << 40))
	}
	prop := func(in, cached, out int64) bool {
		var j openaiUsageJSON
		if err := json.Unmarshal([]byte(openaiUsageObject(in, cached, out)), &j); err != nil {
			return false
		}
		u := j.usage()
		return u.input+u.cacheRead == in && u.output == out && u.cacheCreation == 0 && u.cacheWrite1h == 0 &&
			u.input >= 0 && u.cacheRead >= 0 && u.cacheRead <= in && u.total() == in+out
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}

// TestOpenAIUsageMapping pins the mapping's edge cases as examples.
func TestOpenAIUsageMapping(t *testing.T) {
	tests := []struct {
		raw  string
		want usage
	}{
		{openaiUsageObject(100, 40, 7), usage{input: 60, cacheRead: 40, output: 7}},
		{`{"input_tokens":100,"output_tokens":7}`, usage{input: 100, output: 7}},
		{openaiUsageObject(100, 150, 7), usage{cacheRead: 100, output: 7}},
		{openaiUsageObject(100, -5, 7), usage{input: 100, output: 7}},
		{`{"input_tokens":-3,"output_tokens":-1}`, usage{}},
		{`{}`, usage{}},
	}
	for _, tt := range tests {
		var j openaiUsageJSON
		if err := json.Unmarshal([]byte(tt.raw), &j); err != nil {
			t.Fatal(err)
		}
		if got := j.usage(); got != tt.want {
			t.Errorf("%s: usage = %+v, want %+v", tt.raw, got, tt.want)
		}
	}
	var none *openaiUsageJSON
	if none.usage() != (usage{}) {
		t.Error("a nil usage object is not zero")
	}
}

func TestInspectOpenAIBody(t *testing.T) {
	const fn = `{"type":"function","name":"shell","parameters":{"type":"object","properties":` +
		`{"file_id":{"type":"string"},"image_url":{"type":"string"},"file_url":{"type":"string"}}}}`
	tests := []struct {
		name      string
		body      string
		wantErr   string
		model     string
		maxTokens int64
	}{
		{"empty", ``, "", "", 0},
		{"plain", `{"model":"gpt-5.3-codex","input":"hi","stream":true,"store":false}`, "", "gpt-5.3-codex", 0},
		{"max_output_tokens", `{"model":"gpt-5.3-codex","max_output_tokens":4000}`, "", "gpt-5.3-codex", 4000},
		{"max_output_tokens null", `{"model":"m","max_output_tokens":null}`, "", "m", 0},
		{"max_output_tokens float", `{"max_output_tokens":4000.5}`, "max_output_tokens must be", "", 0},
		{"max_output_tokens exponent", `{"max_output_tokens":4e3}`, "max_output_tokens must be", "", 0},
		{"max_output_tokens string", `{"max_output_tokens":"4000"}`, "max_output_tokens must be", "", 0},
		{"not an object", `[1]`, "not a JSON object", "", 0},
		{"codex tools", `{"model":"m","tools":[` + fn + `,{"type":"custom","name":"apply_patch",` +
			`"format":{"type":"grammar","syntax":"lark","definition":"start: x"}},{"type":"local_shell"}]}`, "", "m", 0},
		{"web_search", `{"tools":[{"type":"web_search"}]}`, `server tool "web_search"`, "", 0},
		{"web_search_preview", `{"tools":[{"type":"web_search_preview"}]}`, "server tool", "", 0},
		{"dated web_search_preview", `{"tools":[{"type":"web_search_preview_2025_03_11"}]}`, "server tool", "", 0},
		{"file_search", `{"tools":[{"type":"file_search","vector_store_ids":["vs_1"]}]}`, "server tool", "", 0},
		{"code_interpreter", `{"tools":[{"type":"code_interpreter","container":{"type":"auto"}}]}`, "server tool",
			"", 0},
		{"mcp", `{"tools":[{"type":"mcp","server_label":"x","server_url":"https://x"}]}`, "server tool", "", 0},
		{"image_generation", `{"tools":[{"type":"image_generation"}]}`, "server tool", "", 0},
		{"computer_use_preview", `{"tools":[{"type":"computer_use_preview"}]}`, "server tool", "", 0},
		{"connector", `{"tools":[{"type":"function","name":"x","connector_id":"connector_gmail"}]}`,
			`tool field "connector_id"`, "", 0},
		{"server_url", `{"tools":[{"type":"function","name":"x","server_url":"https://x"}]}`,
			`tool field "server_url"`, "", 0},
		{"background", `{"background":true}`, `"background"`, "", 0},
		{"background string", `{"background":"yes"}`, `"background"`, "", 0},
		{"background false", `{"model":"m","background":false}`, "", "m", 0},
		{"previous_response_id", `{"previous_response_id":"resp_1"}`, `"previous_response_id"`, "", 0},
		{"conversation", `{"conversation":"conv_1"}`, `"conversation"`, "", 0},
		{"prompt", `{"prompt":{"id":"pmpt_1"}}`, `"prompt"`, "", 0},
		{"null state fields", `{"model":"m","previous_response_id":null,"conversation":null,"prompt":null}`, "", "m", 0},
		{"input_file file_id", `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]}`,
			"file references", "", 0},
		{"input_file file_url", `{"input":[{"role":"user","content":[{"type":"input_file",` +
			`"file_url":"https://x/a.pdf"}]}]}`, "file references", "", 0},
		{"input_file inline", `{"model":"m","input":[{"role":"user","content":[{"type":"input_file",` +
			`"filename":"a.pdf","file_data":"data:application/pdf;base64,AAAA"}]}]}`, "", "m", 0},
		{"input_image url", `{"input":[{"role":"user","content":[{"type":"input_image",` +
			`"image_url":"https://x/a.png"}]}]}`, "image URLs", "", 0},
		{"input_image file_id", `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-1"}]}]}`,
			"file references", "", 0},
		{"input_image object url", `{"input":[{"content":[{"type":"input_image","image_url":{"url":"http://x"}}]}]}`,
			"image URLs", "", 0},
		{"input_image inline", `{"model":"m","input":[{"role":"user","content":[{"type":"input_image",` +
			`"image_url":"DATA:image/png;base64,AAAA"}]}]}`, "", "m", 0},
		{"function output with an image url", `{"input":[{"type":"function_call_output","call_id":"c",` +
			`"output":[{"type":"input_image","image_url":"https://x"}]}]}`, "image URLs", "", 0},
		{"json_schema format", `{"model":"m","text":{"format":{"type":"json_schema","name":"r",` +
			`"schema":{"properties":{"file_id":{"type":"string"}}}}}}`, "", "m", 0},
		{"file_id in a non-function's parameters", `{"tools":[{"type":"custom","parameters":{"file_id":"x"}}]}`,
			"file references", "", 0},
		{"local shell and apply_patch", `{"model":"m","tools":[{"type":"shell"},{"type":"apply_patch"}]}`, "",
			"m", 0},
		{"hosted shell environment", `{"tools":[{"type":"shell","environment":{"type":"container_auto"}}]}`,
			`tool field "environment"`, "", 0},
		{"hosted shell container reference", `{"tools":[{"type":"shell","environment":` +
			`{"type":"container_reference","container_id":"cntr_1"}}]}`, `tool field "environment"`, "", 0},
		{"tool with a container", `{"tools":[{"type":"function","name":"x","container":{"type":"auto"}}]}`,
			`tool field "container"`, "", 0},
		{"unknown tool type", `{"tools":[{"type":"tool_search"}]}`, `server tool "tool_search"`, "", 0},
		{"tool without a type", `{"tools":[{"name":"x"}]}`, `server tool ""`, "", 0},
		{"computer_use", `{"tools":[{"type":"computer_use"}]}`, "server tool", "", 0},
		{"namespace of functions", `{"model":"m","tools":[{"type":"namespace","name":"multi_agent_v1",` +
			`"tools":[` + fn + `,{"type":"custom","name":"x"}]}]}`, "", "m", 0},
		{"namespace with a server tool", `{"tools":[{"type":"namespace","name":"n",` +
			`"tools":[{"type":"web_search"}]}]}`, `server tool "web_search"`, "", 0},
		{"namespace with a connector", `{"tools":[{"type":"namespace","name":"n",` +
			`"tools":[{"type":"function","name":"x","connector_id":"c"}]}]}`, `tool field "connector_id"`, "", 0},
		{"namespace naming a server", `{"tools":[{"type":"namespace","name":"n","server_url":"https://x",` +
			`"tools":[]}]}`, `tool field "server_url"`, "", 0},
		{"nested namespace", `{"tools":[{"type":"namespace","name":"n",` +
			`"tools":[{"type":"namespace","name":"m","tools":[]}]}]}`, `server tool "namespace"`, "", 0},
		{"namespace without tools", `{"tools":[{"type":"namespace","name":"n"}]}`, "without a", "", 0},
		{"file_id in a namespace function's parameters", `{"model":"m","tools":[{"type":"namespace",` +
			`"name":"n","tools":[` + fn + `]}]}`, "", "m", 0},
		{"item_reference input", `{"model":"m","input":[{"type":"item_reference","id":"msg_1"}]}`,
			"item references", "", 0},
		{"nested item_reference", `{"input":[{"type":"function_call_output","call_id":"c",` +
			`"output":[{"type":"item_reference","id":"msg_1"}]}]}`, "item references", "", 0},
		{"item_reference in a function's parameters", `{"model":"m","tools":[{"type":"function","name":"x",` +
			`"parameters":{"type":"item_reference","properties":{}}}]}`, "", "m", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			insp, err := inspectOpenAIBody([]byte(tt.body), nil)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if insp.model != tt.model || insp.maxTokens != tt.maxTokens || insp.rewritten != nil ||
				insp.estimate != int64(len(tt.body))/4 {
				t.Fatalf("inspection = %+v, want model %q max %d", insp, tt.model, tt.maxTokens)
			}
		})
	}
}

func TestWriteOpenAIError(t *testing.T) {
	tests := []struct {
		status          int
		kind, msg       string
		wantType, wantC string
	}{
		{http.StatusTooManyRequests, "rate_limit_error", provider.LimitMessagePrefix + ": token limit (10) reached",
			"insufficient_quota", "insufficient_quota"},
		{http.StatusTooManyRequests, "rate_limit_error", "egress broker: token review throttled; retry",
			"rate_limit_error", "rate_limit_exceeded"},
		{http.StatusNotFound, "not_found_error", "egress broker: not a brokered endpoint",
			"not_found_error", "not_found_error"},
		{http.StatusUnauthorized, "authentication_error", "bad token", "authentication_error",
			"authentication_error"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		writeOpenAIError(rec, tt.status, tt.kind, tt.msg)
		var e struct {
			Error *struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Code    string  `json:"code"`
				Param   *string `json:"param"`
			} `json:"error"`
			Type *string `json:"type"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error == nil || e.Type != nil {
			t.Fatalf("%s: not the OpenAI envelope: %s", tt.msg, rec.Body.String())
		}
		if rec.Code != tt.status || rec.Header().Get("Content-Type") != "application/json" ||
			e.Error.Message != tt.msg || e.Error.Type != tt.wantType || e.Error.Code != tt.wantC {
			t.Errorf("%s: %d %+v", tt.msg, rec.Code, *e.Error)
		}
	}
}

func TestOpenAIUpstream(t *testing.T) {
	target := mustTarget(t, "https://api.openai.com")
	up := OpenAI(target, keyFile(t, "sk-proj-123"))
	if up.Target != target || up.CredentialKind != usagewire.CredentialAPIKey {
		t.Errorf("upstream = %+v", up)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err := up.Credential(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-proj-123" {
		t.Errorf("Authorization = %q, want the trimmed key as a bearer", got)
	}
	if err := up.Ready(context.Background()); err != nil {
		t.Errorf("Ready = %v", err)
	}
	for name, path := range map[string]string{
		"empty": keyFile(t, " "), "missing": filepath.Join(t.TempDir(), "absent"),
	} {
		up := OpenAI(target, path)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		if err := up.Credential(context.Background(), req); err == nil || req.Header.Get("Authorization") != "" {
			t.Errorf("%s key file: Credential = %v, Authorization %q", name, err, req.Header.Get("Authorization"))
		}
		if err := up.Ready(context.Background()); err == nil {
			t.Errorf("%s key file: Ready succeeded", name)
		}
	}
	// A person's credential goes out instead of the org key, never
	// replaced by it.
	ctx := withAttached(context.Background(), &attached{kind: v1alpha1.ModelCredentialAPIKey, credential: "x"})
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err := up.Credential(ctx, req); err != nil || req.Header.Get("Authorization") != "Bearer x" {
		t.Errorf("an attached credential: Credential = %v, Authorization %q", err, req.Header.Get("Authorization"))
	}
}

// openaiUpstream is a fake OpenAI API: it records the last request and
// answers /v1/responses with the stream body and /v1/models with a list.
type openaiUpstream struct {
	*httptest.Server
	hits   atomic.Int64
	header atomic.Pointer[http.Header]
	path   atomic.Pointer[string]
	body   string
}

func newOpenAIUpstream(t *testing.T, body string) *openaiUpstream {
	t.Helper()
	f := &openaiUpstream{body: body}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		h, p := r.Header.Clone(), r.URL.Path
		f.header.Store(&h)
		f.path.Store(&p)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-5.3-codex","object":"model"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(f.Close)
	return f
}

// openaiServer is a broker with only the openai route, on a fake upstream
// streaming body, holding each pod to tokensPerPod.
func openaiServer(t *testing.T, body string, tokensPerPod int64) (*Server, *syncBuffer, *openaiUpstream) {
	t.Helper()
	up := newOpenAIUpstream(t, body)
	s, buf := auditServer(t, Config{
		Limits:    Limits{TokensPerPod: tokensPerPod},
		Upstreams: map[string]Upstream{provider.OpenAI: OpenAI(mustTarget(t, up.URL), keyFile(t, "sk-org"))},
	})
	return s, buf, up
}

// openaiTerminalStream is a completed response whose terminal event is far
// over 64 KiB, reporting 12000 input tokens (8000 cached) and 900 output.
func openaiTerminalStream() string {
	return openaiStream(150<<10, "response.completed", openaiUsageObject(12000, 8000, 900))
}

// TestOpenAIRouteForwards drives the openai route against a fake upstream:
// the org key goes out as a bearer and nothing the caller set to select an
// account or a credential does, and a >64 KiB terminal event is measured
// into the ledger under the bare model id.
func TestOpenAIRouteForwards(t *testing.T) {
	stream := openaiTerminalStream()
	s, buf, up := openaiServer(t, stream, 0)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses",
		strings.NewReader(`{"model":"gpt-5.3-codex","stream":true,"input":"hi"}`))
	req.Header.Set(TokenHeader, tok("jobpod"))
	for _, name := range []string{"Authorization", "X-Api-Key", "OpenAI-Organization", "OpenAI-Project",
		"ChatGPT-Account-Id", "OpenAI-Beta"} {
		req.Header.Set(name, "caller-"+name)
	}
	req.Header.Set("Session_id", "keep-me")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != stream {
		t.Fatalf("status = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	got := *up.header.Load()
	if p := *up.path.Load(); p != "/v1/responses" {
		t.Errorf("upstream path = %q", p)
	}
	if v := got.Get("Authorization"); v != "Bearer sk-org" {
		t.Errorf("Authorization = %q, want the org key", v)
	}
	for _, name := range []string{TokenHeader, "X-Api-Key", "OpenAI-Organization", "OpenAI-Project",
		"ChatGPT-Account-Id", "OpenAI-Beta"} {
		if v := got.Get(name); v != "" {
			t.Errorf("%s forwarded upstream: %q", name, v)
		}
	}
	if got.Get("Session_id") != "keep-me" {
		t.Error("an ordinary header was not forwarded")
	}
	if line := lastAudit(t, buf); line["route"] != "openai" || line["tokens"] != float64(12900) ||
		line["estimated"] == true {
		t.Errorf("audit = %v", line)
	}
	pods := s.ledger.jobUsage(testJob)
	if len(pods) != 1 || len(pods[0].Models) != 1 {
		t.Fatalf("ledger = %+v", pods)
	}
	want := usagewire.Model{Model: "gpt-5.3-codex", Route: "openai", Credential: usagewire.CredentialAPIKey,
		Input: 4000, CacheRead: 8000, Output: 900}
	if m := pods[0].Models[0]; m != want {
		t.Errorf("ledger model = %+v, want %+v", m, want)
	}
}

// TestOpenAIRouteSurface: the listing passes, and everything else —
// compaction, stored responses, websocket upgrades, the other APIs — is an
// OpenAI-shaped 404 before the upstream.
func TestOpenAIRouteSurface(t *testing.T) {
	s, _, up := openaiServer(t, openaiTerminalStream(), 0)
	h := s.Handler()
	list := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	list.Header.Set(TokenHeader, tok("good"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, list)
	if rec.Code != http.StatusOK || up.hits.Load() != 1 {
		t.Fatalf("GET /v1/models = %d", rec.Code)
	}
	for _, c := range []struct {
		method, path string
		header       map[string]string
	}{
		{http.MethodPost, "/openai/v1/responses/compact", nil},
		{http.MethodGet, "/openai/v1/responses/resp_1", nil},
		{http.MethodDelete, "/openai/v1/responses/resp_1", nil},
		{http.MethodPost, "/openai/v1/responses/resp_1/cancel", nil},
		{http.MethodPost, "/openai/v1/responses/input_tokens", nil},
		{http.MethodPost, "/openai/v1/chat/completions", nil},
		{http.MethodPost, "/openai/v1/files", nil},
		{http.MethodGet, "/openai/v1/models/gpt-5.3-codex", nil},
		{http.MethodGet, "/openai/v1/responses", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}},
		{http.MethodPost, "/openai/v1/responses", map[string]string{"Upgrade": "websocket"}},
		{http.MethodPost, "/openai/v1/responses", map[string]string{"Connection": "keep-alive, Upgrade"}},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(`{"model":"gpt-5.3-codex"}`))
		r.Header.Set(TokenHeader, tok("good"))
		for k, v := range c.header {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var e struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); rec.Code != http.StatusNotFound || err != nil ||
			e.Error.Type != "not_found_error" {
			t.Errorf("%s %s %v = %d %s, want an OpenAI-shaped 404", c.method, c.path, c.header, rec.Code,
				rec.Body.String())
		}
	}
	if up.hits.Load() != 1 {
		t.Error("a refused request reached the upstream")
	}
}

// TestOpenAIRouteLimit: a pod that has spent 12900 of its 20000 tokens is
// refused a response that could exceed them, as insufficient_quota in
// OpenAI's envelope carrying the limit prefix, so codex does not retry it.
func TestOpenAIRouteLimit(t *testing.T) {
	s, _, _ := openaiServer(t, openaiTerminalStream(), 20000)
	if rec := post(s.Handler(), "/openai/v1/responses", `{"model":"gpt-5.3-codex"}`); rec.Code != http.StatusOK {
		t.Fatalf("first response = %d", rec.Code)
	}
	rec := post(s.Handler(), "/openai/v1/responses", `{"model":"gpt-5.3-codex","max_output_tokens":9000}`)
	var e struct {
		Error struct {
			Message, Type, Code string
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || rec.Code != http.StatusTooManyRequests ||
		e.Error.Code != "insufficient_quota" || e.Error.Type != "insufficient_quota" ||
		!strings.HasPrefix(e.Error.Message, provider.LimitMessagePrefix) {
		t.Errorf("over the limit = %d %s", rec.Code, rec.Body.String())
	}
}

// TestOpenAIRouteRefusals: body inspection runs on the openai route, its
// refusals and the max_output_tokens ceiling answer in OpenAI's envelope,
// and the model allowlist judges the body's model, a dated snapshot of an
// entry included.
func TestOpenAIRouteRefusals(t *testing.T) {
	up := newOpenAIUpstream(t, openaiStream(0, "response.completed", openaiUsageObject(1, 0, 1)))
	s := newTestServer(t, Config{
		Limits: Limits{MaxTokensCeiling: 8000, ModelAllowlist: []string{"openai/gpt-5.3-codex"}},
		Upstreams: map[string]Upstream{
			provider.OpenAI: OpenAI(mustTarget(t, up.URL), keyFile(t, "sk-org")),
		},
	}, nil)
	tests := []struct {
		body       string
		status     int
		typ, inMsg string
	}{
		{`{"model":"gpt-5.3-codex"}`, http.StatusOK, "", ""},
		{`{"model":"gpt-5.3-codex-2026-02-05"}`, http.StatusOK, "", ""},
		{`{"model":"gpt-5.6-sol"}`, http.StatusForbidden, "permission_error", "not allowlisted"},
		{`{"model":"gpt-5.3-codex","tools":[{"type":"web_search"}]}`, http.StatusForbidden, "permission_error",
			"server tool"},
		{`{"model":"gpt-5.3-codex","previous_response_id":"resp_1"}`, http.StatusForbidden, "permission_error",
			"previous_response_id"},
		{`{"model":"gpt-5.3-codex","input":[{"type":"item_reference","id":"msg_1"}]}`, http.StatusForbidden,
			"permission_error", "item references"},
		{`{"model":"gpt-5.3-codex","tools":[{"type":"shell","environment":{"type":"container_auto"}}]}`,
			http.StatusForbidden, "permission_error", "environment"},
		{`{"model":"gpt-5.3-codex","max_output_tokens":1.5}`, http.StatusBadRequest, "invalid_request_error",
			"max_output_tokens"},
		{`{"model":"gpt-5.3-codex","max_output_tokens":9000}`, http.StatusTooManyRequests, "insufficient_quota",
			"max_output_tokens 9000 exceeds the ceiling 8000"},
	}
	for _, tt := range tests {
		rec := post(s.Handler(), "/openai/v1/responses", tt.body)
		if rec.Code != tt.status {
			t.Errorf("%s: status = %d, want %d: %s", tt.body, rec.Code, tt.status, rec.Body.String())
			continue
		}
		if tt.status == http.StatusOK {
			continue
		}
		var e struct {
			Error struct{ Message, Type string } `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Type != tt.typ ||
			!strings.Contains(e.Error.Message, tt.inMsg) {
			t.Errorf("%s: %s", tt.body, rec.Body.String())
		}
	}
}

// TestOpenAIUpstreamErrorEnvelope: when the upstream cannot be reached, or
// the key file is gone, the openai route answers 502 in OpenAI's envelope
// ({"error":{message,type,code,param}}, no top-level "type"), not the
// Anthropic one the other routes use.
func TestOpenAIUpstreamErrorEnvelope(t *testing.T) {
	live := newOpenAIUpstream(t, openaiStream(0, "response.completed", openaiUsageObject(1, 0, 1)))
	missing := keyFile(t, "sk-org")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		upstream Upstream
	}{
		{"unreachable upstream", OpenAI(mustTarget(t, "http://127.0.0.1:1"), keyFile(t, "sk-org"))},
		{"missing key file", OpenAI(mustTarget(t, live.URL), missing)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, Config{Upstreams: map[string]Upstream{provider.OpenAI: tt.upstream}}, nil)
			rec := post(s.Handler(), "/openai/v1/responses", `{"model":"gpt-5.3-codex"}`)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
			}
			var e map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("502 body is not JSON: %s", rec.Body.String())
			}
			if _, ok := e["type"]; ok {
				t.Errorf("502 body has the Anthropic top-level type: %s", rec.Body.String())
			}
			var inner map[string]any
			if err := json.Unmarshal(e["error"], &inner); err != nil {
				t.Fatalf("502 body has no error object: %s", rec.Body.String())
			}
			for _, k := range []string{"message", "type", "code", "param"} {
				if _, ok := inner[k]; !ok {
					t.Errorf("error object lacks %q: %s", k, rec.Body.String())
				}
			}
			if inner["type"] != "api_error" || !strings.Contains(fmt.Sprint(inner["message"]), "unavailable") {
				t.Errorf("502 error = %v", inner)
			}
		})
	}
	if live.hits.Load() != 0 {
		t.Error("a request without a key reached the upstream")
	}
}

// TestOpenAICutStreamChargesOutputBound: a Responses stream cut before its
// terminal event has reported no usage at all, so it is charged its worst
// case: the request-size estimate plus max_output_tokens, else the
// ceiling, else the streamed bytes over four.
func TestOpenAICutStreamChargesOutputBound(t *testing.T) {
	stream := openaiStream(0, "", "")
	tests := []struct {
		name    string
		body    string
		ceiling int64
		bound   int64
	}{
		{"max_output_tokens", `{"model":"gpt-5.3-codex","max_output_tokens":4000}`, 0, 4000},
		{"ceiling", `{"model":"gpt-5.3-codex"}`, 6000, 6000},
		{"streamed bytes", `{"model":"gpt-5.3-codex"}`, 0, int64(len(stream)) / 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := newOpenAIUpstream(t, stream)
			s, buf := auditServer(t, Config{
				Limits:    Limits{MaxTokensCeiling: tt.ceiling},
				Upstreams: map[string]Upstream{provider.OpenAI: OpenAI(mustTarget(t, up.URL), keyFile(t, "k"))},
			})
			if rec := post(s.Handler(), "/openai/v1/responses", tt.body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if got, want := auditTokens(t, buf), int64(len(tt.body))/4+tt.bound; got != want {
				t.Fatalf("charged %d, want %d", got, want)
			}
			if lastAudit(t, buf)["estimated"] != true {
				t.Error("a cut stream is not marked estimated")
			}
		})
	}
}

// TestInspectOpenAICodexRequest runs a request the real codex CLI sent
// (rust-v0.153.4, the release Dockerfile.codex-agent-runner pins, pointed
// at a recording server through the brokered provider config; instructions
// and message texts elided) through the inspection: codex sends its
// sub-agent tools as a namespace, which the route must forward, or every
// brokered codex run would be refused at its first request.
func TestInspectOpenAICodexRequest(t *testing.T) {
	body, err := os.ReadFile("testdata/codex-0.153.4-responses-request.json")
	if err != nil {
		t.Fatal(err)
	}
	insp, err := inspectOpenAIBody(body, nil)
	if err != nil {
		t.Fatalf("a recorded codex request is refused: %v", err)
	}
	if insp.model != "gpt-5.3-codex" || insp.maxTokens != 0 {
		t.Fatalf("inspection = %+v", insp)
	}
}
