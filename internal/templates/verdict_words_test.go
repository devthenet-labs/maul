// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"regexp"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// overstating are the words no surface of the observed evaluator may use of
// a result: maul checks only that a verdict cites the evaluator's own calls,
// never that a call shows what the verdict says, so nothing it shows may
// read as proof. The negated label "not checked by maul" (owner decision 5)
// is the one use of "checked by maul" allowed, and is taken out first.
var overstating = regexp.MustCompile(`(?i)\b(confirmed|verified|verifies|proves?|proven|proof|re-ran|reran|` +
	`rerun|re-run)\b|checked by maul`)

// overstates is the overstating words s uses, the negated label aside.
func overstates(s string) []string {
	return overstating.FindAllString(strings.ReplaceAll(s, "not checked by maul", ""), -1)
}

// observedReasons are the reasons acceptance.Judge decides with (W1): the
// five new ones and the ones it reuses.
var observedReasons = []string{"seen-to-hold", "seen-not-to-hold", "not-reported", "evaluator-unsure",
	"no-valid-evidence", "manual", "env-failed", "infra", "tools-build-failed", "held-finding"}

// retiredWhys are the hand-off reasons only the saved-check machinery gave
// (a replay round cut short, a suspected defect): the observed evaluator
// never settles a round with them.
var retiredWhys = map[string]bool{HandoffWhyReplay: true, HandoffWhySuspected: true}

// retiredCauses are the settle causes only the saved-check machinery
// recorded.
var retiredCauses = map[v1alpha1.SettleCause]bool{v1alpha1.SettleReplayIncomplete: true}

// TestVerdictWordsNeverOverstate: every string the observed evaluator's
// surfaces build (its reasons, the outcome count, every hand-off label and
// sentence, every settle cause's words, every progress line, an outcome row
// of every result and the comment's own sentences) is free of the words
// that would read as proof.
func TestVerdictWordsNeverOverstate(t *testing.T) {
	check := func(what, s string) {
		t.Helper()
		if s == "" {
			t.Errorf("%s: no words", what)
			return
		}
		if bad := overstates(s); len(bad) > 0 {
			t.Errorf("%s says %q: %q", what, bad, s)
		}
	}
	for _, r := range observedReasons {
		check("reason "+r, outcomeReasonWords[r])
	}
	check("OutcomeCount", OutcomeCount(2, 3))
	whys := []string{""}
	for why := range handoffWhyWords {
		if !retiredWhys[why] {
			whys = append(whys, why)
		}
	}
	for _, why := range whys {
		for _, ws := range []bool{false, true} {
			for _, state := range []string{HandoffReady, HandoffUnconfirmed, HandoffChecking} {
				h := PreviewHandoff{State: state, Why: why, Doubted: 2}
				check("hand-off "+state+"/"+why, labelIn(h, ws))
				check("workspace label "+state+"/"+why, workspaceLabel(h))
			}
			if c := UnconfirmedCause(why, ws); c != "" {
				check("unconfirmed cause "+why, c)
			}
		}
	}
	check("held link", handoffHeld("The preview"))
	check("workspace status, early", workspaceStatusSentence(nil))
	check("workspace status, held", workspaceStatusSentence(&PreviewHandoff{State: HandoffHeld}))
	check("workspace comment, held", workspaceCommentSentence(&PreviewHandoff{State: HandoffHeld}, "`abc1234`"))
	for c := range settleWords {
		if retiredCauses[c] {
			continue
		}
		for _, ws := range []bool{false, true} {
			w := CauseWords(c, ws)
			check("settle "+string(c)+" short", w.Short)
			if w.Why != "" {
				words, _ := whyWords(w.Why, ws)
				check("settle "+string(c)+" why", words)
			}
		}
	}
	for _, state := range []string{VerifyWaiting, VerifyRunning, VerifyPassed, VerifyFailing, VerifyExhausted,
		VerifyUnavailable, VerifyPreviewNotLive, VerifyWaitingForCI, VerifyNeedsReplan, VerifyNotDeclared,
		VerifyNotChecked, VerifyUnreadable, VerifyCriteriaReplan, VerifyEvaluating, VerifyUnconfirmed,
		VerifyHeadsMoved} {
		for _, why := range whys {
			p := VerifyProgress{State: state, Verification: 2, FixesUsed: 1, MaxFixes: 2, Passing: 3, Total: 3,
				ManualOpen: 1, AutoFix: FixRunning, Outcomes: true, Why: why}
			check("progress "+state+"/"+why, verifyLine(p))
		}
	}
	for _, result := range []string{OutcomeResultPassed, OutcomeResultFailed, OutcomeResultUnsure,
		OutcomeResultAccepted} {
		for _, reason := range observedReasons {
			s := VerifySection{Observed: true, Outcomes: []VerifyOutcomeResult{{ID: "o", Statement: "It holds.",
				Result: result, Reason: reason, AcceptedAt: "0123456789abcdef", Note: "I saw it.",
				Cited: []VerifyCitedCall{{Seq: 4, Verb: "http", Input: "GET /", Output: "ok"}}}}}
			out, err := renderVerifySection(&s)
			if err != nil {
				t.Fatal(err)
			}
			check("observed row "+result+"/"+reason, out)
		}
	}
	repro, err := RenderCitedReproduction(testCitedReproduction())
	if err != nil {
		t.Fatal(err)
	}
	check("cited reproduction", repro)
}

// TestObservedStatusFitsGitHub: every commit status description the
// controller composes from these words (maul/acceptance, at most 140
// characters on GitHub) fits at its longest: the count at its widest, the
// longest cause, and the longest login with others.
func TestObservedStatusFitsGitHub(t *testing.T) {
	const limit = 140
	count := OutcomeCount(20, 20)
	longestCause := ""
	for why := range handoffWhyWords {
		for _, ws := range []bool{false, true} {
			if c := UnconfirmedCause(why, ws); len(c) > len(longestCause) {
				longestCause = c
			}
		}
	}
	descriptions := make([]string, 0, 3+2*len(settleWords))
	descriptions = append(descriptions, count, "maul couldn't confirm this: "+count+", but "+longestCause,
		count+"; some accepted by hand by "+strings.Repeat("l", 39)+" and 19 others")
	for c := range settleWords {
		for _, ws := range []bool{false, true} {
			descriptions = append(descriptions, CauseWords(c, ws).Short)
		}
	}
	for _, d := range descriptions {
		if len(d) > limit {
			t.Errorf("%d characters, over %d: %q", len(d), limit, d)
		}
	}
	if count != "20/20 outcomes seen to hold" {
		t.Errorf("OutcomeCount = %q, want the outcomes seen to hold", count)
	}
}
