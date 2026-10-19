// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/tailnet"
)

const (
	oauthClientID     = "kOAUTH0SENTINEL"
	oauthClientSecret = "tskey-client-kOAUTH0SENTINEL-OAUTH-SECRET-SENTINEL"
)

// writeOAuthDir writes an OAuth client directory, the two files with mode
// perm.
func writeOAuthDir(t *testing.T, id, secret string, perm os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tailscale-oauth")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"client_id": id, "client_secret": secret} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v+"\n"), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, perm); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// oauthInSecret is the OAuth Secret file's data.
func oauthInSecret(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	var s corev1.Secret
	if err := yaml.Unmarshal([]byte(readFile(t, dir, previewsetup.TailscaleOAuthFile)), &s); err != nil {
		t.Fatal(err)
	}
	if s.Namespace != "tailscale" || s.Name != "operator-oauth" {
		t.Errorf("the OAuth Secret is %s/%s", s.Namespace, s.Name)
	}
	return s.Data
}

// TestSetupPreviewsTailscaleOAuth: --tailscale-oauth-dir writes the OAuth
// client only into the operator's Secret manifest (0600), never printed or
// recorded; a re-run without it reads it back; a new client needs no
// --force; and the operator already running leaves the file stale.
func TestSetupPreviewsTailscaleOAuth(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	var printed strings.Builder
	run := func(args ...string) string {
		t.Helper()
		out, errOut, err := execSetupIn(t, nil, args...)
		printed.WriteString(out + errOut)
		if err != nil {
			t.Fatalf("%v\n%s", err, errOut)
		}
		return errOut
	}
	errOut := run(tailscaleArgs(dir, "--tailscale-oauth-dir",
		writeOAuthDir(t, oauthClientID, oauthClientSecret, 0o644))...)
	if !strings.Contains(errOut, "readable by other users") {
		t.Errorf("no warning for world-readable OAuth files:\n%s", errOut)
	}
	if !strings.Contains(errOut, "wrote the Tailscale operator's OAuth client into") {
		t.Errorf("no note of the OAuth file:\n%s", errOut)
	}
	data := oauthInSecret(t, dir)
	if string(data["client_id"]) != oauthClientID || string(data["client_secret"]) != oauthClientSecret {
		t.Errorf("the Secret holds %q / %q", data["client_id"], data["client_secret"])
	}
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(previewsetup.TailscaleOAuthFile)))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the OAuth file: %v, mode %v", err, info.Mode())
	}
	next := readFile(t, dir, previewsetup.NextStepsFile)
	if !strings.Contains(next, "kubectl apply -f "+previewsetup.TailscaleOAuthFile) {
		t.Error("NEXT-STEPS.md does not apply the OAuth file")
	}

	if errOut := run("setup", "previews", "-o", dir); !strings.Contains(errOut, "up to date") {
		t.Errorf("a re-run without the directory changed something:\n%s", errOut)
	}
	run("setup", "previews", "-o", dir, "--tailscale-oauth-dir",
		writeOAuthDir(t, "kNEWER0CLIENT", "tskey-client-kNEWER0CLIENT-OAUTH-SECRET-SENTINEL", 0o600))
	if got := string(oauthInSecret(t, dir)["client_id"]); got != "kNEWER0CLIENT" {
		t.Errorf("a new client given: the Secret holds %q", got)
	}

	errOut = run("setup", "previews", "-o", dir, "--tailscale-operator-installed", "--force")
	if !strings.Contains(errOut, previewsetup.TailscaleOAuthFile+" is no longer generated") {
		t.Errorf("the OAuth file is not reported stale once the operator runs:\n%s", errOut)
	}
	for _, f := range []string{previewsetup.AnswersFile, previewsetup.NextStepsFile, previewsetup.ValuesFile} {
		if strings.Contains(readFile(t, dir, f), "OAUTH-SECRET-SENTINEL") {
			t.Errorf("%s holds the client secret", f)
		}
	}
	if strings.Contains(printed.String(), "OAUTH-SECRET-SENTINEL") || strings.Contains(printed.String(), oauthClientID) {
		t.Error("the OAuth client was printed")
	}
}

func TestSetupPreviewsTailscaleOAuthRefusals(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(missing, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no such directory": {[]string{"--tailscale-oauth-dir", "/nonexistent/oauth"}, "--tailscale-oauth-dir"},
		"no files":          {[]string{"--tailscale-oauth-dir", missing}, "--tailscale-oauth-dir"},
		"an auth key": {[]string{"--tailscale-oauth-dir", writeOAuthDir(t, oauthClientID,
			"tskey-auth-kOAUTH0SENTINEL-OAUTH-SECRET-SENTINEL", 0o600)}, "another kind of key"},
		"swapped": {[]string{"--tailscale-oauth-dir", writeOAuthDir(t, oauthClientSecret, oauthClientID, 0o600)},
			"swapped"},
		"operator installed": {[]string{"--tailscale-operator-installed", "--tailscale-oauth-dir",
			writeOAuthDir(t, oauthClientID, oauthClientSecret, 0o600)}, "--tailscale-operator-installed"},
		"another edge": {[]string{"--edge", "gateway-tls", "--tailscale-oauth-dir",
			writeOAuthDir(t, oauthClientID, oauthClientSecret, 0o600)}, "--edge tailscale"},
	} {
		dir := t.TempDir()
		out, errOut, err := execSetupIn(t, nil, tailscaleArgs(dir, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || exitCode(err) != 2 {
			t.Errorf("%s: %v (exit %d), want a usage error naming %q", name, err, exitCode(err), tc.want)
		}
		if err != nil && strings.Contains(err.Error()+out+errOut, "SENTINEL") {
			t.Errorf("%s: the refusal quotes the client: %v", name, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Errorf("%s: a refused run wrote %d entries", name, len(entries))
		}
	}
}

// tailnetWith is a Running client's status holding the previews' device
// at addr.
func tailnetWith(addr string) tailnet.StatusFunc {
	return func(context.Context) ([]byte, error) {
		return []byte(`{"BackendState": "Running", "Peer": {"nodekey:1": {"HostName": "patchy-previews",
			"DNSName": "patchy-previews.tail1234.ts.net.", "TailscaleIPs": ["` + addr + `"],
			"Tags": ["tag:patchy-previews"], "Online": true}}}`), nil
	}
}

// TestSetupPreviewsDetectTailnet: with --detect, the tailscale recipe's
// device is found through this machine's Tailscale client, its address
// filled as --tailscale-address (so the values publish it without a second
// run) and the exact DNS records printed; a device not there yet is said,
// and fills nothing.
func TestSetupPreviewsDetectTailnet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	calls := 0
	env := guardedEnv(t, detectWorld(), &calls)
	args := []string{"setup", "previews", "-o", dir, "--detect", "--edge", "tailscale", "--dns", "route53",
		"--dns-option", "region=us-east-1", "--viewer-team", "acme:qa", "--registry", "ghcr.io",
		"--dex-issuer", "https://dex.other.dev"}
	errOut, err := execPreviewsTailnet(t, false, env, tailnetWith("100.101.12.58"), "", args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	a, err := previewsetup.ParseAnswers([]byte(readFile(t, dir, previewsetup.AnswersFile)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"patchy: detected: tailnet device: patchy-previews.tail1234.ts.net (tag:patchy-previews): 100.101.12.58",
		"patchy: detected answer: --tailscale-address 100.101.12.58",
		"patchy: detected: DNS record to create: *." + a.HostSuffix + ". A 100.101.12.58",
		"patchy: detected: DNS record to create: " + a.SignInHost + ". A 100.101.12.58",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("--detect did not say %q:\n%s", want, errOut)
		}
	}
	if !slices.Equal(a.Tailscale.Addresses, []string{"100.101.12.58"}) {
		t.Errorf("answers.yaml addresses = %v", a.Tailscale.Addresses)
	}
	if !strings.Contains(readFile(t, dir, previewsetup.ValuesFile), "- 100.101.12.58") {
		t.Error("the values do not publish the device's address")
	}

	other := filepath.Join(t.TempDir(), "patchy-previews")
	args[3] = other
	errOut, err = execPreviews(t, false, guardedEnv(t, detectWorld(), &calls), "", args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, "tailnet device: not found: this machine's Tailscale client could not be asked") ||
		strings.Contains(errOut, "--tailscale-address") {
		t.Errorf("no client:\n%s", errOut)
	}
}
