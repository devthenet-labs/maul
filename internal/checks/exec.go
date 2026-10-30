// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// ExecCheck is a program run in the slot's sandbox container: Argv, run
// without a shell in a directory holding Files, whose exit code and
// stdout the tools container asserts on.
type ExecCheck struct {
	Argv  []string `json:"argv"`
	Files []File   `json:"files,omitempty"`
	// Timeout bounds the run, a Go duration of at most MaxExecTimeout.
	Timeout string     `json:"timeout,omitempty"`
	Expect  ExecExpect `json:"expect"`
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
	Exit           *int32                      `json:"exit,omitempty"`
	StdoutContains string                      `json:"stdoutContains,omitempty"`
	StdoutJSON     map[string]acceptance.Value `json:"stdoutJSON,omitempty"`
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

func validateExec(e ExecCheck) *Refusal {
	if len(e.Argv) == 0 || len(e.Argv) > MaxArgs || e.Argv[0] == "" {
		return refuse(RuleGrammar, "argv holds 1 to %d arguments, the first not empty", MaxArgs)
	}
	for _, a := range e.Argv {
		if r := checkText("an argument", a); r != nil {
			return r
		}
	}
	if r := validateFiles(e.Files); r != nil {
		return r
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
