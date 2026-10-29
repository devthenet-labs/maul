// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The requests' bounds.
const (
	// MaxRequestBytes bounds a request body: it is recorded whole as an
	// entry's Args.
	MaxRequestBytes = MaxArgsBytes
	// MaxResponseBytes bounds a response body a client reads.
	MaxResponseBytes = MaxOutputBytes + MaxInlineBytes + 64<<10
	// MaxPathBytes bounds an http request's path with its query.
	MaxPathBytes = 2 << 10
	// MaxBodyBytes bounds an http request's body.
	MaxBodyBytes = 32 << 10
	// MaxArgs bounds a browser call's or an exec's arguments, MaxArgBytes
	// each one.
	MaxArgs     = 64
	MaxArgBytes = 4 << 10
	// MaxFiles bounds an exec's program files, MaxProgramBytes their
	// contents together (SPEC S1-02: program files at most 64 KiB).
	MaxFiles        = 8
	MaxProgramBytes = 64 << 10
	// MaxCheckBytes bounds a check's canonical JSON (SPEC Q1: 8 KiB).
	MaxCheckBytes = 8 << 10
	// MaxConfirm bounds the checks one confirm names (SPEC Q1: at most 40
	// checks).
	MaxConfirm = 40
)

// Path is the tools container's endpoint for verb: POST /v1/<verb>. The
// relay's own VerbSession has none ("").
func Path(v Verb) string {
	if v == VerbSession || !knownVerb(v) {
		return ""
	}
	return "/v1/" + string(v)
}

// Request is a request body of one endpoint.
type Request interface {
	// Check holds the request to its grammar and bounds.
	Check() error
}

// HTTPRequest is `maul-tool http <METHOD> <path> [json]`: a request to the
// preview, its path resolved against the preview's host.
type HTTPRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
}

// BrowserRequest is `maul-tool browser <verb> [args]`: one Playwright CLI
// call, which the relay holds to its allowlist.
type BrowserRequest struct {
	Verb string   `json:"verb"`
	Args []string `json:"args,omitempty"`
}

// ExecRequest is `maul-tool exec [--file f]... -- argv`: argv run in the
// sandbox container, the files written under /work/scratch first.
type ExecRequest struct {
	Argv  []string `json:"argv"`
	Files []File   `json:"files,omitempty"`
}

// File is a program file for an exec: a relative path and its contents.
type File struct {
	Name    string `json:"name"`
	Content []byte `json:"content,omitempty"`
}

// LogsRequest is `maul-tool logs <component>`.
type LogsRequest struct {
	Component string `json:"component"`
}

// EventsRequest is `maul-tool events`.
type EventsRequest struct{}

// CheckRunRequest is `maul-tool check run <file>`: a dry run of the check
// in Spec, whose grammar the relay validates (internal/checks).
type CheckRunRequest struct {
	Spec json.RawMessage `json:"check"`
}

// CheckResultsRequest is `maul-tool check results`: the replay and dry-run
// results so far.
type CheckResultsRequest struct{}

// ConfirmRequest is agent-runner's confirm: run each named check (by
// digest) alone, with a fresh run token. The relay refuses a digest with no
// green dry run.
type ConfirmRequest struct {
	Digests []string `json:"digests"`
}

// FinishRequest is agent-runner's finish: the ledger entries whose images
// the report cites, emitted as evidence before the summary.
type FinishRequest struct {
	Evidence []EvidenceRef `json:"evidence,omitempty"`
}

// EvidenceRef names one image: an artifact of a ledger entry.
type EvidenceRef struct {
	Seq    int64  `json:"seq"`
	SHA256 string `json:"sha256"`
}

var (
	httpMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true,
		"DELETE": true, "OPTIONS": true}
	browserVerb = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)
	dnsLabel    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	fileSegment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// Check implements Request.
func (r *HTTPRequest) Check() error {
	switch {
	case !httpMethods[r.Method]:
		return fmt.Errorf("method %q is not allowed", r.Method)
	case !strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, "//") || len(r.Path) > MaxPathBytes ||
		!printable(r.Path):
		return errors.New("path is not a bounded absolute path")
	case len(r.Body) > MaxBodyBytes || !utf8.ValidString(r.Body):
		return errors.New("body is not bounded UTF-8")
	}
	return nil
}

// Check implements Request.
func (r *BrowserRequest) Check() error {
	if !browserVerb.MatchString(r.Verb) {
		return errors.New("browser verb is not a verb")
	}
	return checkArgs(r.Args, 0)
}

// Check implements Request.
func (r *ExecRequest) Check() error {
	if err := checkArgs(r.Argv, 1); err != nil {
		return err
	}
	if r.Argv[0] == "" {
		return errors.New("argv[0] is empty")
	}
	if len(r.Files) > MaxFiles {
		return errors.New("too many files")
	}
	seen := map[string]bool{}
	total := 0
	for _, f := range r.Files {
		if !relativeFile(f.Name) || seen[f.Name] {
			return fmt.Errorf("file name %q is not a new relative path", f.Name)
		}
		seen[f.Name] = true
		total += len(f.Content)
	}
	if total > MaxProgramBytes {
		return errors.New("program files are too large")
	}
	return nil
}

// Check implements Request.
func (r *LogsRequest) Check() error {
	if !dnsLabel.MatchString(r.Component) {
		return errors.New("component is not a name")
	}
	return nil
}

// Check implements Request.
func (*EventsRequest) Check() error { return nil }

// Check implements Request. The check's own grammar is internal/checks'.
func (r *CheckRunRequest) Check() error {
	if len(r.Spec) == 0 || len(r.Spec) > MaxCheckBytes || !json.Valid(r.Spec) {
		return errors.New("check is not bounded JSON")
	}
	return nil
}

// Check implements Request.
func (*CheckResultsRequest) Check() error { return nil }

// Check implements Request.
func (r *ConfirmRequest) Check() error {
	if len(r.Digests) == 0 || len(r.Digests) > MaxConfirm {
		return errors.New("confirm names no checks or too many")
	}
	seen := map[string]bool{}
	for _, d := range r.Digests {
		if !isSHA256(d) || seen[d] {
			return fmt.Errorf("digest %q is not a new sha256", d)
		}
		seen[d] = true
	}
	return nil
}

// Check implements Request.
func (r *FinishRequest) Check() error {
	if len(r.Evidence) > MaxEvidence {
		return errors.New("too much evidence")
	}
	for _, e := range r.Evidence {
		if e.Seq < 1 || e.Seq > MaxEntries || !isSHA256(e.SHA256) {
			return errors.New("an evidence reference is out of bounds")
		}
	}
	return nil
}

func checkArgs(args []string, least int) error {
	if len(args) < least || len(args) > MaxArgs {
		return errors.New("wrong number of arguments")
	}
	for _, a := range args {
		if len(a) > MaxArgBytes || !utf8.ValidString(a) || strings.ContainsRune(a, 0) {
			return errors.New("an argument is out of bounds")
		}
	}
	return nil
}

// relativeFile reports a path of plain segments under the scratch
// directory: never absolute, never "." or "..".
func relativeFile(name string) bool {
	segs := strings.Split(name, "/")
	if len(segs) > 8 {
		return false
	}
	for _, s := range segs {
		if !fileSegment.MatchString(s) {
			return false
		}
	}
	return true
}

func printable(s string) bool {
	for _, r := range s {
		if r == utf8.RuneError || !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// Refusal codes, a Response's and an entry's Refused.
const (
	RefusedInvalid     = "invalid-request"
	RefusedVerb        = "verb-not-allowed"
	RefusedNavigation  = "navigation-not-allowed"
	RefusedToolCalls   = "tool-calls-exhausted"
	RefusedNoDryRun    = "no-green-dry-run"
	RefusedCheck       = "check-invalid"
	RefusedFinished    = "finished"
	RefusedUnavailable = "unavailable"
)

// Response is every endpoint's answer. Untrusted holds bytes the
// application or a page chose; Observed holds maul's own measurements.
// Neither is ever the other.
type Response struct {
	// Seq is the ledger entry recording the call.
	Seq int64 `json:"seq"`
	// Refused is the refusal code when nothing ran; Reason is maul's words
	// for it.
	Refused  string    `json:"refused,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Observed *Observed `json:"observed,omitempty"`
	// Untrusted is the inline output, capped (an entry's Inline).
	Untrusted string     `json:"untrusted,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// Observed is what the tools container measured itself.
type Observed struct {
	// Status and LatencyMs are an http call's.
	Status    int   `json:"status,omitempty"`
	LatencyMs int64 `json:"latencyMs,omitempty"`
	// Exit is the exit code of the process an exec or browser call ran.
	Exit *int `json:"exit,omitempty"`
	// CheckRun is a dry run's record; CheckRuns are check-results' and
	// confirm's.
	CheckRun  *CheckRunRecord  `json:"checkRun,omitempty"`
	CheckRuns []CheckRunRecord `json:"checkRuns,omitempty"`
	// Summary is finish's.
	Summary *Summary `json:"summary,omitempty"`
}

// ErrRequest is a request body that is not one request of its endpoint.
var ErrRequest = errors.New("toolwire: invalid request")

// DecodeRequest reads one request of at most MaxRequestBytes into req,
// unknown fields refused, and checks it.
func DecodeRequest(r io.Reader, req Request) error {
	b, err := io.ReadAll(io.LimitReader(r, MaxRequestBytes+1))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRequest, err)
	}
	if len(b) > MaxRequestBytes {
		return fmt.Errorf("%w: too large", ErrRequest)
	}
	if err := decodeStrict(b, req); err != nil {
		return fmt.Errorf("%w: %v", ErrRequest, err)
	}
	if err := req.Check(); err != nil {
		return fmt.Errorf("%w: %v", ErrRequest, err)
	}
	return nil
}

// DecodeResponse reads one response of at most MaxResponseBytes, unknown
// fields refused.
func DecodeResponse(r io.Reader, resp *Response) error {
	b, err := io.ReadAll(io.LimitReader(r, MaxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(b) > MaxResponseBytes {
		return errors.New("toolwire: response too large")
	}
	return decodeStrict(b, resp)
}
