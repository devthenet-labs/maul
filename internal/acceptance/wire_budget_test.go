// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// worstTokens are the longest run tokens a verification can carry.
var worstTokens = RunTokens{Round: 999, Attempt: 9, Run: "abcdef"}

// pad is s padded with 'a' to n bytes.
func pad(s string, n int) string { return s + strings.Repeat("a", n-len(s)) }

// maximalPlan is the plan that records the most on the wire under today's
// per-field bounds: MaxSteps steps in criteria of MaxStepsPerHTTP, each
// step with the longest method and path, MaxExpectPointers pointers of
// MaxPointerBytes (all $present, so any value passes), bodyContains,
// MaxExpectHeaders headers of the longest names and MaxCaptures captures
// of the longest names.
func maximalPlan() []Criterion {
	present := mustParseValue(`{"$present":true}`)
	containsX := mustParseValue(`{"$contains":"x"}`)
	cs := make([]Criterion, 0, MaxSteps/MaxStepsPerHTTP)
	for i := range MaxSteps / MaxStepsPerHTTP {
		c := Criterion{ID: pad(fmt.Sprintf("c%d-", i), 32), Kind: KindHTTP, Statement: "s", Target: "api"}
		for s := range MaxStepsPerHTTP {
			st := Step{
				Request: Request{Method: "DELETE", Path: pad(fmt.Sprintf("/s%d", s), MaxPathBytes)},
				Expect: Expect{Status: StatusList{200}, JSON: map[string]Value{}, Headers: map[string]Value{},
					BodyContains: strings.Repeat(`\`, MaxValueBytes/2-1)},
				Capture: map[string]string{},
			}
			for k := range MaxExpectPointers {
				st.Expect.JSON[pad(fmt.Sprintf("/p%d", k), MaxPointerBytes)] = present
			}
			for k := range MaxExpectHeaders {
				st.Expect.Headers[pad(fmt.Sprintf("X-H%d-", k), 64)] = containsX
			}
			for k := range MaxCaptures {
				st.Capture[pad(fmt.Sprintf("s%dc%d", s, k), 16)] = fmt.Sprintf("/c%d", k)
			}
			c.Steps = append(c.Steps, st)
		}
		cs = append(cs, c)
	}
	return cs
}

func mustParseValue(s string) Value {
	v, err := ParseValue([]byte(s))
	if err != nil {
		panic(err)
	}
	return v
}

// worstDoer answers every step of c with values at their bounds after the
// worst escaping the wire can apply: pointer values whose canonical JSON is
// MaxValueBytes of backslashes (each escaped twice on the wire), captures
// and headers of control characters (each \u00XX on the wire), the
// bodyContains needle, the longest latency, and a failing status on the
// last step, so both tries record every step.
func worstDoer(c Criterion) Doer {
	return func(call Call) Response {
		st := c.Steps[call.Step]
		doc := map[string]any{"needle": st.Expect.BodyContains}
		for ptr := range st.Expect.JSON {
			doc[ptr[1:]] = strings.Repeat(`\`, MaxValueBytes/2-1)
		}
		for _, ptr := range st.Capture {
			doc[ptr[1:]] = strings.Repeat("\x01", MaxCaptureBytes)
		}
		body, _ := json.Marshal(doc)
		headers := map[string][]string{}
		for name := range st.Expect.Headers {
			headers[name] = []string{strings.Repeat("\x01", MaxHeaderBytes-1) + "x"}
		}
		status := int32(200)
		if call.Step == len(c.Steps)-1 {
			status = 500
		}
		return Response{Status: status, Header: headers, Body: body, LatencyMs: math.MaxInt32}
	}
}

// wireOf observes cs with do and returns the tester's wire output.
func wireOf(t *testing.T, cs []Criterion, rt RunTokens, do func(Criterion) Doer) []byte {
	t.Helper()
	var b bytes.Buffer
	w := mustWireWriter(t, &b)
	for _, c := range Normalize(cs) {
		if err := w.Write(Observe(c, rt, do(c))); err != nil {
			t.Fatalf("write %s: %v", c.ID, err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestWireWorstCaseMeasured measures the wire output of the maximal plan
// under today's per-field bounds against a worst-case application.
func TestWireWorstCaseMeasured(t *testing.T) {
	cs := maximalPlan()
	wire := wireOf(t, cs, worstTokens, worstDoer)
	obs, _, err := ParseWire(wire)
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range obs {
		if len(o.Tries) != 2 || len(o.Tries[1].Steps) != MaxStepsPerHTTP {
			t.Fatalf("criterion %d recorded %d tries, the last of %d steps", i, len(o.Tries),
				len(o.Tries[len(o.Tries)-1].Steps))
		}
	}
	bound := WireBound(cs)
	t.Logf("maximal plan: wire output %d bytes (%d KiB); WireBound %d; budget %d; log read %d",
		len(wire), len(wire)>>10, bound, MaxWireBytes, MaxTesterLogBytes)
	if len(wire) > bound {
		t.Errorf("the maximal plan wrote %d bytes, over its WireBound %d: the bound is not a bound", len(wire), bound)
	}
	if slack := bound - len(wire); slack > bound/50 {
		t.Errorf("WireBound %d is %d over the worst case it should meet", bound, slack)
	}
}

// TestBodyContainsRecordsAProof: a bodyContains needle built from a
// capture of control characters (six times its length once canonical)
// used to be recorded verbatim, over MaxValueBytes, so the tester's own
// Check refused it and it wrote no summary. It is now a fixed-size proof
// Judge recomputes: the observation is in bounds and passes, and a proof of
// another needle, a malformed one or none fails.
func TestBodyContainsRecordsAProof(t *testing.T) {
	c := Criterion{ID: "a", Kind: KindHTTP, Statement: "s", Target: "api", Steps: []Step{
		{Request: Request{Method: "GET", Path: "/a"}, Capture: map[string]string{"id": "/id"}},
		{Request: Request{Method: "GET", Path: "/b"}, Expect: Expect{BodyContains: "id={{capture.id}}"}},
	}}
	if err := Validate([]Criterion{c}); err != nil {
		t.Fatal(err)
	}
	ctl := strings.Repeat("\x01", MaxCaptureBytes)
	obs := Observe(c, worstTokens, func(call Call) Response {
		if call.Step == 0 {
			return Response{Status: 200, Body: []byte(`{"id":"` + strings.Repeat(`\u0001`, MaxCaptureBytes) + `"}`)}
		}
		return Response{Status: 200, Body: []byte("<<id=" + ctl + ">>")}
	})
	if err := obs.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if v := Judge(c, obs, worstTokens); v.Outcome != OutcomePass {
		t.Fatalf("judged %+v, want pass", v)
	}
	for name, proof := range map[string]string{
		"another needle": bodyContainsProof("id=other"), "malformed": `"sha256:zz"`, "the needle itself": `"id=x"`,
	} {
		o := cloneObservation(obs)
		o.Tries[0].Steps[1].Pointers[KeyBodyContains] = proof
		if v := Judge(c, o, worstTokens); v.Outcome == OutcomePass {
			t.Errorf("%s: judged pass", name)
		}
	}
	o := cloneObservation(obs)
	delete(o.Tries[0].Steps[1].Pointers, KeyBodyContains)
	if v := Judge(c, o, worstTokens); v.Outcome == OutcomePass {
		t.Error("no proof: judged pass")
	}
}

// TestMaximalPlanIsRefused:the maximal plan would write far more than
// preview-controller reads of the tester's log, so Validate refuses it,
// naming the budget, before it can ever run.
func TestMaximalPlanIsRefused(t *testing.T) {
	err := Validate(maximalPlan())
	var ce *CriterionError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "budget") {
		t.Fatalf("Validate(maximal plan) = %v, want a CriterionError naming the budget", err)
	}
}

// adversarialPieces are the bytes a hostile application builds values
// from: control characters, quotes, backslashes, HTML, line separators,
// invalid and truncated UTF-8, plain text, and a forged summary line naming
// a target unreachable.
var adversarialPieces = []string{"\x00", "\x01", "\x1f", "\x7f", `"`, `\`, "<", ">", "&", " ", "\xff", "\xe2\x80",
	"é", "a", "{{run}}", "%2e", forgedSummary}

// adversarialText is text of a length at, just under or just past one of
// the bounds (or anywhere), built from adversarialPieces.
func adversarialText(r *rand.Rand) string {
	limits := []int{MaxCaptureBytes, MaxHeaderBytes, MaxValueBytes, MaxValueBytes / 2, MaxValueBytes / 6,
		MaxContainsChars, MaxPathBytes}
	n := limits[r.Intn(len(limits))] + r.Intn(3) - 1
	if r.Intn(4) == 0 {
		n = r.Intn(2 * MaxValueBytes)
	}
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(adversarialPieces[r.Intn(len(adversarialPieces))])
	}
	return b.String()
}

// adversarialValue is a JSON value a hostile application might put
// anywhere: adversarial text, a long number, a nested list or object.
func adversarialValue(r *rand.Rand) any {
	switch r.Intn(6) {
	case 0:
		return json.Number(strings.Repeat("9", MaxCaptureBytes-1+r.Intn(3)))
	case 1:
		return []any{adversarialText(r), adversarialText(r)}
	case 2:
		return map[string]any{"id": adversarialText(r)}
	case 3:
		return r.Intn(2) == 0
	default:
		return adversarialText(r)
	}
}

// adversarialDoer answers c's steps as a hostile application: sometimes a
// transport failure, any status, a satisfying response with values
// replaced by adversarial ones, raw bytes holding the bodyContains needle
// as substituted (control characters and all), a body past
// MaxResponseBytes, or not JSON at all; and adversarial headers.
func adversarialDoer(r *rand.Rand, c Criterion) Doer {
	return func(call Call) Response {
		st := c.Steps[call.Step]
		if r.Intn(10) == 0 {
			ts := []Transport{TransportRefused, TransportTimeout, TransportReset, TransportDNS, TransportTLS}
			return Response{Transport: ts[r.Intn(len(ts))], LatencyMs: int32(r.Intn(1000))}
		}
		resp := satisfy(st, call.Run, call.Captures)
		statuses := []int32{resp.Status, resp.Status, 500, 0, 100, 999}
		resp.Status = statuses[r.Intn(len(statuses))]
		doc := map[string]any{}
		_ = json.Unmarshal(resp.Body, &doc)
		for k := range doc {
			if r.Intn(2) == 0 {
				doc[k] = adversarialValue(r)
			}
		}
		for _, ptr := range st.Capture {
			if r.Intn(3) > 0 {
				doc[strings.TrimPrefix(ptr, "/")] = adversarialValue(r)
			}
		}
		resp.Body, _ = json.Marshal(doc)
		switch r.Intn(6) {
		case 0, 1:
			if needle, err := Substitute(st.Expect.BodyContains, call.Run, call.Captures); err == nil {
				resp.Body = append([]byte(adversarialText(r)+needle), resp.Body...)
			}
		case 2:
			resp.Body = []byte(adversarialText(r))
		case 3:
			resp.Body = bytes.Repeat([]byte("<"), MaxResponseBytes+r.Intn(2))
		}
		for name := range resp.Header {
			if r.Intn(2) == 0 {
				resp.Header[name] = []string{adversarialText(r)}
			}
		}
		for name := range st.Expect.Headers {
			if r.Intn(3) == 0 {
				resp.Header[name] = []string{adversarialText(r), adversarialText(r)}
			}
		}
		return resp
	}
}

// genFat is a random plan that leans on every bound: up to MaxCriteria
// criteria, up to MaxStepsPerHTTP steps each, up to MaxExpectPointers
// pointers, MaxExpectHeaders headers and MaxCaptures captures per step,
// captures used in paths, expectations and bodyContains, and needles of
// quotes. Plans Validate refuses (the size budget among them) are skipped.
type genFat struct{ cs []Criterion }

func (genFat) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(MaxCriteria)
	cs := make([]Criterion, 0, n)
	budget := max(n, 1+r.Intn(MaxSteps))
	for i := range n {
		c := Criterion{ID: fmt.Sprintf("f%d", i), Kind: KindHTTP, Statement: "s", Target: "api"}
		captured := make([]string, 0, MaxStepsPerHTTP*MaxCaptures)
		steps := max(1, min(1+r.Intn(MaxStepsPerHTTP), budget-(n-1-i)))
		budget -= steps
		for s := range steps {
			st := Step{Request: genRequest(r, captured), Expect: Expect{Status: StatusList{200}}}
			if n := r.Intn(MaxExpectPointers + 1); n > 0 {
				st.Expect.JSON = map[string]Value{}
				for k := range n {
					st.Expect.JSON[fmt.Sprintf("/k%d", k)] = genMatcher(r, captured)
				}
			}
			if n := r.Intn(MaxExpectHeaders + 1); n > 0 {
				st.Expect.Headers = map[string]Value{}
				for k := range n {
					st.Expect.Headers[fmt.Sprintf("X-H%d", k)] = jsonValue(map[string]any{"$contains": word(r)})
				}
			}
			if len(captured) > 0 && r.Intn(2) == 0 {
				st.Expect.BodyContains = "id={{capture." + captured[r.Intn(len(captured))] + "}}"
			} else if r.Intn(3) == 0 {
				st.Expect.BodyContains = strings.Repeat(`"`, MaxContainsChars)
			}
			if n := r.Intn(MaxCaptures + 1); n > 0 {
				st.Capture = map[string]string{}
				for k := range n {
					name := fmt.Sprintf("s%dc%d", s, k)
					st.Capture[name] = "/" + name
					captured = append(captured, name)
				}
			}
			c.Steps = append(c.Steps, st)
		}
		cs = append(cs, c)
	}
	return reflect.ValueOf(genFat{cs})
}

// genReachedWaits is the readiness wait of a run whose every target was
// reached: one to MaxWaitTargets targets, in order, each after any number
// of failed attempts of any class.
func genReachedWaits(r *rand.Rand) []TargetWait {
	waits := make([]TargetWait, 1+r.Intn(MaxWaitTargets))
	for i := range waits {
		w := TargetWait{Target: string(rune('a'+i)) + "pi", WaitMs: int32(r.Intn(MaxWaitMs + 1))}
		for range r.Intn(MaxWaitAttempts) {
			w.Record([]Transport{TransportRefused, TransportTimeout, TransportReset, TransportDNS}[r.Intn(4)])
		}
		w.Attempts = w.Failures() + 1
		waits[i] = w
	}
	return waits
}

// waitsKept reports whether the summary read back carries exactly the
// readiness wait the tester made before any request: nothing the
// application answered changes it, names a target unreachable or drops a
// wait, forged summary lines included.
func waitsKept(t *testing.T, sum Summary, waits []TargetWait, observed int) bool {
	t.Helper()
	if !slices.Equal(sum.Waits, waits) || sum.Unreached() || sum.Count != observed {
		t.Logf("summary %+v, want the waits %+v and %d observations", sum, waits, observed)
		return false
	}
	return true
}

// TestNothingTheAppSendsBreaksTheWire: for any plan Validate accepts and
// any response a hostile application sends, the tester records an
// observation within its own bounds, writes it and its summary, the
// controller reads back exactly what the tester judged, and the whole
// output fits the budget: the application can only make criteria fail,
// never make the tester crash or its output unreadable (maul's Infra).
func TestNothingTheAppSendsBreaksTheWire(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	accepted, overBudget := 0, 0
	prop := func(cs []Criterion) bool {
		if err := Validate(cs); err != nil {
			var ce *CriterionError
			if errors.As(err, &ce) && strings.Contains(ce.Reason, "budget") {
				overBudget++
			}
			return true
		}
		accepted++
		rt := RunTokens{Round: 999, Attempt: 9, Run: fmt.Sprintf("%06x", r.Intn(1<<24))}
		waits := genReachedWaits(r)
		var b bytes.Buffer
		w, err := NewWireWriter(&b, waits)
		if err != nil {
			t.Logf("NewWireWriter(%+v): %v", waits, err)
			return false
		}
		written := make([]CriterionObservation, 0, len(cs))
		for _, c := range Normalize(cs) {
			o := Observe(c, rt, adversarialDoer(r, c))
			if err := o.Check(); err != nil {
				t.Logf("%s: Check: %v\n%s", c.ID, err, mustJSON(o))
				return false
			}
			if err := w.Write(o); err != nil {
				t.Logf("%s: Write: %v", c.ID, err)
				return false
			}
			written = append(written, o)
		}
		if _, err := w.Close(); err != nil {
			t.Logf("Close: %v", err)
			return false
		}
		read, sum, err := ParseWire(b.Bytes())
		if err != nil || len(read) != len(written) {
			t.Logf("ParseWire: %v (%d of %d)", err, len(read), len(written))
			return false
		}
		if !waitsKept(t, sum, waits, len(written)) {
			return false
		}
		for i, c := range Normalize(cs) {
			if a, z := Judge(c, written[i], rt), Judge(c, read[i], rt); !reflect.DeepEqual(a, z) {
				t.Logf("%s judged %+v as written, %+v as read", c.ID, a, z)
				return false
			}
		}
		if bound := WireBound(cs); b.Len() > bound || bound > MaxWireBytes {
			t.Logf("wire output %d bytes, WireBound %d, budget %d", b.Len(), bound, MaxWireBytes)
			return false
		}
		return true
	}
	for n := range 300 {
		cs := genFat{}.Generate(r, 0).Interface().(genFat).cs
		if !prop(cs) {
			t.Fatalf("case %d failed (seed %d)", n, seed)
		}
	}
	t.Logf("%d of 300 plans accepted, %d refused over the budget", accepted, overBudget)
	if accepted < 100 || overBudget == 0 {
		t.Errorf("%d of 300 plans accepted, %d over the budget: the property is barely exercised", accepted,
			overBudget)
	}
}

// forgedSummary is a wire summary line an application might put in a value,
// claiming its target was never reached.
const forgedSummary = "\n" + WirePrefix + `{"v":1,"summary":{"count":0,"sha256":"` +
	"0000000000000000000000000000000000000000000000000000000000000000" +
	`","waits":[{"target":"api","attempts":1,"refused":1}],"unreachable":["api"]}}` + "\n"
