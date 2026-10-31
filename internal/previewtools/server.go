// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/previewauth"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// Port is the tools container's port (DESIGN 2.1).
const Port = 8090

// listenAddr is the released relay's listen address: Port on every
// address, the guard refusing local callers.
const listenAddr = ":8090"

// ReadyPath answers 200 once the replay is recorded and the relay serves
// the agent: the container's readiness probe, and so status.tools.ready.
const ReadyPath = "/readyz"

// Options configure a Relay.
type Options struct {
	Config Config
	// Dir is the mounted tool ConfigMap, read for logs and events.
	Dir string
	// Out receives the ledger: the container's stdout.
	Out io.Writer
	// Client sends requests to the components.
	Client *Client
	// Exec runs programs in the sandbox; nil when the pod has none.
	Exec Execer
	// Artifacts holds the bytes of recorded artifacts; nil has none. The
	// browser relay keeps its artifacts in it when it is an ArtifactKeeper.
	Artifacts ArtifactStore
	// Browser is the browser relay (browser.go); nil when the pod has no
	// browser, which answers every browser call "unavailable".
	Browser *BrowserOptions
	// Token draws a fresh run token; nil draws 16 random hex characters.
	Token func() string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// TerminationPath receives the summary at finish; empty writes none.
	TerminationPath string
}

// Relay is the tools container's relay: the agent's tool endpoints, the
// replay and confirmation runs, and the ledger they all write. One call
// runs at a time, so the ledger's order is the calls' order.
type Relay struct {
	cfg      Config
	dir      string
	client   *Client
	execer   Execer
	store    ArtifactStore
	br       *BrowserOptions
	token    func() string
	now      func() time.Time
	termPath string

	mu        sync.Mutex
	ledger    *toolwire.Writer
	entries   int
	calls     int // the agent's calls, refused ones included
	artifacts map[int64]map[string]bool
	dryRuns   map[string]dryRun // by bare digest
	replays   []toolwire.CheckRunRecord
	dryOrder  []string
	finished  bool
	reopened  bool // the browser session was reopened once
	summary   toolwire.Summary
	done      chan struct{}
}

// dryRun is the agent's dry runs of one check.
type dryRun struct {
	check checks.Check
	green bool
	// red: some dry run was red by assertion (AssertionStep).
	red    bool
	latest toolwire.CheckRunRecord
}

// NewRelay builds a Relay writing its ledger to o.Out.
func NewRelay(o Options) (*Relay, error) {
	if err := o.Config.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if o.Out == nil || o.Client == nil {
		return nil, errors.New("previewtools: a relay needs an output and a client")
	}
	r := &Relay{
		cfg: o.Config, dir: o.Dir, client: o.Client, execer: o.Exec, store: o.Artifacts, br: o.Browser, token: o.Token,
		now: o.Now, termPath: o.TerminationPath, ledger: toolwire.NewWriter(o.Out),
		artifacts: map[int64]map[string]bool{}, dryRuns: map[string]dryRun{}, done: make(chan struct{}),
	}
	if r.token == nil {
		r.token = randomToken
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r, nil
}

// randomToken is 16 random lowercase hex characters (crypto/rand).
func randomToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// Done is closed once the ledger is closed.
func (r *Relay) Done() <-chan struct{} { return r.done }

// result is one call's outcome before it is recorded.
type result struct {
	refused, reason string
	output, inline  string
	exit            int
	locators        []string
	observed        *toolwire.Observed
	artifacts       []toolwire.Artifact
	checkRun        *toolwire.CheckRunRecord
}

func refusal(code, reason string) result { return result{refused: code, reason: reason} }

// endpoint is one tool endpoint: its verb, who calls it, a fresh request
// and the call itself, run under r.mu.
type endpoint struct {
	verb    toolwire.Verb
	kind    toolwire.Kind
	request func() toolwire.Request
	call    callFunc
}

// callFunc runs one call of an endpoint.
type callFunc func(ctx context.Context, r *Relay, req toolwire.Request) toolwire.Response

// agentCall adapts a verb answered by one result into an endpoint call.
func agentCall[T toolwire.Request](verb toolwire.Verb,
	f func(ctx context.Context, r *Relay, req T) result) callFunc {
	return func(ctx context.Context, r *Relay, req toolwire.Request) toolwire.Response {
		return r.recordResult(toolwire.KindAgent, verb, req, f(ctx, r, req.(T)))
	}
}

var endpoints = []endpoint{
	{toolwire.VerbHTTP, toolwire.KindAgent, func() toolwire.Request { return &toolwire.HTTPRequest{} },
		agentCall(toolwire.VerbHTTP, func(ctx context.Context, r *Relay, req *toolwire.HTTPRequest) result {
			return r.http(ctx, req)
		})},
	{toolwire.VerbBrowser, toolwire.KindAgent, func() toolwire.Request { return &toolwire.BrowserRequest{} },
		agentCall(toolwire.VerbBrowser, func(ctx context.Context, r *Relay, req *toolwire.BrowserRequest) result {
			return r.browser(ctx, req)
		})},
	{toolwire.VerbExec, toolwire.KindAgent, func() toolwire.Request { return &toolwire.ExecRequest{} },
		agentCall(toolwire.VerbExec, func(ctx context.Context, r *Relay, req *toolwire.ExecRequest) result {
			return r.exec(ctx, req)
		})},
	{toolwire.VerbLogs, toolwire.KindAgent, func() toolwire.Request { return &toolwire.LogsRequest{} },
		agentCall(toolwire.VerbLogs, func(_ context.Context, r *Relay, req *toolwire.LogsRequest) result {
			return r.logs(req)
		})},
	{toolwire.VerbEvents, toolwire.KindAgent, func() toolwire.Request { return &toolwire.EventsRequest{} },
		agentCall(toolwire.VerbEvents, func(_ context.Context, r *Relay, _ *toolwire.EventsRequest) result {
			return r.events()
		})},
	{toolwire.VerbCheckRun, toolwire.KindAgent, func() toolwire.Request { return &toolwire.CheckRunRequest{} },
		agentCall(toolwire.VerbCheckRun, func(ctx context.Context, r *Relay, req *toolwire.CheckRunRequest) result {
			return r.checkRun(ctx, req)
		})},
	{toolwire.VerbCheckResults, toolwire.KindAgent, func() toolwire.Request { return &toolwire.CheckResultsRequest{} },
		func(_ context.Context, r *Relay, req toolwire.Request) toolwire.Response { return r.checkResults(req) }},
	{toolwire.VerbConfirm, toolwire.KindRunner, func() toolwire.Request { return &toolwire.ConfirmRequest{} },
		func(ctx context.Context, r *Relay, req toolwire.Request) toolwire.Response {
			return r.confirm(ctx, req.(*toolwire.ConfirmRequest))
		}},
	{toolwire.VerbFinish, toolwire.KindRunner, func() toolwire.Request { return &toolwire.FinishRequest{} },
		func(_ context.Context, r *Relay, req toolwire.Request) toolwire.Response {
			return r.finish(toolwire.KindRunner, req, req.(*toolwire.FinishRequest).Evidence)
		}},
}

// Handler serves the tool endpoints (POST /v1/<verb>) and ReadyPath.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ReadyPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/artifacts/{digest}", r.serveArtifact)
	for _, ep := range endpoints {
		mux.HandleFunc(toolwire.Path(ep.verb), func(w http.ResponseWriter, req *http.Request) { r.serve(w, req, ep) })
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(w, http.StatusNotFound, toolwire.Response{Refused: toolwire.RefusedVerb,
			Reason: "no such tool"})
	})
	return mux
}

func (r *Relay) serve(w http.ResponseWriter, hreq *http.Request, ep endpoint) {
	if hreq.Method != http.MethodPost {
		writeResponse(w, http.StatusMethodNotAllowed, toolwire.Response{Refused: toolwire.RefusedInvalid,
			Reason: "tools are called with POST"})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(hreq.Body, toolwire.MaxRequestBytes+1))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		writeResponse(w, http.StatusOK, toolwire.Response{Refused: toolwire.RefusedFinished,
			Reason: "the evaluation has finished"})
		return
	}
	if ep.kind == toolwire.KindAgent {
		r.calls++
		if r.calls > r.cfg.MaxToolCalls {
			resp := r.recordResult(ep.kind, ep.verb, rawArgs(body), refusal(toolwire.RefusedToolCalls,
				fmt.Sprintf("the evaluation's %d tool calls are spent", r.cfg.MaxToolCalls)))
			writeResponse(w, http.StatusOK, resp)
			return
		}
	}
	req := ep.request()
	if err := toolwire.DecodeRequest(bytes.NewReader(body), req); err != nil {
		resp := r.recordResult(ep.kind, ep.verb, rawArgs(body), refusal(toolwire.RefusedInvalid,
			"the request is not one of this tool's: "+strings.TrimPrefix(err.Error(), toolwire.ErrRequest.Error()+": ")))
		writeResponse(w, http.StatusOK, resp)
		return
	}
	writeResponse(w, http.StatusOK, ep.call(hreq.Context(), r, req))
}

// rawArgs is a body as an entry's args: itself when it is bounded JSON,
// else an empty object (the refusal's reason says why).
func rawArgs(body []byte) json.RawMessage {
	if len(body) > 0 && len(body) <= toolwire.MaxArgsBytes && json.Valid(body) {
		return body
	}
	return json.RawMessage(`{}`)
}

// recordResult records one call's result and answers it. A call the
// ledger has no room for is answered refused, unrecorded (Seq 0).
func (r *Relay) recordResult(kind toolwire.Kind, verb toolwire.Verb, args any, res result) toolwire.Response {
	raw, ok := args.(json.RawMessage)
	if !ok {
		b, err := json.Marshal(args)
		if err != nil {
			b = []byte(`{}`)
		}
		raw = b
	}
	e := toolwire.LedgerEntry{Kind: kind, Verb: verb, Args: raw, Exit: res.exit, Refused: res.refused,
		Output: res.output, Inline: res.inline, Artifacts: res.artifacts, Locators: res.locators,
		CheckRun: res.checkRun}
	seq, err := r.record(e)
	if err != nil {
		reason := "the ledger has no room for this call"
		if errors.Is(err, errNoRoom) {
			reason = errNoRoom.Error()
		}
		return toolwire.Response{Refused: toolwire.RefusedToolCalls, Reason: reason}
	}
	resp := toolwire.Response{Seq: seq, Refused: res.refused, Reason: res.reason, Observed: res.observed,
		Artifacts: res.artifacts}
	if res.refused == "" {
		resp.Untrusted = boundInline(res.inline)
	}
	return resp
}

// boundInline is what the agent sees inline, as the ledger records it.
func boundInline(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= toolwire.MaxInlineBytes {
		return s
	}
	cut := toolwire.MaxInlineBytes - len(toolwire.TruncationMarker)
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + toolwire.TruncationMarker
}

func writeResponse(w http.ResponseWriter, status int, resp toolwire.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}

// forward is the gateway header set the relay sends.
func (r *Relay) forward() previewauth.ForwardedFor {
	return previewauth.ForwardedFor{Viewer: r.cfg.Viewer, Host: r.cfg.Host}
}

// http answers `maul-tool http`: the path is routed to the component that
// serves it on the preview host, as the gateway routes it.
func (r *Relay) http(ctx context.Context, req *toolwire.HTTPRequest) result {
	path, _, _ := strings.Cut(req.Path, "?")
	i := maulv1.PreviewRoute(r.cfg.paths(), path)
	if i < 0 {
		return refusal(toolwire.RefusedNavigation, "no previewed component serves "+path)
	}
	o := outbound{Addr: r.cfg.Components[i].Addr, Method: req.Method, Target: req.Path, Host: r.cfg.Host,
		Forward: r.forward()}
	if req.Body != "" {
		o.Body = []byte(req.Body)
		o.Header = map[string]string{"Content-Type": "application/json"}
	}
	rep, err := r.client.send(ctx, o)
	if err != nil {
		return refusal(toolwire.RefusedInvalid, "the path is not one the relay sends as given")
	}
	obs := &toolwire.Observed{Status: rep.Status, LatencyMs: rep.LatencyMs}
	if rep.Transport != "" && rep.Transport != "none" {
		return result{observed: obs, reason: "no response (" + string(rep.Transport) + ")"}
	}
	text := exchange(rep)
	res := result{output: text, inline: text, observed: obs}
	if rep.TooLarge {
		res.reason = "the response body was cut at its bound"
	}
	return res
}

// exchange renders a response as the ledger keeps it: the status line,
// the headers in order, a blank line and the body.
func exchange(rep reply) string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d\n", rep.Status)
	names := make([]string, 0, len(rep.Header))
	for k := range rep.Header {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		for _, v := range rep.Header[k] {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	b.WriteString("\n")
	b.Write(rep.Body)
	return b.String()
}

// checkRun answers `maul-tool check run`: validate, then a dry run under
// a fresh token, recorded against the check's digest.
func (r *Relay) checkRun(ctx context.Context, req *toolwire.CheckRunRequest) result {
	c, err := checks.Parse(req.Spec)
	if err == nil {
		err = checks.Validate(c, checks.Preview{Paths: r.cfg.paths(), Components: r.cfg.keys()})
	}
	if err != nil {
		return refusal(toolwire.RefusedCheck, err.Error())
	}
	if c.Form == checks.FormBrowser && (r.br == nil || r.br.Runner == nil) {
		return refusal(toolwire.RefusedUnavailable, reasonNoBrowser)
	}
	if c.Form == checks.FormExec && r.execer == nil {
		return refusal(toolwire.RefusedUnavailable, reasonNoSandbox)
	}
	rec := r.runCheck(ctx, c, toolwire.ModeDryRun)
	d := r.dryRuns[rec.Digest]
	if _, seen := r.dryRuns[rec.Digest]; !seen {
		r.dryOrder = append(r.dryOrder, rec.Digest)
	}
	d.check, d.latest = c, rec
	d.green = d.green || rec.Result == toolwire.RunPass
	if _, ok := rec.AssertionStep(); ok {
		d.red = true
	}
	r.dryRuns[rec.Digest] = d
	return result{checkRun: &rec, observed: &toolwire.Observed{CheckRun: &rec}}
}

// checkResults answers `maul-tool check results`: the replay, then each
// dry-run check's latest run.
func (r *Relay) checkResults(req toolwire.Request) toolwire.Response {
	runs := slices.Clone(r.replays)
	for _, d := range r.dryOrder {
		runs = append(runs, r.dryRuns[d].latest)
	}
	resp := r.recordResult(toolwire.KindAgent, toolwire.VerbCheckResults, req, result{})
	if resp.Refused == "" {
		resp.Observed = &toolwire.Observed{CheckRuns: runs}
	}
	return resp
}

// confirm answers the runner's confirm: each named check run alone under
// fresh tokens, each run a relay entry. A check expected to pass must have a
// green dry run; one expected to fail (a check showing a defect) a dry run
// red by assertion and none green, and its entry records the expectation.
func (r *Relay) confirm(ctx context.Context, req *toolwire.ConfirmRequest) toolwire.Response {
	for _, d := range req.Digests {
		dr := r.dryRuns[d]
		switch red := slices.Contains(req.ExpectFail, d); {
		case red && (!dr.red || dr.green || r.replayed(d, toolwire.RunPass)):
			return r.recordResult(toolwire.KindRunner, toolwire.VerbConfirm, req, refusal(toolwire.RefusedNoRedDryRun,
				"check "+d+" has no dry run in this evaluation that failed an assertion, or one ran green, or it "+
					"replayed green this round"))
		case red && !checks.WritesRunData(dr.check):
			return r.recordResult(toolwire.KindRunner, toolwire.VerbConfirm, req, refusal(toolwire.RefusedNoRedDryRun,
				"check "+d+" writes no data of its own run: a check that shows a defect names {{run}} in a "+
					"POST, PUT or PATCH, a browser fill or its program"))
		case !red && (!dr.green || r.replayed(d, toolwire.RunFail)):
			return r.recordResult(toolwire.KindRunner, toolwire.VerbConfirm, req, refusal(toolwire.RefusedNoDryRun,
				"check "+d+" has no green dry run in this evaluation, or it replayed red this round"))
		}
	}
	resp := r.recordResult(toolwire.KindRunner, toolwire.VerbConfirm, req, result{})
	if resp.Refused != "" {
		return resp
	}
	var runs []toolwire.CheckRunRecord
	for _, d := range req.Digests {
		rec := r.runCheck(ctx, r.dryRuns[d].check, toolwire.ModeConfirm)
		args := toolwire.ConfirmArgs{Digest: d}
		if slices.Contains(req.ExpectFail, d) {
			args.Expect = toolwire.ExpectFail
		}
		if _, err := r.record(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbConfirm,
			Args: mustArgs(args), CheckRun: &rec}); err != nil {
			return toolwire.Response{Seq: resp.Seq, Refused: toolwire.RefusedToolCalls,
				Reason: "the ledger has no room for the confirmation runs"}
		}
		runs = append(runs, rec)
	}
	resp.Observed = &toolwire.Observed{CheckRuns: runs}
	return resp
}

// replayed reports a replay of check d this round that ended result: a
// saved check that replayed green is never confirmed red, nor one that
// replayed red confirmed green (review finding 3).
func (r *Relay) replayed(d string, result toolwire.RunResult) bool {
	return slices.ContainsFunc(r.replays, func(rec toolwire.CheckRunRecord) bool {
		return rec.Digest == d && rec.Result == result
	})
}

// Finish reasons, the args of the relay's own finish entry.
const (
	FinishDeadline    = "deadline"
	FinishReplayOnly  = "replay-only"
	FinishUnreachable = "components-unreachable"
	FinishStopped     = "stopped"
)

// finish records the finish entry, emits the cited evidence, closes the
// ledger and writes the termination message. kind is the runner's or the
// relay's own; the caller holds r.mu.
func (r *Relay) finish(kind toolwire.Kind, args any, refs []toolwire.EvidenceRef) toolwire.Response {
	evidence, notes := r.gatherEvidence(refs)
	out := strings.Join(notes, "\n")
	seq, err := r.record(toolwire.LedgerEntry{Kind: kind, Verb: toolwire.VerbFinish, Args: mustArgs(args),
		Output: out})
	if err != nil {
		// No room for the entry: the summary still closes what is there.
		seq = 0
	}
	sum, err := r.closeLedger(evidence)
	r.finished = true
	r.summary = sum
	close(r.done)
	if err != nil {
		return toolwire.Response{Seq: seq, Refused: toolwire.RefusedFinished, Reason: "the ledger could not close"}
	}
	return toolwire.Response{Seq: seq, Reason: out, Observed: &toolwire.Observed{Summary: &sum}}
}

// finishRelay finishes on the relay's own account (a deadline, a replay
// round, an unreachable component), once.
func (r *Relay) finishRelay(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.finish(toolwire.KindRelay, map[string]string{"reason": reason}, nil)
}

func mustArgs(v any) json.RawMessage {
	if raw, ok := v.(json.RawMessage); ok {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// GuardListener refuses every connection from a loopback address or from
// one of own (the pod's own addresses), closing it at accept: page JS in
// the browser and code in the sandbox share the pod's network, and must
// never reach the relay (DESIGN 4.2).
func GuardListener(l net.Listener, own []net.IP) net.Listener {
	return &guardListener{Listener: l, own: own}
}

type guardListener struct {
	net.Listener
	own []net.IP
}

func (g *guardListener) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !g.local(c.RemoteAddr()) {
			return c, nil
		}
		_ = c.Close()
	}
}

func (g *guardListener) local(a net.Addr) bool {
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	return slices.ContainsFunc(g.own, ip.Equal)
}

// OwnAddresses are this host's interface addresses.
func OwnAddresses() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out, nil
}
