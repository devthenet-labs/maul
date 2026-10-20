// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
)

// tailscaleArgs are a complete tailscale-recipe answer set writing to dir:
// the desktop's shape, Route53 with an IAM user's key.
func tailscaleArgs(dir string, extra ...string) []string {
	return append([]string{"setup", "previews", "-o", dir, "--platform", "k3d", "--edge", "tailscale",
		"--host-suffix", "acme-previews.dev", "--sign-in-host", "preview-auth.patchy.acme.dev",
		"--dex-issuer", "https://dex.patchy.acme.dev", "--viewer-team", "acme:qa", "--registry", "ghcr.io",
		"--dns", "route53", "--dns-option", "region=us-east-1", "--dns-option", "accessKeyID=AKIAIOSFODNN7EXAMPLE"},
		extra...)
}

// execSetupIn is execSetup with stdin.
func execSetupIn(t *testing.T, stdin io.Reader, args ...string) (string, string, error) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	opts := &Options{Out: out, ErrOut: errOut, Output: "table", NoColor: true,
		promptTerminal: func(io.Reader) bool { return false }}
	root := NewRoot(opts)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetIn(stdin)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// writeCredential writes a credential file with mode perm.
func writeCredential(t *testing.T, value string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dns01")
	if err := os.WriteFile(p, []byte(value), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
	return p
}

// dnsCredential reads the route53 secret access key the DNS Secret file holds.
func dnsCredential(t *testing.T, dir string) string {
	t.Helper()
	var s corev1.Secret
	if err := yaml.Unmarshal([]byte(readFile(t, dir, previewsetup.DNSSecretFile)), &s); err != nil {
		t.Fatal(err)
	}
	return string(s.Data["secret-access-key"])
}

// TestSetupPreviewsTailscale drives the tailscale recipe across runs: the
// credential given in a file is written only into its Secret manifest, a
// re-run without it keeps that file, a new one replaces it without --force,
// and it is printed nowhere and recorded nowhere else.
func TestSetupPreviewsTailscale(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	const first, second = "first-dns01-secret-key-0123456789", "second-dns01-secret-key-9876543210"
	var printed strings.Builder
	run := func(args ...string) string {
		t.Helper()
		out, errOut, err := execSetupIn(t, nil, args...)
		printed.WriteString(out + errOut)
		if err != nil {
			t.Fatalf("%v\n%s%s", err, out, errOut)
		}
		return errOut
	}
	errOut := run(tailscaleArgs(dir, "--dns-credentials-file", writeCredential(t, first+"\n", 0o600))...)
	if !strings.Contains(errOut, "wrote the DNS credential") {
		t.Errorf("no word of the DNS credential written:\n%s", errOut)
	}
	if got := dnsCredential(t, dir); got != first {
		t.Errorf("the Secret holds %q", got)
	}
	for _, rel := range []string{previewsetup.IssuerFile, previewsetup.CertificateFile,
		previewsetup.CertManager.ValuesFile, previewsetup.TailscaleOperator.ValuesFile} {
		if strings.Contains(readFile(t, dir, rel), first) {
			t.Errorf("%s holds the credential", rel)
		}
	}
	values := readFile(t, dir, previewsetup.ValuesFile)
	if !strings.Contains(values, "tailscale.com/expose: \"true\"") || !strings.Contains(values, "forwardedHops: 0") {
		t.Errorf("values do not expose the gateway on the tailnet:\n%s", values)
	}

	// A re-run from answers.yaml alone reads the credential back.
	if errOut := run("setup", "previews", "-o", dir); !strings.Contains(errOut, "up to date") {
		t.Errorf("a re-run without the credential changed something:\n%s", errOut)
	}
	// The device's address, once it appeared, lands in the values.
	run("setup", "previews", "-o", dir, "--tailscale-address", "100.101.12.58", "--force")
	if !strings.Contains(readFile(t, dir, previewsetup.ValuesFile), "- 100.101.12.58") {
		t.Error("--tailscale-address is not a published address")
	}
	// A new credential replaces the old one without --force.
	run("setup", "previews", "-o", dir, "--dns-credentials-file", writeCredential(t, second, 0o600))
	if got := dnsCredential(t, dir); got != second {
		t.Errorf("the Secret holds %q after a new credential", got)
	}
	for _, s := range []string{first, second} {
		if strings.Contains(printed.String(), s) {
			t.Errorf("the credential was printed:\n%s", printed.String())
		}
		if strings.Contains(readFile(t, dir, previewsetup.AnswersFile), s) {
			t.Error("answers.yaml records the credential")
		}
	}
}

// TestSetupPreviewsCredentialFromStdin: a credential piped in is written
// as one read from a file; a file other users can read is warned about.
func TestSetupPreviewsCredentialFromStdin(t *testing.T) {
	dir := t.TempDir()
	_, errOut, err := execSetupIn(t, strings.NewReader("piped-token\n"),
		tailscaleArgs(dir, "--dns-credentials-from-stdin")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if got := dnsCredential(t, dir); got != "piped-token" {
		t.Errorf("the Secret holds %q", got)
	}
	if strings.Contains(errOut, "chmod 600") {
		t.Errorf("warned about stdin:\n%s", errOut)
	}

	dir = t.TempDir()
	_, errOut, err = execSetupIn(t, nil, tailscaleArgs(dir, "--dns-credentials-file",
		writeCredential(t, "open-token", 0o644))...)
	if err != nil || !strings.Contains(errOut, "readable by other users (mode 0644): chmod 600 it") {
		t.Errorf("a world-readable credential file: %v\n%s", err, errOut)
	}
}

// TestSetupPreviewsDNSChangeDropsOptions: answering another solver on a
// re-run drops the last one's options, which it would refuse.
func TestSetupPreviewsDNSChangeDropsOptions(t *testing.T) {
	dir := t.TempDir()
	if _, errOut, err := execSetupIn(t, nil, tailscaleArgs(dir)...); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if _, errOut, err := execSetupIn(t, nil, "setup", "previews", "-o", dir, "--dns", "cloudflare",
		"--force"); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	answers := readFile(t, dir, previewsetup.AnswersFile)
	if strings.Contains(answers, "region") || !strings.Contains(answers, "dns: cloudflare") {
		t.Errorf("answers.yaml after --dns cloudflare:\n%s", answers)
	}
	if !strings.Contains(readFile(t, dir, previewsetup.NextStepsFile), "--from-file=api-token=") {
		t.Error("NEXT-STEPS.md does not say how to create the Cloudflare token's Secret")
	}
}

func TestSetupPreviewsGatewayRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no dns":          {[]string{"--dns", ""}, "answer --dns"},
		"duplicate key":   {[]string{"--dns-option", "region=eu-west-2"}, "given twice"},
		"bare option":     {[]string{"--dns-option", "region"}, "not key=value"},
		"both sources":    {[]string{"--dns-credentials-file", "x", "--dns-credentials-from-stdin"}, "exclusive"},
		"no such file":    {[]string{"--dns-credentials-file", "/nonexistent/dns01"}, "--dns-credentials-file"},
		"ambient + token": {nil, "takes none"},
		"not a file":      {[]string{"--dns-credentials-file", "/"}, "not a regular file"},
		"two-line token":  {[]string{"--dns-credentials-file", "TWO"}, "white space"},
		"public address":  {[]string{"--tailscale-address", "203.0.113.4"}, "--tailscale-address"},
		// Host bits set: refused, never quietly widened to the network.
		"host bits": {[]string{"--edge", "gateway-tls", "--inbound-cidr", "10.0.0.1/8"},
			"--inbound-cidr \"10.0.0.1/8\" is not a CIDR in its network form"},
	} {
		dir := t.TempDir()
		args := tailscaleArgs(dir)
		if name == "ambient + token" { // route53 without accessKeyID: cert-manager's own identity
			args = args[:len(args)-2]
			tc.args = []string{"--dns-credentials-file", writeCredential(t, "tok", 0o600)}
		}
		for i, a := range tc.args {
			if a == "TWO" {
				tc.args[i] = writeCredential(t, "one\ntwo\n", 0o600)
			}
		}
		_, _, err := execSetupIn(t, nil, append(args, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || exitCode(err) != 2 {
			t.Errorf("%s: %v (exit %d), want a usage error naming %q", name, err, exitCode(err), tc.want)
		}
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			t.Errorf("%s: a refused run wrote %d entries", name, len(entries))
		}
	}
}

// awsCredentialsFile is a shared credentials file of two profiles: the
// one the tests read, and one whose secret must never leave the file.
const awsCredentialsFile = `[default]
aws_access_key_id = AKIAOTHERPROFILE0000
aws_secret_access_key = other-profile-secret-never-read

[dns01]
aws_access_key_id = AKIAIOSFODNN7EXAMPLE
aws_secret_access_key = dns01-profile-secret-0123456789
`

// profileArgs are tailscaleArgs without the access key ID, which
// --dns-aws-profile sets.
func profileArgs(dir string, extra ...string) []string {
	args := tailscaleArgs(dir)
	return append(args[:len(args)-2], extra...)
}

// TestSetupPreviewsAWSProfile: --dns-aws-profile reads only that profile
// of a shared credentials file: its secret into the Secret manifest, its
// key ID into the answers and the Issuer, the other profile's secret
// nowhere; from the named file, else $AWS_SHARED_CREDENTIALS_FILE.
func TestSetupPreviewsAWSProfile(t *testing.T) {
	check := func(t *testing.T, dir, printed string) {
		t.Helper()
		if got := dnsCredential(t, dir); got != "dns01-profile-secret-0123456789" {
			t.Errorf("the Secret holds %q", got)
		}
		for _, rel := range []string{previewsetup.AnswersFile, previewsetup.IssuerFile} {
			if !strings.Contains(readFile(t, dir, rel), "AKIAIOSFODNN7EXAMPLE") {
				t.Errorf("%s lacks the profile's access key ID", rel)
			}
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if strings.Contains(string(data), "other-profile-secret") || strings.Contains(string(data), "AKIAOTHER") {
				t.Errorf("%s holds the other profile", path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(printed, "dns01-profile-secret") || strings.Contains(printed, "other-profile-secret") {
			t.Errorf("a secret was printed:\n%s", printed)
		}
	}

	t.Run("named file", func(t *testing.T) {
		dir := t.TempDir()
		out, errOut, err := execSetupIn(t, nil, profileArgs(dir, "--dns-aws-profile", "dns01",
			"--dns-credentials-file", writeCredential(t, awsCredentialsFile, 0o600))...)
		if err != nil {
			t.Fatalf("%v\n%s", err, errOut)
		}
		check(t, dir, out+errOut)
	})
	t.Run("from the environment", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", writeCredential(t, awsCredentialsFile, 0o600))
		out, errOut, err := execSetupIn(t, nil, profileArgs(dir, "--dns-aws-profile", "dns01")...)
		if err != nil {
			t.Fatalf("%v\n%s", err, errOut)
		}
		check(t, dir, out+errOut)
	})
	t.Run("an agreeing --dns-option accessKeyID", func(t *testing.T) {
		dir := t.TempDir()
		_, errOut, err := execSetupIn(t, nil, tailscaleArgs(dir, "--dns-aws-profile", "dns01",
			"--dns-credentials-file", writeCredential(t, awsCredentialsFile, 0o600))...)
		if err != nil {
			t.Fatalf("%v\n%s", err, errOut)
		}
	})

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"another key ID": {[]string{"--dns-option", "accessKeyID=AKIAANOTHERKEY000000", "--dns-aws-profile", "dns01"},
			"is not profile dns01's access key ID"},
		"missing profile": {[]string{"--dns-aws-profile", "prod"}, "no profile prod in the file (it has: default, dns01)"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			args := append(profileArgs(dir, "--dns-credentials-file", writeCredential(t, awsCredentialsFile, 0o600)),
				tc.args...)
			_, _, err := execSetupIn(t, nil, args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "profile-secret") {
				t.Errorf("the error quotes a secret: %v", err)
			}
		})
	}
	t.Run("not route53", func(t *testing.T) {
		dir := t.TempDir()
		args := []string{"setup", "previews", "-o", dir, "--platform", "k3d", "--edge", "tailscale",
			"--host-suffix", "acme-previews.dev", "--sign-in-host", "preview-auth.patchy.acme.dev",
			"--dex-issuer", "https://dex.patchy.acme.dev", "--viewer-team", "acme:qa", "--registry", "ghcr.io",
			"--dns", "cloudflare", "--dns-aws-profile", "dns01",
			"--dns-credentials-file", writeCredential(t, awsCredentialsFile, 0o600)}
		if _, _, err := execSetupIn(t, nil, args...); err == nil ||
			!strings.Contains(err.Error(), "--dns-aws-profile is for --dns route53") {
			t.Fatalf("got %v", err)
		}
	})
}
