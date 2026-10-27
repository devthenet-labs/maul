// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	jsonserializer "k8s.io/apimachinery/pkg/runtime/serializer/json"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/kube"
)

// fitSeed is the seed of the fitStatus property, so the gate is
// deterministic.
const fitSeed = 20261027

// fitRun is the run tokens every fixture in this file is observed under.
var fitRun = acceptance.RunTokens{Round: 1, Attempt: 1, Run: "a1b2c3"}

// observedVerification runs cs against do as the tester would
// (acceptance.Observe) and stores the result as an Observed verification.
func observedVerification(t *testing.T, cs []acceptance.Criterion,
	do func(c acceptance.Criterion) acceptance.Doer) *v1alpha1.PreviewVerification {
	t.Helper()
	cs = acceptance.Normalize(cs)
	if err := acceptance.Validate(cs); err != nil {
		t.Fatalf("fixture criteria: %v", err)
	}
	obs := make([]acceptance.CriterionObservation, len(cs))
	for i, c := range cs {
		obs[i] = acceptance.Observe(c, fitRun, do(c))
		if err := obs[i].Check(); err != nil {
			t.Fatalf("fixture observation %s: %v", c.ID, err)
		}
	}
	crdCriteria, err := crdconv.CriteriaToCRD(cs)
	if err != nil {
		t.Fatal(err)
	}
	crdObs, err := crdconv.ObservationsToCRD(obs)
	if err != nil {
		t.Fatal(err)
	}
	return &v1alpha1.PreviewVerification{
		Spec: v1alpha1.PreviewVerificationSpec{Round: fitRun.Round, Attempt: fitRun.Attempt, Run: fitRun.Run,
			Criteria: crdCriteria},
		Status: v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationObserved,
			Reason: v1alpha1.VerificationReasonOK, Observations: crdObs}}
}

func mustValue(t *testing.T, v any) acceptance.Value {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := acceptance.ParseValue(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// requireFits holds pv to the size cap, Observed and ok, with one
// observation per criterion.
func requireFits(t *testing.T, pv *v1alpha1.PreviewVerification) {
	t.Helper()
	if pv.Status.Phase != v1alpha1.VerificationObserved || pv.Status.Reason != v1alpha1.VerificationReasonOK {
		t.Fatalf("phase %s %s, want Observed ok: the app's bytes are never maul's trouble", pv.Status.Phase,
			pv.Status.Reason)
	}
	if len(pv.Status.Observations) != len(pv.Spec.Criteria) {
		t.Fatalf("%d observations kept of %d", len(pv.Status.Observations), len(pv.Spec.Criteria))
	}
	if n := objectSize(pv); n > maxVerificationObject {
		t.Fatalf("status is %d bytes, over %d", n, maxVerificationObject)
	}
}

// An application that fills every capture of every step with 128 '<'
// (each 770 bytes once the API machinery HTML-escapes it) and fails each
// criterion's last step cannot push the status over its cap: captures the
// verdict does not need go to TooLarge, the one a later step's path is
// built from is kept exactly, and every verdict stays what it was. The plan
// has as many such criteria (of three steps, four captures each) as the
// grammar's wire budget admits.
func TestVerificationStatusShrinksAppInflatedCaptures(t *testing.T) {
	big := strings.Repeat("<", acceptance.MaxCaptureBytes)
	var cs []acceptance.Criterion
	for i := range acceptance.MaxCriteria {
		c := acceptance.Criterion{ID: fmt.Sprintf("c%d", i), Kind: acceptance.KindHTTP,
			Statement: "the flow works", Target: "api"}
		for s := range 3 {
			path := fmt.Sprintf("/s%d", s)
			if s == 1 {
				path = "/s1/{{capture.s0c0}}"
			}
			st := acceptance.Step{Request: acceptance.Request{Path: path}, Capture: map[string]string{}}
			for k := range acceptance.MaxCaptures {
				st.Capture[fmt.Sprintf("s%dc%d", s, k)] = fmt.Sprintf("/c%d", k)
			}
			c.Steps = append(c.Steps, st)
		}
		if acceptance.Validate(append(cs, c)) != nil {
			break
		}
		cs = append(cs, c)
	}
	body := jsonBody(t, map[string]string{"c0": big, "c1": big, "c2": big, "c3": big})
	pv := observedVerification(t, cs, func(acceptance.Criterion) acceptance.Doer {
		return func(call acceptance.Call) acceptance.Response {
			status := int32(200)
			if call.Step == 2 {
				status = 500
			}
			return acceptance.Response{Status: status, Body: body}
		}
	})
	if n := objectSize(pv); n <= maxVerificationObject {
		t.Fatalf("fixture is %d bytes, not over %d", n, maxVerificationObject)
	}
	before := judgeAll(t, pv, fitRun)
	fitStatus(pv)
	requireFits(t, pv)
	after := judgeAll(t, pv, fitRun)
	for i := range before {
		if before[i].Outcome != acceptance.OutcomeFail || before[i].Reason != acceptance.ReasonExpectation ||
			len(before[i].Steps) != 3 {
			t.Fatalf("fixture criterion %s judged %+v, want an expectation failure on step 3", before[i].ID,
				before[i])
		}
		if !reflect.DeepEqual(before[i], after[i]) {
			t.Errorf("criterion %s judged %+v after fitting, %+v before", before[i].ID, after[i], before[i])
		}
	}
	// Judge reads the deciding try's captures (a failed first try stays
	// failed whatever it held): there, the one step 2's path is built from is
	// kept exactly.
	shrunk := 0
	for _, o := range pv.Status.Observations {
		deciding := o.Tries[len(o.Tries)-1]
		if got := deciding.Steps[0].Captures["s0c0"]; got != big {
			t.Errorf("%s: the capture step 2's path is built from became %q", o.ID, got)
		}
		for _, try := range o.Tries {
			for _, step := range try.Steps {
				for _, v := range step.Captures {
					if v == acceptance.TooLarge {
						shrunk++
					}
				}
			}
		}
	}
	if shrunk == 0 {
		t.Error("no capture was shrunk")
	}
}

// One criterion whose observation cannot shrink (every value it records
// decides its verdict) and is alone over the cap fails on its own as
// observation-too-large; every other criterion keeps its real verdict,
// passes included.
func TestVerificationStatusDropsOnlyTheHugeCriterion(t *testing.T) {
	long := strings.Repeat("<", acceptance.MaxValueBytes-2)
	header := strings.Repeat("<", acceptance.MaxHeaderBytes-6)
	huge := acceptance.Criterion{ID: "huge", Kind: acceptance.KindHTTP, Statement: "echoes", Target: "api"}
	doc := map[string]string{}
	for s := range acceptance.MaxStepsPerHTTP {
		st := acceptance.Step{Request: acceptance.Request{Path: fmt.Sprintf("/h%d", s)},
			Expect: acceptance.Expect{JSON: map[string]acceptance.Value{}, Headers: map[string]acceptance.Value{}}}
		for k := range acceptance.MaxExpectPointers {
			st.Expect.JSON[fmt.Sprintf("/p%d", k)] = mustValue(t, long)
			doc[fmt.Sprintf("p%d", k)] = long
		}
		for k := range acceptance.MaxExpectHeaders {
			st.Expect.Headers[fmt.Sprintf("X-H%d", k)] = mustValue(t, header)
		}
		huge.Steps = append(huge.Steps, st)
	}
	cs := make([]acceptance.Criterion, 0, 16)
	cs = append(cs, huge)
	for i := range 15 {
		cs = append(cs, acceptance.Criterion{ID: fmt.Sprintf("small%d", i), Kind: acceptance.KindHTTP,
			Statement: "answers", Target: "api", Steps: []acceptance.Step{{Request: acceptance.Request{Path: "/ok"}}}})
	}
	hugeBody := jsonBody(t, doc)
	headers := map[string][]string{}
	for k := range acceptance.MaxExpectHeaders {
		headers[fmt.Sprintf("X-H%d", k)] = []string{header}
	}
	pv := observedVerification(t, cs, func(c acceptance.Criterion) acceptance.Doer {
		return func(call acceptance.Call) acceptance.Response {
			if c.ID == "huge" {
				status := int32(200)
				if call.Step == acceptance.MaxStepsPerHTTP-1 {
					status = 500
				}
				return acceptance.Response{Status: status, Body: hugeBody, Header: headers}
			}
			var n int
			_, _ = fmt.Sscanf(c.ID, "small%d", &n)
			if n%2 == 1 {
				return acceptance.Response{Status: 404}
			}
			return acceptance.Response{Status: 200}
		}
	})
	if n := objectSize(pv); n <= maxVerificationObject {
		t.Fatalf("fixture is %d bytes, not over %d", n, maxVerificationObject)
	}
	before := judgeAll(t, pv, fitRun)
	if before[0].Outcome != acceptance.OutcomeFail || before[0].Reason != acceptance.ReasonExpectation {
		t.Fatalf("fixture: huge judged %+v, want an expectation failure", before[0])
	}
	fitStatus(pv)
	requireFits(t, pv)
	after := judgeAll(t, pv, fitRun)
	if after[0].Outcome != acceptance.OutcomeFail || after[0].Reason != acceptance.ReasonObservationTooLarge {
		t.Errorf("huge judged %+v, want fail observation-too-large", after[0])
	}
	if len(pv.Status.Observations[0].Tries) != 0 {
		t.Errorf("huge kept %d tries, want only the marker", len(pv.Status.Observations[0].Tries))
	}
	passes := 0
	for i := 1; i < len(before); i++ {
		if !reflect.DeepEqual(before[i], after[i]) {
			t.Errorf("criterion %s judged %+v after fitting, %+v before", before[i].ID, after[i], before[i])
		}
		if after[i].Outcome == acceptance.OutcomePass {
			passes++
		}
	}
	if passes == 0 {
		t.Error("fixture has no passing small criterion to keep")
	}
}

// The log read and the grammar's plan-wide budget are one bound: whatever
// an accepted plan's application answers, the tester's output is read
// whole, with room left over for the tester's own log lines.
func TestTesterLogLimitIsTheGrammars(t *testing.T) {
	if maxTesterLog != acceptance.MaxTesterLogBytes {
		t.Fatalf("maxTesterLog %d, the grammar's MaxTesterLogBytes %d", maxTesterLog, acceptance.MaxTesterLogBytes)
	}
	if margin := maxTesterLog - acceptance.MaxWireBytes; margin < 64<<10 {
		t.Fatalf("the budget %d leaves %d bytes of the %d read for the tester's own lines, want 64 KiB",
			acceptance.MaxWireBytes, margin, maxTesterLog)
	}
}

// objectSize measures the object as the API machinery sends it: the
// client's JSON serializer escapes HTML, as json.Marshal does.
func TestObjectSizeIsTheSerializedSize(t *testing.T) {
	pv, _ := randomVerification(t, rand.New(rand.NewSource(fitSeed)))
	ser := jsonserializer.NewSerializerWithOptions(jsonserializer.DefaultMetaFactory, kube.Scheme(), kube.Scheme(),
		jsonserializer.SerializerOptions{})
	var b bytes.Buffer
	if err := ser.Encode(pv, &b); err != nil {
		t.Fatal(err)
	}
	if got, want := objectSize(pv), len(bytes.TrimSuffix(b.Bytes(), []byte("\n"))); got != want {
		t.Errorf("objectSize %d, the serializer writes %d", got, want)
	}
}

// No observation content an application can choose makes fitStatus settle
// Infra or drop the observations, and fitting never changes a verdict but
// to fail observation-too-large: a fail never becomes a pass.
func TestFitStatusNeverHidesAppFailures(t *testing.T) {
	r := rand.New(rand.NewSource(fitSeed))
	over := 0
	for n := range 250 {
		pv, cs := randomVerification(t, r)
		if objectSize(pv) > maxVerificationObject {
			over++
		}
		before := judgeAll(t, pv, fitRun)
		fitStatus(pv)
		t.Run(fmt.Sprintf("case%d", n), func(t *testing.T) {
			requireFits(t, pv)
			after := judgeAll(t, pv, fitRun)
			for i := range cs {
				if reflect.DeepEqual(before[i], after[i]) {
					continue
				}
				if after[i].Outcome != acceptance.OutcomeFail || after[i].Reason != acceptance.ReasonObservationTooLarge {
					t.Errorf("criterion %s judged %+v after fitting, %+v before", cs[i].ID, after[i], before[i])
				}
			}
		})
	}
	if over < 25 {
		t.Fatalf("only %d of 250 cases were over the cap: the property is not exercised", over)
	}
}

// hostile is n bytes of text an application might answer with to inflate
// the status: characters the API machinery escapes six-fold, which the
// canonical form keeps as they are, so a value at its bound is still
// recorded.
func hostile(r *rand.Rand, n int) string {
	const alphabet = "<>&"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	if n > 0 {
		b[0] = '<'
	}
	return string(b)
}

// size is a value length biased to its bound: mostly exactly at it, else
// anywhere up to it or just past it.
func size(r *rand.Rand, limit int) int {
	switch r.Intn(6) {
	case 0:
		return r.Intn(limit)
	case 1:
		return limit + 1 + r.Intn(32)
	default:
		return limit
	}
}

// randomVerification is a random valid plan of up to 16 http criteria (up
// to MaxSteps steps in all) with pointers, headers and captures of every
// kind, observed against a hostile application that answers each value
// with escape-heavy text near or past its bound, echoes expectations at
// random so some steps pass, and fails at random.
func randomVerification(t *testing.T, r *rand.Rand) (*v1alpha1.PreviewVerification, []acceptance.Criterion) {
	t.Helper()
	var cs []acceptance.Criterion
	for cs == nil || acceptance.Validate(cs) != nil { // over the wire budget: draw again
		n := 1 + r.Intn(16)
		if r.Intn(2) == 0 {
			n = 16
		}
		budget := acceptance.MaxSteps
		cs = make([]acceptance.Criterion, 0, n)
		for i := range n {
			steps := min(1+r.Intn(acceptance.MaxStepsPerHTTP), budget-(n-1-i))
			budget -= steps
			cs = append(cs, randomCriterion(t, r, fmt.Sprintf("c%d", i), steps))
		}
	}
	pv := observedVerification(t, cs, func(c acceptance.Criterion) acceptance.Doer {
		return hostileDoer(r, acceptance.Normalize([]acceptance.Criterion{c})[0])
	})
	return pv, acceptance.Normalize(cs)
}

// sometimes is a count of up to limit a third of the time, else none.
func sometimes(r *rand.Rand, limit int) int {
	if r.Intn(3) > 0 {
		return 0
	}
	return r.Intn(limit + 1)
}

func randomCriterion(t *testing.T, r *rand.Rand, id string, steps int) acceptance.Criterion {
	c := acceptance.Criterion{ID: id, Kind: acceptance.KindHTTP, Statement: "works", Target: "api"}
	captured := make([]string, 0, steps*acceptance.MaxCaptures)
	for s := range steps {
		st := acceptance.Step{Request: acceptance.Request{Path: fmt.Sprintf("/s%d", s)},
			Expect: acceptance.Expect{JSON: map[string]acceptance.Value{}, Headers: map[string]acceptance.Value{}}}
		if len(captured) > 0 && r.Intn(2) == 0 {
			st.Request.Path += "/{{capture." + captured[r.Intn(len(captured))] + "}}"
		}
		// Pointers lean to their bound: their values cost the status (which
		// escapes HTML) up to three times what they cost the wire budget,
		// so they are what can still put an accepted plan over the cap.
		pointers := acceptance.MaxExpectPointers
		if r.Intn(4) == 0 {
			pointers = r.Intn(acceptance.MaxExpectPointers + 1)
		}
		for k := range pointers {
			st.Expect.JSON[fmt.Sprintf("/p%d", k)] = randomMatcher(t, r, captured, false)
		}
		for k := range sometimes(r, acceptance.MaxExpectHeaders) {
			st.Expect.Headers[fmt.Sprintf("X-H%d", k)] = randomMatcher(t, r, nil, true)
		}
		if r.Intn(3) == 0 {
			st.Expect.BodyContains = "<<"
		}
		for k := range sometimes(r, acceptance.MaxCaptures) {
			if st.Capture == nil {
				st.Capture = map[string]string{}
			}
			name := fmt.Sprintf("s%dc%d", s, k)
			st.Capture[name] = fmt.Sprintf("/c%d", k)
			captured = append(captured, name)
		}
		c.Steps = append(c.Steps, st)
	}
	return c
}

// randomMatcher is a literal of '<' text, $contains or, for a pointer,
// $present, $absent or the literal of a capture.
func randomMatcher(t *testing.T, r *rand.Rand, captured []string, header bool) acceptance.Value {
	limit := min(acceptance.MaxValueBytes, acceptance.MaxHeaderBytes) - 2
	switch k := r.Intn(5); {
	case k == 0 && !header:
		return mustValue(t, map[string]bool{"$present": true})
	case k == 1 && !header:
		return mustValue(t, map[string]bool{"$absent": true})
	case k == 2:
		return mustValue(t, map[string]string{"$contains": "<"})
	case k == 3 && len(captured) > 0 && !header:
		return mustValue(t, "{{capture."+captured[r.Intn(len(captured))]+"}}")
	}
	return mustValue(t, strings.Repeat("<", min(size(r, limit), limit)))
}

// hostileDoer answers c's steps as a hostile application would: mostly
// meeting each expectation with values at their bound (so tries run long),
// sometimes answering hostile text instead, and failing a step's status at
// random, a last step's half the time, so many criteria record two tries.
func hostileDoer(r *rand.Rand, c acceptance.Criterion) acceptance.Doer {
	return func(call acceptance.Call) acceptance.Response {
		st := c.Steps[call.Step]
		status := int32(200)
		if r.Intn(40) == 0 || (call.Step == len(c.Steps)-1 && r.Intn(2) == 0) {
			status = 500
		}
		doc := map[string]any{}
		for ptr, v := range st.Expect.JSON {
			if s, ok := answer(r, v, call.Captures, acceptance.MaxValueBytes-2); ok {
				doc[strings.TrimPrefix(ptr, "/")] = s
			}
		}
		for _, ptr := range st.Capture {
			doc[strings.TrimPrefix(ptr, "/")] = hostile(r, size(r, acceptance.MaxCaptureBytes))
		}
		headers := map[string][]string{}
		for name, v := range st.Expect.Headers {
			if s, ok := answer(r, v, call.Captures, acceptance.MaxHeaderBytes); ok {
				headers[name] = []string{s}
			}
		}
		body, _ := json.Marshal(doc)
		return acceptance.Response{Status: status, Body: body, Header: headers}
	}
}

// answer is the application's value for an expectation: most of the time
// one that meets it (the literal, the capture it names, hostile text that
// is present or contains '<', nothing for $absent), else hostile text of
// any size. ok is false for an absent value.
func answer(r *rand.Rand, v acceptance.Value, caps map[string]string, limit int) (string, bool) {
	if r.Intn(8) == 0 {
		return hostile(r, size(r, limit)), true
	}
	var lit string
	if json.Unmarshal([]byte(v.String()), &lit) == nil {
		if name, ok := strings.CutPrefix(lit, "{{capture."); ok {
			return caps[strings.TrimSuffix(name, "}}")], true
		}
		return lit, true
	}
	if strings.Contains(v.String(), "$absent") {
		return "", false
	}
	return hostile(r, limit), true
}
