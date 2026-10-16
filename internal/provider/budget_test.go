// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package provider

import (
	"math/rand"
	"strconv"
	"testing"
	"testing/quick"
)

// budgetSeed fixes the property runs, so the gate stays deterministic.
const budgetSeed = 20261016

func TestParseBudget(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"60000000", 60_000_000, true},
		{"1", 1, true},
		{"0000000000000001", 1, true},
		{"9999999999999999", 9_999_999_999_999_999, true},
		{"", 0, false},
		{"0", 0, false},
		{"00", 0, false},
		{"-5", 0, false},
		{"+5", 0, false},
		{" 5", 0, false},
		{"5 ", 0, false},
		{"5e6", 0, false},
		{"5.0", 0, false},
		{"0x10", 0, false},
		{"10000000000000000", 0, false}, // seventeen digits
		{"٣", 0, false},                 // a non-ASCII digit
	} {
		got, ok := ParseBudget(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseBudget(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestParseBudgetProperties: every positive value of at most sixteen digits
// round-trips through its decimal form, and any string that is not one is
// refused rather than read as some other budget.
func TestParseBudgetProperties(t *testing.T) {
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(budgetSeed))}
	roundTrip := func(n int64) bool {
		n = 1 + (n&(1<<62-1))%9_999_999_999_999_999
		got, ok := ParseBudget(strconv.FormatInt(n, 10))
		return ok && got == n
	}
	if err := quick.Check(roundTrip, cfg); err != nil {
		t.Error(err)
	}
	strict := func(s string) bool {
		got, ok := ParseBudget(s)
		if !ok {
			return got == 0
		}
		return strconv.FormatInt(got, 10) == trimZeros(s) && len(s) <= 16
	}
	if err := quick.Check(strict, cfg); err != nil {
		t.Error(err)
	}
}

func trimZeros(s string) string {
	for len(s) > 1 && s[0] == '0' {
		s = s[1:]
	}
	return s
}
