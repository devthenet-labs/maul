// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing/quick"
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
	c.Expect = &Expect{Exit: &exit, StdoutContains: maybe(r, word(r))}
	if r.Intn(2) == 0 {
		c.Expect.StdoutJSON = map[string]Value{"/kick": jsonValue(map[string]any{
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
		st := Step{Name: maybe(r, fmt.Sprintf("s%d", i))}
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

func genRequest(r *rand.Rand, captured []string) Request {
	methods := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}
	req := Request{Method: methods[r.Intn(len(methods))], Path: "/" + word(r)}
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

func genExpect(r *rand.Rand, captured []string) Expect {
	e := Expect{Status: StatusList{200 + int32(r.Intn(5))}}
	if r.Intn(3) == 0 {
		e.Status = append(e.Status, 404)
	}
	n := r.Intn(5)
	if n > 0 {
		e.JSON = map[string]Value{}
	}
	for i := range n {
		e.JSON[fmt.Sprintf("/k%d", i)] = genMatcher(r, captured)
	}
	if r.Intn(3) == 0 {
		e.Headers = map[string]Value{"X-" + word(r): jsonValue("v-" + word(r))}
		if r.Intn(2) == 0 {
			e.Headers["X-Other"] = jsonValue(map[string]any{"$contains": word(r)})
		}
	}
	if r.Intn(3) == 0 {
		e.BodyContains = word(r)
	}
	return e
}

func genMatcher(r *rand.Rand, captured []string) Value {
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

func jsonValue(v any) Value {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	out, err := ParseValue(raw)
	if err != nil {
		panic(err)
	}
	return out
}

// genScenario is one random http criterion with the run tokens it is
// judged under.
type genScenario struct {
	c  Criterion
	rt RunTokens
}

func (genScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	c := genHTTP(r)
	c.ID = "s-" + word(r)
	rt := RunTokens{Round: 1 + int32(r.Intn(5)), Attempt: 1 + int32(r.Intn(3)), Run: fmt.Sprintf("%06x", r.Intn(1<<24))}
	return reflect.ValueOf(genScenario{c: c, rt: rt})
}

// server answers each built request with a response satisfying the step's
// expectations, as a correct application would. failTries makes the given
// tries answer 500 instead (a flaky application).
type server struct {
	c         Criterion
	failTries map[int]bool
	calls     int
}

func (s *server) do(call Call) Response {
	s.calls++
	if s.failTries[call.Try] {
		return Response{Status: 500, Body: []byte(`{"error":"flaky"}`)}
	}
	return satisfy(Normalize([]Criterion{s.c})[0].Steps[call.Step], call.Run, call.Captures)
}

// satisfy builds a response meeting every expectation of st under run and
// the captures so far, plus a fresh value for each capture it declares.
func satisfy(st Step, run string, caps map[string]string) Response {
	sub := func(s string) string {
		out, err := Substitute(s, run, caps)
		if err != nil {
			panic(fmt.Sprintf("satisfy: %v", err))
		}
		return out
	}
	doc := map[string]any{}
	for ptr, v := range st.Expect.JSON {
		m, err := parseMatcher(v)
		if err != nil {
			panic(err)
		}
		if val, ok := satisfyMatcher(m, sub); ok {
			doc[strings.TrimPrefix(ptr, "/")] = val
		}
	}
	for name, ptr := range st.Capture {
		doc[strings.TrimPrefix(ptr, "/")] = "cap-" + name + "-" + run
	}
	if st.Expect.BodyContains != "" {
		doc["_body"] = sub(st.Expect.BodyContains)
	}
	header := map[string][]string{}
	for name, v := range st.Expect.Headers {
		var s string
		var m map[string]string
		if json.Unmarshal([]byte(v.String()), &s) == nil {
			header[name] = []string{sub(s)}
		} else if json.Unmarshal([]byte(v.String()), &m) == nil {
			header[name] = []string{"pre-" + sub(m["$contains"]) + "-post"}
		}
	}
	body, _ := json.Marshal(doc)
	return Response{Status: st.Expect.Status[0], Header: header, Body: body}
}

func satisfyMatcher(m matcher, sub func(string) string) (any, bool) {
	switch m.kind {
	case matchPresent:
		return "present", true
	case matchAbsent:
		return nil, false
	case matchContains:
		return "<<" + sub(m.contains) + ">>", true
	case matchApprox:
		if !m.approxList {
			return m.approx[0] + m.tolerance/2, true
		}
		out := make([]any, len(m.approx))
		for i, n := range m.approx {
			out[i] = n + m.tolerance/2
		}
		return out, true
	case matchOrder:
		list := []any{wrapBy(m.by, "noise")}
		for _, v := range m.order {
			list = append(list, wrapBy(m.by, substAny(decode(v), sub)), wrapBy(m.by, "noise"))
		}
		return list, true
	default:
		return substAny(decode(m.literal), sub), true
	}
}

func wrapBy(by string, v any) any {
	if by == "" {
		return v
	}
	return map[string]any{strings.TrimPrefix(by, "/"): v, "other": 1}
}

func decode(v Value) any {
	var out any
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		panic(err)
	}
	return out
}

func substAny(v any, sub func(string) string) any {
	switch t := v.(type) {
	case string:
		return sub(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = substAny(e, sub)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			out[k] = substAny(e, sub)
		}
		return out
	default:
		return v
	}
}
