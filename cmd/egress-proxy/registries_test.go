// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/egressproxy"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

const testRegistries = `[
 {"name":"art-npm","ecosystem":"npm","url":"https://npm.reg.example/api/npm/","auth":{"type":"bearer"},
  "healthPackage":"left-pad"},
 {"name":"art-go","ecosystem":"go","url":"https://go.reg.example/api/go","auth":{"type":"basic"},
  "privateModules":["example.com/acme/"],"publicChecksumLookups":false},
 {"name":"spare","ecosystem":"pypi","url":"https://pypi.reg.example/simple/","auth":{"type":"none"}}]`

const registryProfiles = `[{"name":"web","port":8095,"repositories":["acme/web"],"registries":[
  {"name":"art-npm","ecosystem":"npm","hosts":["npm.reg.example"],"packagePolicy":"registry"},
  {"name":"art-go","ecosystem":"go","hosts":["go.reg.example"],"packages":["example.com/acme/"],
   "privateModules":["example.com/acme/"]}]},
 {"name":"go","route":"go","port":8090,"repositories":["acme/api"],"modules":["golang.org/x/"]}]`

// fakeEnv replaces getenv for one test.
func fakeEnv(t *testing.T, env map[string]string) {
	t.Helper()
	old := getenv
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = old })
}

// TestConfigureRegistries: the registries reach the engine with the
// always-denied ranges (--always-deny-cidrs and the Kubernetes Service IP),
// the profiles' forge hosts as redirect denials, the caps and the poll
// interval; PATCHY_EGRESS_REGISTRIES is read when the flag is not set.
func TestConfigureRegistries(t *testing.T) {
	fakeEnv(t, map[string]string{"KUBERNETES_SERVICE_HOST": "10.100.0.1"})
	s, err := configure(serveOpts(t, "--network-profiles", registryProfiles, "--registries", testRegistries,
		"--always-deny-cidrs", "172.20.0.10/32", "--max-artifact-bytes", "1048576",
		"--credential-poll-interval", "10s", "--registry-credentials-dir", "/creds"))
	if err != nil {
		t.Fatal(err)
	}
	e := s.engine
	if e.Registries.Len() != 3 || e.MaxArtifactBytes != 1<<20 || e.SelfCheckInterval != 10*time.Second ||
		s.pollInterval != 10*time.Second || s.credentialsDir != "/creds" {
		t.Errorf("engine = %+v", e)
	}
	want := []netip.Prefix{netip.MustParsePrefix("172.20.0.10/32"), netip.MustParsePrefix("10.100.0.1/32")}
	if len(e.AlwaysDeny) != 2 || e.AlwaysDeny[0] != want[0] || e.AlwaysDeny[1] != want[1] {
		t.Errorf("always deny = %v, want %v", e.AlwaysDeny, want)
	}
	if len(e.DenyHosts) != 1 || e.DenyHosts[0] != netprofile.DefaultHost {
		t.Errorf("deny hosts = %v", e.DenyHosts)
	}
	// --deny-hosts (the broker's base-URL overrides, as the chart stamps
	// them) join the forge hosts, lowercased, once each.
	s, err = configure(serveOpts(t, "--network-profiles", registryProfiles, "--registries", testRegistries,
		"--deny-hosts", "Gateway.Example., github.com,bedrock.corp.example"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.engine.DenyHosts, ","); got != "github.com,gateway.example,bedrock.corp.example" {
		t.Errorf("deny hosts with --deny-hosts = %s", got)
	}
	fakeEnv(t, map[string]string{registriesEnv: testRegistries})
	s, err = configure(serveOpts(t, "--network-profiles", registryProfiles))
	if err != nil || s.engine.Registries.Len() != 3 {
		t.Errorf("registries from %s = %v, %v", registriesEnv, s.engine.Registries.Len(), err)
	}
	// Off by default: no registries, no registry route.
	fakeEnv(t, nil)
	s, err = configure(serveOpts(t, "--network-profiles", twoProfiles))
	if err != nil || s.engine.Registries.Len() != 0 || len(s.engine.AlwaysDeny) != 0 {
		t.Errorf("default = %+v, %v", s.engine, err)
	}
}

// TestConfigureRegistriesRefuses: every startup refusal of §6.2.
func TestConfigureRegistriesRefuses(t *testing.T) {
	internal := `[{"name":"nexus","ecosystem":"npm","url":"https://nexus.corp.internal:8443/npm/",` +
		`"auth":{"type":"none"},"internal":{"cidrs":["10.100.0.0/24"]}}]`
	withCA := `[{"name":"art-npm","ecosystem":"npm","url":"https://npm.reg.example/api/npm/",` +
		`"auth":{"type":"bearer"},"ca":true}]`
	caDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(caDir, "art-npm.pem"), []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		env     map[string]string
		args    []string
		wantErr string
	}{
		{"bad json", nil, []string{"--registries", "{"}, "--registries"},
		{"unknown field", nil, []string{"--registries", `[{"name":"x","credential":"file"}]`}, "unknown field"},
		{"grant without registry", nil, []string{"--network-profiles", registryProfiles}, "names no configured"},
		{"denied host", nil, []string{"--registries", `[{"name":"gh","ecosystem":"npm",` +
			`"url":"https://npm.pkg.github.com/","auth":{"type":"none"}}]`}, "deny list"},
		{"go without checksum choice", nil, []string{"--registries", `[{"name":"g","ecosystem":"go",` +
			`"url":"https://go.reg.example/","auth":{"type":"none"}}]`}, "publicChecksumLookups"},
		{"kubernetes service ip", map[string]string{"KUBERNETES_SERVICE_HOST": "10.100.0.1"},
			[]string{"--registries", internal}, "always-denied"},
		{"always deny cidr", nil, []string{"--registries", internal, "--always-deny-cidrs", "10.100.0.10/32"},
			"always-denied"},
		{"bad always deny", nil, []string{"--always-deny-cidrs", "nope"}, "--always-deny-cidrs"},
		{"deny host not a host", nil, []string{"--deny-hosts", "https://gateway.example/"}, "--deny-hosts"},
		{"registry on a deny host", nil, []string{"--deny-hosts", "reg.example", "--registries",
			`[{"name":"n","ecosystem":"npm","url":"https://npm.reg.example/","auth":{"type":"none"}}]`},
			"deny list"},
		{"redirect pattern over a deny host", nil, []string{"--deny-hosts", "gw.cdn.acme.example", "--registries",
			`[{"name":"n","ecosystem":"npm","url":"https://npm.reg.example/","auth":{"type":"none"},` +
				`"redirectHosts":["*.cdn.acme.example"]}]`}, "overlaps"},
		{"missing CA", nil, []string{"--registries", withCA, "--registry-ca-dir", t.TempDir()}, "CA bundle"},
		{"bad CA", nil, []string{"--registries", withCA, "--registry-ca-dir", caDir}, "no PEM certificate"},
		{"artifact cap", nil, []string{"--max-artifact-bytes", "0"}, "--max-artifact-bytes"},
		{"artifact cap over 4 GiB", nil, []string{"--max-artifact-bytes", "4294967297"}, "--max-artifact-bytes"},
		{"poll interval", nil, []string{"--credential-poll-interval", "1s"}, "--credential-poll-interval"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeEnv(t, tt.env)
			args := tt.args
			if !strings.Contains(strings.Join(args, " "), "--network-profiles") {
				args = append([]string{"--network-profiles", twoProfiles}, args...)
			}
			_, err := configure(serveOpts(t, args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("configure = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestConfigureRegistryCA: a registry's CA bundle is loaded into its own
// pool.
func TestConfigureRegistryCA(t *testing.T) {
	fakeEnv(t, nil)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "art-npm.pem"), testCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	withCA := `[{"name":"art-npm","ecosystem":"npm","url":"https://npm.reg.example/api/npm/",` +
		`"auth":{"type":"bearer"},"ca":true}]`
	s, err := configure(serveOpts(t, "--network-profiles", twoProfiles, "--registries", withCA,
		"--registry-ca-dir", dir))
	if err != nil {
		t.Fatal(err)
	}
	if s.engine.RegistryCAs["art-npm"] == nil {
		t.Error("the CA bundle was not loaded")
	}
}

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "corp root"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestBuildSources: no login, no source; file registries are read at once
// (a missing file is unavailable, not an error; an unreadable one refuses
// the start); a codeartifact registry gets the minter.
func TestBuildSources(t *testing.T) {
	fakeEnv(t, nil)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	open := `[{"name":"spare","ecosystem":"pypi","url":"https://pypi.reg.example/simple/","auth":{"type":"none"}}]`
	s, err := configure(serveOpts(t, "--network-profiles", twoProfiles, "--registries", open))
	if err != nil {
		t.Fatal(err)
	}
	if src, err := buildSources(t.Context(), s, discard()); src != nil || err != nil {
		t.Errorf("no logins = %v, %v; want no source", src, err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "art-npm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "art-npm", "token"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ca := `{"name":"ca-npm","ecosystem":"npm",` +
		`"url":"https://patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com/npm/npm-store/",` +
		`"auth":{"type":"codeartifact","codeartifact":{"domain":"patchy-gate","domainOwner":"377946145366",` +
		`"region":"us-east-1"}}}`
	regs := strings.TrimSuffix(testRegistries, "]") + "," + ca + "]"
	s, err = configure(serveOpts(t, "--network-profiles", registryProfiles, "--registries", regs,
		"--registry-credentials-dir", dir))
	if err != nil {
		t.Fatal(err)
	}
	src, err := buildSources(t.Context(), s, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer src.close()
	if c, err := src.Get("art-npm"); err != nil || c.Authorization() != "Bearer tok" {
		t.Errorf("art-npm = %v", err)
	}
	if _, err := src.Get("art-go"); !errors.Is(err, egressproxy.ErrCredentialUnavailable) {
		t.Errorf("art-go without files = %v, want unavailable", err)
	}
	if _, err := src.Get("spare"); !errors.Is(err, egressproxy.ErrCredentialUnavailable) {
		t.Errorf("a registry without a login has a source: %v", err)
	}
	if src.minter == nil || src.by["ca-npm"] == nil {
		t.Error("the codeartifact registry has no minter")
	}
	if _, err := src.Get("ca-npm"); !errors.Is(err, egressproxy.ErrCredentialUnavailable) {
		t.Errorf("ca-npm before its first mint = %v", err)
	}
	// A credential path that exists and cannot be read refuses the start.
	if err := os.Remove(filepath.Join(dir, "art-npm", "token")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "art-npm", "token"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := buildSources(t.Context(), s, discard()); err == nil {
		t.Error("an unreadable credential file was accepted")
	}
}

// probeRoot resolves args through the probe flag surface.
func probeRoot(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("egress-proxy", "test", opts)
	cmd := newProbeCmd(opts)
	root.AddCommand(cmd)
	root.SetArgs(append([]string{"probe"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root
}

// TestProbe: probe sends one request through the engine and prints its
// outcome and credential kind; with no credential loaded it reports
// credential_unavailable without reaching anything, and fails.
func TestProbe(t *testing.T) {
	fakeEnv(t, nil)
	var out bytes.Buffer
	root := probeRoot(t, "--network-profiles", registryProfiles, "--registries", testRegistries,
		"--registry", "art-npm", "--registry-credentials-dir", t.TempDir(), "--probe-credential-wait", "0s")
	root.SetOut(&out)
	err := root.Execute()
	if !errors.Is(err, errProbeFailed) {
		t.Errorf("probe = %v, want errProbeFailed", err)
	}
	want := "registry=art-npm profile=web path=/r/art-npm/left-pad status=503 upstream_status=0 " +
		"outcome=credential_unavailable reason= credential=none\n"
	if out.String() != want {
		t.Errorf("probe printed %q, want %q", out.String(), want)
	}
	for _, tt := range []struct {
		args    []string
		wantErr string
	}{
		{nil, "--registry is required"},
		{[]string{"--registry", "nope"}, "no such registry"},
		{[]string{"--registry", "spare"}, "no profile grants it"},
		{[]string{"--registry", "art-go"}, "pass --package"},
		{[]string{"--registry", "art-npm", "--package", "Bad Name"}, "--package"},
	} {
		args := append([]string{"--network-profiles", registryProfiles, "--registries", testRegistries}, tt.args...)
		root := probeRoot(t, args...)
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("probe %v = %v, want %q", tt.args, err, tt.wantErr)
		}
	}
}

// TestReportProbe: the line the runbook reads. A forwarded 200 prints
// outcome=ok with the credential's kind and succeeds; anything else prints
// the engine's outcome and fails.
func TestReportProbe(t *testing.T) {
	for _, tt := range []struct {
		name    string
		res     egressproxy.ProbeResult
		want    string
		wantErr error
	}{
		{"static", egressproxy.ProbeResult{Status: 200, UpstreamStatus: 200, Outcome: egressproxy.OutcomeProxied,
			Credential: "static"},
			"status=200 upstream_status=200 outcome=ok reason= credential=static\n", nil},
		{"codeartifact", egressproxy.ProbeResult{Status: 200, UpstreamStatus: 200,
			Outcome: egressproxy.OutcomeProxied, Credential: "codeartifact"},
			"status=200 upstream_status=200 outcome=ok reason= credential=codeartifact\n", nil},
		{"not found", egressproxy.ProbeResult{Status: 404, UpstreamStatus: 404, Outcome: egressproxy.OutcomeProxied,
			Reason: egressproxy.ReasonNotFound, Credential: "static"},
			"status=404 upstream_status=404 outcome=proxied reason=not_found credential=static\n", errProbeFailed},
		{"upstream auth", egressproxy.ProbeResult{Status: 502, UpstreamStatus: 401,
			Outcome: egressproxy.OutcomeUpstreamAuth, Credential: "codeartifact"},
			"status=502 upstream_status=401 outcome=upstream_auth reason= credential=codeartifact\n", errProbeFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := reportProbe(&out, "art-npm", "web", "/r/art-npm/left-pad", tt.res)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("reportProbe = %v, want %v", err, tt.wantErr)
			}
			want := "registry=art-npm profile=web path=/r/art-npm/left-pad " + tt.want
			if out.String() != want {
				t.Errorf("reportProbe printed %q, want %q", out.String(), want)
			}
		})
	}
}

// TestProbePath: the first request each ecosystem's client makes.
func TestProbePath(t *testing.T) {
	for _, tt := range []struct {
		eco, pkg, want string
	}{
		{netprofile.EcosystemNPM, "left-pad", "/r/r/left-pad"},
		{netprofile.EcosystemNPM, "@types/node", "/r/r/@types%2fnode"},
		{netprofile.EcosystemPyPI, "Charset_Normalizer", "/r/r/simple/charset-normalizer/"},
		{netprofile.EcosystemGo, "github.com/BurntSushi/toml", "/r/r/github.com/%21burnt%21sushi/toml/@v/list"},
	} {
		got, err := probePath(netprofile.Registry{Name: "r", Ecosystem: tt.eco}, tt.pkg)
		if err != nil || got != tt.want {
			t.Errorf("probePath(%s, %s) = %q %v, want %q", tt.eco, tt.pkg, got, err, tt.want)
		}
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
