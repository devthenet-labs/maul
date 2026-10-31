// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// Running a check (DESIGN 2.2 steps 5 and 8, 4.3 rule 3): alone, under a
// fresh run token the relay draws, re-run once under another after a
// non-pass, so a fail then a pass is quarantined (toolwire.DeriveResult).
// Every assertion is judged here, in the tools container: the sandbox can
// lie about what its program printed, never about what was asserted.

// reasonNoBrowser is maul's words for a browser call or check in a pod
// with no browser.
const reasonNoBrowser = "the browser is unavailable in this tool pod"

// runCheck runs c and returns its record; mode says why it ran.
func (r *Relay) runCheck(ctx context.Context, c checks.Check, mode toolwire.CheckRunMode) toolwire.CheckRunRecord {
	return r.runCheckAs(ctx, c, mode, false)
}

// runCheckAs is runCheck, patient when the run may show a defect (a red
// confirmation, the replay of a check saved red: third review, F6): a
// browser check is then compiled to wait playwright.PatientExpectTimeoutMS.
func (r *Relay) runCheckAs(ctx context.Context, c checks.Check, mode toolwire.CheckRunMode,
	patient bool) toolwire.CheckRunRecord {
	rec := toolwire.CheckRunRecord{Digest: bareDigestOf(checks.Digest(c)), CheckID: c.ID, Mode: mode}
	for range 2 {
		a := r.attempt(ctx, c, patient)
		rec.Attempts = append(rec.Attempts, a)
		if a.Result == toolwire.ResultPass {
			break
		}
	}
	rec.Result = toolwire.DeriveResult(rec.Attempts)
	return rec
}

// attempt runs c once under a fresh token, patient as runCheckAs says.
func (r *Relay) attempt(ctx context.Context, c checks.Check, patient bool) toolwire.Attempt {
	a := toolwire.Attempt{Run: r.token()}
	start := time.Now()
	switch c.Form {
	case checks.FormHTTP:
		a.Steps = r.httpAttempt(ctx, *c.HTTP, a.Run)
	case checks.FormExec:
		a.Steps = r.execAttempt(ctx, *c.Exec, a.Run)
	case checks.FormBrowser:
		a.Steps = r.browserAttempt(ctx, *c.Browser, a.Run, patient)
	default:
		a.Steps = []toolwire.StepRecord{{Index: 0, Result: toolwire.ResultError, Detail: reasonNoBrowser}}
	}
	a.DurationMs = time.Since(start).Milliseconds()
	a.Result = toolwire.ResultPass
	for _, s := range a.Steps {
		if s.Result != toolwire.ResultPass {
			a.Result = s.Result
		}
	}
	return a
}

// httpAttempt runs an http check's steps in order, stopping at the first
// that fails. A request that gets no response fails: the application did
// not answer.
func (r *Relay) httpAttempt(ctx context.Context, h checks.HTTPCheck, run string) []toolwire.StepRecord {
	comp, ok := r.cfg.component(h.Target)
	if !ok {
		return []toolwire.StepRecord{{Result: toolwire.ResultError, Detail: "no previewed component is named " + h.Target}}
	}
	steps := acceptance.Normalize([]acceptance.Criterion{{Kind: acceptance.KindHTTP, Steps: h.Steps}})[0].Steps
	caps := map[string]string{}
	// own are the captures of the check's own writes: values that name data
	// its run made (third review, F3).
	own := map[string]bool{}
	var out []toolwire.StepRecord
	for i, st := range steps {
		rec := toolwire.StepRecord{Index: i, Result: toolwire.ResultPass}
		if detail, assertion := r.httpStep(ctx, comp.Addr, st, run, caps, own); detail != "" {
			rec.Result, rec.Detail = toolwire.ResultFail, detail
			rec.Assertion, rec.AssertionID = assertion != "", assertion
		}
		out = append(out, rec)
		if rec.Result != toolwire.ResultPass {
			break
		}
		if writeMethod(st.Request.Method) {
			for name := range st.Capture {
				own[name] = true
			}
		}
	}
	return out
}

// writeMethod reports a request that writes: POST, PUT or PATCH.
func writeMethod(m string) bool {
	switch strings.ToUpper(m) {
	case "POST", "PUT", "PATCH":
		return true
	}
	return false
}

// runKeyed reports whether s names the run's own data: {{run}}, or a
// capture of one of the check's own writes.
func runKeyed(s string, own map[string]bool) bool {
	if strings.Contains(s, checks.RunPlaceholder) {
		return true
	}
	for name := range own {
		if strings.Contains(s, "{{capture."+name+"}}") {
			return true
		}
	}
	return false
}

// valueKeyed is runKeyed of an expected value's JSON.
func valueKeyed(v any, own map[string]bool) bool {
	b, err := json.Marshal(v)
	return err == nil && runKeyed(string(b), own)
}

// listShape reports a matcher over a list ($order, $includes): over shared
// rows, a list can differ for data the run did not make.
func listShape(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return true
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	_, order := m["$order"]
	_, includes := m["$includes"]
	return order || includes
}

// httpStep sends one step and judges it, returning maul's words for the
// first expectation it missed ("" when it passed), and, when that miss is
// an assertion, which one (toolwire.StepRecord.AssertionID; "" when it is
// not). Only content the application answered with is an assertion (second
// review, 1b): a JSON pointer that resolves to a different value ("json
// <pointer>"), a header present with a different value ("header <name>"),
// body text missing from a non-HTML answer ("bodyContains"). No status is,
// a 500 included (it can be the check's own malformed request), nor a
// pointer or header that resolves to nothing, a non-JSON body where JSON was
// expected, an unbuilt or unsent request, no response, a body over its
// bound or a capture with nothing to keep. Its captures go into caps. The
// words name what was expected, never the application's bytes.
func (r *Relay) httpStep(ctx context.Context, addr string, st acceptance.Step, run string,
	caps map[string]string, own map[string]bool) (string, string) {
	built, err := acceptance.BuildRequest(st, run, caps)
	if err != nil {
		return "the request could not be built: a capture is missing", ""
	}
	rep, err := r.client.send(ctx, outbound{Addr: addr, Method: built.Method, Target: built.Target(),
		Header: built.Headers, Body: built.Body, Host: r.cfg.Host, Forward: r.forward()})
	switch {
	case err != nil:
		return "the request could not be sent as built", ""
	case rep.Transport != acceptance.TransportNone:
		return "no response (" + string(rep.Transport) + ")", ""
	case rep.TooLarge:
		return "the response body is over " + strconv.Itoa(MaxResponseBytes) + " bytes", ""
	case !slices.Contains(st.Expect.Status, int32(rep.Status)):
		return fmt.Sprintf("status %d, expected one of %v", rep.Status, st.Expect.Status), ""
	}
	doc, valid := decodeJSON(rep.Body)
	if detail, assertion := judgeContent(st, rep, doc, valid, run, caps, own); detail != "" {
		return detail, assertion
	}
	for _, name := range sortedKeys(st.Capture) {
		v, ok := captureValue(doc, valid, st.Capture[name])
		if !ok {
			return "capture " + name + " is not a string, number or boolean at " + strconv.Quote(st.Capture[name]), ""
		}
		caps[name] = v
	}
	return "", ""
}

// judgeContent judges a response's content against a step's expect (its
// JSON pointers, headers and body text, the status already met): maul's
// words for the first miss and its assertion id, as httpStep returns them;
// "" when it all holds.
func judgeContent(st acceptance.Step, rep reply, doc any, valid bool, run string, caps map[string]string,
	own map[string]bool) (string, string) {
	// The step reads the run's own data when its path (never its query)
	// addresses it.
	pathKeyed := runKeyed(strings.SplitN(st.Request.Path, "?", 2)[0], own)
	for _, ptr := range sortedKeys(st.Expect.JSON) {
		raw, present := pointerValue(doc, valid, ptr)
		if want := st.Expect.JSON[ptr]; !checks.MatchJSON(want, run, caps, raw, present) {
			about := ptr != "" && !listShape(want) && (pathKeyed || valueKeyed(want, own))
			return "json " + strconv.Quote(ptr) + " did not match", contentAssertion(valid && present && about,
				"json "+ptr)
		}
	}
	for _, name := range sortedKeys(st.Expect.Headers) {
		raw, present := headerValue(rep.Header, name)
		if want := st.Expect.Headers[name]; !checks.MatchHeader(want, run, caps, raw, present) {
			return "header " + name + " did not match", contentAssertion(present && (pathKeyed || valueKeyed(want, own)),
				"header "+strings.ToLower(name))
		}
	}
	if st.Expect.BodyContains != "" {
		needle, err := acceptance.Substitute(st.Expect.BodyContains, run, caps)
		if err != nil {
			return "the expected text could not be built: a capture is missing", ""
		}
		if !bytes.Contains(rep.Body, []byte(needle)) {
			return "the body does not contain the expected text", contentAssertion(!htmlBody(rep.Header) &&
				(pathKeyed || runKeyed(st.Expect.BodyContains, own)), "bodyContains")
		}
	}
	return "", ""
}

// contentAssertion is id when a missed expectation is on content that was
// there (ok), else "": no assertion.
func contentAssertion(ok bool, id string) string {
	if !ok {
		return ""
	}
	return toolwireID(id)
}

// toolwireID bounds an assertion id to the ledger's bound, on a rune
// boundary, one line.
func toolwireID(id string) string {
	id = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(id, "\uFFFD"))
	if len(id) > toolwire.MaxAssertionIDBytes {
		id = strings.ToValidUTF8(id[:toolwire.MaxAssertionIDBytes], "")
	}
	return id
}

// htmlBody reports an HTML answer: a page (an SPA's fallback included), on
// which text missing is no assertion of an http check.
func htmlBody(h map[string][]string) bool {
	ct, _ := headerValue(h, "Content-Type")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/html")
}

// execAttempt runs an exec check once in the sandbox: argv and its files
// with the run token substituted, the exit code and stdout judged here.
func (r *Relay) execAttempt(ctx context.Context, e checks.ExecCheck, run string) []toolwire.StepRecord {
	errStep := func(detail string) []toolwire.StepRecord {
		return []toolwire.StepRecord{{Result: toolwire.ResultError, Detail: detail}}
	}
	if r.execer == nil {
		return errStep(reasonNoSandbox)
	}
	req, err := execRequest(e, run)
	if err != nil {
		return errStep("the program could not be built: " + err.Error())
	}
	if e.Timeout != "" {
		d, _ := time.ParseDuration(e.Timeout) // checks.Validate admitted it
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	res, err := r.execer.Exec(ctx, req)
	switch {
	case err != nil:
		return errStep("the sandbox did not answer")
	case res.Refused != "":
		return errStep("the sandbox refused the program")
	}
	// fail is an unmet expectation. It is an assertion only when the
	// program said it judged one of its own assertions, by id, and found the
	// outcome does not hold (checks.ExecAssertion): an exit code or missing
	// output alone is what a crashed or broken program shows too.
	marker, asserted := checks.ExecAssertion(res.Output)
	fail := func(detail string) []toolwire.StepRecord {
		rec := toolwire.StepRecord{Result: toolwire.ResultFail, Detail: detail, Assertion: asserted}
		if asserted {
			rec.AssertionID = "marker " + marker
		}
		return []toolwire.StepRecord{rec}
	}
	x := e.Expect
	switch {
	case res.TimedOut:
		return []toolwire.StepRecord{{Result: toolwire.ResultFail, Detail: "the program was killed at its time limit"}}
	case x.Exit != nil && int32(res.Exit) != *x.Exit:
		return fail(fmt.Sprintf("exit %d, expected %d", res.Exit, *x.Exit))
	}
	if x.StdoutContains != "" {
		needle, err := acceptance.Substitute(x.StdoutContains, run, nil)
		if err != nil || !strings.Contains(res.Output, needle) {
			return fail("the output does not contain the expected text")
		}
	}
	doc, valid := decodeJSON([]byte(res.Output))
	for _, ptr := range sortedKeys(x.StdoutJSON) {
		raw, present := pointerValue(doc, valid, ptr)
		if !checks.MatchJSON(x.StdoutJSON[ptr], run, nil, raw, present) {
			return fail("stdoutJSON " + strconv.Quote(ptr) + " did not match")
		}
	}
	return []toolwire.StepRecord{{Result: toolwire.ResultPass}}
}

// execRequest is the shim's request for e under run.
func execRequest(e checks.ExecCheck, run string) (*toolwire.ExecRequest, error) {
	req := &toolwire.ExecRequest{}
	for _, a := range e.Argv {
		s, err := acceptance.Substitute(a, run, nil)
		if err != nil {
			return nil, err
		}
		req.Argv = append(req.Argv, s)
	}
	for _, f := range e.Files {
		s, err := acceptance.Substitute(f.Content, run, nil)
		if err != nil {
			return nil, err
		}
		req.Files = append(req.Files, toolwire.File{Name: f.Path, Content: []byte(s)})
	}
	return req, req.Check()
}

// decodeJSON decodes a body, numbers kept as written.
func decodeJSON(b []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return nil, false
	}
	return v, true
}

// pointerValue is the canonical JSON at an RFC 6901 pointer, absent when
// the document is not JSON or has nothing there.
func pointerValue(doc any, valid bool, ptr string) (string, bool) {
	if !valid {
		return "", false
	}
	v, ok := resolvePointer(doc, ptr)
	if !ok {
		return "", false
	}
	val, err := acceptance.ValueOf(v)
	if err != nil {
		return acceptance.TooLarge, true
	}
	return val.String(), true
}

// captureValue is a capture's value: a string, number or boolean of at
// most acceptance.MaxCaptureBytes.
func captureValue(doc any, valid bool, ptr string) (string, bool) {
	if !valid {
		return "", false
	}
	v, ok := resolvePointer(doc, ptr)
	if !ok {
		return "", false
	}
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case json.Number:
		s = string(t)
	case bool:
		s = strconv.FormatBool(t)
	default:
		return "", false
	}
	return s, len(s) <= acceptance.MaxCaptureBytes
}

// resolvePointer evaluates a pointer acceptance.CheckPointer admits, as
// the tester's judge does: array indexes decimal without leading zeros.
func resolvePointer(doc any, p string) (any, bool) {
	if p == "" {
		return doc, true
	}
	cur := doc
	for _, tok := range strings.Split(p[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[tok]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			if tok == "" || (len(tok) > 1 && tok[0] == '0') {
				return nil, false
			}
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// headerValue is a response header's values joined by ", ", matched
// case-insensitively.
func headerValue(h map[string][]string, name string) (string, bool) {
	var vals []string
	for k, vs := range h {
		if strings.EqualFold(k, name) {
			vals = append(vals, vs...)
		}
	}
	if vals == nil {
		return "", false
	}
	return strings.ToValidUTF8(strings.Join(vals, ", "), "�"), true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
