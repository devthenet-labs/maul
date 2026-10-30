// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Wrappers that build a one-form check around a body, so each refusal row
// shows only what it is about.
func httpCheck(steps string) string {
	return `{"id":"o.c","outcome":"o","form":"http","http":{"target":"api","steps":[` + steps + `]}}`
}

func browserCheck(steps string) string {
	return `{"id":"o.c","outcome":"o","form":"browser","browser":{"steps":[` + steps + `]}}`
}

func execCheck(body string) string {
	return `{"id":"o.c","outcome":"o","form":"exec","exec":{` + body + `}}`
}

// A content assertion and an action, for rows about something else.
const (
	gotoRun   = `{"goto":"/"}`
	seeRun    = `{"expect":{"assert":"visible","locator":{"text":"{{run}}"}}}`
	execBase  = `"argv":["python3","client.py"],"files":[{"path":"client.py","content":"print(1)"}]`
	execRunOK = `,"expect":{"stdoutContains":"{{run}}"}`
)

type refusalCase struct {
	name    string
	check   string
	preview Preview
}

func runRefusals(t *testing.T, rule Rule, cases []refusalCase) {
	t.Helper()
	for _, tc := range cases {
		c, err := Parse([]byte(tc.check))
		if err != nil {
			t.Errorf("%s: Parse: %v", tc.name, err)
			continue
		}
		err = Validate(c, tc.preview)
		var r *Refusal
		if !errors.As(err, &r) || r.Rule != rule {
			t.Errorf("%s: Validate = %v, want rule %s", tc.name, err, rule)
		}
	}
}

func TestRefuseGrammar(t *testing.T) {
	runRefusals(t, RuleGrammar, []refusalCase{
		{"bad id", `{"id":"A","outcome":"o","form":"http","http":{"target":"api","steps":[]}}`, Preview{}},
		{"bad outcome", strings.Replace(httpCheck(`{"request":{"path":"/"},"expect":{"bodyContains":"x"}}`),
			`"outcome":"o"`, `"outcome":"O!"`, 1), Preview{}},
		{"unknown form", `{"id":"o.c","outcome":"o","form":"shell"}`, Preview{}},
		{"form without body", `{"id":"o.c","outcome":"o","form":"http"}`, Preview{}},
		{"two bodies", `{"id":"o.c","outcome":"o","form":"exec","exec":{` + execBase + execRunOK +
			`},"browser":{"steps":[` + gotoRun + `]}}`, Preview{}},
		{"no steps", httpCheck(``), Preview{}},
		{"bad target", strings.Replace(httpCheck(`{"request":{"path":"/"},"expect":{"bodyContains":"x"}}`),
			`"target":"api"`, `"target":"API!"`, 1), Preview{}},
		{"http method", httpCheck(`{"request":{"method":"TRACE","path":"/"},"expect":{"bodyContains":"x"}}`), Preview{}},
		{"http unknown matcher", httpCheck(`{"request":{"path":"/"},"expect":{"json":{"/a":{"$regex":"x"}}}}`),
			Preview{}},
		{"http range reversed", httpCheck(`{"request":{"path":"/"},"expect":{"json":{"/a":{"$range":[2,1]}}}}`),
			Preview{}},
		{"http range not numbers", httpCheck(`{"request":{"path":"/"},"expect":{"json":{"/a":{"$range":["a",1]}}}}`),
			Preview{}},
		{"http includes matcher", httpCheck(`{"request":{"path":"/"},"expect":{"json":{"/a":
			{"$includes":{"$present":true}}}}}`), Preview{}},
		{"http header range", httpCheck(`{"request":{"path":"/"},"expect":{"headers":{"X-A":{"$range":[1,2]}}}}`),
			Preview{}},
		{"http command expect", httpCheck(`{"request":{"path":"/"},"expect":{"exit":0,"bodyContains":"x"}}`),
			Preview{}},
		{"http capture undeclared", httpCheck(`{"request":{"path":"/{{capture.id}}"},"expect":{"bodyContains":"x"}}`),
			Preview{}},
		{"step two actions", browserCheck(`{"goto":"/","click":{"text":"x"}},` + seeRun), Preview{}},
		{"empty step", browserCheck(`{},` + seeRun), Preview{}},
		{"locator two kinds", browserCheck(`{"click":{"text":"a","testid":"b"}},` + seeRun), Preview{}},
		{"locator none", browserCheck(`{"click":{"hasText":"a"}},` + seeRun), Preview{}},
		{"name without role", browserCheck(`{"click":{"text":"a","name":"b"}},` + seeRun), Preview{}},
		{"locator capture", browserCheck(`{"click":{"text":"{{capture.x}}"}},` + seeRun), Preview{}},
		{"locator deep within", browserCheck(`{"click":{"text":"a","within":{"text":"b","within":{"text":"c",
			"within":{"text":"d","within":{"text":"e"}}}}}},` + seeRun), Preview{}},
		{"css list", browserCheck(`{"click":{"css":"a, b"}},` + seeRun), Preview{}},
		{"press key", browserCheck(`{"press":{"key":"Ctrl+Alt+Whatever"}},` + seeRun), Preview{}},
		{"scroll nothing", browserCheck(`{"scroll":{"locator":{"text":"a"},"dx":0,"dy":0}},` + seeRun), Preview{}},
		{"unknown assertion", browserCheck(gotoRun + `,{"expect":{"assert":"pretty","locator":{"text":"a"}}}`), Preview{}},
		{"visible without locator", browserCheck(gotoRun + `,{"expect":{"assert":"visible"}}`), Preview{}},
		{"aligned without axis", browserCheck(gotoRun + `,{"expect":{"assert":"aligned","locator":{"text":"a"},
			"other":{"text":"b"}}},` + seeRun), Preview{}},
		{"size without dimension", browserCheck(gotoRun + `,{"expect":{"assert":"size","locator":{"text":"a"}}},` +
			seeRun), Preview{}},
		{"tolerance off a box", browserCheck(gotoRun + `,{"expect":{"assert":"visible","locator":{"text":"{{run}}"},
			"tolerance":2}}`), Preview{}},
		{"text two matches", browserCheck(gotoRun + `,{"expect":{"assert":"text","locator":{"text":"{{run}}"},
			"text":{"equals":"a","contains":"b"}}}`), Preview{}},
		{"text bad regexp", browserCheck(gotoRun + `,{"expect":{"assert":"text","locator":{"text":"{{run}}"},
			"text":{"matches":"("}}}`), Preview{}},
		{"select no option", browserCheck(`{"select":{"label":"Tempo"}},` + seeRun), Preview{}},
		{"count negative", browserCheck(gotoRun + `,{"expect":{"assert":"count","locator":{"text":"{{run}}"},
			"count":-1}}`), Preview{}},
		{"size zero", browserCheck(gotoRun + `,{"expect":{"assert":"size","locator":{"text":"a"},"height":0}},` +
			seeRun), Preview{}},
		{"tolerance too wide", browserCheck(gotoRun + `,{"expect":{"assert":"above","locator":{"text":"a"},
			"other":{"text":"b"},"tolerance":500}},` + seeRun), Preview{}},
		{"fill control char", browserCheck(`{"fill":{"locator":{"label":"a"},"text":"a\u0007"}},` + seeRun), Preview{}},
		{"body contains control", httpCheck(`{"request":{"path":"/"},"expect":{"bodyContains":"a\nb"}}`), Preview{}},
		{"exec exit range", execCheck(execBase + `,"expect":{"exit":300,"stdoutContains":"{{run}}"}`), Preview{}},
		{"exec stdout capture", execCheck(execBase + `,"expect":{"stdoutContains":"{{capture.x}}"}`), Preview{}},
		{"exec no argv", execCheck(`"argv":[]` + execRunOK), Preview{}},
		{"exec file escapes", execCheck(`"argv":["python3","x.py"],"files":[{"path":"../x.py","content":"1"}]` +
			execRunOK), Preview{}},
		{"exec file twice", execCheck(`"argv":["sh","x"],"files":[{"path":"x","content":"1"},
			{"path":"x","content":"2"}]` + execRunOK), Preview{}},
		{"exec timeout", execCheck(execBase + `,"timeout":"1h"` + execRunOK), Preview{}},
	})
}

func TestRefuseSnapshotRefs(t *testing.T) {
	runRefusals(t, RuleSnapshotRef, []refusalCase{
		{"aria-ref css", browserCheck(`{"click":{"css":"[aria-ref=e5]"}},` + seeRun), Preview{}},
		{"bare ref", browserCheck(`{"click":{"text":"e21"}},` + seeRun), Preview{}},
		{"frame ref", browserCheck(gotoRun + `,{"expect":{"assert":"visible","locator":{"text":"{{run}}",
			"within":{"testid":"f1e19"}}}}`), Preview{}},
		{"ref in http", httpCheck(`{"request":{"path":"/"},"expect":{"bodyContains":"see e12 here"}}`), Preview{}},
		{"ref in exec argv", execCheck(`"argv":["python3","client.py","e7"],"files":[{"path":"client.py",
			"content":"1"}]` + execRunOK), Preview{}},
	})
}

func TestRefuseColourLiterals(t *testing.T) {
	runRefusals(t, RuleColourLiteral, []refusalCase{
		{"rgb text", browserCheck(gotoRun + `,{"expect":{"assert":"text","locator":{"text":"{{run}}"},
			"text":{"contains":"rgb(20, 12, 43)"}}}`), Preview{}},
		{"hex json", httpCheck(`{"request":{"path":"/"},"expect":{"json":{"/colour":"#1a2b3c"}}}`), Preview{}},
		{"hsl stdout", execCheck(execBase + `,"expect":{"stdoutContains":"hsl(1, 2%, 3%)"}`), Preview{}},
		{"oklch header", httpCheck(`{"request":{"path":"/"},"expect":{"headers":{"X-C":{"$contains":"oklch(1 0 0)"}}}}`),
			Preview{}},
	})
	// Text that is not a colour stays allowed.
	for _, s := range []string{
		browserCheck(gotoRun + `,{"expect":{"assert":"text","locator":{"text":"{{run}}"},
			"text":{"contains":"Battle #123"}}}`),
		httpCheck(`{"request":{"path":"/"},"expect":{"bodyContains":"background {{run}}"}}`),
	} {
		c, err := Parse([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(c, Preview{}); err != nil {
			t.Errorf("Validate(%s) = %v", c.ID, err)
		}
	}
}

func TestRefuseCSSDepth(t *testing.T) {
	runRefusals(t, RuleCSSDepth, []refusalCase{
		{"five descendants", browserCheck(`{"click":{"css":"div ul li a span"}},` + seeRun), Preview{}},
		{"five with combinators", browserCheck(`{"click":{"css":"main>section+div~ul li"}},` + seeRun), Preview{}},
		{"within", browserCheck(`{"click":{"text":"x","within":{"css":"a b c d e"}}},` + seeRun), Preview{}},
	})
	c, err := Parse([]byte(browserCheck(`{"click":{"css":"main > div[data-x='a b c d e'] .card:not(.x y)"}},` +
		seeRun)))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(c, Preview{}); err != nil {
		t.Errorf("three compounds with spaces inside brackets refused: %v", err)
	}
}

func TestRefuseTooManySteps(t *testing.T) {
	steps := make([]string, 0, MaxSteps+1)
	for range MaxSteps {
		steps = append(steps, `{"click":{"text":"a"}}`)
	}
	steps = append(steps, seeRun)
	reqs := make([]string, 0, MaxSteps+1)
	for range MaxSteps + 1 {
		reqs = append(reqs, `{"request":{"path":"/"},"expect":{"bodyContains":"{{run}}"}}`)
	}
	runRefusals(t, RuleTooManySteps, []refusalCase{
		{"browser", browserCheck(strings.Join(steps, ",")), Preview{}},
		{"http", httpCheck(strings.Join(reqs, ",")), Preview{}},
	})
}

func TestRefuseNotSubstantive(t *testing.T) {
	runRefusals(t, RuleNotSubstantive, []refusalCase{
		{"bare 200 on /", httpCheck(`{"request":{"path":"/"},"expect":{"status":200}}`), Preview{}},
		{"status only", httpCheck(`{"request":{"method":"POST","path":"/api/x"},"expect":{"status":[201,202]}},
			{"request":{"path":"/api/x"}}`), Preview{}},
		{"status 429 only", httpCheck(`{"request":{"path":"/api"},"expect":{"status":429}}`), Preview{}},
		{"whole body present", httpCheck(`{"request":{"path":"/api"},"expect":{"json":{"":{"$present":true}}}}`),
			Preview{}},
		{"exec exit only", execCheck(execBase + `,"expect":{"exit":0}`), Preview{}},
		{"exec true", execCheck(`"argv":["true"],"expect":{"exit":0,"stdoutContains":"x"}`), Preview{}},
		{"exec sh exit 0", execCheck(`"argv":["/bin/sh","-c","exit 0"],"expect":{"stdoutContains":"x"}`), Preview{}},
		{"exec echo", execCheck(`"argv":["echo","{{run}}"],"expect":{"stdoutContains":"{{run}}"}`), Preview{}},
		{"browser no action", browserCheck(seeRun), Preview{}},
		{"browser url only", browserCheck(gotoRun + `,{"expect":{"assert":"url","url":"/"}}`), Preview{}},
	})
}

func TestRefuseRunTokenScoping(t *testing.T) {
	runRefusals(t, RuleRunToken, []refusalCase{
		{"global order", httpCheck(`{"request":{"path":"/api/leaderboard"},"expect":{"json":{"/entries":
			{"$order":["alice","bob"],"$by":"/name"}}}}`), Preview{}},
		{"order one unscoped", httpCheck(`{"request":{"path":"/api/leaderboard"},"expect":{"json":{"/entries":
			{"$order":["{{run}}-alice","bob"]}}}}`), Preview{}},
		{"global includes", httpCheck(`{"request":{"path":"/api/leaderboard"},"expect":{"json":{"/entries":
			{"$includes":{"name":"alice"}}}}}`), Preview{}},
		{"stdout includes", execCheck(execBase + `,"expect":{"stdoutJSON":{"/a":{"$includes":3}}}`), Preview{}},
		{"global count", browserCheck(gotoRun + `,{"expect":{"assert":"count","locator":{"role":"row"},"count":3}}`),
			Preview{}},
		{"count of text", browserCheck(gotoRun + `,{"expect":{"assert":"count","locator":{"css":".card"},"count":3}}`),
			Preview{}},
		{"unscoped row", browserCheck(gotoRun + `,{"expect":{"assert":"visible","locator":{"role":"row","nth":0}}}`),
			Preview{}},
		{"row in a relation", browserCheck(gotoRun + `,{"expect":{"assert":"above","locator":{"text":"{{run}}"},
			"other":{"role":"listitem","name":"bob"}}}`), Preview{}},
	})
}

func TestRefuseOffPreview(t *testing.T) {
	app := Preview{Paths: []string{"/app"}, Components: []string{"api", "web"}}
	runRefusals(t, RuleOffPreview, []refusalCase{
		{"absolute url", browserCheck(`{"goto":"https://evil.example/"},` + seeRun), Preview{}},
		{"scheme relative", browserCheck(`{"goto":"//evil.example/"},` + seeRun), Preview{}},
		{"javascript", browserCheck(`{"goto":"javascript:alert(1)"},` + seeRun), Preview{}},
		{"dot segment", browserCheck(`{"goto":"/app/../admin"},` + seeRun), app},
		{"outside the paths", browserCheck(`{"goto":"/admin"},` + seeRun), app},
		{"prefix not a segment", browserCheck(`{"goto":"/application"},` + seeRun), app},
		{"wait off preview", browserCheck(`{"goto":"/app"},{"waitForURL":"https://evil.example/*"},` + seeRun), app},
		{"http target not a component", httpCheck(`{"request":{"path":"/"},"expect":{"bodyContains":"{{run}}"}}`),
			Preview{Components: []string{"web"}}},
	})
	c, err := Parse([]byte(browserCheck(`{"goto":"/app/battles/{{run}}?x=1"},` + seeRun)))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(c, app); err != nil {
		t.Errorf("goto under the preview refused: %v", err)
	}
}

func TestRefuseTooLarge(t *testing.T) {
	big := strings.Repeat("x", MaxProgramBytes)
	runRefusals(t, RuleTooLarge, []refusalCase{
		{"program", execCheck(fmt.Sprintf(`"argv":["python3","a.py"],"files":[{"path":"a.py","content":%q},
			{"path":"b.py","content":"1"}]`, big) + execRunOK), Preview{}},
	})
	// A browser check over MaxCanonicalBytes, built from allowed steps.
	steps := make([]string, 0, 27)
	steps = append(steps, gotoRun)
	for range 25 {
		steps = append(steps, fmt.Sprintf(`{"fill":{"locator":{"label":%q},"text":%q}}`,
			strings.Repeat("l", 190), strings.Repeat("t", 190)))
	}
	steps = append(steps, seeRun)
	runRefusals(t, RuleTooLarge, []refusalCase{{"browser", browserCheck(strings.Join(steps, ",")), Preview{}}})
}
