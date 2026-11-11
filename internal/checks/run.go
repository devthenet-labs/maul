// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"path"
	"strings"
)

// RunCheck is a consumer program maul builds and runs against a
// workspace's tree (SPEC-S2 Q25): the agent supplies only the program's
// source files; maul renders the rest (for go, a go.mod requiring the
// tree's module, replaced by /work/tree, and the tree's go.sum), compiles
// it, and runs it with Args, asserting on its exit code and stdout as an
// exec check does. A check has no side: confirmation runs it at head and
// at base alike (Q26). In a cli workspace the program invokes the built
// binary by BinPlaceholder, which the tools container substitutes.
type RunCheck struct {
	// Language is the program's language, one of the languages maul
	// builds consumer programs in (LanguageGo).
	Language string   `json:"language"`
	Files    []File   `json:"files"`
	Args     []string `json:"args,omitempty"`
	// Timeout bounds the run, a Go duration of at most MaxExecTimeout.
	Timeout string     `json:"timeout,omitempty"`
	Expect  ExecExpect `json:"expect"`
}

// LanguageGo is the only language maul builds a consumer program in
// (SPEC-S2 O5); node and python are reserved until proven.
const LanguageGo = "go"

// runLanguages are the languages a run check and a library workspace may
// name.
var runLanguages = map[string]bool{LanguageGo: true}

// validateRun holds a run check to its grammar (validateRunProgram, args,
// timeout, expectation) and substance: in a cli its program names
// BinPlaceholder, and it asserts on stdout content.
func validateRun(r RunCheck, ws *Workspace) *Refusal {
	if rf := validateRunProgram(r, ws); rf != nil {
		return rf
	}
	if len(r.Args) > MaxArgs {
		return refuse(RuleGrammar, "args holds at most %d arguments", MaxArgs)
	}
	for _, a := range r.Args {
		if rf := checkArg("an argument", a, ws); rf != nil {
			return rf
		}
	}
	if rf := validateTimeout(r.Timeout); rf != nil {
		return rf
	}
	if rf := validateExecExpect(r.Expect); rf != nil {
		return rf
	}
	if ws.Binary != "" && !programNames(r.Files, BinPlaceholder) {
		return refuse(RuleNotSubstantive, "in a cli workspace a run check's program invokes the built binary as %s",
			BinPlaceholder)
	}
	if r.Expect.StdoutContains == "" && !contentJSON(r.Expect.StdoutJSON) {
		return refuse(RuleNotSubstantive, "no assertion on stdout; an exit code alone only says the run worked")
	}
	return runScoped("stdoutJSON", r.Expect.StdoutJSON)
}

// validateRunProgram holds a run check's language to one maul builds
// programs in (and, in a library, the library's) and its files to 1 to
// MaxFiles source files of that language.
func validateRunProgram(r RunCheck, ws *Workspace) *Refusal {
	if !runLanguages[r.Language] {
		return refuse(RuleGrammar, "language %q is not one maul builds a program in: %s", r.Language, LanguageGo)
	}
	if ws.Language != "" && r.Language != ws.Language {
		return refuse(RuleGrammar, "language %q is not the library's, %q", r.Language, ws.Language)
	}
	if len(r.Files) == 0 {
		return refuse(RuleGrammar, "a run check holds its program: 1 to %d files", MaxFiles)
	}
	if rf := validateFiles(r.Files); rf != nil {
		return rf
	}
	for _, f := range r.Files {
		if rf := checkRunFile(r.Language, f); rf != nil {
			return rf
		}
		if rf := checkProgramPlaceholders(f, ws); rf != nil {
			return rf
		}
	}
	return nil
}

// checkRunFile holds a program file to its language: for go, a .go
// source file; go.mod and go.sum are maul's, never the agent's.
func checkRunFile(lang string, f File) *Refusal {
	if lang == LanguageGo && path.Ext(f.Path) != ".go" {
		return refuse(RuleGrammar, "program file %q is not a .go source file; maul writes go.mod and go.sum", f.Path)
	}
	return nil
}

// programNames reports whether one of the program files names s.
func programNames(fs []File, s string) bool {
	for _, f := range fs {
		if strings.Contains(f.Content, s) {
			return true
		}
	}
	return false
}
