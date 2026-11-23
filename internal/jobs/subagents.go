// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"errors"
	"fmt"
)

// ErrSubagentsRefused wraps the refusal of a Spec's BuildSubagents on a Job
// that is not an intent build on claude: a contradiction in what the
// controller asked for, never a transient failure, so a caller settles the
// launch instead of retrying it. agent-runner refuses the same grant on the
// same Jobs (agentrun.FromEnv); this keeps such a Job from being created.
var ErrSubagentsRefused = errors.New("jobs: subagents refused")

// subagentsRefusal is why spec may not carry BuildSubagents or
// BuildWorkflows, or nil. Only an intent build (its first build or a revise
// round) on claude, the one harness that renders the subagents posture, may
// carry subagents, and only a Job carrying them may carry workflows.
func subagentsRefusal(spec Spec) error {
	if spec.BuildWorkflows && !spec.BuildSubagents {
		return fmt.Errorf("%w: only a build granted subagents may be granted workflows", ErrSubagentsRefused)
	}
	if !spec.BuildSubagents {
		return nil
	}
	if spec.Kind != "intent" || spec.Phase != "build" || spec.Harness != "claude" {
		return fmt.Errorf("%w: only an intent build on claude may be granted subagents, not a %s %s Job on %q",
			ErrSubagentsRefused, spec.Kind, spec.Phase, spec.Harness)
	}
	return nil
}
