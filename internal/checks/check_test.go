// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The demo checks: the Beat Battle leaderboard order (HTTP), the
// run-token row (browser), the #40 step grid (browser, demo (d)), the
// rate limit (HTTP, demo finding 11) and a websocket client (exec).
const (
	httpOrder = `{"id":"winner-rises.order","outcome":"winner-rises","form":"http","http":{"target":"api","steps":[
	 {"request":{"method":"POST","path":"/api/players","json":{"name":"{{run}}-alice"}},"expect":{"status":201},
	  "capture":{"alice":"/id"}},
	 {"request":{"method":"POST","path":"/api/players","json":{"name":"{{run}}-bob"}},"expect":{"status":201}},
	 {"request":{"method":"POST","path":"/api/battles","json":{"winner":"{{capture.alice}}"}},"expect":{"status":201}},
	 {"request":{"path":"/api/leaderboard?q={{run}}"},
	  "expect":{"json":{"/entries":{"$order":["{{run}}-alice","{{run}}-bob"],"$by":"/name"}}}}]}}`
	httpRate = `{"id":"rate.limit","outcome":"rate-limited","form":"http","http":{"target":"api","steps":[
	 {"request":{"method":"POST","path":"/api/votes"},"expect":{"status":429,
	  "headers":{"Retry-After":{"$present":true}},"json":{"/remaining":{"$range":[0,0]}}}}]}}`
	browserRow = `{"id":"board.row","outcome":"leaderboard-still-loads","form":"browser","browser":{"steps":[
	 {"goto":"/leaderboard"},
	 {"fill":{"locator":{"label":"Name"},"text":"{{run}}-alice"}},
	 {"click":{"role":"button","name":"Add player"}},
	 {"waitForURL":"/leaderboard*"},
	 {"expect":{"assert":"visible","locator":{"role":"row","hasText":"{{run}}-alice"}}},
	 {"expect":{"assert":"count","locator":{"role":"row","hasText":"{{run}}"},"count":1}}]}}`
	browserGrid = `{"id":"grid.steps","outcome":"steps-40px","form":"browser","browser":{"steps":[
	 {"goto":"/sequencer"},
	 {"expect":{"assert":"size","locator":{"testid":"step-1-1"},"width":40,"tolerance":1}},
	 {"scroll":{"locator":{"testid":"row-1"},"dx":200,"dy":0}},
	 {"press":{"key":"ArrowRight"}},
	 {"select":{"label":"Tempo","option":"Fast"}},
	 {"expect":{"assert":"aligned","locator":{"css":".row:nth-child(1) .cell","nth":3},
	  "other":{"css":".row:nth-child(2) .cell","nth":3},"axis":"x","tolerance":1}},
	 {"expect":{"assert":"clickable","locator":{"role":"button","name":"Play","within":{"testid":"card-1"}}}},
	 {"expect":{"assert":"legible","locator":{"role":"link","name":"Back to battles"}}}]}}`
	execWS = `{"id":"ws.echo","outcome":"live-updates","form":"exec","exec":{
	 "argv":["python3","client.py","{{run}}"],"files":[{"path":"client.py","content":"import sys\nprint(sys.argv[1])\n"}],
	 "timeout":"30s","expect":{"exit":0,"stdoutJSON":{"/players":{"$includes":{"name":"{{run}}-alice"}}}}}}`
)

func mustParse(t *testing.T, s string) Check {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func TestDemoChecksAreValid(t *testing.T) {
	for _, s := range []string{httpOrder, httpRate, browserRow, browserGrid, execWS} {
		c := mustParse(t, s)
		if err := Validate(c, Preview{}); err != nil {
			t.Errorf("%s: Validate: %v", c.ID, err)
		}
	}
}

func TestParseIsStrict(t *testing.T) {
	for name, s := range map[string]string{
		"unknown field":  `{"id":"a","outcome":"a","form":"http","script":"x"}`,
		"trailing value": `{"id":"a","outcome":"a","form":"http"} {}`,
		"not an object":  `[1]`,
		"unknown step":   `{"id":"a","outcome":"a","form":"browser","browser":{"steps":[{"evaluate":"1"}]}}`,
		"exec status":    `{"id":"a","outcome":"a","form":"exec","exec":{"argv":["x"],"expect":{"status":200}}}`,
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%s: Parse accepted %s", name, s)
		}
	}
}

func TestCanonicalNormalisesDefaults(t *testing.T) {
	short := mustParse(t, `{"id":"a.b","outcome":"a","form":"http","http":{"target":"api","steps":[
	  {"request":{"path":"/x"},"expect":{"bodyContains":"{{run}}"}}]}}`)
	long := mustParse(t, `{"form":"http","outcome":"a","id":"a.b","http":{"steps":[
	  {"expect":{"status":[200],"bodyContains":"{{run}}"},"request":{"path":"/x","method":"GET"}}],"target":"api"}}`)
	if !bytes.Equal(Canonical(short), Canonical(long)) {
		t.Fatalf("defaults not normalised:\n%s\n%s", Canonical(short), Canonical(long))
	}
	if Digest(short) != Digest(long) {
		t.Fatal("digests differ")
	}
	if !strings.HasPrefix(Digest(short), "sha256:") || len(Digest(short)) != len("sha256:")+64 {
		t.Fatalf("digest %q", Digest(short))
	}
}

func TestCanonicalIsIdempotentOnDemoChecks(t *testing.T) {
	for _, s := range []string{httpOrder, httpRate, browserRow, browserGrid, execWS} {
		c := mustParse(t, s)
		once := Canonical(c)
		again := Canonical(mustParse(t, string(once)))
		if !bytes.Equal(once, again) {
			t.Errorf("%s: not idempotent:\n%s\n%s", c.ID, once, again)
		}
	}
}

func TestRefusalIsTyped(t *testing.T) {
	c := mustParse(t, `{"id":"a.b","outcome":"a","form":"http","http":{"target":"api","steps":[
	  {"request":{"path":"/"},"expect":{"status":200}}]}}`)
	err := Validate(c, Preview{})
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("Validate = %v, want a *Refusal", err)
	}
	if r.Rule != RuleNotSubstantive || r.Check != "a.b" || !strings.Contains(r.Error(), "a.b") {
		t.Fatalf("refusal %+v (%v)", r, r)
	}
}

// TestDigestStableForExactSize: the size bounds add no key to an exact size
// check's canonical form, so a check saved before them keeps its digest.
func TestDigestStableForExactSize(t *testing.T) {
	const want = "sha256:85d4e2b01331b452168dfdeb29e359e7c1806977e5f0d1a1ca980dfb2421d854"
	if got := Digest(mustParse(t, browserGrid)); got != want {
		t.Fatalf("Digest(browserGrid) = %s, want %s", got, want)
	}
}

// TestCanonicalKeepsSizeBounds: a bounded size check round-trips through
// Canonical with its bounds, and digests apart from the exact one.
func TestCanonicalKeepsSizeBounds(t *testing.T) {
	bounded := mustParse(t, `{"id":"a.b","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
	 {"expect":{"maxHeight":56,"minWidth":48,"locator":{"testid":"pad"},"assert":"size","minHeight":48,"maxWidth":64}}]}}`)
	exact := mustParse(t, `{"id":"a.b","outcome":"a","form":"browser","browser":{"steps":[{"goto":"/"},
	 {"expect":{"assert":"size","locator":{"testid":"pad"},"width":48,"height":48}}]}}`)
	got := string(Canonical(bounded))
	for _, k := range []string{`"minWidth":48`, `"minHeight":48`, `"maxWidth":64`, `"maxHeight":56`} {
		if !strings.Contains(got, k) {
			t.Errorf("Canonical lacks %s: %s", k, got)
		}
	}
	if !bytes.Equal(Canonical(mustParse(t, got)), Canonical(bounded)) {
		t.Error("Canonical of a bounded size is not idempotent")
	}
	if Digest(bounded) == Digest(exact) {
		t.Error("a bounded size digests like an exact one")
	}
}
