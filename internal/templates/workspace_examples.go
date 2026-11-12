// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

// The examples the workspace evaluate prompt shows (SPEC-S2), one set per
// surface: a cli's exec and run checks and a library's run check, and a
// report naming them. Tests hold each check to checks.Validate in its
// workspace and each report to report.ParseEvaluation, so the prompt never
// teaches a refused shape.
const (
	ExampleCLIExecCheck = `{
  "id": "describe-bpm-json",
  "outcome": "describe-bpm",
  "form": "exec",
  "exec": {
    "argv": ["{{bin}}", "--describe", "--bpm", "90", "--name", "{{run}}-beat", "kick.json"],
    "files": [{"path": "kick.json", "content": "{\"steps\": [1, 0, 0, 0, 1, 0, 0, 0]}\n"}],
    "timeout": "30s",
    "expect": {"exit": 0, "stdoutJSON": {"/name": "{{run}}-beat", "/bpm": 90, "/steps": 8}}
  }
}`
	ExampleCLIRunCheck = `{
  "id": "onsets-500ms-wav",
  "outcome": "onsets-500ms",
  "form": "run",
  "run": {
    "language": "go",
    "files": [{"path": "main.go", "content":
      "package main\n// runs {{bin}} on a kick it writes; prints {run, gapMs} or {{marker}} onsets: <what>\n"}],
    "args": ["{{run}}"],
    "timeout": "30s",
    "expect": {"exit": 0, "stdoutJSON": {"/run": "{{run}}", "/gapMs": {"$approx": 500, "$tolerance": 5}}}
  }
}`
	ExampleLibraryRunCheck = `{
  "id": "parse-too-long-errors",
  "outcome": "parse-too-long",
  "form": "run",
  "run": {
    "language": "go",
    "files": [{"path": "main.go", "content":
      "package main\n// Parse of 64 and 65 steps; prints {run, ok64, tooLong65} or {{marker}} too-long: <err>\n"}],
    "args": ["{{run}}"],
    "timeout": "30s",
    "expect": {"exit": 0, "stdoutJSON": {"/run": "{{run}}", "/ok64": true, "/tooLong65": true}}
  }
}`
	ExampleCLIEvaluationReport = `---
verdicts:
  - outcome: "onsets-500ms"
    verdict: pass
    ledger: [7, 9]
    note: "At 120 BPM the kick onsets are 500 ms apart at the head; at the base they are 250 ms apart."
  - outcome: "describe-bpm"
    verdict: fail
    ledger: [12, 13]
    note: "--describe prints the tempo the pattern was saved with, 120, not the --bpm 90 it was given."
  - outcome: "sounds-good"
    verdict: unsure
    ledger: []
    note: "Only a person can judge this."
disputes: []
proposed:
  - id: "onsets-500ms-wav"
    outcome: "onsets-500ms"
    file: "/work/scratch/checks/onsets-500ms-wav.json"
  - id: "describe-bpm-json"
    outcome: "describe-bpm"
    file: "/work/scratch/checks/describe-bpm-json.json"
    expect: fail
retire: []
anomalies: []
---

What I checked, in a few lines.
`
	ExampleLibraryEvaluationReport = `---
verdicts:
  - outcome: "parse-too-long"
    verdict: pass
    ledger: [6, 8]
    note: "64 steps parse and 65 return ErrTooLong; the base has no ErrTooLong, so there is no base comparison."
  - outcome: "parse-keeps-names"
    verdict: unsure
    ledger: []
    note: "The clause 'from any file' could not be exercised: the library reads no file."
disputes: []
proposed:
  - id: "parse-too-long-errors"
    outcome: "parse-too-long"
    file: "/work/scratch/checks/parse-too-long-errors.json"
retire: []
anomalies: []
---

What I checked, in a few lines.
`
)

// The outcome lists the plan prompt shows for a Project with no preview,
// one per surface; a test holds each to report.ParsePlan. Each models the
// prompt's rules: a command line or an exported call with inputs other than
// the defaults, what comes out, a rule the request states with its example
// as one instance (G-B-LIB-2), a keep naming one action, and a manual
// outcome only a person can judge.
const (
	ExamplePlanOutcomesCLI = `acceptance:
  - id: onsets-follow-bpm
    outcome: "` + "`bbrender --bpm <n> kick.json`" + ` writes kick onsets 60000/n ms apart, for example 667 ms ` +
		`at ` + "`--bpm 90`" + `."
    from: "bbrender should take --bpm and put the kicks one beat apart at that tempo"
    kind: change
  - id: default-bpm-kept
    outcome: "` + "`bbrender kick.json`" + ` still writes a WAV whose kick onsets are 500 ms apart."
    kind: keep
  - id: sounds-good
    outcome: "The rendered kick sounds good."
    from: "make the kick punchier"
    manual: true
    reason: "only a person can judge how it sounds"`
	ExamplePlanOutcomesLibrary = `acceptance:
  - id: parse-too-long
    outcome: "` + "`pattern.Parse`" + ` of any pattern over 64 steps (for example 65) returns an error matching ` +
		"`ErrTooLong`" + `."
    from: "reject patterns over 64 steps"
    kind: change
  - id: parse-64-kept
    outcome: "` + "`pattern.Parse`" + ` of a pattern of 64 steps still returns a pattern of 64 steps and no error."
    kind: keep`
)

// Workspace is the Project's cli or library surface when its outcomes are
// checked there (SPEC-S2), for the plan prompt's template; nil otherwise.
func (p PlanPrompt) Workspace() *PlanWorkspace {
	if p.Verification == nil {
		return nil
	}
	return p.Verification.Workspace
}

// CLIOutcomesExample and LibraryOutcomesExample are the outcome lists, for
// the plan prompt's template.
func (PlanPrompt) CLIOutcomesExample() string     { return ExamplePlanOutcomesCLI }
func (PlanPrompt) LibraryOutcomesExample() string { return ExamplePlanOutcomesLibrary }
