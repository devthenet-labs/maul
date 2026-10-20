// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnercfg

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/cli"
	"github.com/devthenet-labs/maul/internal/credgate"
)

func newCredOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	o := cli.NewOptions()
	cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return nil }}
	o.Bind(cmd)
	RegisterUserCredentialsFlags(cmd.Flags())
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := o.Load(cmd); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return o
}

func TestUserCredentials(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		attr     attribution.Mode
		wantMode credgate.Mode
		wantURL  string
		wantErr  string
	}{
		{"defaults off", nil, attribution.ModeOff, credgate.ModeOff, "", ""},
		{"attribute without attribution", []string{"--user-credentials", "attribute"}, attribution.ModeOff,
			credgate.ModeAttribute, "", ""},
		{"enforce", []string{"--user-credentials", "enforce", "--account-url", "https://s.example.com/#/account"},
			attribution.ModeAttribute, credgate.ModeEnforce, "https://s.example.com/#/account", ""},
		{"enforce without attribution", []string{"--user-credentials", "enforce"}, attribution.ModeOff, "", "",
			"needs --attribution-mode attribute"},
		{"unknown mode", []string{"--user-credentials", "on"}, attribution.ModeAttribute, "", "", "is not off"},
		{"relative url", []string{"--account-url", "/#/account"}, attribution.ModeOff, "", "", "absolute"},
		{"javascript url", []string{"--account-url", "javascript:alert"}, attribution.ModeOff, "", "",
			"absolute"},
		{"url with space", []string{"--account-url", "https://s.example.com/a b"}, attribution.ModeOff, "", "",
			"whitespace"},
		{"url with userinfo", []string{"--account-url", "https://u:p@s.example.com/"}, attribution.ModeOff, "", "",
			"without credentials"},
		{"url closing a markdown link", []string{"--account-url", "https://s.example.com/)x"}, attribution.ModeOff,
			"", "", "Markdown"},
		{"url too long", []string{"--account-url", "https://s.example.com/" + strings.Repeat("a", 600)},
			attribution.ModeOff, "", "", "longer than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, u, err := UserCredentials(newCredOpts(t, tc.args...), tc.attr)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || mode != tc.wantMode || u != tc.wantURL {
				t.Fatalf("UserCredentials = %q, %q, %v; want %q, %q", mode, u, err, tc.wantMode, tc.wantURL)
			}
		})
	}
}
