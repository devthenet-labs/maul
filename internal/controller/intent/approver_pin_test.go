// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"math/rand"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestIsApproverIsProjectApproversHas pins the one approver-list rule. The
// controller's isApprover (every /maul command, label, review and trigger)
// and v1alpha1's ProjectApprovers.Has (what the status page's settings gate
// asks) must give one answer for every list and login, and that answer is
// GitHub's own comparison: equal ignoring case, nothing looser. Both are held
// to a reference written out here, so the pin still says something while one
// is implemented by calling the other. Seeded, so the gate is deterministic.
func TestIsApproverIsProjectApproversHas(t *testing.T) {
	reference := func(logins []string, login string) bool {
		for _, a := range logins {
			if strings.EqualFold(a, login) {
				return true
			}
		}
		return false
	}
	const alphabet = "abAB-_.9"
	rng := rand.New(rand.NewSource(0xa99405e5))
	word := func() string {
		b := make([]byte, 1+rng.Intn(4))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	recase := func(s string) string {
		b := []byte(s)
		for i, c := range b {
			if rng.Intn(2) == 0 {
				b[i] = strings.ToUpper(string(c))[0]
			} else {
				b[i] = strings.ToLower(string(c))[0]
			}
		}
		return string(b)
	}
	var held, refused int
	for range 5000 {
		logins := make([]string, rng.Intn(5))
		for i := range logins {
			logins[i] = word()
		}
		probe := word()
		switch {
		case len(logins) > 0 && rng.Intn(3) == 0:
			// An entry in another spelling: the case that must hold.
			probe = recase(logins[rng.Intn(len(logins))])
		case rng.Intn(20) == 0:
			probe = ""
		}
		p := &v1alpha1.Project{Spec: v1alpha1.ProjectSpec{Approvers: v1alpha1.ProjectApprovers{Logins: logins}}}
		want := reference(logins, probe)
		if got := isApprover(p, probe); got != want {
			t.Fatalf("isApprover(%q, %q) = %t, want %t", logins, probe, got, want)
		}
		if got := p.Spec.Approvers.Has(probe); got != want {
			t.Fatalf("ProjectApprovers{%q}.Has(%q) = %t, want %t", logins, probe, got, want)
		}
		if want {
			held++
		} else {
			refused++
		}
	}
	// A property that only ever saw one answer pins nothing.
	if held < 500 || refused < 500 {
		t.Fatalf("the cases were lopsided: %d approvers, %d others; want at least 500 of each", held, refused)
	}
}
