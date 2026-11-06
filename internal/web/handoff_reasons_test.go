// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// tsRecordKeys are the keys of the exported Record<string, string> named
// name in src, in order.
func tsRecordKeys(t *testing.T, src, name string) []string {
	t.Helper()
	start := strings.Index(src, "export const "+name+": Record<string, string> = {")
	if start < 0 {
		t.Fatalf("ui/src/intents.ts has no %s", name)
	}
	body := src[start:]
	body = body[strings.Index(body, "{")+1 : strings.Index(body, "};")]
	key := regexp.MustCompile(`(?m)^\s*"?([a-z-]+)"?:`)
	var out []string
	for _, m := range key.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestHandoffReasonTablesMatchTheAPI (S2-20, from S2-19): the status
// page's two hand-off reason tables, the slot's (HANDOFF_REASONS) and the
// agents round's (AGENTS_HANDOFF_REASONS), each name every
// v1alpha1.HandoffReason and nothing else, so a reason added to one is
// added to the other, and a released hand-off never shows its raw reason.
func TestHandoffReasonTablesMatchTheAPI(t *testing.T) {
	src, err := os.ReadFile("ui/src/intents.ts")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{string(v1alpha1.HandoffReasonReady), string(v1alpha1.HandoffReasonUnconfirmed),
		string(v1alpha1.HandoffReasonOutOfTime), string(v1alpha1.HandoffReasonHuman),
		string(v1alpha1.HandoffReasonCommand), string(v1alpha1.HandoffReasonEarly),
		string(v1alpha1.HandoffReasonEvaluatorUnverified)}
	slices.Sort(want)
	for _, table := range []string{"HANDOFF_REASONS", "AGENTS_HANDOFF_REASONS"} {
		got := tsRecordKeys(t, string(src), table)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s keys %v, v1alpha1.HandoffReason %v", table, got, want)
		}
	}
}
