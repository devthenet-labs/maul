// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package agentrun orchestrates the coding-agent stage inside the Job pod:
// render the prompt, drive the harness under a token-budget kill switch, and
// report the stage as an envelope event on stdout. The investigate phase
// runs the analysis and emits the verdict without ever deciding
// continuation; the remediate phase executes a controller-provided analysis,
// then verifies and packages the resulting changeset.
//
// An intent adds two phases beside them. Plan reads the request and the
// tree read-only and emits the plan report as an envelope plan event, for a
// human to approve; build follows the approved plan with the workspace
// writable — for the first build and every revise round — and packages its
// changeset through the remediation path's own helpers, as a remediation
// event. They run on the Finding stages' configuration (plan on
// investigate's, build on remediate's) and on brokered claude only.
//
// An intent may change several repositories. Its plan Job then holds every
// tree — the planning repository as the working tree, the others read-only
// under the workspace's repos/, with no git history — and a manifest of
// them, which the plan stage checks whole before any agent runs and holds
// the plan's repositories to afterwards. Each build is in one repository
// of such a plan and learns which from PATCHY_REPO alone, matched against
// the approved plan's own list: no new configuration key exists for either.
//
// The runner trusts the repository over the agent: a remediation or build
// report claiming success is downgraded unless commit.sh ran cleanly and
// left real commits on the branch. It never talks to a forge — the pod has
// no forge credentials by design; the controllers own every side effect.
//
// A report the runner refuses, missing or failing its parser, does not end
// the stage at once on a harness that can continue a session (claude, and
// the fake harness that stands in for it): the agent is asked, in its own
// session (harness.Resumer), to repair the report, for at most
// repairRounds rounds, each within what the stage has left of its turns,
// output tokens and wall clock, sharing the stage's transcript and adding
// to its recorded spend (repair.go). On a stage that writes the working
// tree a repair may change nothing in the clone, and one that does is
// refused. Only a report still refused after that ends the stage
// report_missing or report_invalid, with how the repair went after the
// reason. Codex and copilot cannot resume, so their stages end on the
// first refusal, as before.
//
// An intent stage (plan, build) never throws away work its run did before it
// was cut off (wrapup.go). Its turn grant is split (splitGrant): the first
// run gets --max-turns of the grant less a few turns held back, and every
// round after it, a wrap-up or a repair, is held to one window over what the
// grant, the output-token budget and the wall clock have left
// (intentWindow); a Finding stage keeps its repair window unchanged. A run
// stopped at its turn limit, on a clean turn boundary, is resumed once with
// the wrap-up prompt: a plan writes its report if it has none; a build makes
// its report match the working tree, which the wrap-up may not change, and
// commits only what the tree held when the wrap-up began (committedSubset).
// A run killed mid-stream (the wall clock, the idle watchdog, the kill
// switch, the broker's per-pod limit) is not resumed: a plan report it left
// is kept if it parses, since an approver reads it, and a build report is
// recorded but never packaged. The stage's event records which limit cut
// the run off and what became of it (envelope.WrapUp), and a run cut off
// before its result event still records its session and the turns its
// stream shows. A run that ends with no event at all (an OOM kill, an
// eviction, the Job deadline) records nothing here; the controller cannot
// see what it spent.
//
// While a run of an intent's stage (plan, build) runs a foreground shell
// command, the runner prints that command's output as it is produced, as
// PATCHY-OUTPUT chunks beside the turns (output.go, on a
// harness.TaskWatcher: claude, and the fake harness whose fixtures write no
// such file); a Finding's stages print none, as nothing shows it. The CLI's own file is read, never its
// stream, so the output is bounded per command and per process, and it is
// live only: never persisted, never a turn, never progress to the idle
// watchdog. All of stdout goes through one lock, so the three streams'
// lines never interleave.
package agentrun
