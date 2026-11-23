// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// The package is neutral ground between the status server, the chart's
// tests and the CLI: it imports the standard library and api/v1alpha1, and
// nothing else, so none of them links a client, a controller or the
// others through it.
func TestImportsOnlyStdlibAndAPI(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
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
		checked++
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") && path != "github.com/devthenet-labs/maul/api/v1alpha1" {
				t.Errorf("%s imports %s: package projectsettings imports only the stdlib and api/v1alpha1", name, path)
			}
		}
	}
	if checked < 5 {
		t.Fatalf("checked %d source files; the glob is broken", checked)
	}
}

// crdNode is a node of the Project CRD's schema.
type crdNode = map[string]any

// projectSpecSchema is spec's schema in the generated Project CRD, the one
// the API server holds a Project to.
func projectSpecSchema(t *testing.T) crdNode {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/kustomize/base/crds/maul.run_projects.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema crdNode `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("the Project CRD has %d versions", len(crd.Spec.Versions))
	}
	return child(t, crd.Spec.Versions[0].Schema.OpenAPIV3Schema, "spec")
}

func child(t *testing.T, node crdNode, name string) crdNode {
	t.Helper()
	if name == "[]" {
		items, ok := node["items"].(crdNode)
		if !ok {
			t.Fatalf("the schema node is no list: %v", node["type"])
		}
		return items
	}
	props, _ := node["properties"].(crdNode)
	c, ok := props[name].(crdNode)
	if !ok {
		t.Fatalf("the Project schema has no property %q", name)
	}
	return c
}

// schemaOf walks the schema to a field's own node.
func schemaOf(t *testing.T, spec crdNode, f Field) crdNode {
	t.Helper()
	node := spec
	path := strings.TrimPrefix(f.Path(), "spec.")
	for _, part := range strings.Split(strings.ReplaceAll(path, "[]", ".[]"), ".") {
		node = child(t, node, part)
	}
	return node
}

var durationRange = regexp.MustCompile(
	`^duration\(self\) >= duration\('(\w+)'\) && duration\(self\) <= duration\('(\w+)'\)$`)

func number(t *testing.T, node crdNode, key string) int64 {
	t.Helper()
	switch n := node[key].(type) {
	case nil:
		return 0
	case float64:
		return int64(n)
	default:
		t.Fatalf("%s is %T", key, n)
		return 0
	}
}

func seconds(t *testing.T, s string) int64 {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return int64(d / time.Second)
}

// The range hints the page shows, and Check refuses outside of, are the
// generated CRD's own: a number's minimum and maximum, a duration's from
// its CEL rule, a list's item counts, and the default where the schema sets
// one. A marker changed in api/v1alpha1 fails here until the table follows.
func TestRangeHintsAreTheSchemas(t *testing.T) {
	spec := projectSpecSchema(t)
	for _, f := range Fields() {
		node := schemaOf(t, spec, f)
		r, inTable := rowByID(f.ID)
		var lo, hi, def int64
		hasDefault := node["default"] != nil
		switch {
		case f.Kind == KindSeconds:
			rules, _ := node["x-kubernetes-validations"].([]any)
			if len(rules) != 1 {
				t.Errorf("%s: %d validation rules, want the one range", f.ID, len(rules))
				continue
			}
			m := durationRange.FindStringSubmatch(rules[0].(crdNode)["rule"].(string))
			if m == nil {
				t.Errorf("%s: the rule %q is no duration range", f.ID, rules[0].(crdNode)["rule"])
				continue
			}
			lo, hi = seconds(t, m[1]), seconds(t, m[2])
			if hasDefault {
				def = seconds(t, node["default"].(string))
			}
		case f.Kind.numeric():
			lo, hi = number(t, node, "minimum"), number(t, node, "maximum")
			def = number(t, node, "default")
		case f.Kind.list():
			lo, hi = number(t, node, "minItems"), number(t, node, "maxItems")
		default:
			continue
		}
		if f.Min != lo || f.Max != hi {
			t.Errorf("%s: range %d to %d, the schema's is %d to %d", f.ID, f.Min, f.Max, lo, hi)
		}
		if hi == 0 {
			t.Errorf("%s: the schema sets no upper bound", f.ID)
		}
		if inTable && r.defaulted != hasDefault {
			t.Errorf("%s: schema-defaulted is %v in the table, %v in the schema", f.ID, r.defaulted, hasDefault)
		}
		if hasDefault && (!f.HasDefault || f.Default != def) {
			t.Errorf("%s: default %d (%v), the schema's is %d", f.ID, f.Default, f.HasDefault, def)
		}
	}
}

// The defaults the schema does not set are the code's, read through the
// API's own accessors: where an unset field lands is one fact.
func TestCodeDefaultsAreTheAPIs(t *testing.T) {
	var s v1alpha1.ProjectSpec
	for id, want := range map[string]int64{
		FieldHandoffMax:       int64(s.HandoffMax() / time.Second),
		FieldMaxToolCalls:     int64(s.MaxToolCalls()),
		FieldMaxEvaluateCost:  s.Limits.MaxEvaluateCostLimit(),
		FieldMaxVerifyFixes:   int64(s.Limits.MaxVerifyFixesLimit()),
		FieldMaxVerifications: int64(s.Limits.MaxVerificationsLimit()),
	} {
		r, _ := rowByID(id)
		if !r.HasDefault || r.Default != want || r.defaulted {
			t.Errorf("%s: default %d (has %v, schema-defaulted %v), the API's accessor gives %d", id, r.Default,
				r.HasDefault, r.defaulted, want)
		}
	}
}

// The approver-list rule is the schema's: its pattern, its lengths, its
// count. And the hand-off modes Check admits are the schema's enum.
func TestApproverRuleAndHandoffModesAreTheSchemas(t *testing.T) {
	spec := projectSpecSchema(t)
	logins := child(t, child(t, spec, "approvers"), "logins")
	items := child(t, logins, "[]")
	if got := items["pattern"]; got != loginPattern.String() {
		t.Errorf("login pattern %q, the schema's is %q", loginPattern, got)
	}
	if got := number(t, items, "maxLength"); got != MaxLoginLength {
		t.Errorf("MaxLoginLength = %d, the schema's is %d", MaxLoginLength, got)
	}
	if got := number(t, logins, "maxItems"); got != v1alpha1.MaxProjectApprovers {
		t.Errorf("MaxProjectApprovers = %d, the schema's is %d", v1alpha1.MaxProjectApprovers, got)
	}
	modes := child(t, child(t, spec, "verification"), "handoff")["enum"].([]any)
	enum := make([]string, 0, len(modes))
	for _, v := range modes {
		enum = append(enum, v.(string))
	}
	if !reflect.DeepEqual(enum, handoffModes) {
		t.Errorf("hand-off modes %v, the schema's enum is %v", handoffModes, enum)
	}
}

// Every field has a distinct id, a group of Groups, an effect, and a path
// that exists in the schema; the stages are the five of spec.limits.
func TestFieldTable(t *testing.T) {
	spec := projectSpecSchema(t)
	seen := map[string]bool{}
	groups := map[Group]bool{}
	for _, f := range Fields() {
		if seen[f.ID] {
			t.Errorf("field %s is in the table twice", f.ID)
		}
		seen[f.ID] = true
		groups[f.Group] = true
		if !slices.Contains(Groups(), f.Group) {
			t.Errorf("%s: group %q is not one of Groups", f.ID, f.Group)
		}
		if !slices.Contains([]Effect{EffectLive, EffectNewRuns, EffectNewIntents, EffectNextRound}, f.Effect) {
			t.Errorf("%s: effect %q", f.ID, f.Effect)
		}
		schemaOf(t, spec, f) // fails when the path is not in the schema
		if f.Nullable != (f.ID == FieldMaxRevisions || f.ID == FieldMaxCheckFixes || f.ID == FieldMaxVerifyFixes) {
			t.Errorf("%s: nullable %v", f.ID, f.Nullable)
		}
	}
	if len(groups) != len(Groups()) {
		t.Errorf("fields are in %d groups, of %d", len(groups), len(Groups()))
	}
	want := []Group{"approvers", "evaluator", "handoff", "models", "cost", "rounds", "activeIntents", "stages",
		"delegation", "checks", "resources"}
	if !reflect.DeepEqual(Groups(), want) {
		t.Errorf("Groups = %v, want %v", Groups(), want)
	}
	stageNames := []string{"plan", "build", "revise", "evaluate", "checkReview"}
	if got := StageNames(); !reflect.DeepEqual(got, stageNames) {
		t.Errorf("StageNames = %v, want %v", got, stageNames)
	}
	limits := child(t, spec, "limits")["properties"].(crdNode)
	for name, node := range limits {
		if _, stage := node.(crdNode)["properties"]; stage != slices.Contains(StageNames(), name) {
			t.Errorf("limits.%s: a stage in the schema %v, in StageNames %v", name, stage, !stage)
		}
	}
	for _, id := range []string{FieldSubagents, FieldWorkflows} {
		r, ok := rowByID(id)
		if !ok || r.Group != GroupDelegation || r.Kind != KindBool || r.Effect != EffectNewRuns || r.HasDefault {
			t.Errorf("%s: row %+v (in the table %v), want a delegation bool for new runs", id, r.Field, ok)
		}
	}
	if len(Fields()) != 37 {
		t.Errorf("%d editable fields; a field added or dropped changes the document (Schema) and the page", len(Fields()))
	}
}

// Every wire type is listed once for the TypeScript pin, under a name that
// starts with ProjectSettings (the page's interfaces share one namespace),
// and every struct a listed type holds is listed too.
func TestWireTypesAreAllListed(t *testing.T) {
	listed := map[reflect.Type]bool{}
	for _, v := range WireTypes() {
		typ := reflect.TypeOf(v)
		if listed[typ] {
			t.Errorf("%s is listed twice", typ.Name())
		}
		listed[typ] = true
		if !strings.HasPrefix(typ.Name(), "ProjectSettings") {
			t.Errorf("wire type %s: the name must start with ProjectSettings", typ.Name())
		}
	}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, from string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		if !listed[typ] {
			t.Errorf("%s holds %s, which WireTypes does not list", from, typ)
			return
		}
		for f := range typ.Fields() {
			if tag := f.Tag.Get("json"); tag == "" || strings.Contains(tag, "omitempty") {
				t.Errorf("%s.%s: json tag %q; a wire field is always written", typ.Name(), f.Name, tag)
			}
			walk(f.Type, typ.Name())
		}
	}
	for typ := range listed {
		walk(typ, "WireTypes")
	}
}
