// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// usage is one response's token usage across every field the API reports.
// cacheCreation is every cache write; cacheWrite1h is the part of it written
// with the one-hour TTL (the cache_creation breakdown), the rest being the
// default five-minute writes, which price differently.
type usage struct {
	input         int64
	cacheCreation int64
	cacheWrite1h  int64
	cacheRead     int64
	output        int64
}

// cacheWrite5m is the five-minute part of the cache writes.
func (u usage) cacheWrite5m() int64 { return max(u.cacheCreation-u.cacheWrite1h, 0) }

// total is the sum every limit counts.
func (u usage) total() int64 { return u.input + u.cacheCreation + u.cacheRead + u.output }

// merge raises each field to the higher of the two: the API reports
// cumulative values (message_delta's output_tokens is the running total, not
// an increment), so the latest event is the truth and never lowers a count.
func (u usage) merge(o usage) usage {
	return usage{
		input:         max(u.input, o.input),
		cacheCreation: max(u.cacheCreation, o.cacheCreation),
		cacheWrite1h:  max(u.cacheWrite1h, o.cacheWrite1h),
		cacheRead:     max(u.cacheRead, o.cacheRead),
		output:        max(u.output, o.output),
	}
}

// minus is the per-field increment from o to u.
func (u usage) minus(o usage) usage {
	return usage{
		input:         u.input - o.input,
		cacheCreation: u.cacheCreation - o.cacheCreation,
		cacheWrite1h:  u.cacheWrite1h - o.cacheWrite1h,
		cacheRead:     u.cacheRead - o.cacheRead,
		output:        u.output - o.output,
	}
}

// usageJSON is the wire shape of a usage object; pointers distinguish an
// absent field from a zero. cache_creation is the per-TTL breakdown of
// cache_creation_input_tokens.
type usageJSON struct {
	Input         *int64 `json:"input_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	Output        *int64 `json:"output_tokens"`
	Breakdown     *struct {
		Ephemeral5m *int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h *int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (j *usageJSON) usage() usage {
	if j == nil {
		return usage{}
	}
	d := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	u := usage{input: d(j.Input), cacheCreation: d(j.CacheCreation), cacheRead: d(j.CacheRead), output: d(j.Output)}
	if b := j.Breakdown; b != nil {
		// The breakdown is the more detailed report: the total is at least
		// its sum, and the one-hour part is never more than the total.
		u.cacheWrite1h = max(d(b.Ephemeral1h), 0)
		u.cacheCreation = max(u.cacheCreation, max(d(b.Ephemeral5m), 0)+u.cacheWrite1h)
	}
	return u
}

// Scanner bounds on the Anthropic-shaped routes: an SSE data line longer
// than maxSSELine is skipped (usage events are a few hundred bytes), and a
// JSON body longer than maxJSONBody is abandoned (its usage then counts as
// unseen, so the estimate applies). The OpenAI route has its own
// (openai.go): its usage rides the terminal event, which carries the whole
// response.
const (
	maxSSELine  = 64 << 10
	maxJSONBody = 1 << 20
)

// usageDialect is how one route's responses report usage: the SSE events
// and the JSON body shape, and how large a line or body may grow before the
// scanner gives up on it.
type usageDialect interface {
	// event parses one SSE data payload (a JSON object): the cumulative
	// usage it reports (ok false when it reports none) and whether it ends
	// the response with its usage final.
	event(data []byte) (u usage, ok, final bool)
	// body parses a whole JSON response body: its usage, ok false when it
	// carries none.
	body(data []byte) (u usage, ok bool)
	// maxLine bounds one SSE line and maxBody one JSON body, in bytes.
	maxLine() int
	maxBody() int
}

// anthropicUsage is the Messages API's usage dialect: message_start carries
// the input side, message_delta the running output, message_stop ends it.
type anthropicUsage struct{}

func (anthropicUsage) maxLine() int { return maxSSELine }
func (anthropicUsage) maxBody() int { return maxJSONBody }

func (anthropicUsage) event(data []byte) (usage, bool, bool) {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage *usageJSON `json:"usage"`
		} `json:"message"`
		Usage *usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return usage{}, false, false
	}
	switch ev.Type {
	case "message_start":
		return ev.Message.Usage.usage(), true, false
	case "message_delta":
		return ev.Usage.usage(), true, false
	case "message_stop":
		return usage{}, false, true
	}
	return usage{}, false, false
}

func (anthropicUsage) body(data []byte) (usage, bool) {
	var resp struct {
		Usage *usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || resp.Usage == nil {
		return usage{}, false
	}
	return resp.Usage.usage(), true
}

type scanMode int

const (
	scanOpaque scanMode = iota // nothing parseable (a binary event stream)
	scanSSE                    // text/event-stream: the dialect's events
	scanJSON                   // application/json: a top-level usage object
)

// usageScanner reads token usage off a response as it streams through the
// proxy, in its route's dialect (nil is the Anthropic one). Every observed
// increment is reported at once, so a stream cut right after message_start
// has already charged its input tokens; complete reports whether the
// response reached its end with its usage final (message_stop, a terminal
// Responses event with usage, or a JSON body with a usage object), which is
// what decides whether the estimate applies.
type usageScanner struct {
	dialect  usageDialect
	mode     scanMode
	line     []byte // SSE: the partial current line
	skipping bool   // SSE: inside an over-long line
	body     []byte // JSON: the accumulated body
	over     bool   // JSON: body exceeded the dialect's maxBody
	seen     usage  // cumulative last-seen values
	complete bool
}

// wire is the scanner's dialect, the Anthropic one by default.
func (s *usageScanner) wire() usageDialect {
	if s.dialect == nil {
		return anthropicUsage{}
	}
	return s.dialect
}

// start picks the mode from the response content type.
func (s *usageScanner) start(contentType string) {
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		s.mode = scanSSE
	case strings.HasPrefix(ct, "application/json"):
		s.mode = scanJSON
	default:
		s.mode = scanOpaque
	}
}

// write consumes one chunk of the response and returns the usage increment
// it revealed.
func (s *usageScanner) write(p []byte) usage {
	switch s.mode {
	case scanSSE:
		return s.writeSSE(p)
	case scanJSON:
		if !s.over {
			if len(s.body)+len(p) > s.wire().maxBody() {
				s.over, s.body = true, nil
			} else {
				s.body = append(s.body, p...)
			}
		}
	}
	return usage{}
}

// writeSSE feeds bytes through the line splitter and parses each complete
// data line.
func (s *usageScanner) writeSSE(p []byte) usage {
	var delta usage
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			break
		}
		s.appendLine(p[:i])
		p = p[i+1:]
		if !s.skipping {
			delta = delta.mergeAdd(s.parseLine(s.line))
		}
		s.line, s.skipping = s.line[:0], false
	}
	return delta
}

// mergeAdd sums two increments (increments, unlike cumulative values, add).
func (u usage) mergeAdd(o usage) usage {
	return usage{
		input:         u.input + o.input,
		cacheCreation: u.cacheCreation + o.cacheCreation,
		cacheWrite1h:  u.cacheWrite1h + o.cacheWrite1h,
		cacheRead:     u.cacheRead + o.cacheRead,
		output:        u.output + o.output,
	}
}

// appendLine buffers part of a line, dropping the line once it exceeds the
// dialect's bound.
func (s *usageScanner) appendLine(p []byte) {
	if s.skipping {
		return
	}
	if len(s.line)+len(p) > s.wire().maxLine() {
		s.skipping, s.line = true, s.line[:0]
		return
	}
	s.line = append(s.line, p...)
}

// parseLine reads a "data: {...}" SSE line and returns the increment over
// what was seen so far.
func (s *usageScanner) parseLine(line []byte) usage {
	line = bytes.TrimRight(line, "\r")
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return usage{}
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return usage{}
	}
	u, ok, final := s.wire().event(data)
	if final {
		s.complete = true
	}
	if !ok {
		return usage{}
	}
	return s.observe(u)
}

// observe merges a cumulative usage report and returns the increment.
func (s *usageScanner) observe(u usage) usage {
	next := s.seen.merge(u)
	delta := next.minus(s.seen)
	s.seen = next
	return delta
}

// finish closes the scan at end of response and returns any final increment
// (a JSON body is parsed here, once whole).
func (s *usageScanner) finish() usage {
	if s.mode != scanJSON || s.over || len(s.body) == 0 {
		return usage{}
	}
	u, ok := s.wire().body(s.body)
	if !ok {
		return usage{}
	}
	s.complete = true
	return s.observe(u)
}

// usageWriter tees the response through a usageScanner. It sits between the
// SSE writer and the audit writer: Unwrap lets the proxy's ResponseController
// reach the real Flusher through it.
type usageWriter struct {
	http.ResponseWriter
	scan    *usageScanner
	onUsage func(usage)
	started bool
	status  int
	// bytes is what a 2xx response streamed through, for the output
	// estimate when neither max_tokens nor a ceiling bounds it.
	bytes int64
}

func (w *usageWriter) WriteHeader(code int) {
	if !w.started {
		w.started, w.status = true, code
		w.scan.start(w.Header().Get("Content-Type"))
		// An encoded body is not parseable text: fail closed to the
		// worst-case charge rather than scan compressed bytes. The broker
		// asks upstreams for identity, so this is a misbehaving upstream.
		if ce := w.Header().Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
			w.scan.mode = scanOpaque
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *usageWriter) Write(p []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if w.status/100 == 2 {
		w.bytes += int64(len(p))
		if d := w.scan.write(p); d.total() > 0 {
			w.onUsage(d)
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *usageWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
