// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// FingerprintVersion prefixes every fingerprint; a change to what is hashed
// is a new version, so a stored signature is never compared across one.
const FingerprintVersion = "maul-verify-fingerprint/1"

type fingerprintStep struct {
	Index     int       `json:"i"`
	Status    int32     `json:"s"`
	Transport Transport `json:"t"`
	Matched   []bool    `json:"m"`
}

type fingerprintCriterion struct {
	ID      string            `json:"id"`
	Outcome Outcome           `json:"o"`
	Reason  string            `json:"r"`
	Steps   []fingerprintStep `json:"steps"`
}

type fingerprintPart struct {
	Name          string `json:"n"`
	Role          string `json:"role"`
	Ready         bool   `json:"ready"`
	Waiting       string `json:"w"`
	Terminated    bool   `json:"term"`
	Reason        string `json:"tr"`
	ExitCode      int32  `json:"x"`
	Evicted       bool   `json:"e"`
	Unschedulable bool   `json:"u"`
	// Crashed, Disrupted and Unreachable are omitted when false, so a
	// fingerprint taken before they existed is unchanged.
	Crashed     bool `json:"cr,omitempty"`
	Disrupted   bool `json:"dis,omitempty"`
	Unreachable bool `json:"unr,omitempty"`
}

// Fingerprint names a verification's failure for RepeatedFailure detection
// (critique fix 10): sha256 over the failing criteria, by id, each with its
// outcome, reason and, per step, the index, status, transport class and the
// matched bit of every declared expectation; and over the failing parts, by
// name, each with its readiness, waiting reason, termination reason and exit
// code, eviction and schedulability, and whether it restarted during the
// attempt, was disrupted during it or was unreachable from the tester. Passing and quarantined criteria and
// healthy parts are left out; so is every actual value, body, latency, run
// token, restart count and log line, which change from run to run or are
// the application's to choose.
func Fingerprint(vs []Verdict, env []PartFact) string {
	var doc struct {
		Criteria []fingerprintCriterion `json:"c"`
		Parts    []fingerprintPart      `json:"p"`
	}
	doc.Criteria = []fingerprintCriterion{}
	doc.Parts = []fingerprintPart{}
	for _, v := range vs {
		if v.Outcome != OutcomeFail {
			continue
		}
		fc := fingerprintCriterion{ID: v.ID, Outcome: v.Outcome, Reason: v.Reason, Steps: []fingerprintStep{}}
		for _, s := range v.Steps {
			fc.Steps = append(fc.Steps, fingerprintStep{Index: s.Index, Status: s.Status, Transport: s.Transport,
				Matched: append([]bool{}, s.Matched...)})
		}
		doc.Criteria = append(doc.Criteria, fc)
	}
	for _, p := range env {
		if partClass(p) == EnvOK {
			continue
		}
		fp := fingerprintPart{Name: p.Name, Role: p.Role, Ready: p.Ready, Waiting: p.Waiting,
			Evicted: p.Evicted, Unschedulable: p.Unschedulable, Crashed: p.CrashedDuring,
			Disrupted: p.Disrupted, Unreachable: p.UnreachableForMs > 0}
		if t := p.LastTermination; t != nil {
			fp.Terminated, fp.Reason, fp.ExitCode = true, t.Reason, t.ExitCode
		}
		doc.Parts = append(doc.Parts, fp)
	}
	sort.Slice(doc.Criteria, func(i, j int) bool { return doc.Criteria[i].ID < doc.Criteria[j].ID })
	sort.Slice(doc.Parts, func(i, j int) bool {
		a, b := doc.Parts[i], doc.Parts[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Role < b.Role
	})
	b, _ := json.Marshal(doc) // plain structs: cannot fail
	sum := sha256.Sum256(b)
	return FingerprintVersion + ":" + hex.EncodeToString(sum[:])
}
