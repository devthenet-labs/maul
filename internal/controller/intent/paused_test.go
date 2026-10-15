// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// pausedLine is what the issue's status comment says while the pause holds
// the intent's run.
const pausedLine = "**Agents paused:**"

// TestPausedRunIsMarkedAndShownOnTheIssue is the 0.12.34 gate's regression:
// with the agents paused an intent's plan run waited with an empty status and
// the issue said only "Planning". The held run now carries AgentsPaused, the
// status comment says the operator paused the agents, and both go once the
// pause is lifted and the run launches.
func TestPausedRunIsMarkedAndShownOnTheIssue(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	pass := func() {
		e.mustIntent(name)
		e.readyRepositories("")
		e.runRuns()
		e.mustIntent(name)
	}
	e.runs.Paused = true
	for range 4 {
		pass()
	}
	runs := e.intentRuns(name)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the one plan run", len(runs))
	}
	cond := meta.FindStatusCondition(runs[0].Status.Conditions, v1alpha1.ConditionAgentsPaused)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonAgentsPaused {
		t.Fatalf("plan run AgentsPaused = %+v, want True/Paused", cond)
	}
	if body := e.statusBody(); !strings.Contains(body, pausedLine) || !strings.Contains(body, "`Planning`") {
		t.Errorf("status comment while paused:\n%s\nwant Planning and %q", body, pausedLine)
	}

	e.runs.Paused = false
	pass()
	if n := len(e.jobs.launched()); n != 1 {
		t.Fatalf("jobs launched = %d, want the plan Job once the pause is lifted", n)
	}
	run := e.intentRuns(name)[0]
	if cond := meta.FindStatusCondition(run.Status.Conditions, v1alpha1.ConditionAgentsPaused); cond != nil {
		t.Errorf("plan run AgentsPaused = %+v after the pause lifted, want none", cond)
	}
	if body := e.statusBody(); strings.Contains(body, pausedLine) {
		t.Errorf("status comment after the pause lifted still says paused:\n%s", body)
	}
}
