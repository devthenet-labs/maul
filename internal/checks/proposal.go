// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import "errors"

// ParseProposal decodes a check the evaluator proposes, as Parse does, and
// makes each label and role name that does not say exact match exactly
// (G4-B-BR-1): Exact true on every Label locator, Role locator with a Name
// and Select. A count's locators, within parents included, stay as
// written: an exact name over-narrows an absence (a count of 0 would pass
// beside an element named a little differently), so a count matches a
// part of the name unless the check says otherwise.
//
// The exact it writes is part of the check's canonical JSON and digest,
// so the body maul records and saves says it and plain Parse reads it back
// alike; a check saved before (no exact) keeps its bytes, its digest and
// the substring match it was proven with. Every reading of an agent's own
// check file or check run request goes through here; a stored body never
// does.
func ParseProposal(b []byte) (Check, error) {
	c, err := Parse(b)
	if err != nil || c.Browser == nil {
		return c, err
	}
	yes := true
	for i := range c.Browser.Steps {
		st := &c.Browser.Steps[i]
		if st.Select != nil && st.Select.Exact == nil {
			st.Select.Exact = &yes
		}
		if st.Expect != nil && st.Expect.Assert == AssertCount {
			continue
		}
		for _, l := range []*Locator{st.Click, fillLocator(st.Fill), pressLocator(st.Press), scrollLocator(st.Scroll),
			expectLocator(st.Expect, false), expectLocator(st.Expect, true)} {
			for p := l; p != nil; p = p.Within {
				if p.Exact == nil && (p.Label != "" || p.Role != "" && p.Name != "") {
					p.Exact = &yes
				}
			}
		}
	}
	return c, nil
}

// ErrNotRecorded is a check-run request whose check does not have the
// digest it was recorded against.
var ErrNotRecorded = errors.New("the check is not the one recorded against its digest")

// ParseRecorded is the check a check-run request's spec was run as, held
// to the digest ("sha256:<hex>") the run was recorded against: its
// ParseProposal form, or the plain Parse form a tools container from
// before exact matching ran and recorded, whichever has that digest. So a
// ledger written across an upgrade keeps the meaning each check was run
// with.
func ParseRecorded(spec []byte, digest string) (Check, error) {
	p, err := ParseProposal(spec)
	if err != nil {
		return Check{}, err
	}
	if Digest(p) == digest {
		return p, nil
	}
	if plain, _ := Parse(spec); Digest(plain) == digest {
		return plain, nil
	}
	return Check{}, ErrNotRecorded
}

func fillLocator(f *Fill) *Locator {
	if f == nil {
		return nil
	}
	return &f.Locator
}

func pressLocator(p *Press) *Locator {
	if p == nil {
		return nil
	}
	return p.Locator
}

func scrollLocator(s *Scroll) *Locator {
	if s == nil {
		return nil
	}
	return &s.Locator
}

func expectLocator(a *Assertion, other bool) *Locator {
	switch {
	case a == nil:
		return nil
	case other:
		return a.Other
	}
	return a.Locator
}
