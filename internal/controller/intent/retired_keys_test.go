// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// TestRetiredReportKeysNeverPass: a report carrying any key the observed
// evaluator retired (a verdict's contrary seqs, disputes, proposed checks,
// retirements) claims nothing, as the strict decoder that refuses such a
// report will (W7): a pass in it, however validly it cites, is never
// passed, the round is never ready and nothing is handed to a fixer. The
// same report without the key passes, so the rule is not vacuous. The test
// reads only raw report text, so it holds before and after the decoder
// changes.
func TestRetiredReportKeysNeverPass(t *testing.T) {
	const verdict = "verdicts:\n  - id: " + oVersion + "\n    verdict: pass\n    ledger: [2]\n" +
		"    note: \"it answers\"\n"
	digest := "sha256:" + strings.Repeat("c", 64)
	for _, tt := range []struct{ name, extra, verdictExtra string }{
		{name: "none"},
		{name: "contrary", verdictExtra: "    contrary: [3]\n"},
		{name: "disputes", extra: "disputes:\n  - check_digest: \"" + digest + "\"\n    reason: \"flaky\"\n" +
			"    ledger: [3]\n"},
		{name: "proposed", extra: "proposed:\n  - id: c-version\n    outcome: " + oVersion + "\n    digest: \"" +
			digest + "\"\n"},
		{name: "retire", extra: "retire:\n  - check_digest: \"" + digest + "\"\n    reason: \"obsolete\"\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			text := "---\n" + verdict + tt.verdictExtra + tt.extra + "---\nbody\n"
			pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: "x-v-r1-a1", UID: types.UID("pv")},
				Spec:   v1alpha1.PreviewVerificationSpec{Round: 1, HeadSetDigest: "sha256:" + strings.Repeat("a", 64)},
				Status: v1alpha1.PreviewVerificationStatus{Ledger: observedLedger(4)}}
			p := evaluatedPass(oneOutcome(), pv, text)
			j := p.decideRound(t.Context(), pv, p.runs[0])
			d := j.judgement.PerOutcome[0]
			if tt.name == "none" {
				if d.Result != acceptance.ResultPassed || j.judgement.Handoff != acceptance.HandoffReady {
					t.Fatalf("the plain report: %+v, hand-off %s; want passed and ready", d, j.judgement.Handoff)
				}
				return
			}
			if d.Result == acceptance.ResultPassed || j.judgement.Handoff == acceptance.HandoffReady ||
				j.judgement.Fix != nil {
				t.Errorf("a report carrying %s: %+v, hand-off %s, fix %+v; want no pass, not ready, no fix",
					tt.name, d, j.judgement.Handoff, j.judgement.Fix)
			}
		})
	}
}
