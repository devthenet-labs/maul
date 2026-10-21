// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentresult

import (
	"strings"
	"testing"
	"unicode/utf8"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/report"
)

// TestReproduction converts the event's reproduction onto the status shape:
// nil stays nil ("not reported"), an explicit false stays false, and every
// value is re-bounded to the CRD's limits, because on a repository-declared
// image the pod that wrote the event is untrusted and need not have bounded
// anything; the command is kept on one line.
func TestReproduction(t *testing.T) {
	if got := Reproduction(nil); got != nil {
		t.Errorf("Reproduction(nil) = %+v, want nil", got)
	}
	if got := Reproduction(&envelope.Reproduction{}); got == nil || got.Reproduced {
		t.Errorf("Reproduction(not reproduced) = %+v, want an explicit false", got)
	}
	ok := &envelope.Reproduction{Reproduced: true, Command: "go test ./...", Before: "FAIL", After: "ok"}
	if got, want := Reproduction(ok), (&v1alpha1.Reproduction{
		Reproduced: true, Command: "go test ./...", Before: "FAIL", After: "ok",
	}); *got != *want {
		t.Errorf("Reproduction() = %+v, want %+v", got, want)
	}

	hostile := &envelope.Reproduction{
		Reproduced: true,
		Command:    "go test\n./...\r\n" + strings.Repeat("x", 10000),
		Before:     strings.Repeat("€", 5000) + "\xff",
		After:      "\xff" + strings.Repeat("a", 10000),
	}
	got := Reproduction(hostile)
	for _, f := range []struct {
		name, value string
		limit       int
	}{
		{"command", got.Command, report.ReproductionCommandMaxBytes},
		{"before", got.Before, report.ReproductionExcerptMaxBytes},
		{"after", got.After, report.ReproductionExcerptMaxBytes},
	} {
		if len(f.value) > f.limit || !utf8.ValidString(f.value) || !strings.HasSuffix(f.value, report.Truncated) {
			t.Errorf("%s is %d bytes (valid UTF-8 %v), want a cut value of at most %d", f.name, len(f.value),
				utf8.ValidString(f.value), f.limit)
		}
	}
	if strings.ContainsAny(got.Command, "\r\n") || !strings.HasPrefix(got.Command, "go test ./... x") {
		t.Errorf("Command = %q, want it on one line", got.Command[:30])
	}
}
