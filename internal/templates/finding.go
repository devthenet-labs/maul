// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// FindingIssueTitle is the tracking-issue title projected from a Finding.
func FindingIssueTitle(f *v1alpha1.Finding) string {
	primary := ""
	if len(f.Spec.Advisories) > 0 {
		primary = f.Spec.Advisories[0]
	}
	return fmt.Sprintf("[%s] %s: %s", f.Spec.Source, primary, f.Spec.Title)
}

// RenderFindingIssue renders the tracking-issue body from the Finding — a
// pure human-facing projection (the CR is the state; the body carries only a
// finding-name marker comment for debugging).
func RenderFindingIssue(f *v1alpha1.Finding) (string, error) {
	return render("finding_issue.md.tmpl", f)
}

// RenderStageReportComment renders an agent report as a tracking-issue
// comment, headed by the stage name and attempt so re-projection can dedup.
// The report is agent text, so it is sanitised (Sanitize): on the issue it
// may not link, show an image (GitHub's image proxy fetches an image's URL
// whenever anyone views the issue, a channel out of the sealed agent pod),
// mention anyone, reference an issue or hide text. A report that would take
// the comment past GitHub's limit is cut, and says so.
func RenderStageReportComment(stage string, attempt int32, report string) string {
	head := fmt.Sprintf("<!-- maul:report %s/%d -->\n## %s report (attempt %d)\n\n",
		stage, attempt, stage, attempt)
	return head + fitReport(report, MaxCommentBytes-len(head), Sanitize)
}

// EnrichmentMarker heads one enhancer's sticky tracking comment; the
// projection finds the comment to update in place by this prefix.
func EnrichmentMarker(enhancer string) string {
	return fmt.Sprintf("<!-- maul:enrichment %s -->", enhancer)
}

// RenderEnrichmentProjection renders one enhancer enrichment as its sticky
// tracking-issue comment.
func RenderEnrichmentProjection(e v1alpha1.Enrichment) string {
	return EnrichmentMarker(e.Enhancer) + "\n" + e.Markdown
}
