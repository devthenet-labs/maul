// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"reflect"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// misplacedReply is what the reply to a misplaced command says.
const misplacedReply = "was not read as a command, and nothing was done"

// TestMisplacedCommandOnThePullRequest: a command below other text on an
// intent's pull request is never read as one, so no round starts, also with
// verification off. An approver's is answered once on the pull request with
// where commands go and the pull request's commands, and again by no later
// poll; anyone else's, a quoted one, and one by a listed approver GitHub
// gives no write access get nothing at all, not even a reaction.
func TestMisplacedCommandOnThePullRequest(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.reviewing()
	n := e.get(name).Status.PullRequests[0].Number
	revise := e.prCommand(n, approver, "Please also cover the error path.\n/maul revise")
	retry := e.prCommand(n, approver, "That failed.\n\n/maul retry")
	other := e.prCommand(n, "anyone", "I agree.\n/maul revise")
	quoted := e.prCommand(n, approver, "You said:\n> /maul revise")
	e.review(name, 2)
	e.gh.perms[approver] = ghclient.PermissionRead
	readOnly := e.prCommand(n, approver, "On second thought:\n/maul revise")
	e.review(name, 2)

	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview || len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 0 {
		t.Fatalf("phase %s, %d revise runs: a misplaced command acted", in.Status.Phase,
			len(e.runsOf(name, v1alpha1.IntentStageRevise)))
	}
	for _, id := range []int64{revise, retry} {
		r := e.prReplies(n, id)
		if len(r) != 1 || !strings.Contains(r[0], misplacedReply) || !strings.Contains(r[0], "/maul revise") ||
			strings.Contains(r[0], "/maul replan") || e.gh.reactions[id] != 1 {
			t.Errorf("comment %d: replies %q, reactions %d; want one reply naming the pull request's commands", id,
				r, e.gh.reactions[id])
		}
	}
	for _, id := range []int64{other, quoted, readOnly} {
		if r := e.prReplies(n, id); len(r) != 0 || e.gh.reactions[id] != 0 {
			t.Errorf("comment %d: replies %q, reactions %d; want nothing", id, r, e.gh.reactions[id])
		}
	}
}

// TestMisplacedAcceptOnThePullRequest: a misplaced /maul accept all or
// /maul verify fix on a verified intent's pull request records nothing (no
// acceptance, no applied command, no verification change) and is answered
// once.
func TestMisplacedAcceptOnThePullRequest(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithOutcomes()
	e.acceptReady(name)
	before := e.get(name).Status
	n := before.PullRequests[0].Number
	accept := e.prCommand(n, approver, "These look fine to me.\n/maul accept all")
	verify := e.prCommand(n, approver, "Try again please.\n/maul verify fix")
	e.settlePR(name)
	e.settlePR(name)

	after := e.get(name).Status
	if !reflect.DeepEqual(after.Acceptance, before.Acceptance) ||
		!reflect.DeepEqual(after.Verification, before.Verification) ||
		!reflect.DeepEqual(after.Commands, before.Commands) {
		t.Fatalf("a misplaced command changed the status:\nacceptance %+v\nverification %+v\ncommands %+v",
			after.Acceptance, after.Verification, after.Commands)
	}
	for _, id := range []int64{accept, verify} {
		if r := e.prReplies(n, id); len(r) != 1 || !strings.Contains(r[0], misplacedReply) {
			t.Errorf("comment %d: replies %q; want one reply saying where commands go", id, r)
		}
	}
}
