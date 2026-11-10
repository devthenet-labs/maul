// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// failEveryFix makes every attempt of every acceptance-fix round fail, as
// beatbattle-44's rev1 did: commit_failed, nothing pushed.
func (e *env) failEveryFix() {
	e.onAcceptanceFix(func(spec jobs.Spec) jobs.RunOutput {
		out := reportOnly(spec, buildReport)
		out.Events[0].Remediation.Outcome = envelope.OutcomeCommitFailed
		return out
	})
}
