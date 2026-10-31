// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package playwright

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
)

// TestCompilePatient (third review, F6): a check run to show a defect is
// compiled patient: every retried judgement and Playwright expectation
// waits PatientExpectTimeoutMS, not the action timeout, under a longer test
// timeout, so a slow but correct page is not read as the defect. A check
// compiled without it is unchanged.
func TestCompilePatient(t *testing.T) {
	c, err := checks.Parse([]byte(`{"id":"v","outcome":"v","form":"browser","browser":{"steps":[{"goto":"/"},` +
		`{"expect":{"assert":"text","locator":{"testid":"s"},"text":{"contains":"1"}}},` +
		`{"expect":{"assert":"count","locator":{"testid":"r","hasText":"{{run}}"},"count":2}},` +
		`{"expect":{"assert":"visible","locator":{"testid":"s"}}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Compile(*c.Browser, Options{RunToken: "tok1"})
	if err != nil {
		t.Fatal(err)
	}
	patient, err := Compile(*c.Browser, Options{RunToken: "tok1", Patient: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"maul.patience(15000)", "test.setTimeout(120000)",
		`toContainText("1", { timeout: 15000 })`, `toHaveCount(2, { timeout: 15000 })`} {
		if !strings.Contains(string(patient), want) {
			t.Errorf("the patient spec lacks %q:\n%s", want, patient)
		}
		if strings.Contains(string(plain), want) {
			t.Errorf("the plain spec carries %q", want)
		}
	}
	if !strings.Contains(string(Support()), "patience(ms: number)") {
		t.Errorf("the support module has no patience")
	}
}
