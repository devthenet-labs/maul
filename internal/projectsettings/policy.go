// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// Policy is the operator's bounds on what the page may change: the maul
// chart's statusServer.projectSettings (allow, locked, ceilings, floors),
// as one JSON object. The chart renders that object once and both of its
// readers take it from there: the status server (ParsePolicy), which
// refuses a change before it is written, and the admission policy on the
// status server's ServiceAccount, which refuses it again.
type Policy struct {
	// Allow are the switches, each off unless set.
	Allow ProjectSettingsAllow `json:"allow"`
	// Locked are the groups no save may change.
	Locked []Group `json:"locked"`
	// Ceilings are the most a save may raise each uncapped limit to.
	Ceilings Ceilings `json:"ceilings"`
	// Floors are the least a save may lower each evaluator limit to.
	Floors Floors `json:"floors"`
}

// Ceilings are the operator's page ceilings for the limits intent-controller
// itself does not clamp. The rule for each is "the new value is at most the
// ceiling, or at most the old value": a higher value an operator set may be
// kept or lowered from the page, never raised.
type Ceilings struct {
	// MaxCostMicroUSD bounds limits.maxCostMicroUSD.
	MaxCostMicroUSD int64 `json:"maxCostMicroUSD"`
	// MaxRevisions bounds limits.maxRevisions.
	MaxRevisions int32 `json:"maxRevisions"`
	// MaxCheckFixes bounds limits.maxCheckFixes.
	MaxCheckFixes int32 `json:"maxCheckFixes"`
	// MaxVerifyFixes bounds limits.maxVerifyFixes.
	MaxVerifyFixes int32 `json:"maxVerifyFixes"`
	// MaxVerifications bounds limits.maxVerifications.
	MaxVerifications int32 `json:"maxVerifications"`
	// MaxActiveIntents bounds limits.maxActiveIntents.
	MaxActiveIntents int32 `json:"maxActiveIntents"`
}

// Floors are the operator's floors under the evaluator's limits: starved of
// turns, time or money every evaluation ends unconfirmed, which is the
// evaluator switched off by another name. The rule for each is "the new
// value is at least the floor, or at least the old value", unless
// Allow.DisableEvaluator. Zero is no floor.
type Floors struct {
	// MaxEvaluateCostMicroUSD is under limits.maxEvaluateCostMicroUSD.
	MaxEvaluateCostMicroUSD int64 `json:"maxEvaluateCostMicroUSD"`
	// EvaluateMaxTurns is under limits.evaluate.maxTurns.
	EvaluateMaxTurns int32 `json:"evaluateMaxTurns"`
	// EvaluateTimeout is under limits.evaluate.timeout.
	EvaluateTimeout Duration `json:"evaluateTimeout"`
	// EvaluateTokenBudget is under limits.evaluate.tokenBudget.
	EvaluateTokenBudget int64 `json:"evaluateTokenBudget"`
	// MaxToolCalls is under verification.maxToolCalls.
	MaxToolCalls int32 `json:"maxToolCalls"`
	// MaxVerifications is under limits.maxVerifications.
	MaxVerifications int32 `json:"maxVerifications"`
	// HandoffMax is under verification.handoffMax.
	HandoffMax Duration `json:"handoffMax"`
}

// Duration is a duration a Policy writes as Go and Kubernetes do ("30m"),
// which is also how an admission policy's duration() reads it.
type Duration time.Duration

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON reads a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("a duration is a string such as \"30m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// DefaultPolicy is the chart's default: every switch off, nothing locked,
// and the ceilings and floors the chart's values start from.
func DefaultPolicy() Policy {
	return Policy{
		Locked: []Group{},
		Ceilings: Ceilings{
			MaxCostMicroUSD:  25_000_000,
			MaxRevisions:     10,
			MaxCheckFixes:    10,
			MaxVerifyFixes:   5,
			MaxVerifications: 50,
			MaxActiveIntents: 5,
		},
		Floors: Floors{
			MaxEvaluateCostMicroUSD: 2_000_000,
			EvaluateMaxTurns:        40,
			EvaluateTimeout:         Duration(10 * time.Minute),
			EvaluateTokenBudget:     100_000,
			MaxToolCalls:            100,
			MaxVerifications:        3,
			HandoffMax:              Duration(30 * time.Minute),
		},
	}
}

// ParsePolicy reads the chart's JSON. It is strict (an unknown key or
// trailing data is refused), so a switch the status server does not know
// is a startup error, never a silently ignored one, and the result is held
// to Validate.
func ParsePolicy(raw []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("project settings policy: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Policy{}, errors.New("project settings policy: more than one JSON value")
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Validate holds a Policy to the Project schema: a locked group is one of
// Groups, and every ceiling and floor is a value the field itself could
// hold, so a bound can never be one no save could meet.
func (p Policy) Validate() error {
	for _, g := range p.Locked {
		if !slices.Contains(Groups(), g) {
			return fmt.Errorf("project settings policy: locked names %q, which is not a group (%v)", g, Groups())
		}
	}
	for _, b := range append(p.ceilings(), p.floors()...) {
		r, _ := rowByID(b.field)
		if b.n == 0 && (b.floor || r.Min == 0) {
			continue
		}
		if b.n < r.Min || b.n > r.Max {
			return fmt.Errorf("project settings policy: %s %s is %d, outside the field's own range %d to %d",
				b.kind(), b.field, b.n, r.Min, r.Max)
		}
	}
	if f, c := int64(p.Floors.MaxVerifications), int64(p.Ceilings.MaxVerifications); f > c {
		return fmt.Errorf("project settings policy: the floor of %s (%d) is above its ceiling (%d)",
			FieldMaxVerifications, f, c)
	}
	return nil
}

// Locks reports whether the operator locked a group.
func (p Policy) Locks(g Group) bool { return slices.Contains(p.Locked, g) }

// bound is one ceiling or floor: the field it is on and its value in the
// field's own unit.
type bound struct {
	field string
	n     int64
	floor bool
}

func (b bound) kind() string {
	if b.floor {
		return "floor"
	}
	return "ceiling"
}

// ceilings are the page ceilings by field.
func (p Policy) ceilings() []bound {
	c := p.Ceilings
	return []bound{
		{field: FieldMaxCost, n: c.MaxCostMicroUSD},
		{field: FieldMaxRevisions, n: int64(c.MaxRevisions)},
		{field: FieldMaxCheckFixes, n: int64(c.MaxCheckFixes)},
		{field: FieldMaxVerifyFixes, n: int64(c.MaxVerifyFixes)},
		{field: FieldMaxVerifications, n: int64(c.MaxVerifications)},
		{field: FieldMaxActiveIntents, n: int64(c.MaxActiveIntents)},
	}
}

// floors are the floors by field.
func (p Policy) floors() []bound {
	f := p.Floors
	return []bound{
		{field: FieldMaxEvaluateCost, n: f.MaxEvaluateCostMicroUSD, floor: true},
		{field: StageField("evaluate", leafTurns), n: int64(f.EvaluateMaxTurns), floor: true},
		{field: StageField("evaluate", leafTimeout), n: wholeSeconds(time.Duration(f.EvaluateTimeout)), floor: true},
		{field: StageField("evaluate", leafTokens), n: f.EvaluateTokenBudget, floor: true},
		{field: FieldMaxToolCalls, n: int64(f.MaxToolCalls), floor: true},
		{field: FieldMaxVerifications, n: int64(f.MaxVerifications), floor: true},
		{field: FieldHandoffMax, n: wholeSeconds(time.Duration(f.HandoffMax)), floor: true},
	}
}

// Ceiling is the operator's page ceiling on a field; false when it has
// none.
func (p Policy) Ceiling(field string) (int64, bool) { return find(p.ceilings(), field) }

// Floor is the operator's floor under a field; false when it has none
// (the field has no floor, or the operator set it to zero).
func (p Policy) Floor(field string) (int64, bool) {
	n, ok := find(p.floors(), field)
	return n, ok && n > 0
}

func find(bounds []bound, field string) (int64, bool) {
	for _, b := range bounds {
		if b.field == field {
			return b.n, true
		}
	}
	return 0, false
}

// controllerCeiling is the ceiling intent-controller publishes for a field
// (status.admits), zero when it publishes none: the most the field takes
// effect at, whatever the Project says.
func controllerCeiling(a *v1alpha1.ProjectAdmits, field string) int64 {
	if a == nil {
		return 0
	}
	switch field {
	case FieldMaxEvaluateCost:
		return a.MaxEvaluateCostMicroUSD
	case FieldMaxToolCalls:
		return int64(a.MaxToolCalls)
	}
	for _, st := range stages {
		c := st.ceiling(&a.Stages)
		switch field {
		case StageField(st.name, leafTurns):
			return int64(c.MaxTurns)
		case StageField(st.name, leafTokens):
			return c.TokenBudget
		case StageField(st.name, leafTimeout):
			if c.Timeout != nil {
				return wholeSeconds(c.Timeout.Duration)
			}
		}
	}
	return 0
}

// clamped reports whether intent-controller clamps a field to a ceiling of
// its own: the fields controllerCeiling answers for.
func clamped(field string) bool {
	if field == FieldMaxEvaluateCost || field == FieldMaxToolCalls {
		return true
	}
	for _, st := range stages {
		if field == StageField(st.name, leafTurns) || field == StageField(st.name, leafTokens) ||
			field == StageField(st.name, leafTimeout) {
			return true
		}
	}
	return false
}
