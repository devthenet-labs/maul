// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// labelCheck is a browser check as the evaluator writes it: a label and a
// role name that say nothing of exact.
const labelCheck = `{"id":"items.label","outcome":"items","form":"browser","browser":{"steps":[{"goto":"/"},` +
	`{"fill":{"locator":{"label":"Item name"},"text":"{{run}} a"}},{"click":{"role":"button","name":"Add"}},` +
	`{"expect":{"assert":"visible","locator":{"role":"listitem","name":"{{run}} a"}}}]}}`

// TestConfirmedChecksKeepTheRunForm: the check saved from a confirmation
// is the one its dry run ran: the exact form a tools container records
// now, the plain form one from before exact matching recorded.
func TestConfirmedChecksKeepTheRunForm(t *testing.T) {
	proposal, err := checks.ParseProposal([]byte(labelCheck))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := checks.Parse([]byte(labelCheck))
	if err != nil {
		t.Fatal(err)
	}
	for name, ran := range map[string]checks.Check{"now": proposal, "before exact": plain} {
		t.Run(name, func(t *testing.T) {
			d := strings.TrimPrefix(checks.Digest(ran), "sha256:")
			args, _ := json.Marshal(toolwire.CheckRunRequest{Spec: json.RawMessage(labelCheck)})
			pass := []toolwire.Attempt{{Run: "0123456789abcdef", Result: toolwire.ResultPass}}
			at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
			entries := []toolwire.LedgerEntry{
				{Seq: 1, TS: at, Kind: toolwire.KindAgent, Verb: toolwire.VerbCheckRun, Args: args,
					CheckRun: &toolwire.CheckRunRecord{Digest: d, Mode: toolwire.ModeDryRun, Result: toolwire.RunPass,
						Attempts: pass}},
				{Seq: 2, TS: at, Kind: toolwire.KindRelay, Verb: toolwire.VerbConfirm, Args: json.RawMessage(`{}`),
					CheckRun: &toolwire.CheckRunRecord{Digest: d, Mode: toolwire.ModeConfirm, Result: toolwire.RunPass,
						Attempts: pass}},
			}
			got := ConfirmedChecks(entries, testOutcomes())
			if len(got) != 1 || got[0].Digest != checks.Digest(ran) || got[0].Body != string(checks.Canonical(ran)) {
				t.Fatalf("confirmed %+v, want the body it ran: %s", got, checks.Canonical(ran))
			}
		})
	}
}
