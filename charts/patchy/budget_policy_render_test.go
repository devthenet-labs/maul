// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"math/rand"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// maxBudget is the largest budget provider.ParseBudget reads: sixteen nines.
const maxBudget = 9999999999999999

// TestBudgetJobGrammarIsTheControllers: the grammar the rendered Job policy
// holds every intent launch to admits exactly the budgets intent-controller
// writes (strconv.FormatInt of a positive budget, the jobs package's only
// form) and the broker reads back unchanged (provider.ParseBudget), as a
// seeded property, so neither side can drift: a narrower chart grammar would
// refuse launches, a wider one admit values the broker reads as no budget.
func TestBudgetJobGrammarIsTheControllers(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	grammar := budgetJobGrammar(t)
	written := func(n int64) bool {
		v := strconv.FormatInt(n, 10)
		got, ok := provider.ParseBudget(v)
		return grammar.MatchString(v) && ok && got == n
	}
	for _, n := range []int64{1, 9, 10, 60000000, maxBudget} {
		if !written(n) {
			t.Errorf("budget %d: the grammar or the broker refuses what intent-controller writes", n)
		}
	}
	for _, v := range []string{"", "0", "00", "05", "-5", "+5", " 5", "5 ", "1e6", "10000000000000000"} {
		if grammar.MatchString(v) {
			t.Errorf("the grammar admits %q, which intent-controller never writes", v)
		}
	}
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(29))}
	if err := quick.Check(func(n uint64) bool { return written(int64(n%maxBudget) + 1) }, cfg); err != nil {
		t.Error(err)
	}
	// Every value the grammar admits is one the broker reads.
	r := rand.New(rand.NewSource(31))
	for range 2000 {
		v := budgetDigits(r)
		if !grammar.MatchString(v) {
			t.Fatalf("the grammar refuses %q", v)
		}
		if _, ok := provider.ParseBudget(v); !ok {
			t.Fatalf("the grammar admits %q, which the broker does not read", v)
		}
	}
}

// budgetJobGrammar is the one matches() grammar of the rendered
// patchy-budget-jobs policy.
func budgetJobGrammar(t *testing.T) *regexp.Regexp {
	t.Helper()
	args := append([]string{"template", "patchy", ".", "--namespace", "patchy"}, budgetArgs...)
	output, err := exec.CommandContext(t.Context(), "helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	var grammars []string
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		if u.GetKind() != "ValidatingAdmissionPolicy" || u.GetName() != "patchy-budget-jobs" {
			continue
		}
		validations, _, _ := unstructured.NestedSlice(u.Object, "spec", "validations")
		for _, v := range validations {
			expr, _ := v.(map[string]any)["expression"].(string)
			for _, m := range matchesArg.FindAllStringSubmatch(expr, -1) {
				grammars = append(grammars, m[1])
			}
		}
	}
	if len(grammars) != 1 {
		t.Fatalf("the budget Job policy holds the annotation to %d grammars, want 1", len(grammars))
	}
	return regexp.MustCompile(grammars[0])
}

// budgetDigits is a random positive decimal of one to sixteen digits with no
// leading zero.
func budgetDigits(r *rand.Rand) string {
	n := 1 + r.Intn(16)
	b := make([]byte, 0, n)
	b = append(b, byte('1'+r.Intn(9)))
	for range n - 1 {
		b = append(b, byte('0'+r.Intn(10)))
	}
	return string(b)
}
