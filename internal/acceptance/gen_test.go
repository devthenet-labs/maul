// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing/quick"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// seed is every property's seed, so the gate is deterministic.
const seed = 20261026

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// genCriteria is a random valid criteria list: http (shorthand or steps),
// command and manual, every matcher kind, captures and placeholders.
type genCriteria struct{ cs []Criterion }

func (genCriteria) Generate(r *rand.Rand, _ int) reflect.Value {
	n := r.Intn(6)
	cs := make([]Criterion, 0, n)
	for i := range n {
		var c Criterion
		switch r.Intn(4) {
		case 0:
			c = Criterion{Kind: KindManual, Statement: word(r) + " " + word(r), Reason: maybe(r, word(r))}
		case 1:
			c = genCommand(r)
		default:
			c = genHTTP(r)
			if len(c.Steps) == 1 && r.Intn(2) == 0 {
				st := c.Steps[0]
				c.Steps = nil
				c.Request, c.Expect, c.Capture = &st.Request, &st.Expect, st.Capture
				if r.Intn(2) == 0 {
					c.Request.Method = ""
				}
			}
		}
		c.ID = fmt.Sprintf("c%d-%s", i, word(r))
		cs = append(cs, c)
	}
	return reflect.ValueOf(genCriteria{cs})
}

func genCommand(r *rand.Rand) Criterion {
	c := Criterion{Kind: KindCommand, Statement: "runs " + word(r), Argv: []string{"/bin/" + word(r), word(r)}}
	if r.Intn(2) == 0 {
		c.In = &Where{Component: word(r)}
	} else {
		c.In = &Where{Repository: word(r)}
	}
	exit := int32(r.Intn(3))
	c.Expect = &httpstep.Expect{Exit: &exit, StdoutContains: maybe(r, word(r))}
	if r.Intn(2) == 0 {
		c.Expect.StdoutJSON = map[string]httpstep.Value{"/kick": jsonValue(map[string]any{
			"$approx": []any{0, 500, 1000}, "$tolerance": 5,
		})}
	}
	if r.Intn(2) == 0 {
		c.Timeout = fmt.Sprintf("%ds", 1+r.Intn(600))
	}
	return c
}

// genHTTP is a random valid http criterion of 1-4 steps. Pointers sit under
// distinct top-level keys so a satisfying document always exists; a step
// may capture /cN and later steps use the capture in paths, headers, bodies
// and expected values.
func genHTTP(r *rand.Rand) Criterion {
	c := Criterion{Kind: KindHTTP, Statement: "serves " + word(r), Target: word(r)}
	captured := make([]string, 0, 4)
	for i := range 1 + r.Intn(4) {
		st := httpstep.Step{Name: maybe(r, fmt.Sprintf("s%d", i))}
		st.Request = genRequest(r, captured)
		st.Expect = genExpect(r, captured)
		if r.Intn(2) == 0 {
			name := fmt.Sprintf("c%d", i)
			st.Capture = map[string]string{name: "/" + name}
		}
		c.Steps = append(c.Steps, st)
		captured = append(captured, keys(st.Capture)...)
	}
	return c
}

func genRequest(r *rand.Rand, captured []string) httpstep.Request {
	methods := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}
	req := httpstep.Request{Method: methods[r.Intn(len(methods))], Path: "/" + word(r)}
	if len(captured) > 0 && r.Intn(2) == 0 {
		req.Path += "/{{capture." + captured[r.Intn(len(captured))] + "}}"
	}
	if r.Intn(2) == 0 {
		req.Path += "?run={{run}}"
	}
	if r.Intn(3) == 0 {
		req.Headers = map[string]string{"X-Maul-" + word(r): "v-{{run}}"}
	}
	switch r.Intn(3) {
	case 0:
		v := jsonValue(map[string]any{"name": "maul-{{run}}", "n": r.Intn(100)})
		req.JSON = &v
	case 1:
		req.Body = "k=" + word(r)
	}
	return req
}

func genExpect(r *rand.Rand, captured []string) httpstep.Expect {
	e := httpstep.Expect{Status: httpstep.StatusList{200 + int32(r.Intn(5))}}
	if r.Intn(3) == 0 {
		e.Status = append(e.Status, 404)
	}
	n := r.Intn(5)
	if n > 0 {
		e.JSON = map[string]httpstep.Value{}
	}
	for i := range n {
		e.JSON[fmt.Sprintf("/k%d", i)] = genMatcher(r, captured)
	}
	if r.Intn(3) == 0 {
		e.Headers = map[string]httpstep.Value{"X-" + word(r): jsonValue("v-" + word(r))}
		if r.Intn(2) == 0 {
			e.Headers["X-Other"] = jsonValue(map[string]any{"$contains": word(r)})
		}
	}
	if r.Intn(3) == 0 {
		e.BodyContains = word(r)
	}
	return e
}

func genMatcher(r *rand.Rand, captured []string) httpstep.Value {
	ref := func() any {
		if len(captured) > 0 && r.Intn(2) == 0 {
			return "{{capture." + captured[r.Intn(len(captured))] + "}}"
		}
		return word(r) + "-{{run}}"
	}
	switch r.Intn(8) {
	case 0:
		return jsonValue(map[string]any{"$present": true})
	case 1:
		return jsonValue(map[string]any{"$absent": true})
	case 2:
		return jsonValue(map[string]any{"$contains": word(r)})
	case 3:
		return jsonValue(map[string]any{"$approx": r.Intn(1000), "$tolerance": 1 + r.Intn(10)})
	case 4:
		return jsonValue(map[string]any{"$approx": []any{r.Intn(9), 10 + r.Intn(9)}, "$tolerance": r.Intn(3)})
	case 5:
		order := []any{ref(), "fixed-" + word(r)}
		if r.Intn(2) == 0 {
			return jsonValue(map[string]any{"$order": order, "$by": "/id"})
		}
		return jsonValue(map[string]any{"$order": order})
	case 6:
		return jsonValue(ref())
	default:
		literals := []any{true, nil, 42, 1.5, "x", []any{1, "two"}, map[string]any{"a": []any{1}}}
		return jsonValue(literals[r.Intn(len(literals))])
	}
}

var words = []string{"api", "web", "render", "kick", "snare", "vote", "battle", "beat", "loop", "pad"}

func word(r *rand.Rand) string { return words[r.Intn(len(words))] }

func maybe(r *rand.Rand, s string) string {
	if r.Intn(2) == 0 {
		return ""
	}
	return s
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func jsonValue(v any) httpstep.Value {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	out, err := httpstep.ParseValue(raw)
	if err != nil {
		panic(err)
	}
	return out
}

// genHTTPCriterion is one random http criterion.
type genHTTPCriterion struct{ c Criterion }

func (genHTTPCriterion) Generate(r *rand.Rand, _ int) reflect.Value {
	c := genHTTP(r)
	c.ID = "s-" + word(r)
	return reflect.ValueOf(genHTTPCriterion{c: c})
}
