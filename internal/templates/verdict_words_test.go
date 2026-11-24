// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// overstating are the words no surface of the observed evaluator may use of
// a result: maul looks only at whether a verdict cites the evaluator's own
// calls, never at whether a call shows what the verdict says, so nothing it
// shows may read as proof, or as maul checking the outcomes itself: no
// form of "confirm" (confirmed, couldn't confirm, confirmation), and no
// "maul checked", "maul checks", "maul is still checking", "it has
// checked" or "it was checked" (FA 3). The negated label "not checked by
// maul" (owner decision 5) is the one use of "checked by maul" allowed, and
// is taken out first; "nothing is checked" and "checks" as a noun (a pull
// request's CI checks) claim nothing.
var overstating = regexp.MustCompile(`(?i)\b(confirm\w*|verified|verifies|proves?|proven|proof|re-ran|reran|` +
	`rerun|re-run)\b|checked by maul|\b(maul|it) (is still |is |has |had |was )?check(s|ed|ing)\b`)

// overstates is the overstating words s uses, the negated label aside.
func overstates(s string) []string {
	return overstating.FindAllString(strings.ReplaceAll(s, "not checked by maul", ""), -1)
}

// observedReasons are the reasons acceptance.Judge decides with (W1): the
// five new ones and the ones it reuses.
var observedReasons = []string{"seen-to-hold", "seen-not-to-hold", "not-reported", "evaluator-unsure",
	"no-valid-evidence", "contrary-note", "manual", "env-failed", "infra", "tools-build-failed", "held-finding"}

// retiredWhys are the hand-off reasons only the saved-check machinery gave
// (a replay round cut short): the observed evaluator never settles a round
// with them.
var retiredWhys = map[string]bool{HandoffWhyReplay: true}

// retiredCauses are the settle causes only the saved-check machinery
// recorded.
var retiredCauses = map[v1alpha1.SettleCause]bool{v1alpha1.SettleReplayIncomplete: true}

// evaluationSentences are maul's sentences about an evaluation under way,
// by what they are: the held link, the workspace status and comment, and
// the replies to /maul verify.
func evaluationSentences() map[string]string {
	held := &PreviewHandoff{State: HandoffHeld}
	return map[string]string{
		"held link":                handoffHeld("The preview"),
		"workspace status, early":  workspaceStatusSentence(nil),
		"workspace status, held":   workspaceStatusSentence(held),
		"workspace comment, held":  workspaceCommentSentence(held, "`abc1234`"),
		"verify requested":         requestedText(VerifyCommandNotice{Verb: "verify"}, "@alice"),
		"verify requested, ws":     requestedText(VerifyCommandNotice{Verb: "verify", Workspace: true}, "@alice"),
		"verify requested, after":  requestedText(VerifyCommandNotice{Verb: "verify", AfterRound: true}, "@alice"),
		"preview, no preview":      noticeOrEmpty(VerifyCommandNotice{Verb: "preview", Outcome: VerifyPreviewNone}),
		"accept, not being judged": noticeOrEmpty(VerifyCommandNotice{Verb: "accept", Outcome: VerifyAcceptNotChecked}),
	}
}

// settleSentences are the words of every settle cause the observed
// evaluator records, short and why, with and without a preview.
func settleSentences() map[string]string {
	out := map[string]string{}
	for c := range settleWords {
		if retiredCauses[c] {
			continue
		}
		for _, ws := range []bool{false, true} {
			w := CauseWords(c, ws)
			out[fmt.Sprintf("settle %s short (workspace %v)", c, ws)] = w.Short
			if w.Why != "" {
				out[fmt.Sprintf("settle %s why (workspace %v)", c, ws)], _ = whyWords(w.Why, ws)
			}
		}
	}
	return out
}

// noticeOrEmpty is a Project with no preview's reply to n.
func noticeOrEmpty(n VerifyCommandNotice) string {
	text, _ := workspaceNoticeText(n, "`/maul "+n.Verb+"`")
	return text
}

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
				h := PreviewHandoff{State: state, Why: why}
				check("hand-off "+state+"/"+why, labelIn(h, ws))
				check("workspace label "+state+"/"+why, workspaceLabel(h))
			}
			if c := UnconfirmedCause(why, ws); c != "" {
				check("unconfirmed cause "+why, c)
			}
		}
	}
	for what, s := range evaluationSentences() {
		check(what, s)
	}
	for what, s := range settleSentences() {
		check(what, s)
	}
	for _, state := range []string{VerifyWaiting, VerifyRunning, VerifyPassed, VerifyFailing, VerifyExhausted,
		VerifyUnavailable, VerifyPreviewNotLive, VerifyWaitingForCI, VerifyNeedsReplan, VerifyNotDeclared,
		VerifyNotChecked, VerifyUnreadable, VerifyCriteriaReplan, VerifyEvaluating, VerifyUnconfirmed,
		VerifyHeadsMoved} {
		for _, why := range whys {
			p := VerifyProgress{State: state, Verification: 2, FixesUsed: 1, MaxFixes: 2, Passing: 3, Total: 3,
				ManualOpen: 1, AutoFix: FixRunning, Outcomes: true, Why: why}
			check("progress "+state+"/"+why, verifyLine(p))
			p.Withdrawn = true
			check("progress withdrawn "+state+"/"+why, verifyLine(p))
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
	descriptions := make([]string, 0, 4+2*len(settleWords))
	descriptions = append(descriptions, count, NotReadyLead+count+", but "+longestCause,
		count+"; some accepted by hand by "+strings.Repeat("l", 39)+" and 19 others",
		EvaluatingLead+"evaluation 9999, 20 outcomes, 99 of 99 automatic fixes used")
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
