// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"math/rand"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"go", "a", "go-1", "g0", strings.Repeat("a", MaxNameLen)} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "Go", "1go", "-go", "go-", "go_x", "go.x", "g o",
		strings.Repeat("a", MaxNameLen+1)} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) = nil, want an error", bad)
		}
	}
}

// TestValidNameAgreesWithValidate: ValidName is exactly the name rule
// Profile.Validate applies, over seeded random names.
func TestValidNameAgreesWithValidate(t *testing.T) {
	rng := rand.New(rand.NewSource(20261014))
	const alphabet = "abcz09-_A."
	for range 2000 {
		b := make([]byte, rng.Intn(MaxNameLen+3))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		p := validProfile()
		p.Name = string(b)
		nameErr, validateErr := ValidName(p.Name), p.Validate()
		if (nameErr == nil) != (validateErr == nil) {
			t.Fatalf("name %q: ValidName = %v, Validate = %v", p.Name, nameErr, validateErr)
		}
	}
}
