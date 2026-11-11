// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package crdconv

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/httpstep"
)

// TestImports keeps the adapter thin: the standard library, the acceptance
// core, the step grammar and the CRD types, nothing else.
func TestImports(t *testing.T) {
	allowed := map[string]bool{
		"github.com/devthenet-labs/maul/api/v1alpha1":        true,
		"github.com/devthenet-labs/maul/internal/acceptance": true,
		"github.com/devthenet-labs/maul/internal/httpstep":   true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") && !allowed[path] {
				t.Errorf("%s imports %s: crdconv imports only the standard library, acceptance, httpstep and v1alpha1",
					name, path)
			}
		}
	}
}

// jsonFields maps a struct's JSON field names to the field.
func jsonFields(rt reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for f := range rt.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out[name] = f
	}
	return out
}

// TestEvidenceMirrorsMatch holds the part-fact, readiness and outcome
// mirrors to the same JSON fields both ways, so a strict conversion of a
// real object never fails and never drops a field.
func TestEvidenceMirrorsMatch(t *testing.T) {
	mirrors := []struct {
		name     string
		acc, crd reflect.Type
	}{
		{"PartFact", reflect.TypeFor[httpstep.PartFact](), reflect.TypeFor[v1alpha1.PartFact]()},
		{"Termination", reflect.TypeFor[httpstep.Termination](), reflect.TypeFor[v1alpha1.Termination]()},
		{"TargetWait", reflect.TypeFor[httpstep.TargetWait](), reflect.TypeFor[v1alpha1.TargetWait]()},
		{"PlanOutcome", reflect.TypeFor[acceptance.PlanOutcome](), reflect.TypeFor[v1alpha1.AcceptanceOutcome]()},
	}
	for _, m := range mirrors {
		acc, crd := jsonFields(m.acc), jsonFields(m.crd)
		for name := range acc {
			if _, ok := crd[name]; !ok {
				t.Errorf("%s.%s has no CRD field", m.name, name)
			}
		}
		for name := range crd {
			if _, ok := acc[name]; !ok {
				t.Errorf("CRD %s.%s has no acceptance field", m.name, name)
			}
		}
	}
}

func TestEmptyIsNil(t *testing.T) {
	if got, err := OutcomesToCRD([]acceptance.PlanOutcome{}); got != nil || err != nil {
		t.Errorf("OutcomesToCRD(empty) = %v, %v", got, err)
	}
	if got, err := OutcomesFromCRD([]v1alpha1.AcceptanceOutcome{}); got != nil || err != nil {
		t.Errorf("OutcomesFromCRD(empty) = %v, %v", got, err)
	}
	if got, err := WaitsToCRD([]httpstep.TargetWait{}); got != nil || err != nil {
		t.Errorf("WaitsToCRD(empty) = %v, %v", got, err)
	}
}

// TestWaitsToCRD: every field of the tester's readiness waits is stored; an
// empty list is nil.
func TestWaitsToCRD(t *testing.T) {
	in := []httpstep.TargetWait{{Target: "api", WaitMs: 1203, Attempts: 9, Refused: 5, Timeout: 1, Reset: 1,
		DNS: 1}, {Target: "web", Attempts: 1}}
	got, err := WaitsToCRD(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []v1alpha1.TargetWait{{Target: "api", WaitMs: 1203, Attempts: 9, Refused: 5, Timeout: 1, Reset: 1,
		DNS: 1}, {Target: "web", Attempts: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WaitsToCRD = %+v, want %+v", got, want)
	}
	if got, err := WaitsToCRD(nil); got != nil || err != nil {
		t.Errorf("WaitsToCRD(nil) = %+v, %v", got, err)
	}
}

// TestPartFactsWhole: every field of a fact is carried, the log tail
// included (preview-controller's old field-by-field copy dropped it).
func TestPartFactsWhole(t *testing.T) {
	env := []v1alpha1.PartFact{{Name: "web", Role: v1alpha1.PreviewRoleComponent, Ready: true, Restarts: 2,
		Waiting: "CrashLoopBackOff", LastTermination: &v1alpha1.Termination{Reason: "OOMKilled", ExitCode: 137},
		Evicted: true, Unschedulable: true, LogTail: "boom"}}
	want := []httpstep.PartFact{{Name: "web", Role: httpstep.RoleComponent, Ready: true, Restarts: 2,
		Waiting: "CrashLoopBackOff", LastTermination: &httpstep.Termination{Reason: "OOMKilled", ExitCode: 137},
		Evicted: true, Unschedulable: true, LogTail: "boom"}}
	if got := PartFacts(env); !reflect.DeepEqual(got, want) {
		t.Fatalf("PartFacts:\n got %+v\nwant %+v", got, want)
	}
}

// TestConvertRefusesUnknownFields: the one strict decode refuses a field
// the target has no place for.
func TestConvertRefusesUnknownFields(t *testing.T) {
	type wide struct {
		Name  string `json:"name"`
		Extra string `json:"extra"`
	}
	type narrow struct {
		Name string `json:"name"`
	}
	var n narrow
	if err := convert(wide{Name: "a", Extra: "b"}, &n); err == nil {
		t.Fatal("an unknown field converted")
	}
	if err := convert(wide{Name: "a"}, &n); err == nil {
		t.Fatal("an unknown empty field converted")
	}
}
