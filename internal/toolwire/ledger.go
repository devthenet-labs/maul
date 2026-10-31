// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The ledger's bounds. The tools container never writes past them, and
// Verify refuses a log that does.
const (
	// MaxEntries is the most entries one ledger holds: maxToolCalls (300)
	// plus replay, confirmation and relay entries, with room.
	MaxEntries = 512
	// MaxOutputBytes bounds an entry's full output; Writer cuts a longer
	// one, ending it with TruncationMarker.
	MaxOutputBytes = 128 << 10
	// MaxInlineBytes bounds what the agent saw inline (DESIGN 2.7: a 16 KiB
	// capped snapshot).
	MaxInlineBytes = 16 << 10
	// MaxArgsBytes bounds an entry's recorded request: an exec request's
	// program files (64 KiB) with room for its argv.
	MaxArgsBytes = 80 << 10
	// MaxArtifacts bounds an entry's artifacts.
	MaxArtifacts = 8
	// MaxLocators bounds the locators one entry records.
	MaxLocators = 16
	// MaxLocatorBytes bounds one locator.
	MaxLocatorBytes = 1 << 10
	// MaxEvidence is the most evidence lines (screenshots) one ledger
	// carries (SPEC Q7).
	MaxEvidence = 6
	// MaxEvidenceBytes bounds one evidence image before base64.
	MaxEvidenceBytes = 4 << 20
	// MaxLedgerBytes bounds the entry lines of one ledger together.
	MaxLedgerBytes = 48 << 20
	// MaxLogBytes is the most of the tools container's log a reader needs:
	// the entry lines, the evidence lines and the summary, with room for
	// the lines Verify ignores.
	MaxLogBytes = MaxLedgerBytes + MaxEvidence*(MaxEvidenceBytes/3*4+4096) + 1<<20
)

// TruncationMarker ends an output or inline text cut to its bound.
const TruncationMarker = "\n[maul: truncated]"

// Line prefixes. Only lines starting with one of them mean anything; the
// two never prefix each other.
const (
	// EntryPrefix starts every entry line and the summary line.
	EntryPrefix = "MAUL-TOOLS: "
	// EvidencePrefix starts every evidence line.
	EvidencePrefix = "MAUL-TOOLS-EVIDENCE: "
)

// WireVersion is the ledger's version: 2, after slice 1's MAUL-VERIFY v1
// and v2 tester wire, which it replaces. A reader refuses any other
// (ErrVersion): the tools image is pinned apart from the controllers.
const WireVersion = 2

// Kind is who caused an entry.
type Kind string

// Kinds.
const (
	// KindAgent is the evaluator's call through maul-tool.
	KindAgent Kind = "agent"
	// KindRunner is agent-runner's call after the agent exited (confirm,
	// finish).
	KindRunner Kind = "runner"
	// KindRelay is the tools container's own action: a replay or
	// confirmation run, a browser session (re)opened, a deadline finish.
	KindRelay Kind = "relay"
)

var kinds = map[Kind]bool{KindAgent: true, KindRunner: true, KindRelay: true}

// Verb is the tool an entry records.
type Verb string

// Verbs: one per tool endpoint (wire.go), plus VerbSession, the relay's
// browser session.
const (
	VerbHTTP         Verb = "http"
	VerbBrowser      Verb = "browser"
	VerbExec         Verb = "exec"
	VerbLogs         Verb = "logs"
	VerbEvents       Verb = "events"
	VerbCheckRun     Verb = "check-run"
	VerbCheckResults Verb = "check-results"
	VerbConfirm      Verb = "confirm"
	VerbFinish       Verb = "finish"
	VerbSession      Verb = "session"
)

// Verbs lists every verb, in a fixed order.
var Verbs = []Verb{VerbHTTP, VerbBrowser, VerbExec, VerbLogs, VerbEvents, VerbCheckRun, VerbCheckResults,
	VerbConfirm, VerbFinish, VerbSession}

func knownVerb(v Verb) bool {
	for _, k := range Verbs {
		if v == k {
			return true
		}
	}
	return false
}

// LedgerEntry is one line of the ledger: one tool call (or relay action)
// and its full result, as the tools container recorded it.
type LedgerEntry struct {
	// Seq numbers the entries 1, 2, 3, … in the order written.
	Seq  int64     `json:"seq"`
	TS   time.Time `json:"ts"`
	Kind Kind      `json:"kind"`
	Verb Verb      `json:"verb"`
	// Args is the request as received, JSON.
	Args json.RawMessage `json:"args"`
	// Exit is the exit code of the process the verb ran (the Playwright
	// CLI, an exec argv); 0 for a verb that runs none.
	Exit int `json:"exit"`
	// Refused is the refusal code when nothing ran, empty otherwise.
	Refused string `json:"refused,omitempty"`
	// Output is the full output (valid UTF-8, at most MaxOutputBytes),
	// OutputSHA256 its sha256.
	Output       string `json:"output"`
	OutputSHA256 string `json:"outputSHA256"`
	// Inline is what the agent saw: capped, ANSI stripped.
	Inline    string     `json:"inline,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// Locators holds generate-locator's result for each ref acted on.
	Locators []string        `json:"locators,omitempty"`
	CheckRun *CheckRunRecord `json:"checkRun,omitempty"`
}

// Artifact is a file a call produced (a screenshot, a snapshot file),
// recorded by digest; its bytes leave the tools container only as
// evidence.
type Artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// CheckRunMode is why a check ran.
type CheckRunMode string

// Check run modes.
const (
	// ModeDryRun is the agent's `maul-tool check run`.
	ModeDryRun CheckRunMode = "dry-run"
	// ModeReplay is the relay's replay of a stored check at start.
	ModeReplay CheckRunMode = "replay"
	// ModeConfirm is the relay's confirmation run of a proposed check.
	ModeConfirm CheckRunMode = "confirm"
)

// AttemptResult is one attempt's result.
type AttemptResult string

// Attempt results. ResultError is a check error (an indeterminate
// assertion, a runner failure): unsure, never a fail.
const (
	ResultPass  AttemptResult = "pass"
	ResultFail  AttemptResult = "fail"
	ResultError AttemptResult = "error"
)

var attemptResults = map[AttemptResult]bool{ResultPass: true, ResultFail: true, ResultError: true}

// RunResult is a check run's result over its attempts (DeriveResult).
type RunResult string

// Run results.
const (
	RunPass        RunResult = "pass"
	RunFail        RunResult = "fail"
	RunError       RunResult = "error"
	RunQuarantined RunResult = "quarantined"
)

// CheckRunRecord is one check's run: alone, in a fresh context, re-run
// once after a failure.
type CheckRunRecord struct {
	// Digest is the check's digest, CheckID its id.
	Digest   string       `json:"digest"`
	CheckID  string       `json:"checkID"`
	Mode     CheckRunMode `json:"mode"`
	Result   RunResult    `json:"result"`
	Attempts []Attempt    `json:"attempts"`
}

// Attempt is one run of a check under its own fresh run token.
type Attempt struct {
	Run        string        `json:"run"`
	Result     AttemptResult `json:"result"`
	DurationMs int64         `json:"durationMs"`
	Steps      []StepRecord  `json:"steps,omitempty"`
}

// StepRecord is one step's result, from the runner's reporter.
type StepRecord struct {
	Index  int           `json:"index"`
	Result AttemptResult `json:"result"`
	// Detail is the runner's message for a step that did not pass.
	Detail string `json:"detail,omitempty"`
	// Assertion marks a failing step that failed an assertion the outcome
	// is the judge of, in a check that otherwise ran; only on a fail step
	// (Check). http: a JSON value of a JSON body, a header, body text of a
	// non-HTML body, or a status that is a success or a 500; never a 404,
	// 405, 400, another 4xx or a 501-504, a non-JSON body where JSON was
	// expected, no response, an unbuilt request or a missing capture. exec:
	// only when the program printed checks.ExecAssertionMarker, never an exit
	// code or missing output alone, never a time limit. browser: an expect
	// step's maul assertion or Playwright matcher on an element that was
	// there; never a timeout, an action, a locator that resolved nothing
	// (a count of 0 included) or more than one element, an element with no
	// box, a trial click or an indeterminate judgement.
	Assertion bool `json:"assertion,omitempty"`
}

// Bounds of a check run record.
const (
	maxRunBytes     = 64
	maxCheckIDBytes = 64
	maxSteps        = 64
	maxDetailBytes  = 2 << 10
)

// DeriveResult is the result of a run's attempts: a pass on the first
// attempt passes; a non-pass followed by a pass is quarantined; otherwise
// an error anywhere is an error and the rest fail. It is "" for attempts no
// run has: none, more than two, or a re-run after a pass.
func DeriveResult(as []Attempt) RunResult {
	switch {
	case len(as) == 0 || len(as) > 2:
		return ""
	case as[0].Result == ResultPass && len(as) == 1:
		return RunPass
	case as[0].Result == ResultPass:
		return ""
	case len(as) == 2 && as[1].Result == ResultPass:
		return RunQuarantined
	}
	for _, a := range as {
		if a.Result == ResultError {
			return RunError
		}
	}
	return RunFail
}

var (
	refusalCode  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	artifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func isSHA256(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == sha256.Size*2 && err == nil && strings.ToLower(s) == s
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Check holds an entry to the bounds and consistency the tools container
// keeps: known kind and verb, bounded fields, an output matching its
// digest, a refusal that ran nothing, and a check run only where its mode
// belongs.
func (e LedgerEntry) Check() error {
	switch {
	case e.Seq < 1:
		return errors.New("seq is not positive")
	case e.TS.IsZero():
		return errors.New("no timestamp")
	case !kinds[e.Kind]:
		return fmt.Errorf("kind %q is not a kind", e.Kind)
	case !knownVerb(e.Verb):
		return fmt.Errorf("verb %q is not a verb", e.Verb)
	case len(e.Args) > MaxArgsBytes || !json.Valid(e.Args):
		return errors.New("args are not bounded JSON")
	case len(e.Output) > MaxOutputBytes || !utf8.ValidString(e.Output):
		return errors.New("output is not bounded UTF-8")
	case e.OutputSHA256 != sha256Hex([]byte(e.Output)):
		return errors.New("output does not match its sha256")
	case len(e.Inline) > MaxInlineBytes || !utf8.ValidString(e.Inline):
		return errors.New("inline is not bounded UTF-8")
	}
	if err := e.checkParts(); err != nil {
		return err
	}
	if e.Refused != "" {
		if !refusalCode.MatchString(e.Refused) {
			return errors.New("refusal is not a code")
		}
		if e.Exit != 0 || len(e.Artifacts) > 0 || len(e.Locators) > 0 || e.CheckRun != nil {
			return errors.New("a refused call ran nothing")
		}
	}
	return e.checkRun()
}

func (e LedgerEntry) checkParts() error {
	if len(e.Artifacts) > MaxArtifacts {
		return errors.New("too many artifacts")
	}
	for _, a := range e.Artifacts {
		if !artifactName.MatchString(a.Name) || !isSHA256(a.SHA256) || a.Bytes < 0 || a.Bytes > MaxEvidenceBytes*4 {
			return errors.New("an artifact is out of bounds")
		}
	}
	if len(e.Locators) > MaxLocators {
		return errors.New("too many locators")
	}
	for _, l := range e.Locators {
		if l == "" || len(l) > MaxLocatorBytes || !utf8.ValidString(l) {
			return errors.New("a locator is out of bounds")
		}
	}
	return nil
}

// checkRun holds a check run to where its mode belongs: the agent's dry
// run on check-run, the relay's replay on check-run, the relay's
// confirmation on confirm; and to its bounds and derived result.
func (e LedgerEntry) checkRun() error {
	r := e.CheckRun
	if r == nil {
		return nil
	}
	switch {
	case r.Mode == ModeDryRun && e.Kind == KindAgent && e.Verb == VerbCheckRun:
	case r.Mode == ModeReplay && e.Kind == KindRelay && e.Verb == VerbCheckRun:
	case r.Mode == ModeConfirm && e.Kind == KindRelay && e.Verb == VerbConfirm:
	default:
		return fmt.Errorf("a %s check run cannot be a %s %s entry", r.Mode, e.Kind, e.Verb)
	}
	if !isSHA256(r.Digest) || len(r.CheckID) > maxCheckIDBytes || !utf8.ValidString(r.CheckID) {
		return errors.New("a check run's digest or id is out of bounds")
	}
	for _, a := range r.Attempts {
		if err := a.check(); err != nil {
			return err
		}
	}
	if d := DeriveResult(r.Attempts); d == "" || d != r.Result {
		return fmt.Errorf("a check run's result %q is not its attempts' (%q)", r.Result, d)
	}
	return nil
}

func (a Attempt) check() error {
	if len(a.Run) > maxRunBytes || !utf8.ValidString(a.Run) || !attemptResults[a.Result] || a.DurationMs < 0 ||
		len(a.Steps) > maxSteps {
		return errors.New("an attempt is out of bounds")
	}
	for _, s := range a.Steps {
		if s.Index < 0 || s.Index >= maxSteps || !attemptResults[s.Result] || len(s.Detail) > maxDetailBytes ||
			!utf8.ValidString(s.Detail) {
			return errors.New("a step record is out of bounds")
		}
		if s.Assertion && s.Result != ResultFail {
			return errors.New("a step that did not fail is marked an assertion")
		}
	}
	return nil
}

// AssertionStep is the step at which a run is red by assertion, and
// whether it is: the run failed (never quarantined, never an error), each
// of its two attempts, under its own fresh token, failed, and each at the
// same step, an assertion (StepRecord.Assertion) that every step before it
// passed. A check red any other way (a check error, a timeout, an action
// on nothing, a fail then a pass) is not red by assertion: it may show a
// broken check, never a defect.
func (r CheckRunRecord) AssertionStep() (int, bool) {
	if r.Result != RunFail || len(r.Attempts) != 2 || DeriveResult(r.Attempts) != RunFail {
		return 0, false
	}
	at := -1
	for _, a := range r.Attempts {
		i, ok := a.assertionStep()
		if !ok || (at >= 0 && i != at) {
			return 0, false
		}
		at = i
	}
	return at, true
}

// assertionStep is the index of a failed attempt's failing step when that
// step is its last, an assertion, and every step before it passed.
func (a Attempt) assertionStep() (int, bool) {
	if a.Result != ResultFail || len(a.Steps) == 0 {
		return 0, false
	}
	for i, s := range a.Steps {
		if s.Result == ResultPass {
			continue
		}
		if s.Result != ResultFail || !s.Assertion || i != len(a.Steps)-1 {
			return 0, false
		}
		return s.Index, true
	}
	return 0, false
}

// Evidence is one cited image's bytes, as the tools container emitted
// them: Data's sha256 is SHA256, an artifact of entry Seq.
type Evidence struct {
	Seq    int64  `json:"seq"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}

// ErrLedgerFull is an Append past MaxEntries or MaxLedgerBytes. The relay
// refuses the call instead.
var ErrLedgerFull = errors.New("ledger is full")

// Writer writes the ledger: entry lines, then evidence lines, then the
// summary. Only the tools container writes one.
type Writer struct {
	w         io.Writer
	h         hash.Hash
	n         int64
	bytes     int
	closed    bool
	artifacts map[int64]map[string]bool
	evidence  map[string]bool
}

// NewWriter writes a ledger to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, h: sha256.New(), artifacts: map[int64]map[string]bool{}, evidence: map[string]bool{}}
}

// Append numbers e, makes its output and inline valid UTF-8 and cuts them
// to their bounds, sets its OutputSHA256, checks it and writes its line.
// It returns the entry as written.
func (w *Writer) Append(e LedgerEntry) (LedgerEntry, error) {
	if w.closed {
		return LedgerEntry{}, errors.New("toolwire: append after close")
	}
	if w.n >= MaxEntries {
		return LedgerEntry{}, ErrLedgerFull
	}
	e.Seq = w.n + 1
	e.TS = e.TS.UTC().Round(0)
	args, err := compactArgs(e.Args)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("toolwire: entry: %w", err)
	}
	e.Args = args
	e.Output = bound(e.Output, MaxOutputBytes)
	e.Inline = bound(e.Inline, MaxInlineBytes)
	e.OutputSHA256 = sha256Hex([]byte(e.Output))
	if err := e.Check(); err != nil {
		return LedgerEntry{}, fmt.Errorf("toolwire: entry: %w", err)
	}
	line, err := encodeLine(EntryPrefix, entryLine{V: WireVersion, Entry: &e})
	if err != nil {
		return LedgerEntry{}, err
	}
	if w.bytes+len(line) > MaxLedgerBytes {
		return LedgerEntry{}, ErrLedgerFull
	}
	if _, err := w.w.Write(line); err != nil {
		return LedgerEntry{}, err
	}
	w.h.Write(line)
	w.n++
	w.bytes += len(line)
	if e.Refused == "" && len(e.Artifacts) > 0 {
		set := map[string]bool{}
		for _, a := range e.Artifacts {
			set[a.SHA256] = true
		}
		w.artifacts[e.Seq] = set
	}
	return e, nil
}

// Evidence writes one evidence line: data, which must be an artifact of
// entry seq already written, at most MaxEvidence lines and each image
// once.
func (w *Writer) Evidence(seq int64, data []byte) error {
	ev := Evidence{Seq: seq, SHA256: sha256Hex(data), Data: data}
	switch {
	case w.closed:
		return errors.New("toolwire: evidence after close")
	case len(w.evidence) >= MaxEvidence:
		return errors.New("toolwire: evidence: too many")
	case len(data) > MaxEvidenceBytes:
		return errors.New("toolwire: evidence: too large")
	case !w.artifacts[seq][ev.SHA256]:
		return fmt.Errorf("toolwire: evidence: entry %d recorded no such artifact", seq)
	case w.evidence[evidenceKey(ev)]:
		return errors.New("toolwire: evidence: emitted twice")
	}
	line, err := encodeLine(EvidencePrefix, evidenceLine{V: WireVersion, Evidence: ev})
	if err != nil {
		return err
	}
	if _, err := w.w.Write(line); err != nil {
		return err
	}
	w.evidence[evidenceKey(ev)] = true
	return nil
}

func evidenceKey(e Evidence) string { return fmt.Sprintf("%d/%s", e.Seq, e.SHA256) }

// Close writes the summary line and returns the summary, which the tools
// container also writes as its termination message.
func (w *Writer) Close() (Summary, error) {
	if w.closed {
		return Summary{}, errors.New("toolwire: closed twice")
	}
	w.closed = true
	s := Summary{Count: w.n, SHA256: hex.EncodeToString(w.h.Sum(nil))}
	line, err := encodeLine(EntryPrefix, entryLine{V: WireVersion, Summary: &s})
	if err != nil {
		return Summary{}, err
	}
	_, err = w.w.Write(line)
	return s, err
}

// compactArgs is args as the line will carry them: compact JSON, "{}" when
// empty.
func compactArgs(args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, args); err != nil {
		return nil, errors.New("args are not JSON")
	}
	return b.Bytes(), nil
}

// bound is s as valid UTF-8, cut on a rune boundary to at most n bytes with
// TruncationMarker when longer.
func bound(s string, n int) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	if len(s) <= n {
		return s
	}
	cut := n - len(TruncationMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + TruncationMarker
}

// entryLine is the JSON of an entry or the summary line.
type entryLine struct {
	V       int          `json:"v"`
	Entry   *LedgerEntry `json:"entry,omitempty"`
	Summary *Summary     `json:"summary,omitempty"`
}

// evidenceLine is the JSON of an evidence line.
type evidenceLine struct {
	V int `json:"v"`
	Evidence
}

// encodeLine is v as one line after prefix. HTML escaping is off: the log
// is never HTML, and escaping would let app bytes grow sixfold.
func encodeLine(prefix string, v any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(prefix)
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // Encode ends the line with "\n"
		return nil, err
	}
	return b.Bytes(), nil
}
