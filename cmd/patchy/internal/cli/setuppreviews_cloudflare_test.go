// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
)

const (
	firstTunnel  = "6ff42ae2-765d-4adf-8112-31c55c1551ef"
	secondTunnel = "0b9f2a3c-1111-4222-8333-444455556666"
)

// cloudflareArgs are a complete cloudflare-tunnel answer set writing to
// dir: previews on a zone of their own, a first-level sign-in host.
func cloudflareArgs(dir string, extra ...string) []string {
	return append([]string{"setup", "previews", "-o", dir, "--platform", "k3s", "--edge", "cloudflare-tunnel",
		"--host-suffix", "acme-previews.dev", "--sign-in-host", "preview-auth.acme.dev",
		"--dex-issuer", "https://dex.patchy.acme.dev", "--viewer-team", "acme:qa", "--registry", "ghcr.io"},
		extra...)
}

// tunnelSecret is a tunnel secret unique to id, easy to find where it
// must never be.
func tunnelSecret(id string) string {
	return base64.StdEncoding.EncodeToString([]byte("TUNNEL-SECRET-" + id[:18]))
}

// writeTunnelFile writes tunnel id's credentials file with mode perm.
func writeTunnelFile(t *testing.T, id string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), id+".json")
	body := `{"AccountTag":"0123456789abcdef0123456789abcdef","TunnelSecret":"` + tunnelSecret(id) +
		`","TunnelID":"` + id + `","Endpoint":""}`
	if err := os.WriteFile(p, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
	return p
}

// tunnelInSecret is the credentials file the tunnel Secret file holds.
func tunnelInSecret(t *testing.T, dir string) string {
	t.Helper()
	var s corev1.Secret
	if err := yaml.Unmarshal([]byte(readFile(t, dir, previewsetup.CloudflaredSecretFile)), &s); err != nil {
		t.Fatal(err)
	}
	if s.Namespace != previewsetup.DefaultCloudflaredNamespace || s.Name != previewsetup.CloudflaredSecret {
		t.Errorf("the tunnel Secret is %s/%s", s.Namespace, s.Name)
	}
	return string(s.Data[previewsetup.CloudflaredSecretKey])
}

// TestSetupPreviewsCloudflare drives the cloudflare-tunnel recipe across
// runs: the credentials file is written only into its Secret manifest and
// the tunnel ID is recorded; a re-run without the file reads it back; a
// file for another tunnel moves the config and needs --force for it; and
// leaving the tunnel drops its answers and reports its files stale. The
// tunnel secret is printed nowhere and recorded nowhere else.
func TestSetupPreviewsCloudflare(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	var printed strings.Builder
	run := func(args ...string) (string, error) {
		t.Helper()
		out, errOut, err := execSetupIn(t, nil, args...)
		printed.WriteString(out + errOut)
		return errOut, err
	}
	errOut, err := run(cloudflareArgs(dir, "--tunnel-credentials", writeTunnelFile(t, firstTunnel, 0o644))...)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	checkFirstTunnelRun(t, dir, errOut)

	// A re-run from answers.yaml alone reads the credentials back.
	if errOut, err := run("setup", "previews", "-o", dir); err != nil || !strings.Contains(errOut, "up to date") {
		t.Errorf("a re-run without the credentials changed something: %v\n%s", err, errOut)
	}

	// Another tunnel's file moves the config: refused without --force,
	// written with it.
	second := writeTunnelFile(t, secondTunnel, 0o600)
	if _, err := run("setup", "previews", "-o", dir, "--tunnel-credentials", second); err == nil {
		t.Error("a new tunnel replaced cloudflared's config without --force")
	}
	if errOut, err := run("setup", "previews", "-o", dir, "--tunnel-credentials", second, "--force"); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if !strings.Contains(tunnelInSecret(t, dir), secondTunnel) ||
		!strings.Contains(readFile(t, dir, previewsetup.CloudflaredFile), "tunnel: "+secondTunnel) {
		t.Error("the new tunnel is not the one written")
	}

	// Leaving the tunnel drops its answers; its files are stale, not deleted.
	errOut, err = run("setup", "previews", "-o", dir, "--edge", "existing", "--ingress-class", "traefik", "--force")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if a := readFile(t, dir, previewsetup.AnswersFile); strings.Contains(a, "cloudflare") {
		t.Errorf("answers.yaml keeps the tunnel's answers:\n%s", a)
	}
	if !strings.Contains(errOut, previewsetup.CloudflaredFile+" is no longer generated") {
		t.Errorf("cloudflared's manifest is not reported stale:\n%s", errOut)
	}

	checkTunnelSecretNowhere(t, dir, printed.String(), firstTunnel, secondTunnel)
}

// checkFirstTunnelRun: the first run warned of the open file, wrote the
// credentials into their Secret, recorded the tunnel and runs it at the
// pinned image behind forwardedHops 1.
func checkFirstTunnelRun(t *testing.T, dir, errOut string) {
	t.Helper()
	if !strings.Contains(errOut, "readable by other users (mode 0644): chmod 600 it") ||
		!strings.Contains(errOut, "wrote the tunnel credentials") {
		t.Errorf("no warning of the open file, or no word of the credentials written:\n%s", errOut)
	}
	if got := tunnelInSecret(t, dir); !strings.Contains(got, tunnelSecret(firstTunnel)) {
		t.Errorf("the Secret holds %q", got)
	}
	if !strings.Contains(readFile(t, dir, previewsetup.AnswersFile), "tunnelID: "+firstTunnel) {
		t.Error("answers.yaml does not record the tunnel")
	}
	manifest := readFile(t, dir, previewsetup.CloudflaredFile)
	if !strings.Contains(manifest, "tunnel: "+firstTunnel) ||
		!strings.Contains(manifest, previewsetup.Cloudflared.Ref()) {
		t.Errorf("cloudflared's manifest does not run the tunnel at the pinned image:\n%s", manifest)
	}
	if v := readFile(t, dir, previewsetup.ValuesFile); !strings.Contains(v, "forwardedHops: 1") {
		t.Errorf("values do not trust Cloudflare's one hop:\n%s", v)
	}
}

// checkTunnelSecretNowhere: no tunnel's secret was printed or reached a
// file that is not its Secret.
func checkTunnelSecretNowhere(t *testing.T, dir, printed string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if strings.Contains(printed, tunnelSecret(id)) {
			t.Errorf("the tunnel secret was printed:\n%s", printed)
		}
		for _, rel := range []string{previewsetup.AnswersFile, previewsetup.ValuesFile, previewsetup.NextStepsFile,
			previewsetup.CloudflaredFile} {
			if strings.Contains(readFile(t, dir, rel), tunnelSecret(id)) {
				t.Errorf("%s holds the tunnel secret", rel)
			}
		}
	}
}

func TestSetupPreviewsCloudflareRefusals(t *testing.T) {
	pem := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(pem, []byte("-----BEGIN ARGO TUNNEL TOKEN-----\nx\n-----END ARGO TUNNEL TOKEN-----\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"cert.pem":     {[]string{"--tunnel-credentials", pem}, "cert.pem"},
		"no such file": {[]string{"--tunnel-credentials", "/nonexistent/tunnel.json"}, "--tunnel-credentials"},
		"not a file":   {[]string{"--tunnel-credentials", "/"}, "not a regular file"},
		// Universal SSL covers the first level only (design §2.3).
		"deep suffix": {[]string{"--tunnel-credentials", "OK", "--host-suffix", "preview.acme-apps.dev"},
			"--cloudflare-acm"},
		"deep sign-in": {[]string{"--tunnel-credentials", "OK", "--sign-in-host", "preview-auth.patchy.acme.dev"},
			"--cloudflare-acm"},
		// One domain is refused whatever the certificate (owner decision Q3).
		"one domain": {[]string{"--tunnel-credentials", "OK", "--cloudflare-acm", "--host-suffix",
			"previews.acme.dev"}, "share the registrable domain acme.dev"},
		"release namespace": {[]string{"--tunnel-credentials", "OK", "--cloudflared-namespace", "patchy"},
			"is the release namespace"},
		// The manifest relabels its namespace and rolling back deletes it.
		"dex namespace": {[]string{"--tunnel-credentials", "OK", "--cloudflared-namespace", "dex"},
			"is Dex's namespace"},
		"kube-system": {[]string{"--tunnel-credentials", "OK", "--cloudflared-namespace", "kube-system"},
			"one of Kubernetes' own namespaces"},
		// A tunnel has no load balancer: its allowlist would be recorded
		// in answers.yaml and silently not applied.
		"inbound cidr": {[]string{"--tunnel-credentials", "OK", "--inbound-cidr", "203.0.113.0/24"},
			"--inbound-cidr is for --edge gateway-tls"},
		"load balancer class": {[]string{"--tunnel-credentials", "OK", "--load-balancer-class", "metallb"},
			"--load-balancer-class is for --edge gateway-tls"},
	} {
		dir := t.TempDir()
		for i, a := range tc.args {
			if a == "OK" {
				tc.args[i] = writeTunnelFile(t, firstTunnel, 0o600)
			}
		}
		_, _, err := execSetupIn(t, nil, cloudflareArgs(dir, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || exitCode(err) != 2 {
			t.Errorf("%s: %v (exit %d), want a usage error naming %q", name, err, exitCode(err), tc.want)
		}
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Errorf("%s: a refused run wrote %d entries", name, len(entries))
		}
	}
	// A deep name is allowed when the zones have Advanced Certificate Manager.
	dir := t.TempDir()
	args := cloudflareArgs(dir, "--tunnel-credentials", writeTunnelFile(t, firstTunnel, 0o600), "--host-suffix",
		"preview.acme-apps.dev", "--cloudflare-acm")
	if _, errOut, err := execSetupIn(t, nil, args...); err != nil {
		t.Errorf("--cloudflare-acm: %v\n%s", err, errOut)
	}
}
