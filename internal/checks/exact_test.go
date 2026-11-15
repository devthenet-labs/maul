// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRefuseExactOffANamedLocator: exact belongs only to a label or a
// role's name, the two Playwright matches it changes.
func TestRefuseExactOffANamedLocator(t *testing.T) {
	runRefusals(t, RuleGrammar, []refusalCase{
		{"exact on text", browserCheck(`{"click":{"text":"a","exact":true}},` + seeRun), Preview{}},
		{"exact on testid", browserCheck(`{"click":{"testid":"a","exact":false}},` + seeRun), Preview{}},
		{"exact on css", browserCheck(`{"click":{"css":"#a","exact":true}},` + seeRun), Preview{}},
		{"exact on a bare role", browserCheck(`{"click":{"role":"button","exact":true}},` + seeRun), Preview{}},
		{"exact on a within text", browserCheck(`{"click":{"label":"a","within":{"text":"b","exact":true}}},` +
			seeRun), Preview{}},
	})
}

// TestExactOnANamedLocatorIsGrammar: exact true or false is admitted on a
// label, a role with its name and a select's label.
func TestExactOnANamedLocatorIsGrammar(t *testing.T) {
	for name, check := range map[string]string{
		"label exact":     browserCheck(`{"fill":{"locator":{"label":"Beat name","exact":true},"text":"x"}},` + seeRun),
		"label substring": browserCheck(`{"fill":{"locator":{"label":"Beat name","exact":false},"text":"x"}},` + seeRun),
		"role name exact": browserCheck(`{"click":{"role":"button","name":"Save","exact":true}},` + seeRun),
		"within role exact": browserCheck(`{"click":{"text":"a","within":{"role":"region","name":"R",` +
			`"exact":false}}},` + seeRun),
		"select exact": browserCheck(`{"select":{"label":"Tempo","option":"120","exact":true}},` + seeRun),
	} {
		c, err := Parse([]byte(check))
		if err != nil {
			t.Errorf("%s: Parse: %v", name, err)
			continue
		}
		if err := Validate(c, slotCtx); err != nil {
			t.Errorf("%s: Validate: %v", name, err)
		}
	}
}

// exactOf is a locator's exact as written in canonical JSON: "true",
// "false" or "" (absent).
func exactOf(l *Locator) string {
	if l == nil || l.Exact == nil {
		return ""
	}
	if *l.Exact {
		return "true"
	}
	return "false"
}

// TestParseProposalMakesNamesExact: a check the evaluator proposes has
// every label, role name and select label exact unless it says exact
// false; text, test ids, CSS, a bare role and a count's locators stay as
// written.
func TestParseProposalMakesNamesExact(t *testing.T) {
	c, err := ParseProposal([]byte(browserCheck(`
		{"goto":"/"},
		{"fill":{"locator":{"label":"Beat name"},"text":"{{run}} a"}},
		{"fill":{"locator":{"label":"Author","exact":false},"text":"{{run}}-x"}},
		{"click":{"role":"button","name":"Save"}},
		{"click":{"role":"button","name":"{{run}} a","within":{"role":"region","name":"Fresh beats"}}},
		{"click":{"text":"Loaded:","within":{"role":"group"}}},
		{"press":{"locator":{"label":"Search"},"key":"Enter"}},
		{"scroll":{"locator":{"testid":"grid"},"dx":0,"dy":40}},
		{"select":{"label":"Tempo","option":"120"}},
		{"expect":{"assert":"no-overlap","locator":{"css":"#loaded"},"other":{"role":"button","name":"Play"}}},
		{"expect":{"assert":"count","locator":{"role":"button","name":"Vote for {{run}} a",
			"within":{"role":"region","name":"Fresh beats"}},"count":0}},
		{"expect":{"assert":"count","locator":{"label":"{{run}} b","exact":true},"count":1}}`)))
	if err != nil {
		t.Fatal(err)
	}
	st := c.Browser.Steps
	for _, tc := range []struct {
		name string
		l    *Locator
		want string
	}{
		{"label", &st[1].Fill.Locator, "true"},
		{"label written substring", &st[2].Fill.Locator, "false"},
		{"role name", st[3].Click, "true"},
		{"role name in a list", st[4].Click, "true"},
		{"within role name", st[4].Click.Within, "true"},
		{"text", st[5].Click, ""},
		{"bare role", st[5].Click.Within, ""},
		{"press label", st[6].Press.Locator, "true"},
		{"testid", &st[7].Scroll.Locator, ""},
		{"css", st[9].Expect.Locator, ""},
		{"box other role name", st[9].Expect.Other, "true"},
		{"count role name", st[10].Expect.Locator, ""},
		{"count within role name", st[10].Expect.Locator.Within, ""},
		{"count written exact", st[11].Expect.Locator, "true"},
	} {
		if got := exactOf(tc.l); got != tc.want {
			t.Errorf("%s: exact = %q, want %q", tc.name, got, tc.want)
		}
	}
	if s := st[8].Select; s.Exact == nil || !*s.Exact {
		t.Errorf("select label: exact = %v, want true", s.Exact)
	}
	if err := Validate(c, slotCtx); err != nil {
		t.Errorf("the proposal no longer validates: %v", err)
	}
}

// TestProposalFormIsItsRecordedForm: the exact a proposal gets is part of
// its canonical JSON, so the body maul saves says it, plain Parse of that
// body reads it back with the same digest, and a second ParseProposal
// changes nothing.
func TestProposalFormIsItsRecordedForm(t *testing.T) {
	for _, s := range loadSaved(t) {
		t.Run(s.ID, func(t *testing.T) {
			p, err := ParseProposal([]byte(s.Body))
			if err != nil {
				t.Fatal(err)
			}
			body := Canonical(p)
			if !strings.Contains(string(body), `"exact":true`) {
				t.Fatalf("the proposal's canonical JSON does not say exact: %s", body)
			}
			if Digest(p) == s.Digest {
				t.Error("a proposal that is exact digests as the saved substring check")
			}
			back, err := Parse(body)
			if err != nil {
				t.Fatal(err)
			}
			if Digest(back) != Digest(p) || string(Canonical(back)) != string(body) {
				t.Error("plain Parse of the recorded body is not the proposal")
			}
			again, err := ParseProposal(body)
			if err != nil || Digest(again) != Digest(p) {
				t.Errorf("ParseProposal is not idempotent: %v", err)
			}
		})
	}
}

// TestParseProposalLeavesOtherFormsAlone: an http, exec or run check
// parses as Parse parses it.
func TestParseProposalLeavesOtherFormsAlone(t *testing.T) {
	for _, check := range []string{
		httpCheck(`{"request":{"path":"/{{run}}"},"expect":{"bodyContains":"{{run}}"}}`),
		execCheck(execBase + execRunOK),
	} {
		a, err := Parse([]byte(check))
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseProposal([]byte(check))
		if err != nil {
			t.Fatal(err)
		}
		if Digest(a) != Digest(b) {
			t.Errorf("ParseProposal changed %s", check)
		}
	}
	if _, err := ParseProposal([]byte(`{"id":`)); err == nil {
		t.Error("ParseProposal accepted what Parse refuses")
	}
}

// TestExactCanonicalPosition: exact is written after nth, and absent when
// unset, so a locator without it keeps its canonical bytes.
func TestExactCanonicalPosition(t *testing.T) {
	yes := true
	n := 0
	b, err := json.Marshal(Locator{Role: "button", Name: "Save", Nth: &n, Exact: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"role":"button","name":"Save","nth":0,"exact":true}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestParseRecorded: a run's spec reads as the form whose digest the run
// was recorded against: exact as proposed now, or plain as a tools
// container from before exact matching ran it; any other digest is
// refused.
func TestParseRecorded(t *testing.T) {
	s := loadSaved(t)[0]
	p, err := ParseProposal([]byte(s.Body))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, digest, want string
	}{
		{"proposal", Digest(p), string(Canonical(p))},
		{"before exact", s.Digest, s.Body},
	} {
		c, err := ParseRecorded([]byte(s.Body), tc.digest)
		if err != nil || string(Canonical(c)) != tc.want {
			t.Errorf("%s: ParseRecorded = %s, %v", tc.name, Canonical(c), err)
		}
	}
	if _, err := ParseRecorded([]byte(s.Body), "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Error("ParseRecorded accepted a digest neither form has")
	}
}
