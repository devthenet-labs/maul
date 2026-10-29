// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// seed is every property's seed, so the gate is deterministic.
const seed = 20261028

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

var words = []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}

func word(r *rand.Rand) string { return words[r.Intn(len(words))] }

// genStatusOnly is a random check whose only assertions are on a status or
// an exit code: HTTP steps with statuses (any, 4xx and 5xx included) and
// no other expectation, or an exec with only an exit code.
type genStatusOnly struct{ c Check }

func (genStatusOnly) Generate(r *rand.Rand, _ int) reflect.Value {
	c := Check{ID: "o." + word(r), Outcome: word(r)}
	if r.Intn(3) == 0 {
		exit := int32(r.Intn(3))
		c.Form = FormExec
		c.Exec = &ExecCheck{Argv: []string{"python3", "client.py", "{{run}}"},
			Files:  []File{{Path: "client.py", Content: "print('" + word(r) + "')"}},
			Expect: ExecExpect{Exit: &exit}}
		if r.Intn(2) == 0 {
			c.Exec.Expect.Exit = nil
		}
		return reflect.ValueOf(genStatusOnly{c})
	}
	c.Form = FormHTTP
	c.HTTP = &HTTPCheck{Target: "api"}
	for range 1 + r.Intn(5) {
		st := acceptance.Step{Request: acceptance.Request{
			Method: []string{"GET", "POST", "PUT", "DELETE", ""}[r.Intn(5)],
			Path:   "/" + word(r) + "/{{run}}",
		}}
		for range r.Intn(3) {
			st.Expect.Status = append(st.Expect.Status, int32(100+r.Intn(500)))
		}
		if r.Intn(2) == 0 {
			st.Capture = map[string]string{fmt.Sprintf("c%d", r.Intn(1000)): "/id"}
		}
		c.HTTP.Steps = append(c.HTTP.Steps, st)
	}
	return reflect.ValueOf(genStatusOnly{c})
}

// TestNothingStatusOnlyIsAccepted: rule 2 of DESIGN 4.3 as a seeded
// property: Validate never accepts a check whose only assertions are a
// status or an exit code.
func TestNothingStatusOnlyIsAccepted(t *testing.T) {
	prop := func(g genStatusOnly) bool {
		err := Validate(g.c, Preview{})
		if err == nil {
			t.Logf("accepted: %s", Canonical(g.c))
			return false
		}
		var r *Refusal
		return errors.As(err, &r)
	}
	if err := quick.Check(prop, quickConfig(500)); err != nil {
		t.Fatal(err)
	}
}

// genCheck is a random check of every form, valid or not: canonicalisation
// is total.
type genCheck struct{ c Check }

func (genCheck) Generate(r *rand.Rand, _ int) reflect.Value {
	c := Check{ID: "o." + word(r), Outcome: word(r)}
	switch r.Intn(3) {
	case 0:
		c.Form = FormHTTP
		c.HTTP = &HTTPCheck{Target: word(r)}
		for range 1 + r.Intn(4) {
			st := acceptance.Step{Request: acceptance.Request{Path: "/" + word(r)}}
			if r.Intn(2) == 0 {
				st.Request.Headers = map[string]string{"X-" + word(r): word(r), "X-" + word(r) + "2": word(r)}
			}
			st.Expect.JSON = map[string]acceptance.Value{}
			for range r.Intn(4) {
				st.Expect.JSON["/"+word(r)] = randomValue(r)
			}
			c.HTTP.Steps = append(c.HTTP.Steps, st)
		}
	case 1:
		c.Form = FormBrowser
		c.Browser = &BrowserCheck{}
		for range 1 + r.Intn(5) {
			c.Browser.Steps = append(c.Browser.Steps, randomBrowserStep(r))
		}
	default:
		c.Form = FormExec
		c.Exec = &ExecCheck{Argv: []string{word(r), "{{run}}"}, Expect: ExecExpect{
			StdoutJSON: map[string]acceptance.Value{"/" + word(r): randomValue(r)}}}
	}
	return reflect.ValueOf(genCheck{c})
}

func randomBrowserStep(r *rand.Rand) BrowserStep {
	loc := &Locator{Role: "button", Name: word(r)}
	if r.Intn(2) == 0 {
		loc = &Locator{Text: word(r), Within: &Locator{TestID: word(r)}}
	}
	switch r.Intn(4) {
	case 0:
		return BrowserStep{Goto: "/" + word(r)}
	case 1:
		return BrowserStep{Click: loc}
	case 2:
		return BrowserStep{Fill: &Fill{Locator: *loc, Text: "{{run}}-" + word(r)}}
	default:
		w := float64(r.Intn(100))
		return BrowserStep{Expect: &Assertion{Assert: AssertSize, Locator: loc, Width: &w, Tolerance: 1}}
	}
}

func randomValue(r *rand.Rand) acceptance.Value {
	var s string
	switch r.Intn(5) {
	case 0:
		s = fmt.Sprintf(`{"%s":%d,"%s":"%s"}`, word(r), r.Intn(10), word(r), word(r))
	case 1:
		s = `{"$range":[1,` + fmt.Sprint(1+r.Intn(10)) + `]}`
	case 2:
		s = `{"$includes":{"name":"{{run}}-` + word(r) + `","n":1.50}}`
	case 3:
		s = `{"$order":["{{run}}-a","{{run}}-b"]}`
	default:
		s = fmt.Sprintf(`%d.0`, r.Intn(10))
	}
	v, err := acceptance.ParseValue([]byte(s))
	if err != nil {
		panic(err)
	}
	return v
}

// TestCanonicalIdempotent: re-parsing a check's canonical form and
// canonicalising again gives the same bytes and digest.
func TestCanonicalIdempotent(t *testing.T) {
	prop := func(g genCheck) bool {
		once := Canonical(g.c)
		back, err := Parse(once)
		if err != nil {
			t.Logf("Parse(%s): %v", once, err)
			return false
		}
		return bytes.Equal(once, Canonical(back)) && Digest(back) == Digest(g.c)
	}
	if err := quick.Check(prop, quickConfig(500)); err != nil {
		t.Fatal(err)
	}
}

// TestDigestStableAcrossKeyOrder: the same check written with its object
// keys in any order digests alike.
func TestDigestStableAcrossKeyOrder(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	prop := func(g genCheck) bool {
		var doc any
		dec := json.NewDecoder(bytes.NewReader(Canonical(g.c)))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return false
		}
		shuffled := encodeShuffled(r, doc)
		back, err := Parse([]byte(shuffled))
		if err != nil {
			t.Logf("Parse(%s): %v", shuffled, err)
			return false
		}
		return Digest(back) == Digest(g.c)
	}
	if err := quick.Check(prop, quickConfig(500)); err != nil {
		t.Fatal(err)
	}
}

// encodeShuffled writes v as JSON with every object's keys in random order.
func encodeShuffled(r *rand.Rand, v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			kb, _ := json.Marshal(k)
			parts = append(parts, string(kb)+":"+encodeShuffled(r, t[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, encodeShuffled(r, e))
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
