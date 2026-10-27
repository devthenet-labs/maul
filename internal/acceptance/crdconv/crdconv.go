// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package crdconv

import (
	"bytes"
	"encoding/json"
	"fmt"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// RoleUnknown is the role a part fact that does not convert is given: a
// part maul knows nothing of, which acceptance.ClassifyEnvironment treats as
// infrastructure, never charging it to the intent.
const RoleUnknown = "unknown"

// CriteriaToCRD converts criteria (normalised: acceptance.Normalize) to
// the CRD mirror. A field the mirror lacks, such as the unfolded one-step
// shorthand, is refused, so a criterion is stored exactly as approved or
// not at all. An empty list is nil.
func CriteriaToCRD(cs []acceptance.Criterion) ([]v1alpha1.AcceptanceCriterion, error) {
	if len(cs) == 0 {
		return nil, nil
	}
	var out []v1alpha1.AcceptanceCriterion
	if err := convert(cs, &out); err != nil {
		return nil, fmt.Errorf("acceptance criteria: %w", err)
	}
	return out, nil
}

// CriteriaFromCRD converts stored criteria back to the grammar's form, as
// strictly. An empty list is nil.
func CriteriaFromCRD(cs []v1alpha1.AcceptanceCriterion) ([]acceptance.Criterion, error) {
	if len(cs) == 0 {
		return nil, nil
	}
	var out []acceptance.Criterion
	if err := convert(cs, &out); err != nil {
		return nil, fmt.Errorf("stored acceptance criteria: %w", err)
	}
	return out, nil
}

// ObservationsToCRD converts the tester's observations to the CRD mirror.
// An empty list is nil.
func ObservationsToCRD(obs []acceptance.CriterionObservation) ([]v1alpha1.CriterionObservation, error) {
	if len(obs) == 0 {
		return nil, nil
	}
	var out []v1alpha1.CriterionObservation
	if err := convert(obs, &out); err != nil {
		return nil, fmt.Errorf("acceptance observations: %w", err)
	}
	return out, nil
}

// ObservationFromCRD converts one stored observation to the grammar's form.
func ObservationFromCRD(o v1alpha1.CriterionObservation) (acceptance.CriterionObservation, error) {
	var out acceptance.CriterionObservation
	if err := convert(o, &out); err != nil {
		return acceptance.CriterionObservation{}, fmt.Errorf("stored acceptance observation: %w", err)
	}
	return out, nil
}

// PartFacts converts a verification's environment evidence to the
// grammar's form, one fact per fact, in order. A fact that does not convert
// keeps its name under RoleUnknown.
func PartFacts(env []v1alpha1.PartFact) []acceptance.PartFact {
	out := make([]acceptance.PartFact, 0, len(env))
	for _, f := range env {
		var a acceptance.PartFact
		if convert(f, &a) != nil {
			a = acceptance.PartFact{Name: f.Name, Role: RoleUnknown}
		}
		out = append(out, a)
	}
	return out
}

// convert re-decodes in's JSON into out, refusing any field out has no
// place for.
func convert(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
