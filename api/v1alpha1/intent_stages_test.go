// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// declaredIntentStages parses intentrun_types.go and returns every constant
// declared with type IntentStage (by value), and the values of the
// IntentStage type's kubebuilder enum marker.
func declaredIntentStages(t *testing.T) (consts, enum []string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "intentrun_types.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse intentrun_types.go: %v", err)
	}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				if id, ok := s.Type.(*ast.Ident); !ok || id.Name != "IntentStage" || gen.Tok != token.CONST {
					continue
				}
				for _, v := range s.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("an IntentStage constant is not a string literal: %T", v)
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", lit.Value, err)
					}
					consts = append(consts, value)
				}
			case *ast.TypeSpec:
				if s.Name.Name != "IntentStage" || gen.Doc == nil {
					continue
				}
				const marker = "+kubebuilder:validation:Enum="
				for _, c := range gen.Doc.List {
					if _, rest, ok := strings.Cut(c.Text, marker); ok {
						enum = strings.Split(strings.TrimSpace(rest), ";")
					}
				}
			}
		}
	}
	return consts, enum
}

// TestIntentStagesExhaustive pins IntentStages to every IntentStage constant
// declared and to the CRD enum, so a new stage cannot be added without the
// list every stage switch is tested against (intent-controller's
// TestStageSwitchesExhaustive) learning of it, and every stage names its runs.
func TestIntentStagesExhaustive(t *testing.T) {
	want := []IntentStage{
		IntentStagePlan,
		IntentStageBuild,
		IntentStageRevise,
		IntentStageEvaluate,
		IntentStageCheckReview,
	}
	got := IntentStages()
	if !slices.Equal(got, want) {
		t.Fatalf("IntentStages() = %v, want %v", got, want)
	}

	consts, enum := declaredIntentStages(t)
	asStrings := make([]string, len(got))
	for i, s := range got {
		asStrings[i] = string(s)
	}
	for what, values := range map[string][]string{"declared constants": consts, "enum marker": enum} {
		sorted, listed := slices.Sorted(slices.Values(values)), slices.Sorted(slices.Values(asStrings))
		if !slices.Equal(sorted, listed) {
			t.Errorf("IntentStage %s = %v, IntentStages() = %v: they must name the same stages", what, sorted, listed)
		}
	}

	for _, stage := range got {
		if name := IntentRunName("target-1", stage, 1, "shop", 1); name == "" {
			t.Errorf("IntentRunName(%q) = \"\", want a name for every stage", stage)
		}
	}

	got[0] = "mutated"
	if IntentStages()[0] != IntentStagePlan {
		t.Error("IntentStages() returned shared storage; a caller's write leaked into the next call")
	}
}
