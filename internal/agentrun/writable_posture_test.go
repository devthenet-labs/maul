// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

// TestWritableStagesDenyDelegation: both writable stages, the Finding
// remediation and the intent build, run claude with every tool that starts
// work off the main thread or wakes the session later denied by name beside
// the web tools: a subagent, a workflow, a wake-up, cron jobs, worktrees and
// a monitor.
// The shell stays allowed. The posture itself, and its probe against the
// real CLI, are internal/harness's.
func TestWritableStagesDenyDelegation(t *testing.T) {
	want := []string{
		"WebFetch", "WebSearch",
		"Agent", "Task", "Workflow", "ScheduleWakeup", "CronCreate", "CronDelete", "CronList",
		"EnterWorktree", "ExitWorktree", "Monitor",
	}
	for _, c := range repairCases() {
		if !c.writable {
			continue
		}
		t.Run(string(c.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := c.setup(t, &out)
			fx := &fakeExec{steps: []step{c.first(ws, c.good)}}
			if err := New(cfg, fx).Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(fx.specs) == 0 {
				t.Fatal("the stage ran no command")
			}
			argv := fx.specs[0].Argv
			if got := strings.Fields(argvFlag(t, argv, "--disallowedTools")); !slices.Equal(got, want) {
				t.Errorf("--disallowedTools = %q, want %q", got, want)
			}
			if allow := strings.Fields(argvFlag(t, argv, "--allowedTools")); !slices.Contains(allow, "Bash") {
				t.Errorf("--allowedTools = %q, want the shell allowed", allow)
			}
		})
	}
}
