// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestEndedEarlierJudgementKeepsItsWords (W13): an intent that ended
// under an earlier release keeps a judgement those rules reached, with no
// judge marker (the supersede never runs on an ended intent). Its status
// comment, re-rendered on this release's first pass, says what is true of
// that record, in the earlier release's words, so a comment that release
// wrote is left untouched; the same record judged under this release's
// rules reads in this release's words.
func TestEndedEarlierJudgementKeepsItsWords(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		applyJudgement(in, passedJudgement(in))
	})
	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/maul accept "+oManual)
	e.settleActions(name)
	e.review(name, 6)
	e.gh.humanClose(1)
	e.reviewUntil(name, func() bool { return e.get(name).Status.Phase == v1alpha1.IntentClosed })
	e.settleActions(name)
	observed := e.statusBody()
	if !strings.Contains(observed, "`Closed`") || !strings.Contains(observed, "**Evaluation:** "+templates.ReadyLabel) {
		t.Fatalf("status comment of a closed intent judged under these rules:\n%s", observed)
	}

	// The same record as an earlier release left it: no judge marker, and
	// its comment in that release's words, as its digest records.
	earlier := strings.Replace(observed, templates.ReadyLabel, templates.EarlierReadyLabel, 1)
	comments := e.gh.ownComments()
	e.gh.mu.Lock()
	for _, c := range comments {
		if c.Body == observed {
			c.Body = earlier
		}
	}
	edits := e.gh.calls["EditIssueComment"]
	e.gh.mu.Unlock()
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Judge = ""
		in.Status.Tracking.StatusDigest = digest([]byte(earlier))
	})
	e.review(name, 4)
	e.gh.mu.Lock()
	after := e.gh.calls["EditIssueComment"]
	e.gh.mu.Unlock()
	if after != edits {
		t.Errorf("the status comment of an ended intent judged earlier was edited %d times on this release's "+
			"passes, want none:\n%s", after-edits, e.statusBody())
	}
	if body := e.statusBody(); body != earlier || strings.Contains(body, templates.ReadyLabel) {
		t.Errorf("status comment = \n%s\nwant the earlier release's words kept:\n%s", body, earlier)
	}
}
