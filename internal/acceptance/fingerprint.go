// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

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

// failingParts is the fingerprint of every part of env that is not healthy,
// sorted by name and role; never nil.
func failingParts(env []httpstep.PartFact) []fingerprintPart {
	parts := []fingerprintPart{}
	for _, p := range env {
		if httpstep.ClassifyEnvironment([]httpstep.PartFact{p}).Clean() {
			continue
		}
		fp := fingerprintPart{Name: p.Name, Role: p.Role, Ready: p.Ready, Waiting: p.Waiting,
			Evicted: p.Evicted, Unschedulable: p.Unschedulable, Crashed: p.CrashedDuring,
			Disrupted: p.Disrupted, Unreachable: p.UnreachableForMs > 0}
		if t := p.LastTermination; t != nil {
			fp.Terminated, fp.Reason, fp.ExitCode = true, t.Reason, t.ExitCode
		}
		parts = append(parts, fp)
	}
	sort.Slice(parts, func(i, j int) bool {
		a, b := parts[i], parts[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Role < b.Role
	})
	return parts
}

// EvaluationFingerprintVersion prefixes every evaluator fingerprint. It is
// not slice 1's maul-verify-fingerprint/1: an evaluation round's failure is
// never compared with a slice-1 tester's.
const EvaluationFingerprintVersion = "maul-evaluation-fingerprint/1"

// EvaluationFingerprint names an evaluator round's failure for
// RepeatedFailure detection: sha256 over the failing outcomes' ids, the
// failing saved checks' digests (each set sorted, duplicates dropped) and
// the failing parts of env (failingParts: by name, each with its
// readiness, waiting reason, termination reason and exit code, eviction
// and schedulability, and whether it restarted during the attempt, was
// disrupted during it or was unreachable). Never a run token, ledger seq,
// observed value, restart count or log line, which change from run to run.
func EvaluationFingerprint(outcomes, checks []string, env []httpstep.PartFact) string {
	doc := struct {
		Outcomes []string          `json:"o"`
		Checks   []string          `json:"c"`
		Parts    []fingerprintPart `json:"p"`
	}{sortedSet(outcomes), sortedSet(checks), failingParts(env)}
	b, _ := json.Marshal(doc) // plain structs: cannot fail
	sum := sha256.Sum256(b)
	return EvaluationFingerprintVersion + ":" + hex.EncodeToString(sum[:])
}

// sortedSet is xs sorted with duplicates dropped; never nil.
func sortedSet(xs []string) []string {
	out := append([]string{}, xs...)
	sort.Strings(out)
	return slices.Compact(out)
}

// OutcomeFingerprintVersion prefixes every observed-evaluator fingerprint
// (Judge's). It is not EvaluationFingerprintVersion: a fingerprint, or a
// piece, recorded before the observed evaluator never matches one taken
// after it, so the first failure after the upgrade is never a repeat.
const OutcomeFingerprintVersion = "maul-evaluation-fingerprint/2"

// OutcomePiecePrefix starts an outcome's fingerprint piece
// (OutcomeFingerprintPiece); slice-era pieces start "o:".
const OutcomePiecePrefix = "o2:"

// OutcomeFingerprint names an observed round's failure for RepeatedFailure
// detection: sha256 over the failing outcomes' ids, each with its outcome
// digest, sorted, duplicates dropped. Nothing the evaluator chose (its
// citations, note or call arguments) goes in, and no environment: a fix
// is asked only of a clean environment.
func OutcomeFingerprint(failing []OutcomeRef) string {
	type piece struct {
		ID     string `json:"id"`
		Digest string `json:"d"`
	}
	pieces := make([]piece, 0, len(failing))
	for _, o := range failing {
		pieces = append(pieces, piece{ID: o.ID, Digest: o.Digest})
	}
	sort.Slice(pieces, func(i, j int) bool {
		if pieces[i].ID != pieces[j].ID {
			return pieces[i].ID < pieces[j].ID
		}
		return pieces[i].Digest < pieces[j].Digest
	})
	doc := struct {
		Outcomes []piece `json:"o"`
	}{slices.Compact(pieces)}
	b, _ := json.Marshal(doc) // plain structs: cannot fail
	sum := sha256.Sum256(b)
	return OutcomeFingerprintVersion + ":" + hex.EncodeToString(sum[:])
}

// OutcomeFingerprintPiece is one failing outcome's piece of a fix round's
// recorded failure: OutcomePiecePrefix, its id, a space and the
// OutcomeFingerprint of it alone.
func OutcomeFingerprintPiece(o OutcomeRef) string {
	return OutcomePiecePrefix + o.ID + " " + OutcomeFingerprint([]OutcomeRef{o})
}
