// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
)

// A build run's delegation grant is recorded when the run is created
// (Settings.grant in createRun, leaseReviseRun and createRoleRun), and the
// launch re-checks only the flags, never the Project. The pages people and
// agents read must say so, and say that the grant needs the Project's
// opt-in as well as the flag.
func TestDelegationDocsStateWhenTheGrantIsRecorded(t *testing.T) {
	prose := func(path string) string { return strings.Join(strings.Fields(repoFile(t, path)), " ") }

	page := prose("docs/intents/project-settings.md")
	for _, want := range []string{
		"records what it was granted when it is created",
		"runs created after the save",
		"still waiting to launch",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/intents/project-settings.md does not state %q", want)
		}
	}
	if strings.Contains(page, "granted when it starts") {
		t.Error("docs/intents/project-settings.md says a run records its grant when it starts; it is recorded at creation")
	}

	operator := prose("docs/configuration/intent-controller.md")
	if want := "records its grant when it is created"; !strings.Contains(operator, want) {
		t.Errorf("docs/configuration/intent-controller.md does not state %q", want)
	}

	_, note, ok := strings.Cut(prose("AGENTS.md"), "intent_subagents_test.go runs")
	if !ok {
		t.Fatal("AGENTS.md has no note on intent_subagents_test.go")
	}
	note, _, _ = strings.Cut(note, "_test.go")
	for _, want := range []string{"spec.delegation.subagents", "--intent-build-subagents", "no opt-in", "status.admits"} {
		if !strings.Contains(note, want) {
			t.Errorf("AGENTS.md's note on intent_subagents_test.go does not state %q: %s", want, note)
		}
	}
}
