// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// textFailure is a failed text matcher's message in @playwright/test
// 1.64.1's shape (formatMatcherMessage): the element found and its text
// received, or, with received "", no element found.
func textFailure(matcher, expectedLabel, receivedLabel, expected, received string, found bool) string {
	var b strings.Builder
	b.WriteString("Error: expect(locator)." + matcher + "(expected) failed\n\n")
	b.WriteString("Locator: getByRole('row').filter({ hasText: 'v1a1t1-abc' })\n")
	b.WriteString(expectedLabel + expected + "\n")
	if found {
		b.WriteString(receivedLabel + received + "\n")
	}
	b.WriteString("Timeout: 5000ms\n")
	if !found {
		b.WriteString("element(s) not found\n")
	}
	b.WriteString("\nCall log:\n  - Expect \"" + matcher + "\" with timeout 5000ms\n")
	if found {
		b.WriteString("      - unexpected value " + received + "\n")
	}
	return b.String()
}

// diffFailure is a failed toContainText's message in @playwright/test
// 1.64.1's diff shape (printed when a side spans lines): its received
// header's count, then its body lines after the expected side's one.
func diffFailure(receivedCount string, body ...string) string {
	return "Error: expect(locator).toContainText(expected) failed\n\nLocator: locator('li')\nTimeout: 5000ms\n" +
		"- Expected substring  - 1\n+ Received string     " + receivedCount + "\n\n- swing 20%\n" +
		strings.Join(body, "\n") + "\n\nCall log:\n  - Expect \"toContainText\" with timeout 5000ms\n"
}

// TestTextFailureReceived (D-B4): a text assertion that failed on an
// element that was there keeps what that element showed, its whitespace
// collapsed, apart from maul's words, so a reviewer can tell the run's own
// row reading the wrong text from no row; a missing element says so in
// maul's words and keeps nothing. Only one well-formed Received value, or
// one whole diff, is ever kept.
func TestTextFailureReceived(t *testing.T) {
	const contains, shown = "toContainText", `"v1a1t1-abc · by ana · 120 bpm · swing 0%"`
	for _, tc := range []struct {
		name, check, message string
		assertion            bool
		detail, received     string
	}{
		{"found, keyed by the run", keyedBrowserCheck,
			textFailure(contains, "Expected substring: ", "Received string:    ", `"swing 20%"`, shown, true),
			true, "the text expectation was not met", "v1a1t1-abc · by ana · 120 bpm · swing 0%"},
		{"found, equals", strings.Replace(keyedBrowserCheck, `"contains":"1"`, `"equals":"1"`, 1),
			textFailure("toHaveText", "Expected: ", "Received: ", `"1"`, `"0"`, true),
			true, "the text expectation was not met", "0"},
		{"found, a pattern", strings.Replace(keyedBrowserCheck, `"contains":"1"`, `"matches":"^1"`, 1),
			textFailure("toHaveText", "Expected pattern: ", "Received string:  ", `/^1/`, `"0 votes"`, true),
			true, "the text expectation was not met", "0 votes"},
		{"quotes and backslashes unescaped", keyedBrowserCheck,
			textFailure(contains, "Expected substring: ", "Received string:    ", `"1"`, `"say \"hi\" \\ bye"`, true),
			true, "the text expectation was not met", `say "hi" \ bye`},
		{"found, empty text", keyedBrowserCheck,
			textFailure(contains, "Expected substring: ", "Received string:    ", `"1"`, `""`, true),
			true, "the text expectation was not met", ""},
		{"not keyed by the run: nothing kept", browserCheck,
			textFailure(contains, "Expected substring: ", "Received string:    ", `"1"`, shown, true),
			false, "the text expectation was not met", ""},
		{"no element", keyedBrowserCheck,
			textFailure(contains, "Expected substring: ", "", `"1"`, "", false),
			false, "the text expectation was not met: no element matched its locator", ""},
		{"no element, older shape", keyedBrowserCheck,
			"Error: expect(locator).toContainText(expected) failed\nReceived: <element(s) not found>",
			false, "the text expectation was not met: no element matched its locator", ""},
		// An element whose children sit on lines of their own: the runner
		// prints a diff (1.64.1's shape, as TestLiveBrowserText reads it).
		{"a diff over lines, collapsed", keyedBrowserCheck, diffFailure("+ 4", "+", "+     v1a1t1-abc",
			"+     swing 0%", "+   "), true, "the text expectation was not met", "v1a1t1-abc swing 0%"},
		{"a diff with a shared line", keyedBrowserCheck, diffFailure("+ 2", "+ a", "  1", "+ b"),
			true, "the text expectation was not met", "a 1 b"},
		{"a diff with lines left out: nothing kept", keyedBrowserCheck,
			diffFailure("+ 2", "@@ -1,1 +1,9 @@", "+ a", "+ b"), true, "the text expectation was not met", ""},
		{"a diff whose counts do not hold: nothing kept", keyedBrowserCheck, diffFailure("+ 3", "+ a", "+ b"),
			true, "the text expectation was not met", ""},
		{"a quoted text over lines, collapsed", strings.Replace(keyedBrowserCheck, `"contains":"1"`,
			`"matches":"^1"`, 1), textFailure("toHaveText", "Expected pattern: ", "Received string:  ", `/^1/`,
			"\"\n   0\n   votes\n\"", true), true, "the text expectation was not met", "0 votes"},
		{"spaces collapsed", keyedBrowserCheck,
			textFailure(contains, "Expected substring: ", "Received string:    ", `"1"`, "\"  0 \t votes \"", true),
			true, "the text expectation was not met", "0 votes"},
		{"two Received lines: nothing kept", keyedBrowserCheck,
			textFailure(contains, "Expected substring: ", "Received string:    ", `"1"`,
				"\"0\"\nReceived string: \"1\"", true),
			true, "the text expectation was not met", ""},
		{"a quote left unescaped: nothing kept", keyedBrowserCheck,
			textFailure(contains, "Expected substring: ", "Received string:    ", `"1"`, `"a" "b"`, true),
			true, "the text expectation was not met", ""},
		{"over the bound: nothing kept", keyedBrowserCheck,
			textFailure(contains, "Expected substring: ", "Received string:    ", `"1"`,
				`"`+strings.Repeat("x", toolwire.MaxReceivedBytes+1)+`"`, true),
			true, "the text expectation was not met", ""},
		{"a count keeps nothing", strings.Replace(keyedBrowserCheck,
			`"assert":"text","locator":{"role":"row","hasText":"{{run}}"},"text":{"contains":"1"}`,
			`"assert":"count","locator":{"role":"row","hasText":"{{run}}"},"count":2`, 1),
			"Error: expect(locator).toHaveCount(expected) failed\n\nLocator: x\nExpected: 2\nReceived: 1\n",
			false, "the count expectation was not met", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matcher := contains
			if strings.Contains(tc.message, "toHaveText") {
				matcher = "toHaveText"
			} else if strings.Contains(tc.message, "toHaveCount") {
				matcher = "toHaveCount"
			}
			h, _ := browserCheckHarness(t, func(_ int, spec string) string {
				return report("failed", nil, tc.message, lineOf(spec, matcher))
			})
			rec := h.relay.runCheck(context.Background(), parseCheck(t, tc.check), toolwire.ModeDryRun)
			if rec.Result != toolwire.RunFail || len(rec.Attempts) != 2 {
				t.Fatalf("got %+v", rec)
			}
			for _, a := range rec.Attempts {
				last := a.Steps[len(a.Steps)-1]
				if last.Assertion != tc.assertion || last.Detail != tc.detail || last.Received != tc.received {
					t.Errorf("step %+v; want assertion %v, detail %q, received %q", last, tc.assertion, tc.detail,
						tc.received)
				}
			}
			if err := checkRunLedgerEntry(rec).Check(); err != nil {
				t.Errorf("the run's ledger entry does not hold: %v", err)
			}
		})
	}
}

// checkRunLedgerEntry is rec as the relay records a dry run.
func checkRunLedgerEntry(rec toolwire.CheckRunRecord) toolwire.LedgerEntry {
	return toolwire.LedgerEntry{Seq: 1, TS: time.Unix(1, 0), Kind: toolwire.KindAgent, Verb: toolwire.VerbCheckRun,
		Args: []byte(`{}`), OutputSHA256: sha256Hex(nil), CheckRun: &rec}
}
