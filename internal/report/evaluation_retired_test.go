// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"strings"
	"testing"
)

// TestParseEvaluationRefusesRetiredKeys ("evaluator observed"): the observed
// evaluator proposes, disputes and retires no check and states no contrary
// seqs, so a report writing any of those keys, even as an empty list, is
// refused whole and claims nothing. Only an earlier release's agent image
// writes them. The same report without the key is accepted.
func TestParseEvaluationRefusesRetiredKeys(t *testing.T) {
	const verdict = "verdicts:\n  - outcome: rate-battle\n    verdict: pass\n    ledger: [2]\n"
	for _, tt := range []struct{ name, keys, key string }{
		{"disputes", "disputes:\n  - check_digest: \"" + digestA + "\"\n    reason: \"flaky\"\n    ledger: [2]\n",
			"disputes"},
		{"an empty disputes", "disputes: []\n", "disputes"},
		{"proposed", "proposed:\n  - id: rate-battle.api\n    outcome: rate-battle\n    digest: \"" + digestA +
			"\"\n", "proposed"},
		{"an empty proposed", "proposed: []\n", "proposed"},
		{"retire", "retire:\n  - check_digest: \"" + digestA + "\"\n    reason: \"stale\"\n", "retire"},
		{"an empty retire", "retire: []\n", "retire"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, err := ParseEvaluation([]byte("---\n" + verdict + tt.keys + "---\nbody\n"))
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("ParseEvaluation() = %+v, %v; want refused naming %q", e, err, tt.key)
			}
		})
	}
	for _, tt := range []struct{ name, contrary string }{
		{"contrary", "    contrary: [3]\n"},
		{"an empty contrary", "    contrary: []\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, err := ParseEvaluation([]byte("---\n" + verdict + tt.contrary + "---\nbody\n"))
			if err == nil || !strings.Contains(err.Error(), "contrary") {
				t.Fatalf("ParseEvaluation() = %+v, %v; want refused naming contrary", e, err)
			}
		})
	}
	if _, err := ParseEvaluation([]byte("---\n" + verdict + "anomalies: []\n---\nbody\n")); err != nil {
		t.Fatalf("ParseEvaluation(no retired key) error = %v, want accepted", err)
	}
}
