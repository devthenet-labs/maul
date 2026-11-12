// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// G-B-LIB-1: what the evaluator, the comment and the replies say of a
// look-around anomaly an earlier round found at the same commits.

// TestEvaluatePromptNamesHeldAnomalies: both evaluate prompts tell the
// evaluator of the anomalies held at its commits, fenced as data, that each
// holds the change whatever it finds, and to examine each again; with none
// the section is absent.
func TestEvaluatePromptNamesHeldAnomalies(t *testing.T) {
	const heading = "### What an earlier round found at these commits"
	held := []HeldAnomaly{{Round: 1, Summary: "the hat track rotates the wrong way", Route: "synth.Shift"}}
	for name, p := range map[string]EvaluatePrompt{
		"slot": testEvaluatePrompt(), "library": testWorkspacePrompt(testLibraryWorkspace),
	} {
		t.Run(name, func(t *testing.T) {
			without, err := RenderEvaluatePrompt(p)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(without, heading) {
				t.Error("the section renders with nothing held")
			}
			p.HeldAnomalies = held
			got, err := RenderEvaluatePrompt(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{heading, "- round 1, at synth.Shift: the hat track rotates the wrong way",
				"keeps maul from\nconfirming this change, whatever you find, until the commits change or an approver " +
					"accepts it", "Examine each one again"} {
				if !strings.Contains(got, want) {
					t.Errorf("prompt does not say %q", want)
				}
			}
		})
	}
}

// TestHeldAnomalyWords: the comment's anomaly line names the round that
// found a carried anomaly and how it clears, or who accepted it; the
// replies to /maul verify fix over it and to /maul accept all that
// accepted it say what they did.
func TestHeldAnomalyWords(t *testing.T) {
	carried := anomalyLine(VerifyAnomaly{Summary: "the hat rotates the wrong way", Route: "synth.Shift", FoundIn: 1})
	if !strings.HasPrefix(carried, "Look-around: round 1's evaluator, at these same commits, noticed") ||
		!strings.Contains(carried, "It holds these commits until they change or an approver accepts it with "+
			"`/maul accept anomalies`.") {
		t.Errorf("carried anomaly line = %q", carried)
	}
	accepted := anomalyLine(VerifyAnomaly{Summary: "odd", FoundIn: 1, AcceptedBy: "peter"})
	if !strings.HasSuffix(accepted, "Accepted at these commits by `peter`.") {
		t.Errorf("accepted anomaly line = %q", accepted)
	}
	if pre := anomalyLine(VerifyAnomaly{Summary: "old help text", Route: "bb --help", PreExisting: true}); pre !=
		"Look-around: maul's evaluator noticed (its words, not checked by maul): old help text, on `bb --help`. "+
			"The evaluator says it was there before this change, and maul found every call it cites printing the "+
			"same at the base, so it does not hold these commits." {
		t.Errorf("pre-existing anomaly line = %q", pre)
	}
	if own := anomalyLine(VerifyAnomaly{Summary: "odd"}); own !=
		"Look-around: maul's evaluator noticed (its words, not checked by maul): odd." {
		t.Errorf("this round's anomaly line = %q, want it as before", own)
	}

	fix, err := RenderVerifyCommandNotice(VerifyCommandNotice{Namespace: "maul", Intent: "x", Key: "c1",
		Actor: "peter", Verb: "verify", Outcome: VerifyRequested, Fix: true, OpenAnomalies: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fix, "No outcome fails at these commits, so there is nothing to fix yet: the anomaly "+
		"maul noticed while it looked around is not a failing outcome, and it holds these commits until they change "+
		"or an approver accepts it with `/maul accept anomalies`. The evaluator examines it again.") {
		t.Errorf("verify fix reply = %q", fix)
	}
	acc, err := RenderVerifyCommandNotice(VerifyCommandNotice{Namespace: "maul", Intent: "x", Key: "c2",
		Actor: "peter", Verb: "accept", Criterion: "all", Outcomes: true, Outcome: VerifyAccepted,
		Heads: []string{"4938490"}, Anomalies: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(acc, "`/maul accept all` from `peter` is done: the anomaly maul noticed while it looked "+
		"around is accepted at `4938490`.") {
		t.Errorf("accept all reply = %q", acc)
	}

	// /maul accept anomalies: the anomalies alone, or nothing to accept.
	only, err := RenderVerifyCommandNotice(VerifyCommandNotice{Namespace: "maul", Intent: "x", Key: "c3",
		Actor: "peter", Verb: "accept", Criterion: "anomalies", Outcomes: true, Outcome: VerifyAccepted,
		Heads: []string{"4938490"}, Anomalies: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(only, "`/maul accept anomalies` from `peter` is done: the 2 anomalies maul noticed while "+
		"it looked around are accepted at `4938490`. An anomaly is a finding") || strings.Contains(only, "outcome") {
		t.Errorf("accept anomalies reply = %q", only)
	}
	none, err := RenderVerifyCommandNotice(VerifyCommandNotice{Namespace: "maul", Intent: "x", Key: "c4",
		Actor: "peter", Verb: "accept", Criterion: "anomalies", Outcomes: true, Outcome: VerifyAcceptNoAnomaly,
		Acceptable: []string{"looks-right"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none, "`/maul accept anomalies` does nothing: no anomaly maul noticed while it looked "+
		"around, and recorded before you commented, holds these commits. It accepts no outcome. You can accept: "+
		"`looks-right`.") {
		t.Errorf("accept anomalies with none reply = %q", none)
	}
}
