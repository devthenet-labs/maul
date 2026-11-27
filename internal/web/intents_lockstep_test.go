// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/projectsettings"
	"github.com/devthenet-labs/maul/internal/usageview"
)

// tsInterfaces reads every `export interface` in a TypeScript source into
// its field names, an interface's own plus those of the one it extends.
func tsInterfaces(t *testing.T, src string) map[string][]string {
	t.Helper()
	decl := regexp.MustCompile(`(?s)export interface (\w+)(?: extends (\w+))? \{(.*?)\n\}`)
	field := regexp.MustCompile(`^\s*(\w+)\??:`)
	own := map[string][]string{}
	parent := map[string]string{}
	for _, m := range decl.FindAllStringSubmatch(src, -1) {
		var fields []string
		for line := range strings.SplitSeq(m[3], "\n") {
			if f := field.FindStringSubmatch(line); f != nil {
				fields = append(fields, f[1])
			}
		}
		own[m[1]], parent[m[1]] = fields, m[2]
	}
	out := map[string][]string{}
	for name := range own {
		var all []string
		for n := name; n != ""; n = parent[n] {
			all = append(all, own[n]...)
		}
		slices.Sort(all)
		out[name] = all
	}
	return out
}

// jsonFields are a struct's JSON keys, embedded structs flattened as
// encoding/json flattens them.
func jsonFields(typ reflect.Type) []string {
	var out []string
	for f := range typ.Fields() {
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, jsonFields(f.Type)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// TestIntentWireTypesMatchTypeScript keeps every intents wire type and the
// SPA's interface of the same name in lockstep: a field added, renamed or
// dropped on one side fails here.
func TestIntentWireTypesMatchTypeScript(t *testing.T) {
	src, err := os.ReadFile("ui/src/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	ts := tsInterfaces(t, string(src))
	for _, v := range []any{
		Me{}, ProjectAccess{}, IntentBoard{}, BoardProject{}, ProjectRepo{}, IntentLimits{}, IntentCard{},
		IntentWaiting{},
		IntentPR{}, PreviewLink{}, RunningRun{}, AttemptCount{}, IntentDetail{}, IntentInputView{},
		IntentPlanView{}, IntentApproval{}, IntentRunRow{}, RunGrant{}, IntentRunDetail{}, RunJobClock{},
		IntentPlanText{}, RunActivity{}, RunOutput{}, StreamNotice{},
		RunEvaluation{}, EvaluationHandoff{}, EvaluationOutcome{}, EvaluationBuild{},
		EvaluationAnomaly{}, EvaluationLedger{}, EvaluationLedgerEntry{},
		EvaluationArtifact{}, EvaluationCapture{}, EvaluationComponentCapture{},
	} {
		typ := reflect.TypeOf(v)
		want, ok := ts[typ.Name()]
		if !ok {
			t.Errorf("ui/src/types.ts has no interface %s", typ.Name())
			continue
		}
		if got := jsonFields(typ); !slices.Equal(got, want) {
			t.Errorf("%s: Go fields %v, TypeScript fields %v", typ.Name(), got, want)
		}
	}
	// The parser itself sees the long-standing Finding types, so a regex
	// that silently matched nothing cannot pass the loop above.
	if len(ts) < 30 || !slices.Contains(slices.Collect(maps.Keys(ts)), "FindingSummary") {
		t.Fatalf("parsed %d interfaces from types.ts; the parser is broken", len(ts))
	}
	// The saved-check views are retired ("evaluator observed"): the page
	// shows each outcome's cited calls instead, so their mirrors are gone.
	for _, retired := range []string{"EvaluationCheck", "EvaluationCheckRun", "EvaluationDispute"} {
		if _, ok := ts[retired]; ok {
			t.Errorf("ui/src/types.ts still has the retired interface %s", retired)
		}
	}
}

// TestIntentStagesMatchTypeScript: the SPA's IntentStage names every stage
// the API does, the retired checkreview included, which an earlier
// release's runs still carry and their rows still show.
func TestIntentStagesMatchTypeScript(t *testing.T) {
	src, err := os.ReadFile("ui/src/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`export type IntentStage = ([^;]+);`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("ui/src/types.ts has no IntentStage")
	}
	stages := regexp.MustCompile(`"([a-z]+)"`).FindAllStringSubmatch(m[1], -1)
	got := make([]string, 0, len(stages))
	for _, s := range stages {
		got = append(got, s[1])
	}
	// A retired stage stays: a stored run of it is still shown.
	all := append(v1alpha1.IntentStages(), v1alpha1.RetiredIntentStages()...)
	want := make([]string, 0, len(all))
	for _, s := range all {
		want = append(want, string(s))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("IntentStage %v, want the API's stages %v", got, want)
	}
}

// TestUsageWireTypesMatchTypeScript keeps the usage report (internal/usageview,
// which `maul get usage -o json` prints too) and UsageAccess in lockstep with
// the SPA's interfaces of the same names.
func TestUsageWireTypesMatchTypeScript(t *testing.T) {
	src, err := os.ReadFile("ui/src/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	ts := tsInterfaces(t, string(src))
	for _, v := range []any{
		UsageAccess{}, usageview.UsageFigures{}, usageview.UsageSegment{}, usageview.UsageDay{},
		usageview.UsageIntent{}, usageview.UsagePerson{}, usageview.UsageProject{}, usageview.UsageReport{},
	} {
		typ := reflect.TypeOf(v)
		want, ok := ts[typ.Name()]
		if !ok {
			t.Errorf("ui/src/types.ts has no interface %s", typ.Name())
			continue
		}
		if got := jsonFields(typ); !slices.Equal(got, want) {
			t.Errorf("%s: Go fields %v, TypeScript fields %v", typ.Name(), got, want)
		}
	}
}

// TestAccountWireTypesMatchTypeScript pins the Account page's wire types to
// their ui/src/types.ts mirrors.
func TestAccountWireTypesMatchTypeScript(t *testing.T) {
	src, err := os.ReadFile("ui/src/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	ts := tsInterfaces(t, string(src))
	for _, v := range []any{Account{}, AccountCredential{}} {
		typ := reflect.TypeOf(v)
		want, ok := ts[typ.Name()]
		if !ok {
			t.Errorf("ui/src/types.ts has no interface %s", typ.Name())
			continue
		}
		if got := jsonFields(typ); !slices.Equal(got, want) {
			t.Errorf("%s: Go fields %v, TypeScript fields %v", typ.Name(), got, want)
		}
	}
}

// TestProjectSettingsWireTypesMatchTypeScript pins every wire type of the
// Project settings page (internal/projectsettings.WireTypes, so the list
// cannot drift from the package) to its ui/src/types.ts mirror.
func TestProjectSettingsWireTypesMatchTypeScript(t *testing.T) {
	src, err := os.ReadFile("ui/src/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	ts := tsInterfaces(t, string(src))
	types := projectsettings.WireTypes()
	if len(types) < 20 {
		t.Fatalf("projectsettings lists %d wire types; the pin would check too little", len(types))
	}
	for _, v := range types {
		typ := reflect.TypeOf(v)
		want, ok := ts[typ.Name()]
		if !ok {
			t.Errorf("ui/src/types.ts has no interface %s", typ.Name())
			continue
		}
		if got := jsonFields(typ); !slices.Equal(got, want) {
			t.Errorf("%s: Go fields %v, TypeScript fields %v", typ.Name(), got, want)
		}
	}
}
