// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// TestEvaluatorRoleLabels pins the evaluator's role vocabulary: the chart's
// network policies and the maul-evaluator-labels admission policy repeat
// these strings, so a change here must be a change there too.
func TestEvaluatorRoleLabels(t *testing.T) {
	keys := []struct{ got, want string }{
		{LabelRole, "maul.run/role"},
		{LabelPreviewVerification, "maul.run/preview-verification"},
	}
	for _, k := range keys {
		if k.got != k.want {
			t.Errorf("label key = %q, want %q", k.got, k.want)
		}
		if errs := validation.IsQualifiedName(k.got); len(errs) > 0 {
			t.Errorf("label key %q is not a qualified name: %v", k.got, errs)
		}
	}
	values := []struct{ got, want string }{
		{RoleEvaluator, "evaluator"},
		{RoleCheckReview, "checkreview"},
		{PreviewRoleTools, "tools"},
	}
	for _, v := range values {
		if v.got != v.want {
			t.Errorf("role value = %q, want %q", v.got, v.want)
		}
		if errs := validation.IsValidLabelValue(v.got); len(errs) > 0 {
			t.Errorf("role value %q is not a label value: %v", v.got, errs)
		}
	}
	for _, role := range []PreviewPartRole{
		PreviewRoleComponent, PreviewRoleService, PreviewRoleTester, PreviewRoleCommand, PreviewRoleSeed,
	} {
		if string(role) == PreviewRoleTools {
			t.Errorf("PreviewRoleTools %q collides with an existing preview part role", PreviewRoleTools)
		}
	}
}
