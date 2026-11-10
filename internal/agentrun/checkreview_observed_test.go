// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
)

// TestCheckReviewObservedReachesThePrompt (D-B4): the text a red browser
// check's element showed in maul's runs reaches the reviewer, quoted as the
// application's, with the rule for an element the check found by its own
// run data.
func TestCheckReviewObservedReachesThePrompt(t *testing.T) {
	check := string(checks.Canonical(mustCheck(`{"id":"swing-shown","outcome":"swing","form":"browser","browser":` +
		`{"steps":[{"goto":"/"},{"fill":{"locator":{"label":"Name"},"text":"{{run}}-beat1"}},` +
		`{"click":{"role":"button","name":"Save"}},{"expect":{"assert":"text","locator":` +
		`{"css":"li","hasText":"{{run}}-beat1"},"text":{"contains":"swing 20%"}}}]}}`)))
	h := CheckReviewHandoff{Reviews: []CheckReviewItem{{
		CheckDigest: checks.Digest(mustCheck(check)), OutcomeDigest: reviewDigest("o", 0),
		Outcome: "Every row in Fresh beats shows the beat's swing after its bpm.", Check: check,
		Expect: "fail", FailedStep: 4, FailedAssertion: "expect",
		Observed: []string{"{{run}}-beat1 · by ana · 120 bpm · swing 0%", "{{run}}-beat1 · by ana · 120 bpm · swing 0%"},
	}}}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseCheckReview, string(raw), &out)
	brokeredClaude(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/review-1.md": reviewReport("proves")}}}}
	if c := onlyEvent(t, cfg, fx, &out).CheckReview; c == nil || len(c.Answers) != 1 {
		t.Fatalf("checkreview = %+v", c)
	}
	prompt := argvFlag(t, fx.specs[0].Argv, "-p")
	for _, want := range []string{"run 1: {{run}}-beat1 · by ana · 120 bpm · swing 0%",
		"That element is the check's own", "4. The text the element showed contradicts the outcome"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
}
