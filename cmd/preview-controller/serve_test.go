// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/devthenet-labs/maul/internal/cli"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("preview-controller", "test", opts)
	serve := newServeCmd(opts)
	serve.RunE = func(*cobra.Command, []string) error { return nil }
	root.AddCommand(serve)
	root.SetArgs(append([]string{"serve"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return opts
}

// validArgs is a settings surface preview.Settings.Validate accepts.
func validArgs(extra ...string) []string {
	return append([]string{
		"--namespace", "maul",
		"--preview-image-prefix", "123456789012.dkr.ecr.us-east-1.amazonaws.com/maul/previews/",
		"--preview-host-suffix", "previews.example.com",
		"--preview-node-pool", "previews", "--preview-node-class", "previews",
		"--preview-taint-key", "maul.run/preview",
	}, extra...)
}

func TestAuthSettings(t *testing.T) {
	const set = `{"maul-preview-0":{"alb.ingress.kubernetes.io/auth-type":"oidc"}}`
	for _, tt := range []struct {
		name         string
		args         []string
		wantRequired bool
		wantCurrent  int
		wantPrevious int
		wantErr      string
	}{
		{name: "unset"},
		{
			name: "required with both generations",
			args: []string{"--preview-auth-required", "--preview-auth-annotations", set,
				"--preview-auth-previous-annotations", set},
			wantRequired: true, wantCurrent: 1, wantPrevious: 1,
		},
		{name: "bad current", args: []string{"--preview-auth-annotations", "{"}, wantErr: "preview auth annotations"},
		{
			name:    "trailing data",
			args:    []string{"--preview-auth-annotations", set + " {}"},
			wantErr: "trailing data",
		},
		{
			name:    "bad previous",
			args:    []string{"--preview-auth-annotations", set, "--preview-auth-previous-annotations", "[]"},
			wantErr: "previous generation:",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := authSettings(serveOpts(t, tt.args...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("authSettings error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("authSettings: %v", err)
			}
			if got.Required != tt.wantRequired || len(got.Annotations) != tt.wantCurrent ||
				len(got.Previous) != tt.wantPrevious {
				t.Errorf("authSettings = %+v, want required=%v current=%d previous=%d",
					got, tt.wantRequired, tt.wantCurrent, tt.wantPrevious)
			}
		})
	}
}

// TestServeStartupErrors: serve refuses an invalid configuration before it
// builds a manager, and reaches the manager (here: a kubeconfig that does
// not exist) only once the settings are valid.
func TestServeStartupErrors(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	missing := filepath.Join(t.TempDir(), "absent")
	for _, tt := range []struct {
		name    string
		env     string
		args    []string
		wantErr string
	}{
		{name: "bad annotations", args: validArgs("--preview-auth-annotations", "nope"),
			wantErr: "preview auth annotations"},
		{name: "no image prefix", args: []string{"--namespace", "maul"}, wantErr: "image prefix"},
		{name: "no namespace", args: validArgs("--namespace", ""), wantErr: "invalid preview-controller settings"},
		{name: "too many retries", args: validArgs("--preview-max-retries", "4"),
			wantErr: "invalid preview-controller settings"},
		{name: "annotations without required", args: validArgs("--preview-auth-annotations",
			`{"maul-preview-0":{}}`), wantErr: "preview auth is not required"},
		{name: "required without annotations", args: validArgs("--preview-auth-required"),
			wantErr: "no annotations are set"},
		{name: "bad node selector", args: validArgs("--preview-node-selector", "{"),
			wantErr: "preview node selector"},
		{name: "node selector without a taint", args: []string{"--namespace", "maul",
			"--preview-image-prefix", "registry.example/maul/previews/", "--preview-host-suffix", "previews.example.com",
			"--preview-node-selector", `{"node-role.example.com/preview":"true"}`},
			wantErr: "node isolation is required"},
		{name: "no isolation without opting out", args: []string{"--namespace", "maul",
			"--preview-image-prefix", "registry.example/maul/previews/", "--preview-host-suffix", "previews.example.com"},
			wantErr: "node isolation is required"},
		{name: "unknown isolation mode", args: validArgs("--preview-node-isolation", "optional"),
			wantErr: "neither"},
		{name: "valid settings reach the manager", args: validArgs("--kubeconfig", missing),
			wantErr: "kubeconfig"},
		{name: "generic node isolation reaches the manager", args: []string{"--namespace", "maul",
			"--preview-image-prefix", "registry.example/maul/previews/", "--preview-host-suffix", "previews.example.com",
			"--preview-node-selector", `{"node-role.example.com/preview":"true"}`,
			"--preview-taint-key", "node-role.example.com/preview", "--kubeconfig", missing},
			wantErr: "kubeconfig"},
		{name: "gateway mode reaches the manager", args: validArgs("--preview-legacy-ingress=false",
			"--preview-target-health", "--kubeconfig", missing), wantErr: "kubeconfig"},
		{name: "isolation none reaches the manager", args: []string{"--namespace", "maul",
			"--preview-image-prefix", "registry.example/maul/previews/", "--preview-host-suffix", "previews.example.com",
			"--preview-node-isolation", "none", "--kubeconfig", missing},
			wantErr: "kubeconfig"},
		{name: "verify deadline under a minute", args: evaluatorArgs("--preview-verify-deadline-max", "30s"),
			wantErr: "preview-verify-deadline-max"},
		{name: "verify deadline over an hour", args: evaluatorArgs("--preview-verify-deadline-max", "2h"),
			wantErr: "preview-verify-deadline-max"},
		{name: "verification reaches the manager", args: evaluatorArgs("--kubeconfig", missing),
			wantErr: "kubeconfig"},
		{name: "namespace from the pod", env: "maul", args: validArgs("--namespace", "", "--kubeconfig", missing),
			wantErr: "kubeconfig"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("POD_NAMESPACE", tt.env)
			}
			err := serve(t.Context(), serveOpts(t, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("serve error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestLegacyIngressFlag: the legacy slot Ingress is on unless the operator
// turns it off, so an install that never sets the flag keeps its behaviour.
func TestLegacyIngressFlag(t *testing.T) {
	if !serveOpts(t, validArgs()...).Bool("preview-legacy-ingress") {
		t.Error("--preview-legacy-ingress defaults to false; want today's behaviour, true")
	}
	if serveOpts(t, validArgs("--preview-legacy-ingress=false")...).Bool("preview-legacy-ingress") {
		t.Error("--preview-legacy-ingress=false not honoured")
	}
	t.Setenv("MAUL_PREVIEW_LEGACY_INGRESS", "false")
	if serveOpts(t, validArgs()...).Bool("preview-legacy-ingress") {
		t.Error("MAUL_PREVIEW_LEGACY_INGRESS=false not honoured")
	}
}

// testerImage is a digest-pinned image of the retired preview tester.
const testerImage = "ghcr.io/devthenet-labs/maul-preview-tester@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestVerificationFlags: the verification deadline bound defaults to the
// chart's preview.verification.deadlineMaxSeconds default, 30 minutes, and
// the environment sets it.
func TestVerificationFlags(t *testing.T) {
	if got := serveOpts(t, validArgs()...).Duration("preview-verify-deadline-max"); got != 30*time.Minute {
		t.Errorf("--preview-verify-deadline-max defaults to %s, want 30m", got)
	}
	t.Setenv("MAUL_PREVIEW_VERIFY_DEADLINE_MAX", "1200s")
	if got := serveOpts(t, validArgs()...).Duration("preview-verify-deadline-max"); got != 20*time.Minute {
		t.Errorf("MAUL_PREVIEW_VERIFY_DEADLINE_MAX not honoured: %s", got)
	}
}

// TestTesterImageFlagRemoved: --preview-tester-image is removed with the
// preview tester (S4): set, from the command line or the environment, with
// or without the evaluator, digest-pinned or not, it stops startup with the
// migration it names; it is hidden from the help. Unset or empty, it
// changes nothing.
func TestTesterImageFlagRemoved(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	missing := filepath.Join(t.TempDir(), "absent")
	for _, tc := range []struct {
		name string
		env  string
		args []string
	}{
		{"alone", "", validArgs("--preview-tester-image", testerImage, "--kubeconfig", missing)},
		{"with the evaluator", "", evaluatorArgs("--preview-tester-image", testerImage, "--kubeconfig", missing)},
		{"by tag", "", evaluatorArgs("--preview-tester-image", "ghcr.io/devthenet-labs/maul-preview-tester:v1",
			"--kubeconfig", missing)},
		{"from the environment", testerImage, evaluatorArgs("--kubeconfig", missing)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("MAUL_PREVIEW_TESTER_IMAGE", tc.env)
			}
			err := serve(t.Context(), serveOpts(t, tc.args...))
			if err == nil || !strings.Contains(err.Error(), "--preview-tester-image") ||
				!strings.Contains(err.Error(), "preview.verification.testerImage") ||
				!strings.Contains(err.Error(), "docs/intents/evaluator.md#migrating-from-slice-1") {
				t.Fatalf("serve(%v) = %v, want the removal and its migration", tc.args, err)
			}
		})
	}
	if err := serve(t.Context(), serveOpts(t, evaluatorArgs("--preview-tester-image", "",
		"--kubeconfig", missing)...)); err == nil || !strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("an empty --preview-tester-image: %v, want it ignored (reaching the manager)", err)
	}
	cmd := newServeCmd(cli.NewOptions())
	if f := cmd.Flags().Lookup("preview-tester-image"); f == nil || !f.Hidden {
		t.Errorf("--preview-tester-image = %+v, want it registered and hidden", f)
	}
}

const (
	toolsImage = "ghcr.io/devthenet-labs/maul-preview-tools@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	browserImage = "ghcr.io/devthenet-labs/maul-preview-browser@sha256:" +
		"2222222222222222222222222222222222222222222222222222222222222222"
	toolboxImage = "ghcr.io/devthenet-labs/maul-preview-toolbox@sha256:" +
		"3333333333333333333333333333333333333333333333333333333333333333"
)

// evaluatorArgs turns the evaluator on with every value it needs.
func evaluatorArgs(extra ...string) []string {
	return validArgs(append([]string{"--preview-evaluator",
		"--preview-tools-image", toolsImage, "--preview-browser-image", browserImage,
		"--preview-toolbox-image", toolboxImage, "--preview-agents-namespace", "maul-agents"}, extra...)...)
}

// TestEvaluatorFlags: the evaluator is off unless the operator turns it
// on, and on it needs its three digest-pinned images and the agents
// namespace: it turns verification on by itself.
func TestEvaluatorFlags(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	opts := serveOpts(t, validArgs()...)
	if opts.Bool("preview-evaluator") || opts.Int("preview-max-evaluations") != 0 {
		t.Error("the evaluator is on by default; it must be opt-in")
	}
	missing := filepath.Join(t.TempDir(), "absent")
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "on reaches the manager", args: evaluatorArgs("--kubeconfig", missing), wantErr: "kubeconfig"},
		{name: "no toolbox image", args: evaluatorArgs("--preview-toolbox-image", ""), wantErr: "toolbox"},
		{name: "a tag", args: evaluatorArgs("--preview-browser-image", "ghcr.io/x/browser:v1"),
			wantErr: "not pinned by digest"},
		{name: "no agents namespace", args: evaluatorArgs("--preview-agents-namespace", ""),
			wantErr: "agents namespace"},
		{name: "negative max", args: evaluatorArgs("--preview-max-evaluations", "-1"), wantErr: "max evaluations"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := serve(t.Context(), serveOpts(t, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("serve error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
