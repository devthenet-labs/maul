// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package maultool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// EnvToolsURL is the one variable maul-tool reads: the tools container's
// origin.
const EnvToolsURL = "MAUL_TOOLS_URL"

// Exit codes.
const (
	// ExitOK: the call ran (whatever the program it ran exited with).
	ExitOK = 0
	// ExitUsage: the command line is not a call; nothing was sent.
	ExitUsage = 2
	// ExitRefused: the tools container refused the call (a ledger entry).
	ExitRefused = 3
	// ExitToolError: maul-tool or the tools container failed.
	ExitToolError = 4
)

// Options are Run's surroundings.
type Options struct {
	// Getenv reads the environment; only EnvToolsURL is asked for.
	Getenv func(string) string
	Stdout io.Writer
	Stderr io.Writer
	// Scratch is the scratch directory, ScratchDir in the pod.
	Scratch string
}

const usage = `usage:
  maul-tool http <METHOD> <path> [json]
  maul-tool browser <verb> [args]...
  maul-tool exec [--file <file>]... -- <argv>...
  maul-tool logs <component>
  maul-tool events
  maul-tool check run <file>
  maul-tool check results`

// usageError is a command line that is not a call.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// Run runs one maul-tool command line (args without the program name) and
// returns its exit code.
func Run(ctx context.Context, args []string, opt Options) int {
	sc := scratch{dir: opt.Scratch}
	verb, req, err := parse(args, sc)
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			_, _ = fmt.Fprintf(opt.Stderr, "maul-tool: %s\n%s\n", ue.msg, usage)
			return ExitUsage
		}
		_, _ = fmt.Fprintf(opt.Stderr, "maul-tool: %v\n", err)
		return ExitToolError
	}
	client, err := NewClient(opt.Getenv(EnvToolsURL))
	if err != nil {
		_, _ = fmt.Fprintf(opt.Stderr, "maul-tool: %v\n", err)
		return ExitToolError
	}
	resp, err := client.Call(ctx, verb, req)
	if errors.Is(err, errRefused) {
		_, _ = fmt.Fprintf(opt.Stdout, "[maul-tool ledger seq %d]\n[maul-tool refused: %s] %s\n",
			resp.Seq, oneLine(resp.Refused), oneLine(resp.Reason))
		return ExitRefused
	}
	if err != nil {
		_, _ = fmt.Fprintf(opt.Stderr, "maul-tool: %v\n", err)
		return ExitToolError
	}
	return render(ctx, client, sc, resp, opt)
}

// parse turns a command line into one request; files are read here, from
// the scratch directory only.
func parse(args []string, sc scratch) (toolwire.Verb, toolwire.Request, error) {
	if len(args) == 0 {
		return "", nil, usagef("no command")
	}
	verb, rest := args[0], args[1:]
	var (
		v   toolwire.Verb
		req toolwire.Request
		err error
	)
	switch verb {
	case "http":
		v, req, err = parseHTTP(rest)
	case "browser":
		if len(rest) == 0 {
			return "", nil, usagef("browser needs a verb")
		}
		v, req = toolwire.VerbBrowser, &toolwire.BrowserRequest{Verb: rest[0], Args: rest[1:]}
	case "exec":
		v, req, err = parseExec(rest, sc)
	case "logs":
		if len(rest) != 1 {
			return "", nil, usagef("logs takes one component")
		}
		v, req = toolwire.VerbLogs, &toolwire.LogsRequest{Component: rest[0]}
	case "events":
		if len(rest) != 0 {
			return "", nil, usagef("events takes no arguments")
		}
		v, req = toolwire.VerbEvents, &toolwire.EventsRequest{}
	case "check":
		v, req, err = parseCheck(rest, sc)
	default:
		return "", nil, usagef("unknown command %q", verb)
	}
	if err != nil {
		return "", nil, err
	}
	if err := req.Check(); err != nil {
		return "", nil, usagef("%s: %v", verb, err)
	}
	return v, req, nil
}

func parseHTTP(rest []string) (toolwire.Verb, toolwire.Request, error) {
	if len(rest) < 2 || len(rest) > 3 {
		return "", nil, usagef("http takes a method, a path and an optional JSON body")
	}
	req := &toolwire.HTTPRequest{Method: strings.ToUpper(rest[0]), Path: rest[1]}
	if len(rest) == 3 {
		if !json.Valid([]byte(rest[2])) {
			return "", nil, usagef("http body is not JSON")
		}
		req.Body = rest[2]
	}
	return toolwire.VerbHTTP, req, nil
}

func parseExec(rest []string, sc scratch) (toolwire.Verb, toolwire.Request, error) {
	req := &toolwire.ExecRequest{}
	total := 0
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		var name string
		switch {
		case a == "--":
			req.Argv = rest[i+1:]
			if len(req.Argv) == 0 {
				return "", nil, usagef("exec has no argv after --")
			}
			return toolwire.VerbExec, req, nil
		case a == "--file":
			if i+1 >= len(rest) {
				return "", nil, usagef("--file needs a file")
			}
			i++
			name = rest[i]
		case strings.HasPrefix(a, "--file="):
			name = strings.TrimPrefix(a, "--file=")
		default:
			return "", nil, usagef("exec: unknown argument %q before --", a)
		}
		rel, data, err := sc.read(name, toolwire.MaxProgramBytes-total)
		if err != nil {
			return "", nil, usagef("exec: %v", err)
		}
		total += len(data)
		req.Files = append(req.Files, toolwire.File{Name: rel, Content: data})
	}
	return "", nil, usagef("exec needs -- before its argv")
}

func parseCheck(rest []string, sc scratch) (toolwire.Verb, toolwire.Request, error) {
	switch {
	case len(rest) == 2 && rest[0] == "run":
		_, data, err := sc.read(rest[1], toolwire.MaxCheckBytes)
		if err != nil {
			return "", nil, usagef("check run: %v", err)
		}
		if !json.Valid(data) {
			return "", nil, usagef("check run: %s is not JSON", rest[1])
		}
		return toolwire.VerbCheckRun, &toolwire.CheckRunRequest{Spec: json.RawMessage(data)}, nil
	case len(rest) == 1 && rest[0] == "results":
		return toolwire.VerbCheckResults, &toolwire.CheckResultsRequest{}, nil
	}
	return "", nil, usagef("check takes `run <file>` or `results`")
}

// render prints a response in the fixed order the package documents and
// copies its screenshots.
func render(ctx context.Context, client *Client, sc scratch, resp *toolwire.Response, opt Options) int {
	var b strings.Builder
	fmt.Fprintf(&b, "[maul-tool ledger seq %d]\n", resp.Seq)
	if o := resp.Observed; o != nil {
		if o.Status != 0 {
			fmt.Fprintf(&b, "[observed] http status %d in %d ms\n", o.Status, o.LatencyMs)
		}
		if o.Exit != nil {
			fmt.Fprintf(&b, "[observed] exit %d\n", *o.Exit)
		}
		if o.CheckRun != nil {
			writeCheckRun(&b, *o.CheckRun)
		}
		for _, r := range o.CheckRuns {
			writeCheckRun(&b, r)
		}
	}
	code := ExitOK
	n := 0
	for _, a := range resp.Artifacts {
		if !toolwire.IsScreenshot(a) {
			continue
		}
		n++
		path, err := copyShot(ctx, client, sc, resp.Seq, n, a)
		if err != nil {
			_, _ = fmt.Fprintf(opt.Stderr, "maul-tool: screenshot %s of ledger seq %d: %v\n", oneLine(a.Name),
				resp.Seq, err)
			code = ExitToolError
			continue
		}
		fmt.Fprintf(&b, "[screenshot] %s\n", path)
	}
	if resp.Untrusted != "" {
		b.WriteString("[untrusted output]\n")
		b.WriteString(resp.Untrusted)
		if !strings.HasSuffix(resp.Untrusted, "\n") {
			b.WriteString("\n")
		}
	}
	if _, err := io.WriteString(opt.Stdout, b.String()); err != nil {
		return ExitToolError
	}
	return code
}

func copyShot(ctx context.Context, client *Client, sc scratch, seq int64, n int, a toolwire.Artifact) (string, error) {
	data, err := client.Artifact(ctx, a)
	if err != nil {
		return "", err
	}
	return sc.writeShot(seq, n, data)
}

func writeCheckRun(b *strings.Builder, r toolwire.CheckRunRecord) {
	results := make([]string, 0, len(r.Attempts))
	for _, a := range r.Attempts {
		results = append(results, oneLine(string(a.Result)))
	}
	// The ledger keys a check by its bare hex; the agent's report names it
	// as checks.Digest does ("sha256:<hex>"), so it is printed that way.
	fmt.Fprintf(b, "[observed] check %s digest sha256:%s %s: %s (attempts: %s)\n", oneLine(r.CheckID),
		oneLine(strings.TrimPrefix(r.Digest, "sha256:")),
		oneLine(string(r.Mode)), oneLine(string(r.Result)), strings.Join(results, ", "))
	for i, a := range r.Attempts {
		for _, s := range a.Steps {
			if s.Result == toolwire.ResultPass {
				continue
			}
			kind := ""
			if s.Assertion {
				kind = " (assertion)"
			}
			fmt.Fprintf(b, "[observed]   attempt %d step %d %s%s: %q\n", i+1, s.Index, oneLine(string(s.Result)),
				kind, s.Detail)
		}
	}
	// A check that shows a defect is one red by assertion: only such a run
	// may back a proposal expecting it to fail.
	if step, ok := r.AssertionStep(); ok {
		fmt.Fprintf(b, "[observed] red by assertion at step %d: it can show a defect, proposed with expect: fail\n",
			step)
	}
}

// oneLine keeps a value on its own line: every control character becomes a
// space.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
}
