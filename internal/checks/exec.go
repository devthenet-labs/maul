// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// ExecCheck is a program run in the slot's sandbox container, or a cli
// workspace's built binary (argv[0] BinPlaceholder): Argv, run without a
// shell in a directory holding Files, whose exit code and stdout the tools
// container asserts on.
type ExecCheck struct {
	Argv  []string `json:"argv"`
	Files []File   `json:"files,omitempty"`
	// Timeout bounds the run, a Go duration of at most MaxExecTimeout.
	Timeout string     `json:"timeout,omitempty"`
	Expect  ExecExpect `json:"expect"`
}

// ExecMarkerPlaceholder is what an exec check's program files write where
// they print the assertion marker: maul replaces it, in the files only, with
// the attempt's marker (ExecMarker of a nonce drawn afresh for every
// attempt, never sent to the application), so only the program can print it
// (PR #320 review F1). A program prints "{{marker}} <id>: <what it saw>"
// when it reached and judged one of its own assertions and found the
// outcome does not hold, <id> naming that assertion (lowercase letters,
// digits and hyphens, at most 32, starting with a letter). The output is the
// program's stdout and stderr combined, so text the application controls
// that the program echoes (a body, a traceback) lands there too; it cannot
// know the nonce, so it can never pass for a marker.
//
// A run whose output has a line that starts with the attempt's marker fails
// the check, whatever its exit code and its expect (ExecMarked, S1 demo
// #57: a program that prints it and goes on to exit 0 is no pass). Only such
// a run whose marker names a valid id is red by assertion, its assertion
// that id; an exit code alone, a crash, a traceback or missing output never
// is: a broken program fails the same way. The marker is the program's own
// word, so the check reviewer judges the condition it is printed under.
const ExecMarkerPlaceholder = "{{marker}}"

// execMarkerPrefix starts every attempt's marker.
const execMarkerPrefix = "MAUL-ASSERTION-FAILED-"

// ExecMarker is the marker of the attempt whose nonce is nonce: the line
// start its program's ExecMarkerPlaceholder stands for.
func ExecMarker(nonce string) string { return execMarkerPrefix + nonce + ":" }

// ExecProgram is a program file's content with ExecMarkerPlaceholder
// replaced by the attempt's marker; nothing else is touched.
func ExecProgram(content, marker string) string {
	return strings.ReplaceAll(content, ExecMarkerPlaceholder, marker)
}

// validMarker reports a marker ExecMarker could have made with a nonce.
func validMarker(marker string) bool {
	return len(marker) > len(execMarkerPrefix)+1 && strings.HasPrefix(marker, execMarkerPrefix) &&
		strings.HasSuffix(marker, ":")
}

// MaxExecAssertionIDLen bounds a marker's assertion id: lowercase letters,
// digits and hyphens, starting with a letter. It is no outcome or check id,
// so it is not held to their 32; an evaluator names an assertion after what
// it judges, and an id past the bound silently stopped counting (gate
// D-B2). "marker <id>" stays well inside toolwire.MaxAssertionIDBytes.
const MaxExecAssertionIDLen = 128

// execAssertionID is a marker's assertion id.
var execAssertionID = regexp.MustCompile(fmt.Sprintf(`^[a-z][a-z0-9-]{0,%d}$`, MaxExecAssertionIDLen-1))

// ExecAssertion is the id of the assertion a program's output says failed:
// the first line that starts with the attempt's marker and goes on
// "<id>: <what>" with a valid id (PR #320 review F6). ok is false when no
// line does.
func ExecAssertion(output, marker string) (string, bool) {
	if !validMarker(marker) {
		return "", false
	}
	for line := range strings.SplitSeq(output, "\n") {
		rest, found := strings.CutPrefix(strings.TrimSuffix(line, "\r"), marker)
		if !found {
			continue
		}
		if id, _, ok := strings.Cut(strings.TrimSpace(rest), ":"); ok && execAssertionID.MatchString(id) {
			return id, true
		}
	}
	return "", false
}

// ExecMarked reports whether a program's output has a line that starts
// with the attempt's marker, naming a valid id or not: the program found
// the outcome does not hold, so the check fails.
func ExecMarked(output, marker string) bool {
	if !validMarker(marker) {
		return false
	}
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}

// File is one program file, at a relative path under the run directory.
type File struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Executable bool   `json:"executable,omitempty"`
}

// ExecExpect is what a run must show: its exit code, text its stdout
// contains, and values at JSON pointers into its stdout, with the http
// form's matchers.
type ExecExpect struct {
	Exit           *int32                    `json:"exit,omitempty"`
	StdoutContains string                    `json:"stdoutContains,omitempty"`
	StdoutJSON     map[string]httpstep.Value `json:"stdoutJSON,omitempty"`
}

// Bounds of the exec form.
const (
	// MaxArgs bounds argv.
	MaxArgs = 16
	// MaxFiles bounds the program files.
	MaxFiles = 8
	// MaxExecTimeout bounds an exec check's timeout.
	MaxExecTimeout = 5 * time.Minute
)

var (
	filePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}(/[A-Za-z0-9_][A-Za-z0-9._-]{0,63}){0,3}$`)
	// trivialPrograms prove nothing whatever they print.
	trivialPrograms = map[string]bool{"true": true, "false": true, ":": true, "exit": true, "echo": true,
		"printf": true, "sleep": true, "yes": true, "test": true, "[": true}
	shells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ash": true}
)

// validateExec holds an exec check to its grammar and substance: in a
// preview slot (ws nil) as S1 did; in a cli workspace its argv[0] is
// exactly BinPlaceholder, the built binary (SPEC-S2 S2-04).
func validateExec(e ExecCheck, ws *Workspace) *Refusal {
	if len(e.Argv) == 0 || len(e.Argv) > MaxArgs || e.Argv[0] == "" {
		return refuse(RuleGrammar, "argv holds 1 to %d arguments, the first not empty", MaxArgs)
	}
	for _, a := range e.Argv {
		if r := checkArg("an argument", a, ws); r != nil {
			return r
		}
	}
	if r := validateFiles(e.Files); r != nil {
		return r
	}
	for _, f := range e.Files {
		if r := checkProgramPlaceholders(f, ws); r != nil {
			return r
		}
	}
	if e.Timeout != "" {
		d, err := time.ParseDuration(e.Timeout)
		if err != nil || d <= 0 || d > MaxExecTimeout {
			return refuse(RuleGrammar, "timeout %q must be a duration in (0, %s]", e.Timeout, MaxExecTimeout)
		}
	}
	if r := validateExecExpect(e.Expect); r != nil {
		return r
	}
	if ws != nil && e.Argv[0] != BinPlaceholder {
		return refuse(RuleNotSubstantive, "in a cli workspace an exec check runs the built binary: argv[0] is %s",
			BinPlaceholder)
	}
	if trivial(e.Argv) {
		return refuse(RuleNotSubstantive, "argv %q runs a program that proves nothing; run a client against the "+
			"preview", e.Argv)
	}
	if e.Expect.StdoutContains == "" && !contentJSON(e.Expect.StdoutJSON) {
		return refuse(RuleNotSubstantive, "no assertion on stdout; an exit code alone only says the run worked")
	}
	return runScoped("stdoutJSON", e.Expect.StdoutJSON)
}

func validateFiles(fs []File) *Refusal {
	if len(fs) > MaxFiles {
		return refuse(RuleGrammar, "more than %d program files", MaxFiles)
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if !filePattern.MatchString(f.Path) || seen[f.Path] {
			return refuse(RuleGrammar, "program file path %q is not a unique relative path of at most 4 plain "+
				"segments", f.Path)
		}
		seen[f.Path] = true
		if !utf8.ValidString(f.Content) || strings.ContainsRune(f.Content, 0) {
			return refuse(RuleGrammar, "program file %q is not UTF-8 text", f.Path)
		}
	}
	return nil
}

func validateExecExpect(e ExecExpect) *Refusal {
	if e.Exit != nil && (*e.Exit < 0 || *e.Exit > 255) {
		return refuse(RuleGrammar, "exit is 0 to 255")
	}
	if r := checkText("stdoutContains", e.StdoutContains); r != nil {
		return r
	}
	return validateExpectedJSON("stdoutJSON", e.StdoutJSON, nil)
}

// trivial reports a program that proves nothing: true, echo and their
// kin, directly or as a shell's -c script.
func trivial(argv []string) bool {
	prog := path.Base(argv[0])
	if trivialPrograms[prog] {
		return true
	}
	if shells[prog] && len(argv) >= 3 && argv[1] == "-c" {
		fields := strings.Fields(argv[2])
		return len(fields) == 0 || trivialPrograms[fields[0]]
	}
	return false
}
