// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"testing/quick"
)

func TestRunTokens(t *testing.T) {
	rt := RunTokens{Round: 3, Attempt: 2, Run: "0a1b2c"}
	if got := rt.Token(1); got != "v3a2t1-0a1b2c" {
		t.Errorf("Token(1) = %q", got)
	}
	if got := rt.Token(2); got != "v3a2t2-0a1b2c" {
		t.Errorf("Token(2) = %q", got)
	}
	// The longest token stays within the 40-character name budget left
	// for "maul-{{run}}"-style names.
	long := RunTokens{Round: 999, Attempt: 3, Run: "ffffff"}
	if n := len("maul-" + long.Token(2)); n > 40 {
		t.Errorf("token name is %d characters", n)
	}
}

// TestJudgeSatisfied: an application that meets every expectation passes on
// its first try, and the tester makes exactly one call per step.
func TestJudgeSatisfied(t *testing.T) {
	prop := func(g genScenario) bool {
		srv := &server{c: g.c}
		obs := Observe(g.c, g.rt, srv.do)
		v := Judge(g.c, obs, g.rt)
		if v.Outcome != OutcomePass || len(obs.Tries) != 1 || srv.calls != len(Normalize([]Criterion{g.c})[0].Steps) {
			t.Logf("criterion %s\nobservation %s\nverdict %+v", mustJSON(g.c), mustJSON(obs), v)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(500)); err != nil {
		t.Error(err)
	}
}

// TestJudgeFlaky: a first try that fails and a retry that passes is
// quarantined, never passed (D7); two failing tries fail.
func TestJudgeFlaky(t *testing.T) {
	prop := func(g genScenario) bool {
		flaky := Observe(g.c, g.rt, (&server{c: g.c, failTries: map[int]bool{1: true}}).do)
		broken := Observe(g.c, g.rt, (&server{c: g.c, failTries: map[int]bool{1: true, 2: true}}).do)
		if len(flaky.Tries) != 2 || len(broken.Tries) != 2 {
			return false
		}
		if flaky.Tries[0].Run == flaky.Tries[1].Run {
			return false // every try gets a fresh {{run}}
		}
		fv, bv := Judge(g.c, flaky, g.rt), Judge(g.c, broken, g.rt)
		return fv.Outcome == OutcomeQuarantined && bv.Outcome == OutcomeFail
	}
	if err := quick.Check(prop, quickConfig(300)); err != nil {
		t.Error(err)
	}
}

// TestJudgeMutatedExpectationFails: an observation of a satisfied criterion,
// judged against the criterion with any one expectation changed, fails.
func TestJudgeMutatedExpectationFails(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	prop := func(g genScenario) bool {
		obs := Observe(g.c, g.rt, (&server{c: g.c}).do)
		mutated, what := mutateExpectation(r, Normalize([]Criterion{g.c})[0])
		if what == "" {
			return true
		}
		if v := Judge(mutated, obs, g.rt); v.Outcome != OutcomeFail {
			t.Logf("mutation %s passed: %s", what, mustJSON(mutated))
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(800)); err != nil {
		t.Error(err)
	}
}

// mutateExpectation changes exactly one expectation (or one request field) of
// one step of c so that the satisfying response no longer meets it.
func mutateExpectation(r *rand.Rand, c Criterion) (Criterion, string) {
	raw, _ := json.Marshal(c)
	var out Criterion
	_ = json.Unmarshal(raw, &out)
	i := r.Intn(len(out.Steps))
	st := &out.Steps[i]
	var options []string
	options = append(options, "status", "method", "path")
	for p := range st.Expect.JSON {
		options = append(options, "json "+p)
	}
	for h := range st.Expect.Headers {
		options = append(options, "header "+h)
	}
	if st.Expect.BodyContains != "" {
		options = append(options, "body")
	}
	sort.Strings(options)
	pick := options[r.Intn(len(options))]
	switch {
	case pick == "status":
		st.Expect.Status = StatusList{599}
	case pick == "method":
		if st.Request.Method == "GET" {
			st.Request.Method = "DELETE"
		} else {
			st.Request.Method = "GET"
		}
	case pick == "path":
		st.Request.Path += "x"
	case pick == "body":
		st.Expect.BodyContains += "-zzz"
	case strings.HasPrefix(pick, "header "):
		h := strings.TrimPrefix(pick, "header ")
		st.Expect.Headers[h] = mutateValue(st.Expect.Headers[h])
	default:
		p := strings.TrimPrefix(pick, "json ")
		st.Expect.JSON[p] = mutateValue(st.Expect.JSON[p])
	}
	return out, fmt.Sprintf("step %d %s", i, pick)
}

func mutateValue(v Value) Value {
	m, err := parseMatcher(v)
	if err != nil {
		panic(err)
	}
	switch m.kind {
	case matchPresent:
		return jsonValue(map[string]any{"$absent": true})
	case matchAbsent:
		return jsonValue(map[string]any{"$present": true})
	case matchContains:
		return jsonValue(map[string]any{"$contains": m.contains + "-zzz"})
	case matchApprox:
		shifted := make([]any, len(m.approx))
		for i, n := range m.approx {
			shifted[i] = n + m.tolerance + 10
		}
		if !m.approxList {
			return jsonValue(map[string]any{"$approx": shifted[0], "$tolerance": m.tolerance})
		}
		return jsonValue(map[string]any{"$approx": shifted, "$tolerance": m.tolerance})
	case matchOrder:
		rev := make([]any, len(m.order))
		for i, o := range m.order {
			rev[len(m.order)-1-i] = decode(o)
		}
		out := map[string]any{"$order": rev}
		if m.by != "" {
			out["$by"] = m.by
		}
		return jsonValue(out)
	default:
		return jsonValue(map[string]any{"changed": decode(m.literal)})
	}
}

// TestJudgeTransportErrors: every transport failure is a fail, never an
// error or a pass, whatever else the observation says.
func TestJudgeTransportErrors(t *testing.T) {
	for _, tr := range []Transport{TransportRefused, TransportTimeout, TransportReset, TransportDNS,
		TransportTLS, TransportTooLarge, TransportCaptureMissing, "made-up"} {
		prop := func(g genScenario) bool {
			srv := &server{c: g.c}
			do := func(call Call) Response {
				resp := srv.do(call)
				if call.Step == len(Normalize([]Criterion{g.c})[0].Steps)-1 {
					resp.Transport = tr
				}
				return resp
			}
			obs := Observe(g.c, g.rt, do)
			return Judge(g.c, obs, g.rt).Outcome == OutcomeFail && len(obs.Tries) == 2
		}
		if err := quick.Check(prop, quickConfig(50)); err != nil {
			t.Errorf("%s: %v", tr, err)
		}
	}
}

// TestJudgeRefusesInconsistentObservations: an observation that does not
// fit its criterion is a fail.
func TestJudgeRefusesInconsistentObservations(t *testing.T) {
	prop := func(g genScenario) bool {
		good := Observe(g.c, g.rt, (&server{c: g.c}).do)
		if Judge(g.c, good, g.rt).Outcome != OutcomePass {
			return false
		}
		bad := map[string]func(o *CriterionObservation){
			"id":          func(o *CriterionObservation) { o.ID = "other" },
			"no tries":    func(o *CriterionObservation) { o.Tries = nil },
			"three tries": func(o *CriterionObservation) { o.Tries = append(o.Tries, o.Tries[0], o.Tries[0]) },
			"run":         func(o *CriterionObservation) { o.Tries[0].Run = "v9a9t9-000000" },
			"no steps":    func(o *CriterionObservation) { o.Tries[0].Steps = nil },
			"extra step": func(o *CriterionObservation) {
				o.Tries[0].Steps = append(o.Tries[0].Steps, o.Tries[0].Steps[0])
			},
			"method": func(o *CriterionObservation) { o.Tries[0].Steps[0].Method = "TRACE" },
			"path":   func(o *CriterionObservation) { o.Tries[0].Steps[0].Path += "/other" },
		}
		for name, mutate := range bad {
			o := cloneObservation(good)
			mutate(&o)
			if v := Judge(g.c, o, g.rt); v.Outcome != OutcomeFail {
				t.Logf("%s: %+v", name, v)
				return false
			}
		}
		// Judged under another run's tokens, the same observation fails.
		other := g.rt
		other.Run = "ffffff"
		if g.rt.Run != other.Run && Judge(g.c, good, other).Outcome != OutcomeFail {
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(200)); err != nil {
		t.Error(err)
	}
	manual := Criterion{ID: "m", Kind: KindManual, Statement: "s"}
	if v := Judge(manual, CriterionObservation{ID: "m"}, RunTokens{}); v.Outcome != OutcomeFail {
		t.Errorf("a manual criterion judged %s", v.Outcome)
	}
}

// TestJudgeNeverPanics feeds random observations (garbage pointers,
// captures, statuses) to random criteria: Judge always returns a verdict.
func TestJudgeNeverPanics(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	junk := []string{"", "null", "{", "!too-large", `"x"`, "1e999", "[1,2", `{"id":1}`, "[]", "true", "\x00"}
	prop := func(g genScenario) bool {
		o := Observe(g.c, g.rt, (&server{c: g.c}).do)
		for ti := range o.Tries {
			for si := range o.Tries[ti].Steps {
				s := &o.Tries[ti].Steps[si]
				s.Status = int32(r.Intn(700) - 50)
				if s.Pointers == nil {
					s.Pointers = map[string]string{}
				}
				for k := range s.Pointers {
					s.Pointers[k] = junk[r.Intn(len(junk))]
				}
				s.Pointers["/new"] = junk[r.Intn(len(junk))]
				for k := range s.Captures {
					s.Captures[k] = junk[r.Intn(len(junk))]
				}
				s.Headers = map[string]string{"x": junk[r.Intn(len(junk))]}
			}
		}
		v := Judge(g.c, o, g.rt)
		return v.Outcome == OutcomePass || v.Outcome == OutcomeFail || v.Outcome == OutcomeQuarantined
	}
	if err := quick.Check(prop, quickConfig(300)); err != nil {
		t.Error(err)
	}
}

func TestMatchers(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		observed string // "" means absent
		present  bool
		want     bool
	}{
		{"literal equal", `{"a":1}`, `{"a":1}`, true, true},
		{"literal number forms", `1`, `1.0`, true, true},
		{"literal number exp", `1000`, `1e3`, true, true},
		{"literal big ints", `12345678901234567890`, `12345678901234567891`, true, false},
		{"literal differs", `"a"`, `"b"`, true, false},
		{"literal absent", `null`, "", false, false},
		{"literal too large", `"a"`, TooLarge, true, false},
		{"present", `{"$present":true}`, `null`, true, true},
		{"present too large", `{"$present":true}`, TooLarge, true, true},
		{"present absent", `{"$present":true}`, "", false, false},
		{"absent", `{"$absent":true}`, "", false, true},
		{"absent present", `{"$absent":true}`, `1`, true, false},
		{"contains", `{"$contains":"ell"}`, `"hello"`, true, true},
		{"contains no", `{"$contains":"x"}`, `"hello"`, true, false},
		{"contains number", `{"$contains":"1"}`, `1`, true, false},
		{"approx", `{"$approx":10,"$tolerance":1}`, `10.9`, true, true},
		{"approx out", `{"$approx":10,"$tolerance":1}`, `11.5`, true, false},
		{"approx list", `{"$approx":[0,500],"$tolerance":5}`, `[2,497]`, true, true},
		{"approx list len", `{"$approx":[0,500],"$tolerance":5}`, `[2]`, true, false},
		{"approx list vs scalar", `{"$approx":[0],"$tolerance":5}`, `0`, true, false},
		{"approx string", `{"$approx":1,"$tolerance":1}`, `"1"`, true, false},
		{"order", `{"$order":["b","a"]}`, `["b","a"]`, true, true},
		{"order reversed", `{"$order":["b","a"]}`, `["a","b"]`, true, false},
		{"order first occurrence", `{"$order":["b","a"]}`, `["a","b","a"]`, true, false},
		{"order duplicate later", `{"$order":["b","a"]}`, `["b","a","b"]`, true, true},
		{"order missing", `{"$order":["b","a"]}`, `["b"]`, true, false},
		{"order not list", `{"$order":["b"]}`, `"b"`, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := parseMatcher(mustValue(t, c.expected))
			if err != nil {
				t.Fatal(err)
			}
			ok, err := m.substitute("r", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := ok.match(c.observed, c.present); got != c.want {
				t.Errorf("match(%q) = %v, want %v", c.observed, got, c.want)
			}
		})
	}
}

func cloneObservation(o CriterionObservation) CriterionObservation {
	raw, _ := json.Marshal(o)
	var out CriterionObservation
	_ = json.Unmarshal(raw, &out)
	return out
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(raw)
}

// TestJudgeStepTransportBits: after a transport failure no expectation
// counts as matched, whatever the observation carries, so the fingerprint
// of a timeout never looks like a half-met response.
func TestJudgeStepTransportBits(t *testing.T) {
	st := Step{
		Request: Request{Path: "/x"},
		Expect: Expect{Status: StatusList{200}, JSON: map[string]Value{"/a": mustValue(t, `1`)},
			Headers: map[string]Value{"X-A": mustValue(t, `"v"`)}, BodyContains: "ok"},
		Capture: map[string]string{"id": "/id"},
	}
	o := StepObservation{Method: "GET", Path: "/x", Status: 200, TransportError: TransportTimeout, JSONValid: true,
		Pointers: map[string]string{"/a": "1", KeyBodyContains: `"ok"`}, Headers: map[string]string{"x-a": "v"},
		Captures: map[string]string{"id": "7"}}
	v := JudgeStep(st, o, "r", nil)
	if v.Pass || len(v.Matched) != 6 || !v.Matched[0] {
		t.Fatalf("verdict %+v", v)
	}
	for i, m := range v.Matched[1:] {
		if m {
			t.Errorf("bit %d matched after a timeout", i+1)
		}
	}
	o.TransportError = TransportNone
	if v := JudgeStep(st, o, "r", nil); !v.Pass {
		t.Errorf("the same observation without the timeout = %+v", v)
	}
}
