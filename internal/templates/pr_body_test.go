// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/report"
)

// TestPRBodyReproductionIsInert: the reproduction evidence is agent text —
// on a repository-declared image, written by the image's process — so a
// command and excerpts built to break out (a line closing the fence, a
// heading with instructions, a closing keyword, a mention, raw HTML, a bidi
// override) stay shown and inert. Read as markdown, nothing in them is
// prose, links or notifies; read as the plain text a squash commit copies
// onto the default branch, nothing in the body but patchy's own first line
// references an issue or mentions anyone.
func TestPRBodyReproductionIsInert(t *testing.T) {
	const injected = "INJECTED heading"
	hostile := "```\n````\n## " + injected + "\nFixes #7 and closes acme/app#8 @admin <img src=x>\u202e\n" +
		"https://github.com/acme/app/issues/9 GH-10 &#64;someone"
	for _, reproduced := range []bool{true, false} {
		body, err := PRBody(123, "report", false, &v1alpha1.Reproduction{
			Reproduced: reproduced,
			Command:    "go test ./... `# closes #5 @root` | tee out",
			Before:     hostile,
			After:      hostile,
		}, "")
		if err != nil {
			t.Fatalf("PRBody() error = %v", err)
		}
		first, rest, _ := strings.Cut(body, "\n")
		if first != "Fixes #123" {
			t.Fatalf("first line = %q, want patchy's own reference", first)
		}
		if msg := checkSanitized(rest); msg != "" {
			t.Errorf("reproduced=%v: %s\n%s", reproduced, msg, body)
		}
		seen := parse(rest)
		if strings.Contains(seen.prose, injected) {
			t.Errorf("reproduced=%v: the injected heading renders outside code:\n%s", reproduced, body)
		}
		if !strings.Contains(seen.visible, injected) || !strings.Contains(seen.visible, "[U+202E]") {
			t.Errorf("reproduced=%v: the evidence is not shown as written:\n%s", reproduced, body)
		}
		for _, re := range plainReferences {
			if m := re.FindString(rest); m != "" {
				t.Errorf("reproduced=%v: the plain text keeps %q:\n%s", reproduced, m, body)
			}
		}
	}
}

// plainReferences are the shapes GitHub reads as a reference or a mention
// in plain text (a squash commit's message), code or not.
var plainReferences = []*regexp.Regexp{
	regexp.MustCompile(`#[0-9]`),
	regexp.MustCompile(`(?i)/(?:issues|pulls?|discussions)/[0-9]`),
	regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])gh-[0-9]`),
	regexp.MustCompile(`(?:^|[^A-Za-z0-9])@[A-Za-z0-9]`),
}

// TestPRBodySaysWhetherReproduced: the body states plainly what the agent
// reported, and warns whenever it does not show a reproduction — said no,
// or said nothing.
func TestPRBodySaysWhetherReproduced(t *testing.T) {
	claimed := &v1alpha1.Reproduction{Reproduced: true, Command: "make test", Before: "FAIL", After: "ok"}
	for _, tt := range []struct {
		name        string
		untested    bool
		repr        *v1alpha1.Reproduction
		wantWarning bool
	}{
		{"reproduced", false, claimed, false},
		{"not reproduced", false, &v1alpha1.Reproduction{}, true},
		{"not reported", false, nil, true},
		// The default image runs none of the repository's tests, so a
		// claimed reproduction there is not accepted: the body must not say
		// both "nothing was compiled or run" and "the agent saw it fail".
		{"reproduced on the default image", true, claimed, true},
		{"not reproduced on the default image", true, &v1alpha1.Reproduction{}, true},
		{"not reported on the default image", true, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, err := PRBody(1, "report", tt.untested, tt.repr, "")
			if err != nil {
				t.Fatalf("PRBody() error = %v", err)
			}
			if got := strings.Contains(body, NotReproducedMarker); got != tt.wantWarning {
				t.Errorf("not-reproduced warning present = %v, want %v:\n%s", got, tt.wantWarning, body)
			}
			if got := strings.Contains(body, "reports that it reproduced the bug"); got == tt.wantWarning {
				t.Errorf("reproduced statement present = %v, want %v:\n%s", got, !tt.wantWarning, body)
			}
		})
	}
}

// TestRemediatePromptProvesTheBug pins the order the remediation prompt asks
// for — the regression test first, run against the unfixed code and seen to
// fail for the expected reason, then the fix and the same test passing — and
// the report fields that record it, with the bounds the parser applies, on
// a first attempt and a retry alike.
func TestRemediatePromptProvesTheBug(t *testing.T) {
	for _, prev := range []*PreviousAttempt{nil, {Attempt: 1, Outcome: "timeout", Detail: "stage timed out"}} {
		got, err := RenderRemediatePrompt(RemediatePrompt{
			IssuePath:         "/workspace/input/issue.md",
			InvestigationPath: "/workspace/input/investigation.md",
			ReportPath:        "/workspace/reports/remediation.md",
			CommitScriptPath:  "/workspace/commit.sh",
			PreviousAttempt:   prev,
		})
		if err != nil {
			t.Fatal(err)
		}
		last := -1
		for _, step := range []string{
			"Write the regression test first",
			"Run it against the UNFIXED code and confirm it fails for the expected reason",
			"Apply the fix, then run the same test again and confirm it passes",
		} {
			i := strings.Index(got, step)
			if i <= last {
				t.Errorf("previous attempt %+v: step %q missing or out of order", prev, step)
			}
			last = i
		}
		for _, want := range []string{
			"reproduction:\n  reproduced: true | false\n  command:",
			"report `reproduced: false`",
			"Never claim a reproduction you did not see",
			fmt.Sprintf("cuts an excerpt at %d bytes and the command at %d", report.ReproductionExcerptMaxBytes,
				report.ReproductionCommandMaxBytes),
		} {
			if !strings.Contains(got, want) {
				t.Errorf("previous attempt %+v: remediation prompt lacks %q", prev, want)
			}
		}
	}
}
