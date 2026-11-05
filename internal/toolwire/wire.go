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
	// MaxFileReadBytes, MaxGrepHits and MaxListEntries bound what the files
	// verb returns (SPEC S2-05): a read at most 64 KiB, a grep at most 200
	// hits, an ls at most 2000 entries. The relay cuts to them; a request
	// cannot ask for more.
	MaxFileReadBytes = 64 << 10
	MaxGrepHits      = 200
	MaxListEntries   = 2000
	// MaxPatternBytes bounds a grep pattern.
	MaxPatternBytes = 1 << 10
	// MaxTreeSegments bounds the depth of a files path.
	MaxTreeSegments = 32
)

// KeyHeader carries the per-evaluation key (SPEC O7): 256 bits, lower-case
// hex, on every request to the tools container's :8090 once the tool pod
// has one. It binds one evaluator to one tool pod, which NetworkPolicy
// cannot; it is never recorded in a ledger entry.
const KeyHeader = "X-Maul-Tools-Key"

// IsKey reports a well-formed per-evaluation key: 64 lower-case hex digits.
func IsKey(s string) bool { return isSHA256(s) }

// Side is the revision a call runs against: the PR head (the default, an
// empty Side) or the base, the merge base of the head and the PR's base
// branch (SPEC O3). The two are separate sandboxes (SPEC O2).
type Side string

// Sides.
const (
	SideHead Side = "head"
	SideBase Side = "base"
)

// Valid reports a side a request or an entry may carry: the empty side
// (head), "head" or "base".
func (s Side) Valid() bool { return s == "" || s == SideHead || s == SideBase }

// Op is what the files verb does.
type Op string

// Ops.
const (
	OpLs   Op = "ls"
	OpGrep Op = "grep"
	OpRead Op = "read"
)

// LanguageGo is the only language the run verb takes (SPEC O5): node and
// python are reserved, and refused.
const LanguageGo = "go"

// Path is the tools container's endpoint for verb: POST /v1/<verb>. The
// relay's own VerbSession and VerbBuild have none ("").
func Path(v Verb) string {
	if v == VerbSession || v == VerbBuild || !knownVerb(v) {
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
	// Side is the sandbox the argv runs in; empty is the head.
	Side Side `json:"side,omitempty"`
}

// File is a program file for an exec: a relative path and its contents.
type File struct {
	Name    string `json:"name"`
	Content []byte `json:"content,omitempty"`
}

// FilesRequest is `maul-tool files ls|grep|read [--base] ...`: a look at a
// pristine, digest-verified source tree, never at what a build rewrote and
// never executing it. Path is relative to the tree's root; ls and grep take
// an empty one (the root), read needs a file. Pattern is grep's.
type FilesRequest struct {
	Side    Side   `json:"side,omitempty"`
	Op      Op     `json:"op"`
	Path    string `json:"path,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// RunRequest is `maul-tool run <dir> [--base] -- args`: a small program the
// agent wrote, compiled and run against the tree of Side. For Go the relay
// renders the module file itself (SPEC Q25); the agent supplies only .go
// files.
type RunRequest struct {
	Side     Side     `json:"side,omitempty"`
	Language string   `json:"language"`
	Files    []File   `json:"files"`
	Args     []string `json:"args,omitempty"`
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
	// Side is the sandbox the dry run uses; empty is the head.
	Side Side `json:"side,omitempty"`
}

// CheckResultsRequest is `maul-tool check results`: the replay and dry-run
// results so far.
type CheckResultsRequest struct{}

// ConfirmRequest is agent-runner's confirm: run each named check (by
// digest) alone, with a fresh run token. The relay refuses a digest with no
// green dry run, or, for one named in ExpectFail (a check the evaluator
// proposes to show a defect of the outcome it is bound to), with no dry run
// red by assertion (CheckRunRecord.AssertionStep) or with a green one.
type ConfirmRequest struct {
	Digests []string `json:"digests"`
	// ExpectFail are the digests of Digests whose proposal expects them to
	// fail: the outcome they are bound to does not hold.
	ExpectFail []string `json:"expectFail,omitempty"`
}

// ExpectFail is the expectation a confirmation entry's args record for a
// check confirmed as showing a defect: {"digest": <hex>, "expect": "fail"}.
// A confirmation of a check expected to pass records the digest alone.
const ExpectFail = "fail"

// ConfirmArgs are the args of the relay's own confirmation entry, one per
// check a confirm ran.
type ConfirmArgs struct {
	Digest string `json:"digest"`
	Expect string `json:"expect,omitempty"`
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
	if !r.Side.Valid() {
		return fmt.Errorf("side %q is not a side", r.Side)
	}
	return checkFiles(r.Files, 0)
}

// Check implements Request.
func (r *FilesRequest) Check() error {
	switch {
	case !r.Side.Valid():
		return fmt.Errorf("side %q is not a side", r.Side)
	case r.Op != OpLs && r.Op != OpGrep && r.Op != OpRead:
		return fmt.Errorf("op %q is not ls, grep or read", r.Op)
	case r.Op == OpRead && r.Path == "":
		return errors.New("read needs a file path")
	case r.Op == OpGrep && r.Pattern == "":
		return errors.New("grep needs a pattern")
	case r.Op != OpGrep && r.Pattern != "":
		return errors.New("only grep takes a pattern")
	case len(r.Pattern) > MaxPatternBytes || !utf8.ValidString(r.Pattern) || strings.ContainsRune(r.Pattern, 0):
		return errors.New("pattern is not bounded UTF-8")
	}
	return checkTreePath(r.Path)
}

// Check implements Request.
func (r *RunRequest) Check() error {
	if !r.Side.Valid() {
		return fmt.Errorf("side %q is not a side", r.Side)
	}
	if r.Language != LanguageGo {
		return fmt.Errorf("language %q is not supported (only %s)", r.Language, LanguageGo)
	}
	if err := checkFiles(r.Files, 1); err != nil {
		return err
	}
	for _, f := range r.Files {
		if !strings.HasSuffix(f.Name, ".go") {
			return fmt.Errorf("file %q is not a .go file", f.Name)
		}
	}
	return checkArgs(r.Args, 0)
}

// checkFiles holds program files to their bounds: at least least of them,
// each a new relative path under the scratch directory.
func checkFiles(files []File, least int) error {
	if len(files) < least {
		return errors.New("no program files")
	}
	if len(files) > MaxFiles {
		return errors.New("too many files")
	}
	seen := map[string]bool{}
	total := 0
	for _, f := range files {
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

// checkTreePath holds a files path to a plain relative path into a source
// tree: "" (the root) or slash-separated segments, none empty, "." or "..",
// no control characters.
func checkTreePath(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > MaxPathBytes || !utf8.ValidString(p) || strings.HasPrefix(p, "/") {
		return errors.New("path is not a bounded relative path")
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxTreeSegments {
		return errors.New("path is too deep")
	}
	for _, sg := range segs {
		if sg == "" || sg == "." || sg == ".." || strings.IndexFunc(sg, unicode.IsControl) >= 0 {
			return errors.New("path is not a plain relative path")
		}
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
	if !r.Side.Valid() {
		return fmt.Errorf("side %q is not a side", r.Side)
	}
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
	red := map[string]bool{}
	for _, d := range r.ExpectFail {
		if !seen[d] || red[d] {
			return fmt.Errorf("expected failure %q names no check of the confirm, or one twice", d)
		}
		red[d] = true
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
	RefusedNoRedDryRun = "no-red-dry-run"
	// RefusedRedRule: a check proposed to show a defect has its red dry
	// run, but breaks a rule for showing one (checks.ValidateRed), told
	// apart from RefusedNoRedDryRun so the evaluator can correct it (S1
	// demo bug 9).
	RefusedRedRule     = "red-check-rule"
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
