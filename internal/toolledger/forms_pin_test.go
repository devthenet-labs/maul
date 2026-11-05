// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
)

// TestCheckFormsMatchAPI pins every checks.Form to its v1alpha1.CheckForm
// twin: confirmedCheck stores a check's form by conversion, so a form the
// CRD enum lacks would be refused on the status write that saves it.
func TestCheckFormsMatchAPI(t *testing.T) {
	for _, c := range []struct {
		form checks.Form
		api  v1alpha1.CheckForm
	}{
		{checks.FormHTTP, v1alpha1.CheckFormHTTP},
		{checks.FormBrowser, v1alpha1.CheckFormBrowser},
		{checks.FormExec, v1alpha1.CheckFormExec},
		{checks.FormRun, v1alpha1.CheckFormRun},
	} {
		if got := v1alpha1.CheckForm(c.form); got != c.api {
			t.Errorf("checks form %q converts to %q, want v1alpha1 %q", c.form, got, c.api)
		}
	}
}
