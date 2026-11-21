// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// libRepoURL is a second Project repository nothing evaluates: no preview,
// no surface.
const libRepoURL = "https://github.com/acme/lib"

// withUnevaluatedLib adds the lib repository to the stored Project, beside
// the one its tool host evaluates, and an open pull request of the intent
// in it at head. A single-repository Project's spec.preview becomes the
// app repository's own, as a multi-repository Project states it.
func (e *env) withUnevaluatedLib(name, head string) {
	e.t.Helper()
	proj := e.getProject()
	if proj.Spec.Preview != nil {
		proj.Spec.Repositories[0].Preview = &v1alpha1.ProjectRepositoryPreview{ProjectPreview: *proj.Spec.Preview}
		proj.Spec.Preview = nil
	}
	if s := proj.Spec.Surfaces; s != nil && s.CLI != nil {
		s.CLI.Repository = proj.Spec.Repositories[0].Name
	}
	proj.Spec.Repositories = append(proj.Spec.Repositories, v1alpha1.ProjectRepository{Name: "lib", URL: libRepoURL})
	if err := e.c.Update(context.Background(), proj); err != nil {
		e.t.Fatal(err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.PullRequests = append(in.Status.PullRequests, v1alpha1.IntentPullRequest{Repository: libRepoURL,
			Number: 99, State: prOpen, HeadSHA: head})
	})
}

// syncAcceptance runs the maul/acceptance sync once over name.
func (e *env) syncAcceptance(name string) {
	e.t.Helper()
	if _, err := e.prVerifyPass(name).syncAcceptanceStatuses(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// TestAcceptanceStatusOnlyOnEvaluatedHeads (code review 2026-11-18, rounds
// 3; SM-2): a judgement is of the head set and of nothing else. A pull
// request of the intent in a repository outside it (no preview on a
// previewed Project; not the surface's on a cli Project) carries commits
// nothing checked, so maul/acceptance is never posted there, at its first
// head or after a push; and a status maul posted there before is answered
// once by a final error, never left green.
func TestAcceptanceStatusOnlyOnEvaluatedHeads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready func(*testing.T) (*env, string)
	}{
		{"slot", readySlotJudgement},
		{"agents", readyAgentsJudgement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := tc.ready(t)
			appHead := e.get(name).Status.PullRequests[0].HeadSHA
			first, pushed, later := strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40)
			e.withUnevaluatedLib(name, first)

			e.syncAcceptance(name)
			if st := e.latestAcceptance(appHead); st.State != statusSuccess {
				t.Fatalf("maul/acceptance on the evaluated head = %+v, want success (the control)", st)
			}
			if got := e.acceptanceStatuses(first); len(got) != 0 {
				t.Errorf("maul/acceptance on a head outside the evaluated set = %+v, want none", got)
			}

			// A push to the unevaluated pull request moves no head set.
			before := e.get(name).Status.Verification.HeadSetDigest
			e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.PullRequests[1].HeadSHA = pushed })
			e.syncAcceptance(name)
			if got := e.acceptanceStatuses(pushed); len(got) != 0 {
				t.Errorf("maul/acceptance on a commit pushed outside the evaluated set = %+v, want none", got)
			}
			if p := e.prVerifyPass(name); !p.atHeads(p.in.Status.Verification) ||
				p.in.Status.Verification.HeadSetDigest != before {
				t.Fatal("the push moved the head set: the case no longer tests an unevaluated repository")
			}
			if st := e.latestAcceptance(appHead); st.State != statusSuccess {
				t.Errorf("maul/acceptance on the evaluated head after the push = %+v, want success still", st)
			}

			// A success an earlier release posted there is answered, once.
			e.gh.mu.Lock()
			e.gh.nextStatusID++
			stale := e.gh.nextStatusID
			e.gh.statuses[pushed] = []ghclient.CommitStatus{{ID: stale, Context: acceptanceContext,
				State: statusSuccess, Description: "3/3 outcomes confirmed", Creator: "maul[bot]"}}
			e.gh.mu.Unlock()
			e.setStatus(name, func(in *v1alpha1.Intent) {
				pr := &in.Status.PullRequests[1]
				pr.AcceptanceStatusID, pr.AcceptanceHead, pr.AcceptanceDigest = stale, pushed, "sha256:stale"
			})
			e.syncAcceptance(name)
			st := e.latestAcceptance(pushed)
			if st.State != statusError || !strings.Contains(st.Description, "does not evaluate this repository") {
				t.Errorf("a stale success outside the evaluated set was answered by %+v, want a final error", st)
			}
			e.syncAcceptance(name)
			if got := e.acceptanceStatuses(pushed); len(got) != 2 {
				t.Errorf("statuses at the answered head = %d, want 2 (the stale one and one answer)", len(got))
			}

			// The next push there starts clean again.
			e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.PullRequests[1].HeadSHA = later })
			e.syncAcceptance(name)
			if got := e.acceptanceStatuses(later); len(got) != 0 {
				t.Errorf("maul/acceptance on a later push outside the evaluated set = %+v, want none", got)
			}
		})
	}
}
