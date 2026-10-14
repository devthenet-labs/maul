// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// seed is every property's seed, so the gate is deterministic.
const seed = 20261014

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// elements is a deliberately small pool, with shared prefixes ("x", "xyz",
// "x-y"), so generated paths collide often and sibling confusion has every
// chance to show.
var elements = []string{"golang.org", "github.com", "x", "xyz", "x-y", "mod", "Mod", "a_b", "v2", "~t", "a.b"}

// modPath is a module path (or prefix) of 1-4 elements from the pool.
type modPath string

func (modPath) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(4)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = elements[r.Intn(len(elements))]
	}
	return reflect.ValueOf(modPath(strings.Join(parts, "/")))
}

// derived is a path that is often below, beside or equal to a base: the
// generator returns a pair so the interesting cases are not left to chance.
type derived struct{ base, m string }

func (derived) Generate(r *rand.Rand, size int) reflect.Value {
	base := string(modPath("").Generate(r, size).Interface().(modPath))
	var m string
	switch r.Intn(5) {
	case 0:
		m = base
	case 1: // below
		m = base + "/" + elements[r.Intn(len(elements))]
	case 2: // a sibling sharing the string prefix
		m = base + elements[r.Intn(len(elements))]
	case 3: // the parent
		if i := strings.LastIndex(base, "/"); i > 0 {
			m = base[:i]
		} else {
			m = base + "/x"
		}
	default:
		m = string(modPath("").Generate(r, size).Interface().(modPath))
	}
	return reflect.ValueOf(derived{base: base, m: m})
}

func TestAllowsExamples(t *testing.T) {
	tests := []struct {
		prefixes Prefixes
		m        string
		want     bool
	}{
		{Prefixes{"golang.org/x"}, "golang.org/x", true},
		{Prefixes{"golang.org/x"}, "golang.org/x/mod", true},
		{Prefixes{"golang.org/x"}, "golang.org/xyz", false},
		{Prefixes{"golang.org/x/"}, "golang.org/xyz", false},
		{Prefixes{"golang.org/x/"}, "golang.org/x/mod", true},
		{Prefixes{"golang.org/x/"}, "golang.org", false},
		{Prefixes{"github.com/google/uuid"}, "github.com/google/uuid", true},
		{Prefixes{"github.com/google/uuid"}, "github.com/google/uuidx", false},
		{Prefixes{"github.com/google/uuid"}, "github.com/google", false},
		{Prefixes{"github.com/google/uuid"}, "github.com/@v", false},
		{Prefixes{"github.com/"}, "github.com/anyone/anything", true},
		{Prefixes{"github.com/BurntSushi/toml"}, "github.com/burntsushi/toml", false},
		{Prefixes{"github.com/prometheus/client_golang"}, "github.com/prometheus/client_golang/prometheus", true},
		{Prefixes{"a.com", "b.com/x"}, "b.com/x/y", true},
		{Prefixes{"a.com"}, "", false},
		{Prefixes{""}, "anything", false},
		{Prefixes{"/"}, "anything", false},
		{nil, "golang.org/x", false},
	}
	for _, tt := range tests {
		if got := tt.prefixes.Allows(tt.m); got != tt.want {
			t.Errorf("%v.Allows(%q) = %v, want %v", tt.prefixes, tt.m, got, tt.want)
		}
	}
}

// TestPropertyPrefixBoundary: one prefix admits exactly itself and the
// paths below it on an element boundary.
func TestPropertyPrefixBoundary(t *testing.T) {
	prop := func(d derived) bool {
		want := d.m == d.base || strings.HasPrefix(d.m, d.base+"/")
		return Prefixes{d.base}.Allows(d.m) == want
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyPrefixMonotone: adding a prefix never removes an allowed
// path, and removing one never adds one.
func TestPropertyPrefixMonotone(t *testing.T) {
	prop := func(a, b []modPath, extra, m modPath) bool {
		ps := toPrefixes(a)
		more := append(append(Prefixes(nil), ps...), string(extra))
		if ps.Allows(string(m)) && !more.Allows(string(m)) {
			return false
		}
		if len(ps) > 0 {
			fewer := ps[1:]
			if fewer.Allows(string(m)) && !ps.Allows(string(m)) {
				return false
			}
		}
		// The union admits exactly what either part does.
		union := append(toPrefixes(a), toPrefixes(b)...)
		return union.Allows(string(m)) == (toPrefixes(a).Allows(string(m)) || toPrefixes(b).Allows(string(m)))
	}
	if err := quick.Check(prop, quickConfig(3000)); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyTrailingSlashNeutral: p and p+"/" admit the same paths.
func TestPropertyTrailingSlashNeutral(t *testing.T) {
	prop := func(d derived) bool {
		return Prefixes{d.base}.Allows(d.m) == Prefixes{d.base + "/"}.Allows(d.m)
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyValidPrefixNeverAdmitsToolchain: no prefix ValidPrefix accepts
// admits a toolchain module path.
func TestPropertyValidPrefixNeverAdmitsToolchain(t *testing.T) {
	toolchains := []string{"golang.org/toolchain", "golang.org/toolchain/x"}
	prop := func(p modPath) bool {
		for _, cand := range []string{string(p), string(p) + "/", "golang.org/" + string(p)} {
			if ValidPrefix(cand) != nil {
				continue
			}
			for _, tc := range toolchains {
				if (Prefixes{cand}).Allows(tc) {
					t.Logf("%q is valid and admits %s", cand, tc)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(3000)); err != nil {
		t.Fatal(err)
	}
}

func TestPrefixesString(t *testing.T) {
	got := Prefixes{"github.com/google/uuid", "golang.org/x/"}.String()
	if got != "github.com/google/uuid,golang.org/x" {
		t.Fatalf("String = %q", got)
	}
}

func toPrefixes(ps []modPath) Prefixes {
	out := make(Prefixes, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}
