// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"encoding/base64"
	"strings"
)

// The tester's output bounds, shared by the grammar (Validate's plan-wide
// budget) and preview-controller (how much of the tester's log it reads),
// so the two cannot drift apart.
const (
	// MaxTesterLogBytes is the most of the tester's log preview-controller
	// reads; a longer log is refused as tester-output-invalid, maul's
	// trouble.
	MaxTesterLogBytes = 512 << 10
	// MaxWireBytes is the plan-wide budget: Validate refuses a plan whose
	// WireBound is over it, so no application can make an accepted plan's
	// wire output reach MaxTesterLogBytes. The quarter left over is for the
	// tester's own log lines.
	MaxWireBytes = MaxTesterLogBytes * 3 / 4
)

// WireBound is the most wire output the tester can write for cs, whatever
// the application answers: for each http criterion, two tries of every
// step, each step at every bound after the worst escaping the wire can
// apply, plus the summary line. The worst case is built and encoded, not
// estimated: each expected pointer's value is MaxValueBytes of backslashes
// (canonical JSON holds no raw control character or invalid UTF-8, so no
// byte of it takes more than two on the wire), each capture and header is
// all control characters (six bytes each as \u00XX, as many as an invalid
// byte's �), the path is MaxPathBytes of the path grammar's characters
// (none of which the wire escapes), and the numbers, transport class, run
// token and body prefix are their longest. Keys and the method are the
// plan's own.
func WireBound(cs []Criterion) int {
	n := summaryWireBytes()
	for _, c := range cs {
		n += criterionWireBound(c)
	}
	return n
}

// criterionWireBound is the longest wire line the tester can write for c.
func criterionWireBound(c Criterion) int {
	c = normalize(c)
	o := CriterionObservation{ID: c.ID}
	if c.Kind == KindHTTP {
		steps := make([]StepObservation, 0, len(c.Steps))
		for _, st := range c.Steps {
			steps = append(steps, worstStep(st))
		}
		run := strings.Repeat("v", maxRunBytes)
		o.Tries = []TryObservation{{Run: run, Steps: steps}, {Run: run, Steps: steps}}
	}
	line, err := encodeLine(wireLine{V: WireVersion, Observation: &o})
	if err != nil {
		return MaxWireBytes + 1
	}
	return len(line)
}

// worstStep is st's observation at every bound, escaped worst on the wire.
func worstStep(st Step) StepObservation {
	so := StepObservation{
		Method: st.Request.Method, Path: strings.Repeat("a", MaxPathBytes), Status: 999,
		TransportError: longestTransport(), JSONValid: true, BodyBytes: math32, LatencyMs: math32,
		BodyPrefix: strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxBodyPrefixBytes)),
	}
	if len(st.Expect.JSON) > 0 || st.Expect.BodyContains != "" {
		so.Pointers = map[string]string{}
		for ptr := range st.Expect.JSON {
			so.Pointers[ptr] = strings.Repeat(`\`, MaxValueBytes)
		}
		if st.Expect.BodyContains != "" {
			so.Pointers[KeyBodyContains] = bodyContainsProof("")
		}
	}
	if len(st.Capture) > 0 {
		so.Captures = map[string]string{}
		for name := range st.Capture {
			so.Captures[name] = strings.Repeat("\x01", MaxCaptureBytes)
		}
	}
	if len(st.Expect.Headers) > 0 {
		so.Headers = map[string]string{}
		for name := range st.Expect.Headers {
			so.Headers[strings.ToLower(name)] = strings.Repeat("\x01", MaxHeaderBytes)
		}
	}
	return so
}

func longestTransport() Transport {
	var out Transport
	for t := range transports {
		if len(t) > len(out) {
			out = t
		}
	}
	return out
}

// summaryWireBytes is the summary line's length at its longest.
func summaryWireBytes() int {
	line, err := encodeLine(wireLine{V: WireVersion, Summary: worstSummary()})
	if err != nil {
		return MaxWireBytes + 1
	}
	return len(line)
}

// worstSummary is a summary at every bound at once, which no tester writes
// (an unreachable target means no observation) but which bounds every one
// it can: MaxWaitTargets waits of the longest keys, every number at its
// largest, each named unreachable.
func worstSummary() *Summary {
	s := &Summary{Count: MaxCriteria, SHA256: strings.Repeat("f", 64)}
	for i := range MaxWaitTargets {
		key := strings.Repeat("k", MaxKeyLength-1) + string(rune('a'+i))
		s.Waits = append(s.Waits, TargetWait{Target: key, WaitMs: MaxWaitMs, Attempts: MaxWaitAttempts,
			Refused: MaxWaitAttempts, Timeout: MaxWaitAttempts, Reset: MaxWaitAttempts, DNS: MaxWaitAttempts})
		s.Unreachable = append(s.Unreachable, key)
	}
	return s
}

// MaxSummaryBytes is the longest summary's termination message
// (Summary.TerminationMessage), which the tester's container must hold.
func MaxSummaryBytes() int { return len(worstSummary().TerminationMessage()) }
