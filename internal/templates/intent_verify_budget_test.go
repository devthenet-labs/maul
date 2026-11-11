// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// repeatTo is fill repeated to n characters, or as close below as fill's
// own length allows.
func repeatTo(fill string, n int) string {
	return strings.Repeat(fill, max(1, n/utf8.RuneCountInString(fill)))
}

// worstSection is a verification of the most failing outcomes a plan may
// state (acceptance.MaxOutcomes), each with the longest statement the
// grammar admits (acceptance.MaxOutcomeChars characters), a long check
// line and a full app output whose bytes are the application's own, every
// one fill, plus the most anomalies and log tails a comment shows, a
// suspected defect's full reproduction and every note maul can add.
func worstSection(fill string) VerifySection {
	s := VerifySection{Progress: VerifyProgress{State: VerifyFailing, Verification: 3, FixesUsed: 2, MaxFixes: 2,
		Total: acceptance.MaxOutcomes, Outcomes: true, AutoFix: FixHalted, AutoFixReason: repeatTo(fill, 200)},
		NoDefect: &VerifyNoDefect{Run: "r", Reason: repeatTo(fill, 1000)},
		Suspected: &VerifySuspected{Outcomes: []string{"o00", "o01"}, Reproduction: repeatTo(fill, 8192),
			Held: true},
		Impostor: "mallory", StatusNotPosted: true}
	for i := range acceptance.MaxOutcomes {
		s.Outcomes = append(s.Outcomes, VerifyOutcomeResult{ID: fmt.Sprintf("o%02d", i), Kind: "change",
			Statement: repeatTo(fill, acceptance.MaxOutcomeChars), Result: OutcomeResultFailed,
			Reason: "evaluator-disagrees", Check: repeatTo(fill, 1024), AppOutput: repeatTo(fill, 8192)})
	}
	for range verifyAnomaliesMax {
		s.Anomalies = append(s.Anomalies, VerifyAnomaly{Summary: repeatTo(fill, 1024), Route: repeatTo(fill, 256)})
	}
	for i := range verifyLogsMax {
		s.Logs = append(s.Logs, VerifyLog{Part: fmt.Sprintf("part-%d", i), Tail: repeatTo(fill, 4096)})
	}
	return s
}

// budgetFills are application bytes that expand when made inert or
// visible: backticks (longer code delimiters and fences), table pipes,
// mentions and references (defanged), markdown punctuation (escaped),
// variation selectors (written as their code points), and wide characters.
var budgetFills = []string{"v", "`", "a`", "@x|", "|", "#1 ", "<", `\`, "*_", "️", "\U000E0100", "😀"}

// TestVerifyCommentsFitGitHub (review of #286, S4 wave-3): however much
// evidence the application's answers make, the issue's verification
// comment and a pull request's preview comment always render under
// GitHub's comment limit (the section under its own share), with every
// outcome's verdict line, and maul's own notes, whole. Evidence past the
// budget is left out deterministically and the comment says so.
func TestVerifyCommentsFitGitHub(t *testing.T) {
	for _, fill := range budgetFills {
		t.Run(fmt.Sprintf("%q", fill), func(t *testing.T) {
			s := worstSection(fill)
			issue, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "i", Round: 3, Attempt: 1,
				Section: s})
			if err != nil {
				t.Fatal(err)
			}
			c := testPreviewComment(PreviewLive, "", "/api")
			c.Verify = &s
			preview, err := RenderIntentPreviewComment(c)
			if err != nil {
				t.Fatal(err)
			}
			section, err := renderVerifySection(&s)
			if err != nil {
				t.Fatal(err)
			}
			if len(section) > verifySectionMaxBytes {
				t.Errorf("section is %d bytes, over %d", len(section), verifySectionMaxBytes)
			}
			for name, body := range map[string]string{"issue": issue, "preview": preview} {
				if len(body) > MaxCommentBytes {
					t.Errorf("%s comment is %d bytes, over %d", name, len(body), MaxCommentBytes)
				}
				for _, o := range s.Outcomes {
					id := code(shown(o.ID))
					verdict := outcomeVerdict(o, outcomeResultWords[o.Result])
					if !strings.Contains(body, "- "+id+" ("+code("change")+"): "+verdict+". ") &&
						!strings.Contains(body, "| "+cell(id)+" | "+cell(code("change"))+" | "+cell(verdict)+" |") {
						t.Errorf("%s comment lost %s's verdict line", name, o.ID)
					}
				}
				for _, note := range []string{"The maul App may not post commit statuses", "found no defect",
					"maul believes", "posted its own over it"} {
					if !strings.Contains(body, note) {
						t.Errorf("%s comment lost maul's note %q", name, note)
					}
				}
				if !strings.Contains(body, evidenceCutLead) {
					t.Errorf("%s comment cut evidence without saying so", name)
				}
			}
			again, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "i", Round: 3, Attempt: 1,
				Section: worstSection(fill)})
			if err != nil || again != issue {
				t.Error("the cut is not deterministic")
			}
		})
	}
}

// TestVerifyEvidenceCutOrder: evidence that does not fit is left out least
// important first (log tails, then the look-around's anomalies, then app
// outputs, the last outcome's first), and what is kept is whole: a pull
// request's section, which also shows every statement.
func TestVerifyEvidenceCutOrder(t *testing.T) {
	s := worstSection("v")
	s.Suspected = nil
	out, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	logs := strings.Count(out, "Log tail of ")
	anomalies := strings.Count(out, "Look-around: ")
	outputs := strings.Count(out, "App output of ")
	if outputs < acceptance.MaxOutcomes && (logs > 0 || anomalies > 0) ||
		anomalies < verifyAnomaliesMax && logs > 0 {
		t.Errorf("kept %d log tails, %d anomalies, %d app outputs: a less important piece outlived a more "+
			"important one", logs, anomalies, outputs)
	}
	for i := range outputs {
		if !strings.Contains(out, fmt.Sprintf("App output of `o%02d`", i)) {
			t.Errorf("app output of o%02d left out before a later outcome's", i)
		}
	}
	if outputs == 0 {
		t.Error("no app output kept: the budget left nothing for evidence")
	}
	if !strings.Contains(out, evidenceCutLead) {
		t.Error("no cut note")
	}
}

// TestVerifyStatementsShortenedSaySo: statements that leave no room even
// with every piece of evidence left out (wide characters at the grammar's
// bound) are shortened, the comment says so, and each still opens with
// its own words.
func TestVerifyStatementsShortenedSaySo(t *testing.T) {
	s := worstSection("😀")
	out, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the end of each outcome's statement") {
		t.Errorf("statements shortened without saying so:\n%s", out)
	}
	if !strings.Contains(out, ". "+strings.Repeat("😀", verifyStatementShortBytes/4)+"…") {
		t.Error("a shortened statement does not keep its opening words")
	}
}

// TestVerifyEvidenceUncutSaysNothing:a comment that fits whole carries
// all its evidence and no cut note.
func TestVerifyEvidenceUncutSaysNothing(t *testing.T) {
	s := testVerifySection()
	out, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "i", Round: 1, Attempt: 1, Section: s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, evidenceCutLead) {
		t.Errorf("a whole comment says evidence was left out:\n%s", out)
	}
}
