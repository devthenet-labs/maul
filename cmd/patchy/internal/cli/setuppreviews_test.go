// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
)

// previewsArgs are a complete existing-edge answer set writing to dir.
func previewsArgs(dir string, extra ...string) []string {
	return append([]string{"setup", "previews", "-o", dir, "--platform", "k3s", "--edge", "existing",
		"--ingress-class", "traefik", "--host-suffix", "acme-previews.dev",
		"--sign-in-host", "preview-auth.patchy.acme.dev", "--dex-issuer", "https://dex.patchy.acme.dev",
		"--viewer-team", "acme:qa", "--registry", "ghcr.io", "--image-path-prefix", "acme/patchy/previews"},
		extra...)
}

// clientSecret reads the Dex client secret the relay's Secret file holds.
func clientSecret(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, previewsetup.RelayDexSecretFile))
	if err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := yaml.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return string(s.Data[previewsetup.RelayDexSecretKey])
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// previewsRun is one operator's directory across runs, and everything
// the runs printed.
type previewsRun struct {
	t       *testing.T
	dir     string
	printed strings.Builder
}

// run runs `patchy setup previews -o <dir>` with args.
func (r *previewsRun) run(args ...string) (string, string, error) {
	r.t.Helper()
	out, errOut, err := execSetup(r.t, nil, append([]string{"setup", "previews", "-o", r.dir}, args...)...)
	r.printed.WriteString(out + errOut)
	return out, errOut, err
}

// TestSetupPreviewsLifecycle drives the command as an operator would: a
// first run, a re-run from answers.yaml alone, a changed answer refused
// and then accepted with --force, a dry run and a rotation. The secret is
// stable across every re-run, changes only on --rotate, and is printed
// nowhere.
func TestSetupPreviewsLifecycle(t *testing.T) {
	r := &previewsRun{t: t, dir: filepath.Join(t.TempDir(), "patchy-previews")}
	secret := r.first()
	r.unchanged()
	r.changed(secret)
	r.dryRun()
	rotated := r.rotate(secret)
	for _, s := range []string{secret, rotated} {
		if strings.Contains(r.printed.String(), s) {
			t.Error("the client secret was printed")
		}
	}
	for _, rel := range []string{previewsetup.AnswersFile, previewsetup.ValuesFile, previewsetup.NextStepsFile} {
		if strings.Contains(readFile(t, r.dir, rel), rotated) {
			t.Errorf("%s holds the client secret", rel)
		}
	}
}

// first is the first run: every file written, a 32-byte secret drawn.
func (r *previewsRun) first() string {
	_, errOut, err := r.run(previewsArgs(r.dir)[4:]...)
	if err != nil {
		r.t.Fatalf("first run: %v\n%s", err, errOut)
	}
	secret := clientSecret(r.t, r.dir)
	if len(secret) != 43 {
		r.t.Errorf("client secret %d characters, want 43 (32 bytes, base64url)", len(secret))
	}
	if !strings.Contains(errOut, "NEXT-STEPS.md") || !strings.Contains(errOut, "the only copy") {
		r.t.Errorf("first run stderr:\n%s", errOut)
	}
	return secret
}

// unchanged: a re-run with no flags starts from answers.yaml and changes
// nothing.
func (r *previewsRun) unchanged() {
	if _, errOut, err := r.run(); err != nil || !strings.Contains(errOut, "up to date") {
		r.t.Fatalf("re-run: %v\n%s", err, errOut)
	}
}

// changed: a changed answer is shown and refused without --force, then
// laid over answers.yaml with it; the secret stays.
func (r *previewsRun) changed(secret string) {
	out, _, err := r.run("--viewer-user", "octocat")
	var conflict *previewsetup.ConflictError
	if !errors.As(err, &conflict) || exitCode(err) != 1 {
		r.t.Fatalf("a changed answer without --force: %v", err)
	}
	if !strings.Contains(out, "+    - octocat") || !strings.Contains(out, "changed   "+previewsetup.ValuesFile) {
		r.t.Errorf("the difference was not shown:\n%s", out)
	}
	if strings.Contains(readFile(r.t, r.dir, previewsetup.ValuesFile), "octocat") {
		r.t.Error("a refused change was written")
	}
	if _, errOut, err := r.run("--viewer-user", "octocat", "--force"); err != nil {
		r.t.Fatalf("--force: %v\n%s", err, errOut)
	}
	values := readFile(r.t, r.dir, previewsetup.ValuesFile)
	if !strings.Contains(values, "octocat") || !strings.Contains(values, "acme:qa") {
		r.t.Errorf("--force did not lay the new viewer over answers.yaml:\n%s", values)
	}
	if clientSecret(r.t, r.dir) != secret {
		r.t.Error("a re-run drew a new client secret")
	}
}

// dryRun lists the secrets by name, shows the difference and writes
// nothing.
func (r *previewsRun) dryRun() {
	out, _, err := r.run("--release", "previews", "--dry-run")
	if err != nil {
		r.t.Fatalf("--dry-run: %v", err)
	}
	if !strings.Contains(out, "secrets/patchy-preview-auth-dex.secret.yaml  (a secret: never shown)") ||
		!strings.Contains(out, "+#   helm upgrade previews") {
		r.t.Errorf("--dry-run output:\n%s", out)
	}
	if strings.Contains(readFile(r.t, r.dir, previewsetup.AnswersFile), "release: previews") {
		r.t.Error("--dry-run wrote answers.yaml")
	}
}

// rotate draws a new secret, which needs no --force.
func (r *previewsRun) rotate(secret string) string {
	_, errOut, err := r.run("--rotate", "dex-client-secret")
	if err != nil {
		r.t.Fatalf("--rotate: %v\n%s", err, errOut)
	}
	rotated := clientSecret(r.t, r.dir)
	if rotated == secret || !strings.Contains(errOut, "drew a new Dex client secret") {
		r.t.Errorf("--rotate kept the secret, or said nothing:\n%s", errOut)
	}
	return rotated
}

// TestSetupPreviewsNamespace: -n names the release namespace, on a first
// run and over answers.yaml.
func TestSetupPreviewsNamespace(t *testing.T) {
	dir := t.TempDir()
	if _, errOut, err := execSetup(t, nil, previewsArgs(dir, "-n", "patchy-system")...); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if !strings.Contains(readFile(t, dir, previewsetup.RelayDexSecretFile), "namespace: patchy-system") {
		t.Error("-n did not name the release namespace")
	}
	if _, _, err := execSetup(t, nil, "setup", "previews", "-o", dir, "-n", "patchy", "--force"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, dir, previewsetup.AnswersFile), "namespace: patchy\n") {
		t.Error("-n did not override answers.yaml")
	}
}

func TestSetupPreviewsRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"nothing answered": {[]string{"setup", "previews", "-o", "x"},
			"answer --platform, --edge, --host-suffix"},
		"no recipe yet":     {previewsArgs("x", "--edge", "tailscale"), "not available yet"},
		"same site":         {previewsArgs("x", "--sign-in-host", "auth.previews.acme-previews.dev"), "sits under"},
		"bad node label":    {previewsArgs("x", "--node-label", "nolabel"), "key=value"},
		"bad rotate":        {previewsArgs("x", "--rotate", "keys"), "--rotate"},
		"registry path":     {previewsArgs("x", "--registry", "ghcr.io/acme"), "holds a path"},
		"both edges":        {previewsArgs("x", "--gateway-parent", "gw/edge"), "exclusive"},
		"platform unknown":  {previewsArgs("x", "--platform", "openshift"), "--platform"},
		"auto mode form":    {previewsArgs("x", "--platform", "eks", "--eks-auto-mode", "pool"), "<nodePool>/<nodeClass>"},
		"auto mode off eks": {previewsArgs("x", "--eks-auto-mode", "pool/class"), "--platform eks"},
	} {
		dir := t.TempDir()
		args := tc.args
		for i, a := range args {
			if a == "x" {
				args[i] = dir
			}
		}
		_, _, err := execSetup(t, nil, args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || exitCode(err) != 2 {
			t.Errorf("%s: %v (exit %d), want a usage error naming %q", name, err, exitCode(err), tc.want)
		}
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Errorf("%s: a refused run wrote %d entries", name, len(entries))
		}
	}
}

// TestSetupPreviewsEKSAutoMode: --eks-auto-mode is recorded and renders
// the chart's Auto Mode pair instead of the node label.
func TestSetupPreviewsEKSAutoMode(t *testing.T) {
	dir := t.TempDir()
	args := previewsArgs(dir, "--platform", "eks", "--ingress-class", "alb", "--eks-auto-mode", "previews/previews-class")
	if _, errOut, err := execSetup(t, nil, args...); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	values := readFile(t, dir, previewsetup.ValuesFile)
	if !strings.Contains(values, "nodePool: previews\n") || !strings.Contains(values, "nodeClass: previews-class\n") ||
		strings.Contains(values, "patchy.bitwisemedia.uk/previews") {
		t.Errorf("values do not select the Auto Mode pair alone:\n%s", values)
	}
	if _, errOut, err := execSetup(t, nil, "setup", "previews", "-o", dir); err != nil ||
		!strings.Contains(errOut, "up to date") {
		t.Errorf("a re-run from answers.yaml changed something: %v\n%s", err, errOut)
	}
}

// TestSetupPreviewsIsHidden: the command stays out of help and the
// generated docs until it asks its questions (W9).
func TestSetupPreviewsIsHidden(t *testing.T) {
	out, _, err := execSetup(t, nil, "setup", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "previews") {
		t.Errorf("setup --help lists previews:\n%s", out)
	}
}
