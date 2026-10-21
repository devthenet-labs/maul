// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/provider"
	"github.com/devthenet-labs/maul/internal/usagewire"
)

// OpenAI is the OpenAI Responses route (codex broker design, group 1): the
// API key is read from a mounted Secret file per request, so a rotated
// Secret propagates without a restart, and sent as Authorization: Bearer.
// Only an API key: ChatGPT-plan sign-in and Codex access tokens are not
// brokered (owner decision D1).
//
// Under per-user credentials a request whose run is attributed to a person
// carries that person's OpenAI API key in its context (resolved by the
// server from the person's openai record), and the route sends it instead
// of the org key. A person's credential of any other kind is never sent:
// the request fails rather than going out with the org key.
func OpenAI(target *url.URL, keyFile string) Upstream {
	return Upstream{
		Target:         target,
		CredentialKind: usagewire.CredentialAPIKey,
		Credential: func(ctx context.Context, req *http.Request) error {
			if a := attachedFrom(ctx); a != nil {
				if a.kind != v1alpha1.ModelCredentialAPIKey {
					return fmt.Errorf("a %s is not an OpenAI credential", a.kind)
				}
				req.Header.Set("Authorization", "Bearer "+a.credential)
				return nil
			}
			key, err := readKeyFile(keyFile)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+key)
			return nil
		},
		Ready: func(context.Context) error {
			_, err := readKeyFile(keyFile)
			return err
		},
	}
}

// OpenAIProbe is the health probe of a person's OpenAI API key (design
// revision 1: health comes only from probes the broker issues itself): it
// lists one model (GET /v1/models), which costs nothing. A 2xx is Valid
// and a 401 Rejected; anything else (a 403, a 429, a 5xx, no answer) is
// not a verdict on the key. Only an api-key is ever probed.
func OpenAIProbe(target *url.URL, client *http.Client) ProbeFunc {
	return func(ctx context.Context, kind v1alpha1.ModelCredentialKind, credential string) ProbeResult {
		if kind != v1alpha1.ModelCredentialAPIKey {
			return ProbeInconclusive
		}
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.JoinPath("v1", "models").String(), nil)
		if err != nil {
			return ProbeInconclusive
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		resp, err := client.Do(req)
		if err != nil {
			return ProbeInconclusive
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode/100 == 2:
			return ProbeValid
		case resp.StatusCode == http.StatusUnauthorized:
			return ProbeRejected
		}
		return ProbeInconclusive
	}
}

// openaiSurface is the Responses surface: creating a response (metered,
// the model in the body) and the model listing. Everything else — the
// stored-response endpoints (/v1/responses/{id}, its input items and
// cancel), /v1/responses/compact, input-token counting, Files, Vector
// Stores, Conversations, Chat Completions — is not listed, so 404.
func openaiSurface() surface {
	return surface{
		{method: http.MethodPost, pattern: segs("/v1/responses"), metered: true, model: modelBody, body: true},
		{method: http.MethodGet, pattern: segs("/v1/models")},
	}
}

// The OpenAI route's scanner bounds. A Responses stream reports usage only
// in its terminal event, and that event carries the whole response object
// (every output item, reasoning included), so its data line is routinely
// far over the Anthropic routes' 64 KiB; a non-streamed body is the same
// object. Both are bounded at 8 MiB: a line or body past it is abandoned
// and the request takes the shortfall path, charged its worst case.
const (
	maxOpenAIEventLine = 8 << 20
	maxOpenAIJSONBody  = 8 << 20
)

// openaiTerminalEvents are the Responses stream events that end a response
// and carry its final usage in response.usage.
var openaiTerminalEvents = map[string]bool{
	"response.completed":  true,
	"response.incomplete": true,
	"response.failed":     true,
}

// openaiUsage is the Responses API's usage dialect.
type openaiUsage struct{}

func (openaiUsage) maxLine() int { return maxOpenAIEventLine }
func (openaiUsage) maxBody() int { return maxOpenAIJSONBody }

// event reads a terminal event's usage. Earlier events (response.created,
// response.in_progress, the deltas) report none. A terminal event without a
// usage object does not complete the scan: the response is then charged
// its worst case, as a cut stream is.
func (openaiUsage) event(data []byte) (usage, bool, bool) {
	var ev struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *openaiUsageJSON `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &ev); err != nil || !openaiTerminalEvents[ev.Type] {
		return usage{}, false, false
	}
	if ev.Response == nil || ev.Response.Usage == nil {
		return usage{}, false, false
	}
	return ev.Response.Usage.usage(), true, true
}

func (openaiUsage) body(data []byte) (usage, bool) {
	var resp struct {
		Usage *openaiUsageJSON `json:"usage"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || resp.Usage == nil {
		return usage{}, false
	}
	return resp.Usage.usage(), true
}

// openaiUsageJSON is the Responses usage object. output_tokens already
// includes reasoning tokens; input_tokens includes the cached ones.
type openaiUsageJSON struct {
	Input   *int64 `json:"input_tokens"`
	Details *struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	Output *int64 `json:"output_tokens"`
}

// usage maps a Responses usage object onto the ledger's fields: cache
// reads are the cached part of the input, fresh input the rest, and output
// as reported. OpenAI reports no cache writes apart from input, so there
// are none here: a write is fresh input (priced at the input rate), which
// keeps model.MeasuredCostUSD and the price table untouched. Negative
// figures count as zero and the cached part never exceeds the input, so
// input + cacheRead is always the reported input_tokens.
func (j *openaiUsageJSON) usage() usage {
	if j == nil {
		return usage{}
	}
	d := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return max(*p, 0)
	}
	in := d(j.Input)
	var cached int64
	if j.Details != nil {
		cached = min(d(j.Details.Cached), in)
	}
	return usage{input: in - cached, cacheRead: cached, output: d(j.Output)}
}

// openaiClientTools are the tool types the broker forwards: the ones the
// client runs itself (function and custom tools, codex's local_shell, and
// shell and apply_patch executed locally). It is an allowlist on purpose:
// every other type, the hosted tools that reach the internet, a vector
// store, a sandbox or an MCP server on the pod's behalf (web_search and its
// dated variants, file_search, code_interpreter, mcp, image_generation,
// computer_use) and any server tool OpenAI adds later, is refused, so a new
// hosted tool fails closed.
var openaiClientTools = map[string]bool{
	"function": true, "custom": true, "local_shell": true, "shell": true, "apply_patch": true,
}

// openaiServerToolFields mark a tool that names a remote server or a
// connector, or that runs in an OpenAI-hosted environment or container,
// whatever its type says.
var openaiServerToolFields = []string{"connector_id", "server_url", "environment", "container"}

// openaiNamespaceTool is the type of a tool that only groups other tools
// under one name: codex (0.153 onwards) sends its sub-agent tools
// (spawn_agent, send_input, ...) as a namespace of plain function tools,
// which codex itself runs. A namespace is forwarded only when every tool in
// it is one the broker would forward on its own, and it may not nest.
const openaiNamespaceTool = "namespace"

// checkOpenAITools refuses a tool the client does not run itself, one that
// names a connector, server or hosted environment, and a namespace holding
// either or holding another namespace. top is false inside a namespace.
func checkOpenAITools(tools []any, top bool) error {
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := tool["type"].(string)
		namespace := typ == openaiNamespaceTool && top
		if !namespace && !openaiClientTools[typ] {
			return fmt.Errorf("server tool %q is not brokered", clip(typ))
		}
		for _, f := range openaiServerToolFields {
			if _, ok := tool[f]; ok {
				return fmt.Errorf("tool field %q is not brokered", f)
			}
		}
		if namespace {
			inner, ok := tool["tools"].([]any)
			if !ok {
				return errors.New(`a namespace tool without a "tools" list is not brokered`)
			}
			if err := checkOpenAITools(inner, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// openaiDeniedTopLevel are request fields that bring server-side state into
// the call: a stored conversation, a previous response, a stored prompt.
// background (asynchronous, stored responses) is refused unless false.
var openaiDeniedTopLevel = []string{"previous_response_id", "conversation", "prompt"}

// inspectOpenAIBody is inspectBody's Responses counterpart: it refuses any
// tool the client does not run itself or that names a connector, server or
// hosted environment, server-side state (including an item_reference), a
// Files API reference, a file URL or an image the upstream would fetch, and
// reads the model and max_output_tokens. It never rewrites the body (there are no betas in it).
func inspectOpenAIBody(body []byte, _ []string) (inspection, error) {
	insp := inspection{estimate: int64(len(body)) / 4}
	if len(bytes.TrimSpace(body)) == 0 {
		return insp, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return insp, errors.New("request body is not a JSON object")
	}
	for _, key := range openaiDeniedTopLevel {
		if v, ok := top[key]; ok && v != nil {
			return insp, fmt.Errorf("request field %q is not brokered", key)
		}
	}
	if v, ok := top["background"]; ok && v != nil && v != false {
		return insp, errors.New(`request field "background" is not brokered`)
	}
	if tools, ok := top["tools"].([]any); ok {
		if err := checkOpenAITools(tools, true); err != nil {
			return insp, err
		}
	}
	if err := walkOpenAIReferences(top); err != nil {
		return insp, err
	}
	insp.model, _ = top["model"].(string)
	if raw, ok := top["max_output_tokens"]; ok && raw != nil {
		n, ok := plainInt(raw)
		if !ok {
			return insp, invalidRequestError{"max_output_tokens must be a non-negative integer"}
		}
		insp.maxTokens = n
	}
	return insp, nil
}

// walkOpenAIReferences refuses, at any depth outside caller-defined data, a
// Files API reference (file_id), a file the upstream would download
// (file_url), an image_url that is not an inline data: URI, and an
// item_reference input item, which loads an item stored by an earlier
// response under the org key (the stored state previous_response_id would
// reach, by another field). A function
// tool's parameters and a json_schema format's schema are the caller's own
// data, never dereferenced, so a property named file_id there is not a
// reference.
func walkOpenAIReferences(v any) error {
	switch x := v.(type) {
	case map[string]any:
		if typ, _ := x["type"].(string); typ == "item_reference" {
			return errors.New("item references are not brokered; send the item itself")
		}
		for _, key := range []string{"file_id", "file_url"} {
			if _, ok := x[key]; ok {
				return errFileReference
			}
		}
		if raw, ok := x["image_url"]; ok && raw != nil && !inlineImage(raw) {
			return errors.New("image URLs are not brokered; send the image inline as a data: URI")
		}
		for key, child := range x {
			if openaiCallerData(x, key) {
				continue
			}
			if err := walkOpenAIReferences(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range x {
			if err := walkOpenAIReferences(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// inlineImage reports whether an image_url value is a data: URI, as a
// string or as an object's url.
func inlineImage(v any) bool {
	if obj, ok := v.(map[string]any); ok {
		v = obj["url"]
	}
	s, ok := v.(string)
	return ok && strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "data:")
}

// openaiCallerData reports whether obj[key] is caller-defined data: a
// function tool's parameters (input_schema's counterpart) or a json_schema
// text format's schema.
func openaiCallerData(obj map[string]any, key string) bool {
	typ, _ := obj["type"].(string)
	switch key {
	case "parameters":
		return typ == "function"
	case "schema":
		return typ == "json_schema"
	}
	return false
}

// writeOpenAIError emits the OpenAI error envelope, which is what codex
// knows how to surface. A broker spend-limit refusal (its message opens
// with provider.LimitMessagePrefix) carries type and code
// insufficient_quota, which the client treats as final rather than
// retrying; any other 429 is rate_limit_exceeded, which it retries.
func writeOpenAIError(w http.ResponseWriter, status int, kind, message string) {
	typ, code := kind, kind
	switch {
	case status == http.StatusTooManyRequests && strings.HasPrefix(message, provider.LimitMessagePrefix):
		typ, code = "insufficient_quota", "insufficient_quota"
	case kind == "rate_limit_error":
		code = "rate_limit_exceeded"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": typ, "code": code, "param": nil},
	})
}
