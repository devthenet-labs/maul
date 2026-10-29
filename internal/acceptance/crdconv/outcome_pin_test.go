// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package crdconv

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// outcomeFields is the plan grammar's outcome, field for field (DESIGN 2.3,
// SPEC S1-01/S1-05): the JSON names an approved plan's acceptance list
// carries and v1alpha1.AcceptanceOutcome stores. acceptance.Outcome (S1-05)
// joins TestEvidenceMirrorsMatch's table against the same CRD type once it
// exists; until then this list is the contract both sides are held to.
var outcomeFields = []string{"id", "outcome", "from", "kind", "manual", "reason"}

// TestOutcomeMirrorFields: the CRD outcome has exactly the grammar's fields,
// every one but id and outcome optional.
func TestOutcomeMirrorFields(t *testing.T) {
	got := jsonFields(reflect.TypeFor[v1alpha1.AcceptanceOutcome]())
	names := make([]string, 0, len(got))
	for name, f := range got {
		names = append(names, name)
		required := name == "id" || name == "outcome"
		if omit := strings.Contains(f.Tag.Get("json"), ",omitempty"); omit == required {
			t.Errorf("AcceptanceOutcome.%s omitempty = %t, want %t", name, omit, !required)
		}
	}
	slices.Sort(names)
	want := slices.Clone(outcomeFields)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("AcceptanceOutcome JSON fields = %v, want %v", names, want)
	}
}

// TestOutcomeRoundTrip: a seeded property that any outcome survives the
// grammar's JSON shape (a strict decode into a struct holding only the
// grammar's fields) and back unchanged, so the conversion S1-11 writes can
// be the package's one strict convert.
func TestOutcomeRoundTrip(t *testing.T) {
	type grammarOutcome struct {
		ID      string `json:"id"`
		Outcome string `json:"outcome"`
		From    string `json:"from,omitempty"`
		Kind    string `json:"kind,omitempty"`
		Manual  bool   `json:"manual,omitempty"`
		Reason  string `json:"reason,omitempty"`
	}
	kinds := []v1alpha1.OutcomeKind{"", v1alpha1.OutcomeKindChange, v1alpha1.OutcomeKindKeep}
	prop := func(id, outcome, from, reason string, manual bool, k uint8) bool {
		in := v1alpha1.AcceptanceOutcome{ID: id, Outcome: outcome, From: from, Kind: kinds[int(k)%len(kinds)],
			Manual: manual, Reason: reason}
		var g grammarOutcome
		if err := convert(in, &g); err != nil {
			t.Logf("to grammar: %v", err)
			return false
		}
		var back v1alpha1.AcceptanceOutcome
		if err := convert(g, &back); err != nil {
			t.Logf("from grammar: %v", err)
			return false
		}
		a, _ := json.Marshal(in)
		b, _ := json.Marshal(back)
		return bytes.Equal(a, b) && reflect.DeepEqual(in, back)
	}
	cfg := &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(20261028))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}
