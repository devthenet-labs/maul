// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

const testSessionID = "0b9a3c1e-7d2f-4e5a-9b6c-1d2e3f4a5b6c"

// TestRecordPodFacts: what the pod reports about its run is held to the
// schema rather than refused whole. Turns are clamped into range, a session
// id the schema would refuse is dropped, and so is a wrap-up whose reason it
// does not know; what is in range is copied as reported.
func TestRecordPodFacts(t *testing.T) {
	tests := []struct {
		name  string
		stage envelope.Stage
		want  v1alpha1.IntentRunStatus
	}{
		{"nothing reported", envelope.Stage{}, v1alpha1.IntentRunStatus{}},
		{"a salvaged run", envelope.Stage{NumTurns: 34, SessionID: testSessionID,
			WrapUp: &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true}},
			v1alpha1.IntentRunStatus{NumTurns: 34, SessionID: testSessionID,
				WrapUp: &v1alpha1.IntentRunWrapUp{Reason: v1alpha1.WrapUpMaxTurns, Resumed: true, Kept: true}}},
		{"negative turns", envelope.Stage{NumTurns: -5}, v1alpha1.IntentRunStatus{}},
		{"absurd turns", envelope.Stage{NumTurns: math.MaxInt}, v1alpha1.IntentRunStatus{NumTurns: maxRunTurns}},
		{"a 64-character session id", envelope.Stage{SessionID: strings.Repeat("a", 64)},
			v1alpha1.IntentRunStatus{SessionID: strings.Repeat("a", 64)}},
		{"a session id past 64", envelope.Stage{SessionID: strings.Repeat("a", 65)}, v1alpha1.IntentRunStatus{}},
		{"a session id with markup", envelope.Stage{SessionID: "<!-- @octocat -->"}, v1alpha1.IntentRunStatus{}},
		{"a session id with a newline", envelope.Stage{SessionID: "abc\ndef"}, v1alpha1.IntentRunStatus{}},
		{"a session id led by a dash", envelope.Stage{SessionID: "-rf"}, v1alpha1.IntentRunStatus{}},
		{"each known reason", envelope.Stage{WrapUp: &envelope.WrapUp{Reason: envelope.WrapUpIdle}},
			v1alpha1.IntentRunStatus{WrapUp: &v1alpha1.IntentRunWrapUp{Reason: v1alpha1.WrapUpIdle}}},
		{"an unknown reason", envelope.Stage{WrapUp: &envelope.WrapUp{Reason: "report_invalid", Kept: true}},
			v1alpha1.IntentRunStatus{}},
		{"no reason", envelope.Stage{WrapUp: &envelope.WrapUp{Kept: true}}, v1alpha1.IntentRunStatus{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// What an earlier write recorded is replaced, never kept.
			got := v1alpha1.IntentRunStatus{NumTurns: 9, SessionID: "stale",
				WrapUp: &v1alpha1.IntentRunWrapUp{Reason: v1alpha1.WrapUpBudget}}
			recordPodFacts(&got, &tt.stage)
			got.Usage = v1alpha1.UsageSummary{}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("recordPodFacts(%+v) = %+v, want %+v", tt.stage, got, tt.want)
			}
		})
	}
}

// TestRecordPodFactsProperty: whatever a pod reports, the facts recorded
// are ones the schema accepts, so a hostile envelope can never make the
// run's status write fail (and its run wedge).
func TestRecordPodFactsProperty(t *testing.T) {
	reasons := []envelope.WrapUpReason{envelope.WrapUpMaxTurns, envelope.WrapUpBudget, envelope.WrapUpDeadline,
		envelope.WrapUpIdle, "", "report_missing", "max_turns ", "MAX_TURNS"}
	alphabet := []rune("abcXYZ019-_ .\n<>@/é\u200b")
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261007))}
	prop := func(turns int, idLen uint8, idSeed int64, reason uint8, wrapped, resumed, kept bool) bool {
		rng := rand.New(rand.NewSource(idSeed))
		id := make([]rune, int(idLen)%80)
		for i := range id {
			id[i] = alphabet[rng.Intn(len(alphabet))]
		}
		st := envelope.Stage{NumTurns: turns, SessionID: string(id)}
		if wrapped {
			st.WrapUp = &envelope.WrapUp{Reason: reasons[int(reason)%len(reasons)], Resumed: resumed, Kept: kept}
		}
		var s v1alpha1.IntentRunStatus
		recordPodFacts(&s, &st)
		if s.NumTurns < 0 || s.NumTurns > maxRunTurns {
			return false
		}
		if s.SessionID != "" && (len(s.SessionID) > maxSessionID || !sessionIDPattern.MatchString(s.SessionID) ||
			s.SessionID != st.SessionID) {
			return false
		}
		if w := s.WrapUp; w != nil {
			switch w.Reason {
			case v1alpha1.WrapUpMaxTurns, v1alpha1.WrapUpBudget, v1alpha1.WrapUpDeadline, v1alpha1.WrapUpIdle:
			default:
				return false
			}
			if w.Resumed != resumed || w.Kept != kept {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// salvaged is a stage cut off at its turn limit and kept after a wrap-up.
func salvaged(stage envelope.Stage) envelope.Stage {
	stage.NumTurns, stage.SessionID = 40, testSessionID
	stage.WrapUp = &envelope.WrapUp{Reason: envelope.WrapUpMaxTurns, Resumed: true, Kept: true}
	return stage
}

// checkFacts fails unless the run recorded what salvaged reports.
func checkFacts(t *testing.T, where string, run *v1alpha1.IntentRun) {
	t.Helper()
	want := &v1alpha1.IntentRunWrapUp{Reason: v1alpha1.WrapUpMaxTurns, Resumed: true, Kept: true}
	if s := run.Status; s.NumTurns != 40 || s.SessionID != testSessionID || !reflect.DeepEqual(s.WrapUp, want) {
		t.Errorf("%s: run %s recorded turns %d, session %q, wrap-up %+v; want 40, %q, %+v", where, run.Name,
			s.NumTurns, s.SessionID, s.WrapUp, testSessionID, want)
	}
}

// TestSalvagedRunsAreRecordedAndMarked: a plan and a build whose agents were
// cut off at their turn limit and kept their reports after a wrap-up turn
// record their turns, session and wrap-up at every write of the run: the
// plan's settle, and the build's hold, push and settle. The plan comment and
// the pull request body say the run was cut off, in patchy's own words.
func TestSalvagedRunsAreRecordedAndMarked(t *testing.T) {
	e := newEnv(t, testProject())
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		out := defaultOutput(spec)
		ev := out.Events[0]
		if ev.Plan != nil {
			ev.Plan.Stage = salvaged(ev.Plan.Stage)
		} else {
			ev.Remediation.Stage = salvaged(ev.Remediation.Stage)
		}
		return out
	}
	ctx := context.Background()
	name := e.awaiting()
	checkFacts(t, "settled plan", &e.runsOf(name, v1alpha1.IntentStagePlan)[0])
	plans := e.gh.withMarker("patchy:plan")
	if len(plans) != 1 || !strings.Contains(plans[0].Body,
		"The planner was cut off before it finished: it reached its turn limit. patchy gave it one short final turn") {
		t.Errorf("plan comments = %d, want one that says its planner was cut off", len(plans))
	}

	e.gh.label(1, "patchy:approved", approver)
	build := e.buildLaunched(name)
	e.suspend(name, true)
	for range 3 {
		if _, err := e.runs.Reconcile(ctx, req(build.Name)); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(time.Minute)
	}
	held := e.runsOf(name, v1alpha1.IntentStageBuild)[0]
	if !pushHeld(&held) {
		t.Fatalf("build %s is not held", held.Name)
	}
	checkFacts(t, "held build", &held)

	e.suspend(name, false)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	done := e.runsOf(name, v1alpha1.IntentStageBuild)[0]
	if done.Status.PushedCommit == "" || done.Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("build = %s, commit %q; want complete and pushed", done.Status.Phase, done.Status.PushedCommit)
	}
	checkFacts(t, "pushed build", &done)
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	if len(e.gh.prs) != 1 || !strings.Contains(e.gh.prs[1].body,
		"The build was cut off before it finished: it reached its turn limit.") {
		t.Errorf("pull requests = %d, want one whose body says its build was cut off", len(e.gh.prs))
	}
}

// TestUncutRunsCarryNoMarker: a run that was never cut off records no
// wrap-up, and its plan comment and pull request body are exactly as
// before; a wrap-up that kept nothing marks nothing either.
func TestUncutRunsCarryNoMarker(t *testing.T) {
	for _, tt := range []struct {
		name   string
		wrapUp *v1alpha1.IntentRunWrapUp
	}{
		{"never cut off", nil},
		{"cut off, nothing kept", &v1alpha1.IntentRunWrapUp{Reason: v1alpha1.WrapUpDeadline}},
	} {
		run := &v1alpha1.IntentRun{Status: v1alpha1.IntentRunStatus{WrapUp: tt.wrapUp}}
		if c := runCutOff(run); c != nil {
			t.Errorf("%s: runCutOff = %+v, want nil", tt.name, c)
		}
	}
	e := newEnv(t, testProject())
	name := e.awaiting()
	if w := e.runsOf(name, v1alpha1.IntentStagePlan)[0].Status.WrapUp; w != nil {
		t.Errorf("plan run recorded wrap-up %+v, want none", w)
	}
	for _, c := range e.gh.withMarker("patchy:plan") {
		if strings.Contains(c.Body, "cut off") {
			t.Errorf("plan comment says its run was cut off: %q", c.Body)
		}
	}
}

// TestSalvagedRoundIsMarked: a revision round whose agent was cut off and
// kept its report records its facts with the push, and its pull request
// comment says the round was cut off after the commit it pushed.
func TestSalvagedRoundIsMarked(t *testing.T) {
	e := newEnv(t, testProject())
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		out := defaultOutput(spec)
		if ev := out.Events[0]; ev.Remediation != nil {
			ev.Remediation.Stage = salvaged(ev.Remediation.Stage)
		}
		return out
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	pr := e.drive(name, v1alpha1.IntentInReview, repoImage).Status.PullRequests[0]
	e.gh.reviews[pr.Number] = []ghclient.Review{{ID: 63, NodeID: "review-63", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Mention the endpoint too.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	round := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	checkFacts(t, "pushed round", &round)
	want := "Revision round pushed commit `" + round.Status.PushedCommit + "`. The round's agent was cut off " +
		"before it finished: it reached its turn limit."
	if got := e.roundNotices(name, pr.Number)[1]; !strings.Contains(got, want) {
		t.Errorf("round notice = %q, want it to contain %q", got, want)
	}
}
