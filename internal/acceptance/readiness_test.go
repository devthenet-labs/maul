// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
)

// reachedWaits is a run whose one target accepted its first connection.
var reachedWaits = []TargetWait{{Target: "api", WaitMs: 2, Attempts: 1}}

// mustWireWriter is a writer for a run whose targets were all reached.
func mustWireWriter(t *testing.T, w *bytes.Buffer) *WireWriter {
	t.Helper()
	ww, err := NewWireWriter(w, reachedWaits)
	if err != nil {
		t.Fatal(err)
	}
	return ww
}

var okObservation = CriterionObservation{ID: "a", Tries: []TryObservation{{Run: "v1a1t1-abcdef",
	Steps: []StepObservation{{Method: "GET", Path: "/", Status: 200, TransportError: TransportNone}}}}}

// TestWireCarriesWaits: the readiness wait the tester made reads back
// exactly, from the log's summary and from the termination message.
func TestWireCarriesWaits(t *testing.T) {
	waits := []TargetWait{
		{Target: "api", WaitMs: 1203, Attempts: 5, Refused: 3, Timeout: 1},
		{Target: "web", WaitMs: 0, Attempts: 1},
	}
	var b bytes.Buffer
	w, err := NewWireWriter(&b, waits)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(okObservation); err != nil {
		t.Fatal(err)
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sum.Waits, waits) || sum.Unreached() || sum.Count != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	obs, got, err := ParseWire(b.Bytes())
	if err != nil || len(obs) != 1 || !got.Equal(sum) {
		t.Fatalf("ParseWire = %+v, %+v, %v\n%s", obs, got, err, b.String())
	}
	term, err := ParseSummary(sum.TerminationMessage())
	if err != nil || !term.Equal(sum) {
		t.Errorf("ParseSummary = %+v, %v; want %+v", term, err, sum)
	}
	if other := (Summary{Count: sum.Count, SHA256: sum.SHA256, Waits: waits[:1]}); other.Equal(sum) {
		t.Error("Equal ignores the waits")
	}
}

// TestWireUnreachable: a target never reached ends the run before its
// first criterion. The writer refuses an observation, and the summary
// names the target, with no observation, which the parser reads back.
func TestWireUnreachable(t *testing.T) {
	waits := []TargetWait{
		{Target: "api", WaitMs: 15000, Attempts: 20, Refused: 19, Timeout: 1},
		{Target: "web", WaitMs: 3, Attempts: 1},
	}
	var b bytes.Buffer
	w, err := NewWireWriter(&b, waits)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(okObservation); err == nil {
		t.Error("the writer wrote an observation after an unreachable target")
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sum.Unreachable, []string{"api"}) || sum.Count != 0 || !sum.Unreached() {
		t.Fatalf("summary = %+v, want api unreachable and nothing observed", sum)
	}
	obs, got, err := ParseWire(b.Bytes())
	if err != nil || len(obs) != 0 || !got.Equal(sum) {
		t.Fatalf("ParseWire = %+v, %+v, %v\n%s", obs, got, err, b.String())
	}
	if !strings.Contains(b.String(), `"unreachable":["api"]`) {
		t.Errorf("the summary line does not name the target: %s", b.String())
	}
}

// TestNewWireWriterRefusals: there is no writer without a well-formed
// readiness wait, so no summary is ever written without one.
func TestNewWireWriterRefusals(t *testing.T) {
	ok := TargetWait{Target: "api", WaitMs: 1, Attempts: 2, Refused: 1}
	many := make([]TargetWait, MaxWaitTargets+1)
	for i := range many {
		many[i] = TargetWait{Target: string(rune('a' + i)), Attempts: 1}
	}
	for name, waits := range map[string][]TargetWait{
		"none":           nil,
		"too many":       many,
		"out of order":   {{Target: "web", Attempts: 1}, ok},
		"duplicate":      {ok, ok},
		"not a key":      {{Target: "API", Attempts: 1}},
		"long key":       {{Target: strings.Repeat("a", 17), Attempts: 1}},
		"no attempt":     {{Target: "api"}},
		"too many tries": {{Target: "api", Attempts: MaxWaitAttempts + 1, Refused: MaxWaitAttempts + 1}},
		"too long":       {{Target: "api", Attempts: 1, WaitMs: MaxWaitMs + 1}},
		"negative wait":  {{Target: "api", Attempts: 1, WaitMs: -1}},
		"negative count": {{Target: "api", Attempts: 1, Refused: -1, Timeout: 1}},
		"unaccounted":    {{Target: "api", Attempts: 5, Refused: 2}},
		"over-counted":   {{Target: "api", Attempts: 2, Refused: 2, DNS: 1}},
		// Counts whose int32 sum wraps to the attempts: each count is
		// bounded first, so the sum cannot overflow.
		"wrapping counts": {{Target: "api", Attempts: 1, Refused: math.MaxInt32, Timeout: math.MaxInt32, Reset: 2,
			DNS: 1}},
		"count over bound": {{Target: "api", Attempts: MaxWaitAttempts,
			Refused: MaxWaitAttempts + 1, Timeout: -1}},
	} {
		if _, err := NewWireWriter(&bytes.Buffer{}, waits); err == nil {
			t.Errorf("%s: NewWireWriter accepted %+v", name, waits)
		}
	}
}

// TestSummaryStrict: the parser holds a summary to what the tester writes,
// from the log and from the termination message alike: every target's
// wait, the unreachable ones named exactly, nothing observed after one.
func TestSummaryStrict(t *testing.T) {
	head := func(count string) string { return `{"count":` + count + `,"sha256":"` + strings.Repeat("0", 64) + `"` }
	const refused = `,"waits":[{"target":"api","attempts":1,"refused":1}]`
	for name, msg := range map[string]string{
		"no waits":         head("0") + `}`,
		"empty waits":      head("0") + `,"waits":[]}`,
		"unknown field":    head("0") + `,"waits":[{"target":"api","attempts":1,"colour":1}]}`,
		"claims reached":   head("0") + `,"waits":[{"target":"api","attempts":1}],"unreachable":["api"]}`,
		"unlisted":         head("0") + refused + `}`,
		"names another":    head("0") + refused + `,"unreachable":["web"]}`,
		"observed after":   head("1") + refused + `,"unreachable":["api"]}`,
		"listed twice":     head("0") + refused + `,"unreachable":["api","api"]}`,
		"unaccounted wait": head("0") + `,"waits":[{"target":"api","attempts":3,"refused":1}]}`,
		"wrapping counts": head("0") + `,"waits":[{"target":"api","attempts":1,"refused":2147483647,` +
			`"timeout":2147483647,"reset":2,"dns":1}],"unreachable":["api"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if s, err := ParseSummary(msg); err == nil {
				t.Errorf("ParseSummary accepted %+v", s)
			}
			if _, _, err := ParseWire([]byte(WirePrefix + `{"v":2,"summary":` + msg + "}\n")); !errors.Is(err,
				ErrWireInvalid) {
				t.Errorf("ParseWire = %v, want ErrWireInvalid", err)
			}
		})
	}
	good := head("0") + `,"waits":[{"target":"api","waitMs":9,"attempts":2,"refused":1}]}`
	if _, err := ParseSummary(good); err != nil {
		t.Errorf("ParseSummary(%s) = %v", good, err)
	}
}

// TestWorstSummaryIsBounded: the longest summary is in WireBound, and its
// termination message is small (the kubelet keeps 4 KiB of one).
func TestWorstSummaryIsBounded(t *testing.T) {
	if n := MaxSummaryBytes(); n > 2<<10 {
		t.Errorf("the longest termination message is %d bytes", n)
	}
	if summaryWireBytes() < MaxSummaryBytes() {
		t.Errorf("summaryWireBytes %d is under the longest summary's %d", summaryWireBytes(), MaxSummaryBytes())
	}
}

// TestUnreached: the one definition of the targets a run never reached.
func TestUnreached(t *testing.T) {
	waits := []TargetWait{{Target: "api", Attempts: 2, Refused: 2}, {Target: "db", Attempts: 1},
		{Target: "web", Attempts: 3, DNS: 3}}
	if got := Unreached(waits); !slices.Equal(got, []string{"api", "web"}) {
		t.Errorf("Unreached = %v", got)
	}
	if got := Unreached(waits[1:2]); got != nil {
		t.Errorf("Unreached of a reached run = %v, want nil", got)
	}
}

// oldParserAccepts models a controller that reads only wire version 1, as
// every release before this one: each prefixed line must declare v 1.
func oldParserAccepts(log []byte) bool {
	for _, line := range bytes.Split(log, []byte("\n")) {
		body, ok := bytes.CutPrefix(line, []byte(WirePrefix))
		if !ok {
			continue
		}
		var l struct {
			V int `json:"v"`
		}
		if json.Unmarshal(body, &l) != nil || l.V != 1 {
			return false
		}
	}
	return true
}

// TestWireVersionSkew: the tester image is pinned apart from the
// controllers (mirrored or overridden), so the two can disagree on the wire
// version. An old tester's output, read by this controller, is a version
// mismatch naming both versions, never output that is merely invalid; so is
// a newer tester's. This tester's output, read by an old controller, is
// refused outright (never misread as observations), since every line
// declares the new version.
func TestWireVersionSkew(t *testing.T) {
	sha := strings.Repeat("0", 64)
	oldLog := WirePrefix + `{"v":1,"observation":{"id":"a","tries":[{"run":"v1a1t1-abcdef","steps":[]}]}}` + "\n" +
		WirePrefix + `{"v":1,"summary":{"count":1,"sha256":"` + sha + `"}}` + "\n"
	for name, c := range map[string]struct {
		log string
		got int
	}{
		"old tester":          {oldLog, 1},
		"old tester, summary": {WirePrefix + `{"v":1,"summary":{"count":0,"sha256":"` + sha + `"}}` + "\n", 1},
		"newer tester":        {strings.ReplaceAll(oldLog, `{"v":1,`, `{"v":3,`), 3},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseWire([]byte(c.log))
			var ve *WireVersionError
			if !errors.As(err, &ve) || ve.Got != c.got || !errors.Is(err, ErrWireVersion) {
				t.Fatalf("ParseWire = %v, want a version mismatch from v%d", err, c.got)
			}
			if msg := err.Error(); !strings.Contains(msg, fmt.Sprintf("v%d", c.got)) ||
				!strings.Contains(msg, fmt.Sprintf("v%d", WireVersion)) {
				t.Errorf("error %q does not name both versions", msg)
			}
			if errors.Is(err, ErrWireInvalid) {
				t.Error("a version mismatch reads as invalid output")
			}
		})
	}
	if WireVersion < 2 {
		t.Fatalf("WireVersion %d: the summary's required waits changed the format", WireVersion)
	}
	var b bytes.Buffer
	w := mustWireWriter(t, &b)
	if err := w.Write(okObservation); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if oldParserAccepts(b.Bytes()) {
		t.Errorf("a v1-only controller would read this tester's output:\n%s", b.String())
	}
	if !oldParserAccepts([]byte(oldLog)) {
		t.Error("the old-controller model refuses old output: it models nothing")
	}
}
