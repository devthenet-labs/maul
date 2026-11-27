// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

// The observed evaluator's three status fields are additive: optional, so a
// record written before them stays valid, and bounded, so a rollback prunes
// them harmlessly and no write can grow them past what the judge keeps. Read
// from the generated CRDs, so this runs without an API server; the envtest
// suite holds the same fields to a live one.

// schemaNode is one node of a CRD's openAPIV3Schema.
type schemaNode struct {
	Type       string                `json:"type"`
	Enum       []string              `json:"enum"`
	Required   []string              `json:"required"`
	Properties map[string]schemaNode `json:"properties"`
	Items      *schemaNode           `json:"items"`
	MaxItems   *int64                `json:"maxItems"`
	MaxLength  *int64                `json:"maxLength"`
	Minimum    *int64                `json:"minimum"`
	Maximum    *int64                `json:"maximum"`
	Pattern    string                `json:"pattern"`
	ListType   string                `json:"x-kubernetes-list-type"`
}

// crdSchema reads the openAPIV3Schema of the generated CRD file.
func crdSchema(t *testing.T, file string) schemaNode {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/kustomize/base/crds/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema schemaNode `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("%s: %d versions, want 1", file, len(crd.Spec.Versions))
	}
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema
}

// optionalField walks path (a property name, or "[]" for an array's
// items) from root and returns the last property, failing the test when
// any step is missing or the last one is required by its parent.
func optionalField(t *testing.T, root schemaNode, path ...string) schemaNode {
	t.Helper()
	node, parent, name := root, root, ""
	for _, step := range path {
		if step == "[]" {
			if node.Items == nil {
				t.Fatalf("%v: %q has no items", path, name)
			}
			node = *node.Items
			continue
		}
		child, ok := node.Properties[step]
		if !ok {
			t.Fatalf("%v: no property %q", path, step)
		}
		parent, node, name = node, child, step
	}
	if slices.Contains(parent.Required, name) {
		t.Errorf("%v: %q is required, want optional", path, name)
	}
	return node
}

func TestObservedFieldsAreOptionalAndBounded(t *testing.T) {
	pv := crdSchema(t, "maul.run_previewverifications.yaml")
	side := optionalField(t, pv, "status", "ledger", "entries", "[]", "side")
	if !slices.Equal(side.Enum, []string{string(TreeHead), string(TreeBase)}) {
		t.Errorf("ledger index side enum = %v, want [head base]", side.Enum)
	}

	intent := crdSchema(t, "maul.run_intents.yaml")
	judge := optionalField(t, intent, "status", "verification", "judge")
	if judge.Type != "string" || judge.MaxLength == nil || *judge.MaxLength != 32 || judge.Pattern == "" {
		t.Errorf("judge = %+v, want a string bounded by maxLength 32 and a pattern", judge)
	} else {
		re := regexp.MustCompile(judge.Pattern)
		for v, want := range map[string]bool{
			string(JudgeObserved): true, "observed/2": true, "observed/12": true,
			"": false, "observed": false, "observed/0": false, "Observed/1": false, "observed/1 ": false,
		} {
			if re.MatchString(v) != want {
				t.Errorf("judge pattern %q admits %q = %v, want %v", judge.Pattern, v, !want, want)
			}
		}
		if len(JudgeObserved) > 32 {
			t.Errorf("JudgeObserved %q is over the schema's 32 characters", JudgeObserved)
		}
	}

	cites := optionalField(t, intent, "status", "verification", "outcomes", "[]", "cites")
	if cites.Type != "array" || cites.MaxItems == nil || *cites.MaxItems != MaxOutcomeCites ||
		cites.ListType != "atomic" {
		t.Errorf("cites = %+v, want an atomic array of at most %d", cites, MaxOutcomeCites)
	}
	if it := cites.Items; it == nil || it.Type != "integer" || it.Minimum == nil || *it.Minimum != 1 ||
		it.Maximum == nil || *it.Maximum != MaxLedgerIndexEntries {
		t.Errorf("cites items = %+v, want integers 1..%d (a ledger index seq)", cites.Items, MaxLedgerIndexEntries)
	}
}
