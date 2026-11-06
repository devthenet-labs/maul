// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// The agents run (`--as agents`, run.sh agents; SPEC S2-21): the
// agents-namespace tool pod's rows, driven from a Pod labelled as an
// evaluator is, against a LIVE agents verification's tool pod. It holds that
// evaluation's :8090 key (the tool Job's Secret, mounted by the kubelet:
// run.sh never reads it) and:
//
//  1. reaches the tool pod's :8090 (the control);
//  2. makes one files call with the key (it must be served) and the same
//     call with no key, a well-formed key of no evaluation's and a malformed
//     one (each must be refused): "an evaluator with another PV's key";
//  3. with --other-tool-pod, makes the keyed call at another evaluation's
//     tool pod, which must refuse this evaluation's key;
//  4. sends the sandbox program (sandboxprog/probe.go, embedded) to the exec
//     verb as PR code would run: `go run probe.go --as sandbox-head` at the
//     head, `--as sandbox-base` at the base, the head again, then `--as
//     tools-agents` at the head for the tool pod's network. Each run plants
//     under its own nonce and looks for the other side's last one, so the
//     base run sees what the first head run left and the second head run
//     what the base left.
//
// Every row prints one JSON line, the program's rows prefixed with their
// step; the summary is PASS, SECURITY_FAILURE, INCONCLUSIVE or NOT_REACHED.
// run.sh then holds the tools container's log and termination message to
// the ledger (agentscheck), which the forged lines must not have reached.
const agentsRole = "agents"

// agentsProgram is the sandbox program, sent as the exec's one file.
//
//go:embed sandboxprog/probe.go
var agentsProgram string

const agentsProgramFile = "probe.go"

// The relay's contract (internal/toolwire): its port, the key header and
// the refusal a busy relay answers.
const (
	agentsToolsPort   = "8090"
	agentsKeyHeader   = "X-Maul-Tools-Key"
	agentsUnavailable = "unavailable"
)

// The leftover listeners' ports, one per side.
const (
	agentsHeadPort = "47801"
	agentsBasePort = "47802"
)

// Row outcomes beyond the egress run's: a wall that held, a denied target
// that answered, a program that found a wall broken.
const (
	agentsHeld      = "held"
	agentsBlocked   = "blocked"
	agentsReachable = "REACHABLE"
	agentsBroken    = "BROKEN"
)

type agentsConfig struct {
	// tool and other are the tool pod's and another evaluation's tool
	// pod's <ip>:8090 (other optional).
	tool, other string
	key, nonce  string
	// goCmd is the go command on the sandbox image's PATH.
	goCmd string
	// reach and deny are the tool pod's network rows, name=<ipv4>:<port>.
	reach, deny string
	refusals    bool
	// reachWithin bounds the wait for the tool pod, retryWithin the wait
	// on a busy relay.
	reachWithin, retryWithin time.Duration
}

var (
	agentsNonce = regexp.MustCompile(`^[0-9a-f]{16,62}$`)
	agentsGo    = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
)

// agentsConfigFrom reads the run's environment; false on anything missing
// or malformed, so the run is inconclusive, never a smaller one.
func agentsConfigFrom(getenv func(string) string, refusals bool) (agentsConfig, bool) {
	cfg := agentsConfig{nonce: getenv("PROBE_NONCE"), goCmd: getenv("PROBE_GO"), reach: getenv("PROBE_REACH"),
		deny: getenv("PROBE_DENY"), refusals: refusals, reachWithin: evaluatorReachWithin,
		retryWithin: 2 * time.Minute}
	if cfg.goCmd == "" {
		cfg.goCmd = "go"
	}
	ipv4 := func(s string) (string, bool) {
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() == nil {
			return "", false
		}
		return net.JoinHostPort(ip.String(), agentsToolsPort), true
	}
	var ok bool
	if cfg.tool, ok = ipv4(getenv("PROBE_TOOL_POD")); !ok {
		return cfg, false
	}
	if o := getenv("PROBE_OTHER_TOOL_POD"); o != "" {
		if cfg.other, ok = ipv4(o); !ok {
			return cfg, false
		}
	}
	raw, err := os.ReadFile(getenv("PROBE_TOOLS_KEY_FILE"))
	cfg.key = strings.TrimSpace(string(raw))
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(cfg.key) {
		return cfg, false
	}
	reach, ok1 := addrTargets(cfg.reach)
	deny, ok2 := addrTargets(cfg.deny)
	if !agentsNonce.MatchString(cfg.nonce) || !agentsGo.MatchString(cfg.goCmd) || !ok1 || !ok2 ||
		len(reach) == 0 || len(deny) == 0 {
		return cfg, false
	}
	return cfg, true
}

// relayResponse is the part of a toolwire.Response the run reads.
type relayResponse struct {
	Refused   string `json:"refused"`
	Reason    string `json:"reason"`
	Untrusted string `json:"untrusted"`
	Observed  *struct {
		Exit *int `json:"exit"`
	} `json:"observed"`
}

var agentsClient = &http.Client{
	Timeout:       3 * time.Minute,
	Transport:     &http.Transport{DisableKeepAlives: true, Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// relayCall POSTs body to verb at addr with key (none when empty).
func relayCall(addr, verb, key string, body any) (int, relayResponse, error) {
	var resp relayResponse
	b, err := json.Marshal(body)
	if err != nil {
		return 0, resp, err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/v1/"+verb,
		bytes.NewReader(b))
	if err != nil {
		return 0, resp, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(agentsKeyHeader, key)
	}
	r, err := agentsClient.Do(req)
	if err != nil {
		return 0, resp, err
	}
	defer func() { _ = r.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &resp)
	}
	return r.StatusCode, resp, nil
}

// callPatiently retries a call the relay answers "unavailable" (one call at
// a time; the evaluator may be mid-call) until within passes.
func callPatiently(addr, verb, key string, body any, within time.Duration) (int, relayResponse, error) {
	deadline := time.Now().Add(within)
	for {
		status, resp, err := relayCall(addr, verb, key, body)
		if err != nil || resp.Refused != agentsUnavailable || time.Now().After(deadline) {
			return status, resp, err
		}
		time.Sleep(time.Second)
	}
}

// keyedOutcome judges a call that must be refused: a closed connection, a
// 4xx or a refusal is blocked; a call served is REACHABLE; a refused
// connection (unless refusals count) or a 5xx proves nothing.
func keyedOutcome(status int, resp relayResponse, err error, refusals bool) string {
	switch {
	case err != nil:
		if errors.Is(err, syscall.ECONNREFUSED) {
			if refusals {
				return agentsBlocked
			}
			return outcomeInconclusive
		}
		if classifyHTTPError(err) == "blocked" {
			return agentsBlocked
		}
		return outcomeInconclusive
	case status >= 500:
		return outcomeInconclusive
	case status >= 400:
		return agentsBlocked
	case resp.Refused != "":
		return agentsBlocked
	}
	return agentsReachable
}

// filesLs is a call every workspace relay serves with the key: a look at
// the head's pristine tree.
var filesLs = map[string]string{"op": "ls"}

// agentsStep is one exec of the program.
type agentsStep struct {
	name, as, side, nonce, other, port, otherPort string
}

// runAgents runs the agents rows (see agentsRole) and summarises them.
func runAgents(cfg agentsConfig, emit func(result)) (string, int) {
	start := time.Now()
	var outcomes []string
	row := func(name, outcome string) {
		outcomes = append(outcomes, outcome)
		emit(result{Name: name, Outcome: outcome, ElapsedMS: time.Since(start).Milliseconds()})
	}
	finish := func() (string, int) {
		outcome, code := outcomePass, 0
		switch {
		case slices.Contains(outcomes, agentsBroken) || slices.Contains(outcomes, agentsReachable) ||
			slices.Contains(outcomes, outcomeSecurityFailure):
			outcome, code = outcomeSecurityFailure, 2
		case slices.Contains(outcomes, outcomeNotReached):
			outcome, code = outcomeNotReached, 4
		case slices.Contains(outcomes, outcomeInconclusive):
			outcome, code = outcomeInconclusive, 3
		}
		emit(result{Name: "summary", Outcome: outcome, ElapsedMS: time.Since(start).Milliseconds()})
		return outcome, code
	}
	reach := awaitReach(target{name: "tool-pod", addr: cfg.tool}, start.Add(cfg.reachWithin))
	row("tool-pod", reach)
	if reach != outcomeReached {
		return finish()
	}
	status, resp, err := callPatiently(cfg.tool, "files", cfg.key, filesLs, cfg.retryWithin)
	if err != nil || status != http.StatusOK || resp.Refused != "" {
		fmt.Fprintf(os.Stderr, "key-control: status %d, refused %q (%s), error %v\n", status, resp.Refused,
			resp.Reason, err)
		row("key-control", outcomeInconclusive)
		return finish()
	}
	row("key-control", agentsHeld)
	wrong := make([]byte, 32)
	_, _ = rand.Read(wrong)
	for _, k := range []struct{ name, key string }{
		{"key-missing", ""}, {"key-wrong", hex.EncodeToString(wrong)}, {"key-malformed", "not-a-key"},
	} {
		status, resp, err := callPatiently(cfg.tool, "files", k.key, filesLs, cfg.retryWithin)
		row(k.name, keyedOutcome(status, resp, err, cfg.refusals))
	}
	deny := cfg.deny
	if cfg.other != "" {
		status, resp, err := relayCall(cfg.other, "files", cfg.key, filesLs)
		row("other-tool-pod-own-key", keyedOutcome(status, resp, err, cfg.refusals))
		deny += ";other-tool-pod=" + cfg.other
	}
	n := func(s string) string { return cfg.nonce + s }
	for _, s := range []agentsStep{
		{"head-1", "sandbox-head", "head", n("01"), n("02"), agentsHeadPort, agentsBasePort},
		{"base", "sandbox-base", "base", n("02"), n("01"), agentsBasePort, agentsHeadPort},
		{"head-2", "sandbox-head", "head", n("03"), n("02"), agentsHeadPort, agentsBasePort},
	} {
		runStep(cfg, s.name, s.side, []string{"--as", s.as, "--nonce", s.nonce, "--other-nonce", s.other,
			"--port", s.port, "--other-port", s.otherPort}, row)
	}
	args := []string{"--as", "tools-agents", "--reach", cfg.reach, "--deny", deny}
	if cfg.refusals {
		args = append(args, refusedIsBlocked)
	}
	runStep(cfg, "network", "head", args, row)
	return finish()
}

// runStep execs the program with args at side and re-emits its rows as
// <step>/<row>, then its summary as <step>/summary (INCONCLUSIVE when the
// program printed none: it did not compile or run).
func runStep(cfg agentsConfig, step, side string, args []string, row func(string, string)) {
	body := map[string]any{
		"argv":  append([]string{cfg.goCmd, "run", agentsProgramFile}, args...),
		"files": []map[string]any{{"name": agentsProgramFile, "content": []byte(agentsProgram)}},
		"side":  side,
	}
	status, resp, err := callPatiently(cfg.tool, "exec", cfg.key, body, cfg.retryWithin)
	summary := ""
	if err == nil && status == http.StatusOK && resp.Refused == "" {
		for line := range strings.SplitSeq(resp.Untrusted, "\n") {
			var r struct{ Name, Outcome string }
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &r) != nil || r.Name == "" || r.Outcome == "" {
				continue
			}
			if r.Name == "summary" {
				summary = r.Outcome
				continue
			}
			row(step+"/"+r.Name, r.Outcome)
		}
	}
	if summary == "" {
		summary = outcomeInconclusive
	}
	if summary != outcomePass {
		fmt.Fprintf(os.Stderr, "%s: status %d, refused %q (%s), error %v, output:\n%s\n", step, status,
			resp.Refused, resp.Reason, err, resp.Untrusted)
	}
	row(step+"/summary", summary)
}

func agentsMode(refusals bool) int {
	cfg, ok := agentsConfigFrom(os.Getenv, refusals)
	if !ok {
		emit(result{Name: "summary", Outcome: outcomeInconclusive})
		return 3
	}
	_, code := runAgents(cfg, emit)
	return code
}
