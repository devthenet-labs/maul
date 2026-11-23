// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package envelope

import (
	"reflect"
	"strings"
	"testing"
)

// TestEvaluatorEventsRoundTrip: the evaluation event is an additive type at
// the current version, as plan was: it encodes at Version 4 and decodes
// back whole.
func TestEvaluatorEventsRoundTrip(t *testing.T) {
	stage := Stage{Outcome: OutcomeOK, Harness: "claude", Model: "anthropic/claude-sonnet-5", NumTurns: 30,
		Usage: Usage{InputTokens: 10, OutputTokens: 20, CostUSD: 0.5}, ElapsedSeconds: 300}
	tests := []struct {
		name, prefix string
		ev           Event
	}{
		{"evaluation", `{"v":4,"type":"evaluation",`, Event{
			Type: TypeEvaluation, Repo: "devthenet-labs/beat-battle", Finding: "bb-1-ev-r1-a1",
			Evaluation: &Evaluation{
				Stage:          stage,
				ReportMarkdown: "---\nverdicts: []\n---\n",
				Verdicts: []OutcomeVerdict{{ID: "rate-battle", Verdict: "pass", Ledger: []int32{12, 14},
					Note: "moved by 16"}},
				Anomalies: []Anomaly{{Summary: "TypeError", Route: "/battles", Ledger: []int32{20}}},
				Cited:     []int32{14},
			},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, err := tt.ev.Encode()
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			if !strings.HasPrefix(line, Prefix+tt.prefix) {
				t.Errorf("line %q is not a version-4 %s event", line, tt.name)
			}
			got, ok := Decode([]byte(line))
			if !ok {
				t.Fatal("Decode() ok = false")
			}
			want := tt.ev
			want.V = Version
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round-trip = %+v, want %+v", got, want)
			}
		})
	}
}

// TestDecodeEvaluatorWire pins the evaluation payload's wire names. An
// earlier release's pod writes the retired check keys (disputes, proposed,
// retire) too: the line still decodes, and they are dropped.
func TestDecodeEvaluatorWire(t *testing.T) {
	line := Prefix + `{"v":4,"type":"evaluation","repo":"o/r","finding":"x-ev-r1-a1","evaluation":{` +
		`"outcome":"ok","harness":"claude","model":"m","usage":{"input_tokens":0,"output_tokens":0,` +
		`"cache_read_tokens":0,"cache_creation_tokens":0,"cost_usd":0},"elapsed_seconds":1,` +
		`"report_markdown":"r","verdicts":[{"id":"a","verdict":"fail","ledger":[2],"note":"n"}],` +
		`"disputes":[{"check_digest":"d","reason":"r","replacement":"c","ledger":[1]}],` +
		`"proposed":[{"id":"c","outcome":"a","digest":"d2"}],"retire":[{"check_digest":"d3","reason":"r"}],` +
		`"anomalies":[{"summary":"s","route":"/","ledger":[3]}],"cited":[2]}}`
	got, ok := Decode([]byte(line))
	if !ok || got.Type != TypeEvaluation || got.Evaluation == nil {
		t.Fatalf("Decode() = %+v, %v; want an evaluation event", got, ok)
	}
	e := got.Evaluation
	if e.ReportMarkdown != "r" || e.Verdicts[0].Verdict != "fail" || e.Verdicts[0].Ledger[0] != 2 ||
		e.Anomalies[0].Route != "/" || e.Cited[0] != 2 {
		t.Errorf("evaluation = %+v", e)
	}
}

// TestDecodeTypeRuleUnchanged: the new types change nothing about which
// lines decode. Any non-empty type at Version decodes (the collectors
// ignore the ones they do not own); an empty type, or any other version,
// does not.
func TestDecodeTypeRuleUnchanged(t *testing.T) {
	for line, want := range map[string]bool{
		Prefix + `{"v":4,"type":"evaluation"}`:  true,
		Prefix + `{"v":4,"type":"checkreview"}`: true,
		Prefix + `{"v":4,"type":"someday"}`:     true,
		Prefix + `{"v":4,"type":""}`:            false,
		Prefix + `{"v":3,"type":"evaluation"}`:  false,
		Prefix + `{"v":5,"type":"checkreview"}`: false,
	} {
		if _, ok := Decode([]byte(line)); ok != want {
			t.Errorf("Decode(%s) ok = %v, want %v", line, ok, want)
		}
	}
}
