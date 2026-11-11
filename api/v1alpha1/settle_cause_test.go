// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// enumOf is the values of typeName's kubebuilder enum marker in file, the
// CRD's vocabulary as the API server enforces it.
func enumOf(t *testing.T, file, typeName string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	const marker = "+kubebuilder:validation:Enum="
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Doc == nil {
			continue
		}
		for _, spec := range gen.Specs {
			if s, ok := spec.(*ast.TypeSpec); !ok || s.Name.Name != typeName {
				continue
			}
			for _, c := range gen.Doc.List {
				if _, rest, ok := strings.Cut(c.Text, marker); ok {
					return strings.Split(strings.TrimSpace(rest), ";")
				}
			}
		}
	}
	t.Fatalf("%s: no enum marker on %s", file, typeName)
	return nil
}

// TestCauseOfEveryReason: every PreviewVerificationReason the schema
// admits, the retired spellings included, maps to exactly one SettleCause
// the Intent's schema admits (SettleNone is the field left empty), and
// only the reasons a cause names map to one: the rest are SettleNone.
func TestCauseOfEveryReason(t *testing.T) {
	named := map[PreviewVerificationReason]SettleCause{
		VerificationReasonSandboxImageUnavailable: SettleSandboxImage,
		VerificationReasonTreeUnavailable:         SettleTree,
		VerificationReasonToolsBuildFailed:        SettleToolsBuildFailed,
		VerificationReasonSandboxUnenforced:       SettleSandboxUnenforced,
		VerificationReasonToolsEvicted:            SettleToolsLost,
		VerificationReasonToolsDeadline:           SettleToolsLost,
		VerificationReasonKeptCopyFailed:          SettleToolsLost,
		VerificationReasonReplayIncomplete:        SettleReplayIncomplete,
	}
	reasons := enumOf(t, "previewverification_types.go", "PreviewVerificationReason")
	for r := range named {
		if !slices.Contains(reasons, string(r)) {
			t.Errorf("%s is not in the PreviewVerification schema", r)
		}
	}
	for _, r := range reasons {
		got := CauseOf(PreviewVerificationReason(r))
		if !slices.Contains(SettleCauses(), got) {
			t.Errorf("%s maps to %q, not a cause", r, got)
		}
		if want := named[PreviewVerificationReason(r)]; got != want {
			t.Errorf("%s maps to %q, want %q", r, got, want)
		}
	}
	if got := CauseOf("@octocat"); got != SettleNone {
		t.Errorf("an unknown reason maps to %q", got)
	}
}

// TestSettleCausesMatchTheSchema: SettleCauses are every cause and the
// Intent schema's enum exactly (SettleNone being the field left empty), so
// a cause cannot be recorded that the API server refuses, nor added without
// the list every wording test walks.
func TestSettleCausesMatchTheSchema(t *testing.T) {
	enum := enumOf(t, "intent_types.go", "SettleCause")
	var got []string
	for _, c := range SettleCauses() {
		if c != SettleNone {
			got = append(got, string(c))
		}
	}
	if !slices.Equal(got, enum) {
		t.Errorf("SettleCauses = %q, schema enum %q", got, enum)
	}
	if SettleCauses()[0] != SettleNone {
		t.Errorf("SettleCauses do not start with SettleNone: %q", SettleCauses())
	}
	// The spellings recorded before the cause had its own name.
	for c, old := range map[SettleCause]string{SettlePreviewNotLive: "PreviewNotLive",
		SettleSandboxImage: "SandboxImageUnavailable", SettleTree: "TreeUnavailable",
		SettleToolsBuildFailed: "ToolsBuildFailed"} {
		if string(c) != old {
			t.Errorf("%q no longer reads a record written as %q", c, old)
		}
	}
}
