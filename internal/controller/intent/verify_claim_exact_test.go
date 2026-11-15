// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"encoding/json"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestCitedCallShowsTheFormTheRelayRan (G4-B-BR-1): a cited dry run of a
// browser check shows the check as the relay ran it, held to the digest
// its run was recorded against: a label the evaluator sent without exact
// is shown exact when the relay ran it exact, and as sent when an older
// tools container ran it as a part match. A person reproducing it then
// reads the match the run used.
func TestCitedCallShowsTheFormTheRelayRan(t *testing.T) {
	spec := `{"id":"name-ui","outcome":"name-shown","form":"browser","browser":{"steps":[{"goto":"/"},` +
		`{"fill":{"locator":{"label":"Beat name"},"text":"x"}}]}}`
	proposed, err := checks.ParseProposal([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := checks.Parse([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		digest string
		exact  bool
	}{
		{"exact relay", checks.Digest(proposed), true},
		{"pre-change relay", checks.Digest(plain), false},
	} {
		e := checkRunEntry(4, tc.digest, "name-ui", 1, "dry run")
		e.Kind, e.CheckRun.Mode = toolwire.KindAgent, toolwire.ModeDryRun
		e.Args = json.RawMessage(`{"check":` + spec + `}`)
		call, ok := citedCall(&v1alpha1.PreviewVerification{}, []toolwire.LedgerEntry{e}, e)
		if !ok || call.Check == nil {
			t.Fatalf("%s: call = %+v, %v; want the check shown", tc.name, call, ok)
		}
		got := call.Check.Browser.Steps[1].Fill.Locator.Exact
		if (got != nil && *got) != tc.exact {
			t.Errorf("%s: shown label exact = %v, want %v", tc.name, got, tc.exact)
		}
		if checks.Digest(*call.Check) != tc.digest {
			t.Errorf("%s: shown check's digest %s, want the recorded %s", tc.name,
				checks.Digest(*call.Check), tc.digest)
		}
	}
}
