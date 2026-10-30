// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package playwright

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/devthenet-labs/maul/internal/checks"
)

// seed is every property's seed, so the gate is deterministic.
const seed = 20261028

// hostile are the pieces random agent text is built from: every way out of
// a TypeScript string literal or comment, and characters JSON escapes.
var hostile = []string{"a", "Z", "9", " ", `"`, `'`, "`", `\`, `\"`, "${x}", "*/", "/*", "//", "\n", "\r", "\t",
	"\u2028", "\u2029", "</script>", "<!--", "é", "🔥", "\x00", "\x7f", "); process.exit(1); (", "{{run}}", "}",
	"\ufeff", "\\u0022"}

func hostileText(r *rand.Rand) string {
	var b strings.Builder
	for range 1 + r.Intn(6) {
		b.WriteString(hostile[r.Intn(len(hostile))])
	}
	return b.String()
}

// genLocator builds a random locator with up to depth within parents.
func genLocator(r *rand.Rand, depth int) *checks.Locator {
	l := &checks.Locator{}
	switch r.Intn(5) {
	case 0:
		l.Role = []string{"button", "link", "row", "heading", "textbox"}[r.Intn(5)]
		if r.Intn(2) == 0 {
			l.Name = hostileText(r)
		}
	case 1:
		l.Label = hostileText(r)
	case 2:
		l.Text = hostileText(r)
	case 3:
		l.TestID = hostileText(r)
	default:
		l.CSS = hostileText(r)
	}
	if r.Intn(3) == 0 {
		l.HasText = hostileText(r)
	}
	if r.Intn(3) == 0 {
		n := r.Intn(4) - 1
		l.Nth = &n
	}
	if depth > 0 && r.Intn(3) == 0 {
		l.Within = genLocator(r, depth-1)
	}
	return l
}

var assertKinds = []checks.AssertKind{checks.AssertVisible, checks.AssertText, checks.AssertCount, checks.AssertURL,
	checks.AssertLeftOf, checks.AssertAbove, checks.AssertNoOverlap, checks.AssertEqualSize, checks.AssertAligned,
	checks.AssertSize, checks.AssertClickable, checks.AssertLegible}

func genAssertion(r *rand.Rand) *checks.Assertion {
	a := &checks.Assertion{Assert: assertKinds[r.Intn(len(assertKinds))]}
	if a.Assert != checks.AssertURL {
		a.Locator = genLocator(r, 2)
	}
	switch a.Assert {
	case checks.AssertText:
		a.Text = &checks.TextMatch{}
		switch r.Intn(3) {
		case 0:
			a.Text.Equals = hostileText(r)
		case 1:
			a.Text.Contains = hostileText(r)
		default:
			a.Text.Matches = hostileText(r)
		}
	case checks.AssertCount:
		n := r.Intn(5)
		a.Count = &n
	case checks.AssertURL:
		a.URL = hostileText(r)
	case checks.AssertLeftOf, checks.AssertAbove, checks.AssertNoOverlap, checks.AssertEqualSize,
		checks.AssertAligned:
		a.Other = genLocator(r, 1)
		a.Tolerance = float64(r.Intn(10)) / 2
		if a.Assert == checks.AssertAligned {
			a.Axis = []string{"x", "y"}[r.Intn(2)]
		}
	case checks.AssertSize:
		w := float64(1 + r.Intn(500))
		a.Width = &w
		if r.Intn(2) == 0 {
			h := 0.5 + float64(r.Intn(300))
			a.Height = &h
		}
	}
	return a
}

func genStep(r *rand.Rand) checks.BrowserStep {
	switch r.Intn(9) {
	case 0:
		return checks.BrowserStep{Goto: "/" + hostileText(r)}
	case 1:
		return checks.BrowserStep{WaitForURL: "/" + hostileText(r)}
	case 2:
		return checks.BrowserStep{Click: genLocator(r, 2)}
	case 3:
		return checks.BrowserStep{Fill: &checks.Fill{Locator: *genLocator(r, 1), Text: hostileText(r)}}
	case 4:
		return checks.BrowserStep{Select: &checks.Select{Label: hostileText(r), Option: hostileText(r)}}
	case 5:
		p := &checks.Press{Key: hostileText(r)}
		if r.Intn(2) == 0 {
			p.Locator = genLocator(r, 1)
		}
		return checks.BrowserStep{Press: p}
	case 6:
		return checks.BrowserStep{Scroll: &checks.Scroll{Locator: *genLocator(r, 1), DX: r.Intn(81) - 40,
			DY: r.Intn(81) - 40}}
	default:
		return checks.BrowserStep{Expect: genAssertion(r)}
	}
}

// genCheck is a random browser check whose every string is hostile text.
type genCheck struct{ b checks.BrowserCheck }

func (genCheck) Generate(r *rand.Rand, _ int) reflect.Value {
	var b checks.BrowserCheck
	for range 1 + r.Intn(8) {
		b.Steps = append(b.Steps, genStep(r))
	}
	return reflect.ValueOf(genCheck{b})
}

// TestAgentTextOnlyInLiterals is the escaping property: whatever text a
// check carries, the compiled spec differs from the spec of the same check
// with every string replaced by "x" only inside string literals, and each
// literal decodes back to exactly the text it stands for.
func TestAgentTextOnlyInLiterals(t *testing.T) {
	prop := func(g genCheck) bool {
		return checkLiteralsOnly(t, g.b)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(seed))}); err != nil {
		t.Error(err)
	}
}

// FuzzCompile: any check Compile accepts carries its text only inside
// string literals.
func FuzzCompile(f *testing.F) {
	for _, tc := range goldenCases {
		f.Add([]byte(tc.check))
	}
	f.Add([]byte(`{"steps":[{"goto":"/\"); x(\""},{"expect":{"assert":"visible","locator":{"text":"\\\"*/"}}}]}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		var b checks.BrowserCheck
		if json.Unmarshal(in, &b) != nil {
			return
		}
		if _, err := Compile(b, Options{RunToken: testToken}); err != nil {
			return
		}
		checkLiteralsOnly(t, b)
	})
}

// checkLiteralsOnly emits b and its skeleton and compares them outside
// string literals.
func checkLiteralsOnly(t *testing.T, b checks.BrowserCheck) bool {
	t.Helper()
	opts := Options{RunToken: testToken}.withDefaults()
	out, err := emit(b, opts)
	if err != nil {
		// Only a placeholder error is expected of hostile text.
		if !errors.Is(err, errPlaceholder) {
			t.Errorf("emit: %v", err)
			return false
		}
		return true
	}
	skel, err := emit(skeleton(b), opts)
	if err != nil {
		t.Errorf("emit of the skeleton: %v", err)
		return false
	}
	code, lits, ok := splitLiterals(string(out))
	if !ok {
		t.Errorf("unterminated or undecodable string literal in:\n%s", out)
		return false
	}
	skelCode, _, ok := splitLiterals(string(skel))
	if !ok || code != skelCode {
		t.Errorf("agent text changed the code outside string literals:\n%s\nskeleton:\n%s", out, skel)
		return false
	}
	for _, s := range texts(b) {
		want, _ := substitute(s, testToken)
		// A CSS locator's literal carries the css engine prefix.
		if !slices.Contains(lits, want) && !slices.Contains(lits, "css="+want) {
			t.Errorf("no string literal decodes to %q in:\n%s", want, out)
			return false
		}
	}
	return true
}

// splitLiterals returns src with every double-quoted string literal
// emptied, the decoded literals, and whether every literal was terminated
// and decodes as JSON. A compiled spec writes literals only as JSON
// strings.
func splitLiterals(src string) (code string, lits []string, ok bool) {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		if src[i] != '"' {
			b.WriteByte(src[i])
			continue
		}
		j := i + 1
		for ; j < len(src) && src[j] != '"'; j++ {
			if src[j] == '\\' {
				j++
			}
			if j < len(src) && src[j] == '\n' {
				return "", nil, false
			}
		}
		if j >= len(src) {
			return "", nil, false
		}
		var s string
		if json.Unmarshal([]byte(src[i:j+1]), &s) != nil {
			return "", nil, false
		}
		lits = append(lits, s)
		b.WriteString(`""`)
		i = j
	}
	return b.String(), lits, true
}

// texts lists every non-empty string of b.
func texts(b checks.BrowserCheck) []string {
	var out []string
	mapStrings(&b, func(s string) string {
		out = append(out, s)
		return s
	})
	return out
}

// skeleton is b with every non-empty string replaced by "x".
func skeleton(b checks.BrowserCheck) checks.BrowserCheck {
	raw, _ := json.Marshal(b)
	var c checks.BrowserCheck
	_ = json.Unmarshal(raw, &c)
	mapStrings(&c, func(string) string { return "x" })
	return c
}

// mapStrings replaces every non-empty string field of b, in place.
func mapStrings(b *checks.BrowserCheck, f func(string) string) {
	str := func(s *string) {
		if *s != "" {
			*s = f(*s)
		}
	}
	var loc func(l *checks.Locator)
	loc = func(l *checks.Locator) {
		if l == nil {
			return
		}
		for _, s := range []*string{&l.Role, &l.Name, &l.Label, &l.Text, &l.TestID, &l.CSS, &l.HasText} {
			str(s)
		}
		loc(l.Within)
	}
	for i := range b.Steps {
		st := &b.Steps[i]
		str(&st.Goto)
		str(&st.WaitForURL)
		loc(st.Click)
		if st.Fill != nil {
			loc(&st.Fill.Locator)
			str(&st.Fill.Text)
		}
		if st.Select != nil {
			str(&st.Select.Label)
			str(&st.Select.Option)
		}
		if st.Press != nil {
			loc(st.Press.Locator)
			str(&st.Press.Key)
		}
		if st.Scroll != nil {
			loc(&st.Scroll.Locator)
		}
		if a := st.Expect; a != nil {
			loc(a.Locator)
			loc(a.Other)
			str(&a.URL)
			str(&a.Axis)
			if a.Text != nil {
				str(&a.Text.Equals)
				str(&a.Text.Contains)
				str(&a.Text.Matches)
			}
		}
	}
}
