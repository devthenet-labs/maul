// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

// WrapUpPrompt is the data for the one message an intent stage's agent is
// sent, in its own session, when its first run stopped at its turn limit
// (claude's error_max_turns): a plan is told to write its report from what
// it has read, a build to make its report match the working tree as it
// stands, with success false if the work is unfinished. agent-runner sends
// it at most once, with the turns held back from the stage's grant for it
// (StageLimits.WrapUpTurns).
//
// It carries patchy's fixed text, paths and numbers alone, nothing the
// agent or the repository wrote, so nothing in it is fenced.
type WrapUpPrompt struct {
	// ReportPath is the report the stage's instructions asked for.
	ReportPath string
	// CommitScriptPath names commit.sh on a build; empty on a plan.
	CommitScriptPath string
	// FirstTurns is the turn limit the first run reached.
	FirstTurns int
	// Turns is the most the wrap-up run may take.
	Turns int
	// ReportWritten says the build's report was on disk when the run
	// stopped, so the prompt asks for it to be checked against the tree
	// rather than written.
	ReportWritten bool
}

// RenderWrapUpPrompt renders the wrap-up prompt.
func RenderWrapUpPrompt(p WrapUpPrompt) (string, error) {
	return render("prompt_wrapup.md.tmpl", p)
}
