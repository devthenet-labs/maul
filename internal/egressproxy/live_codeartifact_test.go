// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy_test

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/egressproxy"
	"github.com/devthenet-labs/maul/internal/egressproxy/awsca"
	"github.com/devthenet-labs/maul/internal/netprofile"
)

// liveCodeArtifactEnv names a real CodeArtifact repository,
// <domain>/<owner>/<region>/<repo>, whose npm and pypi endpoints the
// pre-gate check runs the real npm and pip against, with a token the
// engine mints from the developer's own AWS credentials (AWS_PROFILE).
const liveCodeArtifactEnv = "MAUL_LIVE_CODEARTIFACT"

// TestLiveCodeArtifact is TestLiveRegistry's npm install and pip download
// against real CodeArtifact (§10.6 of the step-2 spec): the proxy mints
// the token, the clients never see it, and the lockfile keeps
// CodeArtifact's URLs. It records nothing about redirects; the audit log
// it prints does (redirects=, redirect_host=). Opt-in.
func TestLiveCodeArtifact(t *testing.T) {
	spec := os.Getenv(liveCodeArtifactEnv)
	if spec == "" {
		t.Skipf("set %s=<domain>/<owner>/<region>/<repo> to run against real CodeArtifact", liveCodeArtifactEnv)
	}
	parts := strings.Split(spec, "/")
	if len(parts) != 4 {
		t.Fatalf("%s=%q is not <domain>/<owner>/<region>/<repo>", liveCodeArtifactEnv, spec)
	}
	domain, owner, region, repo := parts[0], parts[1], parts[2], parts[3]
	host := domain + "-" + owner + ".d.codeartifact." + region + ".amazonaws.com"
	auth := netprofile.Auth{Type: netprofile.AuthCodeArtifact,
		CodeArtifact: &netprofile.CodeArtifact{Domain: domain, DomainOwner: owner, Region: region,
			DurationSeconds: netprofile.MinCodeArtifactDuration}}
	regs := []netprofile.Registry{
		{Name: "ca-npm", Ecosystem: netprofile.EcosystemNPM, URL: "https://" + host + "/npm/" + repo + "/", Auth: auth},
		{Name: "ca-pypi", Ecosystem: netprofile.EcosystemPyPI, URL: "https://" + host + "/pypi/" + repo + "/simple/",
			Auth: auth},
	}
	set, err := netprofile.NewRegistrySet(regs, netprofile.RegistryContext{})
	if err != nil {
		t.Fatal(err)
	}
	grants := make([]netprofile.Grant, 0, len(regs))
	minted := make([]awsca.Registry, 0, len(regs))
	for _, r := range regs {
		grants = append(grants, netprofile.Grant{Name: r.Name, Ecosystem: r.Ecosystem, Hosts: r.Hosts(),
			PackagePolicy: netprofile.PackagePolicyRegistry})
		m, err := awsca.FromNetprofile(r)
		if err != nil {
			t.Fatal(err)
		}
		minted = append(minted, m)
	}
	profiles, err := netprofile.NewSet([]netprofile.Profile{{Name: "web", Port: 8091, Host: "github.com",
		Repositories: []string{"devthenet-labs/maul-polyglot-demo"}, Registries: grants}})
	if err != nil {
		t.Fatal(err)
	}
	clients := awsca.NewClients(egressproxy.AddrPolicy{})
	minter, err := awsca.New(minted, func(region string) (awsca.API, error) {
		cfg, err := clients.LoadConfig(t.Context(), region)
		if err != nil {
			return nil, err
		}
		return awsca.NewAPI(cfg), nil
	}, awsca.Options{Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), AddressClass: clients.LastAddressClass})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = minter.Run(t.Context()) }()
	deadline := time.Now().Add(time.Minute)
	for _, err := minter.Get("ca-npm"); err != nil; _, err = minter.Get("ca-npm") {
		if time.Now().After(deadline) {
			t.Fatalf("no CodeArtifact token within a minute: %v", err)
		}
		time.Sleep(time.Second)
	}
	var audit strings.Builder
	srv, err := egressproxy.New(egressproxy.Config{Profiles: profiles, Registries: set, Credentials: minter},
		slog.New(slog.NewJSONHandler(&audit, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler, _ := srv.Handler("web")
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	tmp := t.TempDir()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"demo","version":"1.0.0","private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(env []string, name string, args ...string) string {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s %v: %v\n%s", name, args, err, out)
		}
		return string(out)
	}
	outs := []string{
		run([]string{"NPM_CONFIG_REGISTRY=" + ts.URL + "/r/ca-npm/", "NPM_CONFIG_REPLACE_REGISTRY_HOST=always",
			"NPM_CONFIG_AUDIT=false", "NPM_CONFIG_FUND=false", "NPM_CONFIG_UPDATE_NOTIFIER=false",
			"NPM_CONFIG_IGNORE_SCRIPTS=true", "NPM_CONFIG_CACHE=" + filepath.Join(tmp, "npm")},
			"npm", "install", "--loglevel", "silly", "left-pad", "@types/node"),
		run([]string{"PIP_CONFIG_FILE=/dev/null", "PIP_INDEX_URL=" + ts.URL + "/r/ca-pypi/simple/",
			"PIP_EXTRA_INDEX_URL= ", "PIP_FIND_LINKS= ", "PIP_TRUSTED_HOST=" + strings.TrimPrefix(ts.URL, "http://"),
			"PIP_ONLY_BINARY=:all:", "PIP_NO_INPUT=1", "PIP_DISABLE_PIP_VERSION_CHECK=1",
			"PIP_CACHE_DIR=" + filepath.Join(tmp, "pip")},
			"python3", "-m", "pip", "download", "-vvv", "--no-deps", "-d", dir, "six"),
	}
	lock, _ := os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if !strings.Contains(string(lock), `"resolved": "https://`+host+`/npm/`+repo+`/`) ||
		strings.Contains(string(lock), "127.0.0.1") {
		t.Errorf("lockfile resolved URLs are not CodeArtifact's:\n%s", lock)
	}
	if strings.Contains(audit.String(), "eyJ") || strings.Contains(strings.Join(outs, ""), "eyJ") {
		t.Error("a token-shaped string reached the audit log or a client's output")
	}
	t.Logf("audit log:\n%s", audit.String())
}
