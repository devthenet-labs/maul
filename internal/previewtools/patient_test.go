// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestRedBrowserRunsArePatient (third review, F6): maul's own run of a
// browser check that may show a defect (its red confirmation) is compiled
// patient, so a slow but correct page is not read as the defect; the
// agent's dry run and green runs are not.
func TestRedBrowserRunsArePatient(t *testing.T) {
	failing := func(_ int, spec string) string {
		return report("failed", nil, "Error: expect(locator).toContainText(expected) failed\nReceived string: \"0\"",
			lineOf(spec, "toContainText"))
	}
	h, run := browserCheckHarness(t, failing)
	h.call(toolwire.VerbCheckRun, `{"check":`+keyedBrowserCheck+`}`)
	d := bareDigest(t, keyedBrowserCheck)
	if _, resp := h.call(toolwire.VerbConfirm, redConfirm(t, d, true)); resp.Refused != "" {
		t.Fatalf("confirm: %+v", resp)
	}
	h.finish()
	if len(run.specs) != 4 {
		t.Fatalf("specs %d, want the dry run's two attempts and the confirmation's", len(run.specs))
	}
	for i, spec := range run.specs {
		if patient := strings.Contains(spec, "maul.patience("); patient != (i >= 2) {
			t.Errorf("spec %d patient %v, want only the confirmation's", i, patient)
		}
	}

}
