// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"fmt"
	"strings"
	"testing"
)

func TestCheckApprovers(t *testing.T) {
	many := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("user-%d", i)
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		logins []string
		want   *ApproverListError
	}{
		{name: "one", logins: []string{"alice"}},
		{name: "every character a login may hold", logins: []string{"A-b_c.9", "0x"}},
		{name: "twenty", logins: many(20)},
		{name: "the longest login", logins: []string{strings.Repeat("a", 64)}},
		{name: "none", logins: nil, want: &ApproverListError{Reason: ApproversEmpty}},
		{name: "twenty-one", logins: many(21), want: &ApproverListError{Reason: ApproversTooMany, Count: 21}},
		{name: "a Bot", logins: []string{"maul[bot]"},
			want: &ApproverListError{Reason: ApproverNotALogin, Login: "maul[bot]"}},
		{name: "a leading hyphen", logins: []string{"alice", "-bob"},
			want: &ApproverListError{Reason: ApproverNotALogin, Login: "-bob"}},
		{name: "empty", logins: []string{""}, want: &ApproverListError{Reason: ApproverNotALogin}},
		{name: "blanks", logins: []string{" alice"},
			want: &ApproverListError{Reason: ApproverNotALogin, Login: " alice"}},
		{name: "a line break after it", logins: []string{"alice\n"},
			want: &ApproverListError{Reason: ApproverNotALogin, Login: "alice\n"}},
		{name: "too long", logins: []string{strings.Repeat("a", 65)},
			want: &ApproverListError{Reason: ApproverNotALogin, Login: strings.Repeat("a", 65)}},
		{name: "twice", logins: []string{"alice", "alice"},
			want: &ApproverListError{Reason: ApproverDuplicate, Login: "alice"}},
		{name: "twice but for case, which GitHub does not tell apart", logins: []string{"alice", "bob", "Alice"},
			want: &ApproverListError{Reason: ApproverDuplicate, Login: "Alice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckApprovers(tc.logins)
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Errorf("CheckApprovers = %+v, want %+v", got, tc.want)
			}
			if got != nil && got.Error() == "" {
				t.Error("an error with no text")
			}
		})
	}
}
