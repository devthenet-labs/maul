// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/projectsettings"
)

// The Project settings policy pins every key of a Project's spec but the
// editable ones without naming the pinned ones: at each level it iterates
// the object's keys and lets through only those its editable list names
// (variables.editable*), comparing every other with the old object's. So a
// field added to ProjectSpec later is pinned the day it is added. What can
// still go wrong is the lists themselves: a list naming a key the page does
// not edit leaves that key open to the status server, and one missing a
// key the page does edit makes every save of it fail.
//
// This test derives the lists from the one table the page edits
// (projectsettings.Fields) and holds the rendered policy's lists to them,
// level by level, then walks ProjectSpec by reflection so that every field
// of the type is accounted for: editable by the table, or pinned by the
// policy. It needs helm, never a cluster.

// settingsLevels are the levels of a spec the policy iterates, each with
// the variable that lists its editable keys. Every stage of spec.limits the
// page edits shares one list.
var settingsLevels = map[string]string{
	"":             "editableSpec",
	"approvers":    "editableApprovers",
	"verification": "editableVerification",
	"models":       "editableModels",
	"limits":       "editableLimits",
	"limits.stage": "editableStage",
	"checks":       "editableChecks",
	"repositories": "editableRepository",
}

// quoted are the string literals of a CEL list.
var quoted = regexp.MustCompile(`'([^']*)'`)

// policyVariables are the settings policy's variables by name.
func policyVariables(t *testing.T) (map[string]string, []string) {
	t.Helper()
	vap := findObject(renderObjects(t, projectSettingsArgs), "ValidatingAdmissionPolicy", settingsPolicy)
	if vap == nil {
		t.Fatalf("the render has no %s", settingsPolicy)
	}
	vars, _, _ := unstructured.NestedSlice(vap.Object, "spec", "variables")
	out := map[string]string{}
	for _, v := range vars {
		m, _ := v.(map[string]any)
		out[m["name"].(string)], _ = m["expression"].(string)
	}
	validations, _, _ := unstructured.NestedSlice(vap.Object, "spec", "validations")
	exprs := make([]string, 0, len(validations))
	for _, v := range validations {
		m, _ := v.(map[string]any)
		e, _ := m["expression"].(string)
		exprs = append(exprs, e)
	}
	return out, exprs
}

// expectedLevels are the editable keys per level, from the table: each
// field id is a path of keys below spec, and every key on the way is
// editable at its level (a block holding an editable field is let through
// to the next level's list).
func expectedLevels(t *testing.T) map[string][]string {
	t.Helper()
	stages := projectsettings.StageNames()
	out := map[string][]string{}
	add := func(level, key string) {
		if !slices.Contains(out[level], key) {
			out[level] = append(out[level], key)
		}
	}
	for _, f := range projectsettings.Fields() {
		parts := strings.Split(f.ID, ".")
		level := ""
		for i, key := range parts {
			add(level, key)
			next := key
			if level != "" {
				next = level + "." + key
			}
			if level == "limits" && slices.Contains(stages, key) && i < len(parts)-1 {
				next = "limits.stage"
			}
			level = next
		}
	}
	for _, keys := range out {
		slices.Sort(keys)
	}
	return out
}

// TestProjectSettingsPolicyCoversEveryProjectSpecField: the policy's
// editable lists are exactly the table's, at every level, and every field
// of ProjectSpec is either an editable field of the table or pinned by the
// policy; the two fields the page no longer offers (owner decision 8) are
// pinned.
func TestProjectSettingsPolicyCoversEveryProjectSpecField(t *testing.T) {
	needHelm(t)
	vars, exprs := policyVariables(t)
	want := expectedLevels(t)
	checkEditableLists(t, vars, exprs, want)
	checkSpecAccounted(t, want)
}

// checkEditableLists: each level's editable list in the policy is exactly
// the table's, and its other keys are pinned both ways.
func checkEditableLists(t *testing.T, vars map[string]string, exprs []string, want map[string][]string) {
	t.Helper()
	for level, name := range settingsLevels {
		expr, ok := vars[name]
		if !ok {
			t.Errorf("the policy has no variable %s for level %q", name, level)
			continue
		}
		var got []string
		for _, m := range quoted.FindAllStringSubmatch(expr, -1) {
			got = append(got, m[1])
		}
		slices.Sort(got)
		if !slices.Equal(got, want[level]) {
			t.Errorf("level %q: the policy lets %v through (%s), the table edits %v", level, got, name, want[level])
		}
		// The level is pinned both ways: no key but the editable ones
		// may be added or changed, and none may be removed.
		for _, needle := range []string{"k in variables." + name + " || (k in ", "k in variables." + name + " || k in "} {
			if !slices.ContainsFunc(exprs, func(e string) bool { return strings.Contains(e, needle) }) {
				t.Errorf("level %q: no validation pins the other keys with %q", level, needle)
			}
		}
	}
	for level := range want {
		if _, ok := settingsLevels[level]; !ok {
			t.Errorf("the table edits a field at level %q, which the policy does not iterate", level)
		}
	}
}

// checkSpecAccounted walks ProjectSpec: every field is an editable field
// of the table, let through by the policy, or pinned by it.
func checkSpecAccounted(t *testing.T, want map[string][]string) {
	t.Helper()
	editable := map[string]bool{}
	for _, f := range projectsettings.Fields() {
		editable[f.ID] = true
	}
	seen := map[string]bool{}
	walkSpec(reflect.TypeFor[v1alpha1.ProjectSpec](), "", func(path string, block bool) bool {
		seen[path] = true
		level, key := splitLevel(path)
		open := slices.Contains(want[level], key)
		switch {
		case block && open:
			// Let through to the next level, whose list judges its keys.
			if _, ok := settingsLevels[levelOf(path)]; !ok {
				t.Errorf("spec.%s is let through but the policy judges none of its keys", path)
			}
			return true
		case block:
			// Pinned whole, with everything in it.
		case open && !editable[path]:
			t.Errorf("spec.%s is let through by the policy but the page does not edit it", path)
		case !open && editable[path]:
			t.Errorf("spec.%s is editable on the page but the policy pins it", path)
		}
		return false
	})
	for id := range editable {
		if !seen[id] {
			t.Errorf("the table edits %s, which is no field of ProjectSpec", id)
		}
	}
	for _, pinned := range []string{"models.checkReview", "limits.checkReview"} {
		level, key := splitLevel(pinned)
		if slices.Contains(want[level], key) || !seen[pinned] {
			t.Errorf("spec.%s must be a ProjectSpec field the policy pins", pinned)
		}
	}
}

// splitLevel is a path's level (as settingsLevels names it) and last key.
func splitLevel(path string) (string, string) {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return "", path
	}
	level, key := path[:i], path[i+1:]
	if parent, stage, ok := strings.Cut(level, "."); ok && parent == "limits" &&
		slices.Contains(projectsettings.StageNames(), stage) {
		level = "limits.stage"
	}
	return level, key
}

// levelOf is the level a block's own keys are judged at.
func levelOf(path string) string {
	if parent, stage, ok := strings.Cut(path, "."); ok && parent == "limits" &&
		slices.Contains(projectsettings.StageNames(), stage) && !strings.Contains(stage, ".") {
		return "limits.stage"
	}
	return path
}

// walkSpec calls visit for every field path below spec (a repository
// entry's keys as repositories.<key>), block for one of the API's own
// structs (a list of them included). It goes into a block when visit says
// so.
func walkSpec(t reflect.Type, prefix string, visit func(path string, block bool) bool) {
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		block := ft.Kind() == reflect.Struct && ft.PkgPath() == reflect.TypeFor[v1alpha1.ProjectSpec]().PkgPath()
		if visit(path, block) {
			walkSpec(ft, path, visit)
		}
	}
}
