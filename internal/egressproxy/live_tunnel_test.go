// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/provider"
)

// liveTunnelEnv opts into TestLiveTunnel, which runs the real package
// managers found on PATH through the CONNECT tunnel against the real public
// registries (mise run live-tunnel). Never in CI.
const liveTunnelEnv = "MAUL_LIVE_TUNNEL"

// liveTunnelTimeout bounds one client run; a refused install retries, and
// npm's retries back off for tens of seconds.
const liveTunnelTimeout = 5 * time.Minute

// liveDecoyHost is the one host of a refused run's profile when the client
// needs a single host: a valid, never-dialled name, so the client's own
// registry is not allowlisted.
const liveDecoyHost = "decoy.example.com"

// Measurement caps: high enough that no fixture install is cut, so the
// recorded bytes are what the client really sends (§2.5 of the step-3 spec
// sets its upload defaults from them). The defaults are judged separately
// against the measured numbers.
const (
	liveMaxUpload    = 256 << 20
	liveSourceBudget = 1 << 30
)

// The provisional upload defaults (§2.5), judged against each measurement.
const (
	provisionalPerTunnel = 1 << 20
	provisionalPerSource = 8 << 20
)

// Shows is what a client prints on a refused CONNECT (§2.8 point 2).
type shows string

const (
	showsCodeAndPhrase shows = "code and phrase"
	showsCodeOnly      shows = "code only"
	showsNeither       shows = "neither"
)

// liveClient is one package manager fixture.
type liveClient struct {
	// name is the row's name; tool the binary that must answer --version.
	name, tool string
	// preset is the netprofile client preset the profile lists.
	preset string
	// hosts is the allowlist the install needs.
	hosts []string
	// drop is the host removed for the refused run; "" refuses with only
	// liveDecoyHost listed.
	drop string
	// fixture writes the fixture into dir and returns the command and its
	// extra environment (beyond the tunnel's); it may prepare the
	// toolchain itself (corepack's yarn) over the developer's own network.
	fixture func(t *testing.T, r *liveTunnelRun, dir string, bulk bool) (argv, env []string)
	// version is added to the tool's own (yarn runs through corepack).
	version string
	// expect is what the refused run's output must show; evidence matches
	// the lines of it worth recording.
	expect   shows
	evidence *regexp.Regexp
}

// measurement is one client run's audit, summed.
type measurement struct {
	client, version, run           string
	ok                             bool
	tunnels, refused               int
	upSum, upMax, downSum          int64
	peakOpen, peakPerSecond        int
	hosts                          []string
	refusedOutcomes, refusedOutput []string
	elapsed                        time.Duration
}

// liveTunnelRun is one engine on loopback serving one tunnel profile, with
// its audit log and the client environment jobs.TunnelEnv builds for it.
type liveTunnelRun struct {
	t     *testing.T
	log   *lockedBuffer
	proxy string
	env   []string
	home  string
}

// newLiveTunnelRun starts the engine with one tunnel profile (hosts,
// presets) over the production address policy and real DNS, and builds the
// agent container's tunnel environment with the shipping jobs.TunnelEnv,
// the fake broker and the proxy itself as the pod's endpoints.
//
// With natural set, the tunnel concurrency and rate limits are lifted too,
// so a bulk install shows the client's own demand (its peak open tunnels
// and new tunnels per second), judged against the defaults afterwards.
func newLiveTunnelRun(t *testing.T, hosts, presets []string, broker string, natural bool) *liveTunnelRun {
	t.Helper()
	profile := map[string]any{"name": "live", "route": netprofile.RouteTunnel, "port": 8092,
		"repositories": []string{"devthenet-labs/patchy-target"}, "modules": []string{},
		"hosts": hosts, "clients": presets}
	raw, err := json.Marshal([]any{profile})
	if err != nil {
		t.Fatal(err)
	}
	set, err := netprofile.Parse(raw)
	if err != nil {
		t.Fatalf("profile %s: %v", raw, err)
	}
	limits := TunnelLimits{MaxUploadBytes: liveMaxUpload, SourceUploadBudget: liveSourceBudget}
	if natural {
		limits.MaxConcurrent, limits.PerSourceConcurrent = 4096, 4096
		limits.PerSourceRate, limits.PerSourceBurst = 1000, 4096
	}
	log := &lockedBuffer{}
	srv, err := New(Config{Profiles: set, TunnelLimits: limits}, slog.New(slog.NewJSONHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := srv.Handler("live")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(h)
	srv.ConfigureListener(ts.Config)
	ts.Start()
	t.Cleanup(func() {
		srv.CloseTunnels()
		ts.Close()
	})

	tunnelEnv, err := jobs.TunnelEnv(jobs.NetworkProfile{Name: "live", Route: netprofile.RouteTunnel,
		ProxyURL: ts.URL, Hosts: hosts, Clients: presets}, []string{broker, ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	// The agent container's scrub leaves the other proxy names blank;
	// the tunnel block then sets its own.
	env := make([]string, 0, 8+len(tunnelEnv))
	env = append(env, "PATH="+os.Getenv("PATH"), "HOME="+home, "TMPDIR="+t.TempDir(), "LANG=C.UTF-8",
		"HTTP_PROXY=", "http_proxy=", "ALL_PROXY=", "all_proxy=")
	for _, e := range tunnelEnv {
		env = append(env, e.Name+"="+e.Value)
	}
	return &liveTunnelRun{t: t, log: log, proxy: ts.URL, env: env, home: home}
}

// run runs argv in dir with the run's environment plus extra, returning
// the combined output.
func (r *liveTunnelRun) run(dir string, extra []string, argv ...string) (string, error) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveTunnelTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(r.env), extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// measure sums the run's close lines, once every opened tunnel has
// closed: the client has exited, but its tunnels' close lines follow the
// splice's end by a moment.
func (r *liveTunnelRun) measure() measurement {
	r.t.Helper()
	lines := r.log.lines(r.t)
	for deadline := time.Now().Add(10 * time.Second); ; {
		opened, closed := 0, 0
		for _, l := range lines {
			switch {
			case l["msg"] == "tunnel opened":
				opened++
			case closesOpenedTunnel(l):
				closed++
			}
		}
		if closed >= opened {
			break
		}
		if time.Now().After(deadline) {
			r.t.Errorf("%d tunnels opened, %d closed after the client exited", opened, closed)
			break
		}
		time.Sleep(20 * time.Millisecond)
		lines = r.log.lines(r.t)
	}
	var m measurement
	hosts := map[string]bool{}
	for _, l := range lines {
		if l["route"] != "tunnel" {
			continue
		}
		switch l["msg"] {
		case "tunnel":
			m.tunnels++
			up, down := int64(num(l["bytes_up"])), int64(num(l["bytes_down"]))
			m.upSum += up
			m.downSum += down
			m.upMax = max(m.upMax, up)
			hosts[fmt.Sprint(l["host"])] = true
		case "refused":
			m.refused++
			outcome := fmt.Sprint(l["outcome"])
			if reason, _ := l["reason"].(string); reason != "" && outcome != "not_allowed" {
				outcome += "(" + reason + ")"
			}
			m.refusedOutcomes = append(m.refusedOutcomes, fmt.Sprintf("%s %v", outcome, l["host"]))
		}
	}
	m.peakOpen, m.peakPerSecond = peaks(lines)
	m.hosts = slices.Sorted(func(yield func(string) bool) {
		for h := range hosts {
			if !yield(h) {
				return
			}
		}
	})
	return m
}

// closesOpenedTunnel reports whether l is the close line of a tunnel that
// logged "tunnel opened". The open line follows the upstream dial and
// precedes the 200 and the ClientHello, so a tunnel refused after it (a
// failed 200 write, bad_hello, sni_missing, sni_mismatch, ech) closes with
// msg "refused", not "tunnel"; what tells it from a refusal before the open
// line is the upstream address only a dialled tunnel carries.
func closesOpenedTunnel(l map[string]any) bool {
	if l["route"] != "tunnel" {
		return false
	}
	switch l["msg"] {
	case "tunnel":
		return true
	case "refused":
		ip, _ := l["upstream_ip"].(string)
		return ip != ""
	}
	return false
}

// peaks is the most tunnels open at once and the most opened within any
// one second, from the opened lines and the close lines of opened tunnels
// (those carry an upstream address).
func peaks(lines []map[string]any) (open, perSecond int) {
	type event struct {
		at    time.Time
		delta int
	}
	var events []event
	var opens []time.Time
	for _, l := range lines {
		at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(l["time"]))
		if err != nil || l["route"] != "tunnel" {
			continue
		}
		switch {
		case l["msg"] == "tunnel opened":
			events = append(events, event{at, 1})
			opens = append(opens, at)
		case closesOpenedTunnel(l):
			events = append(events, event{at, -1})
		}
	}
	slices.SortStableFunc(events, func(a, b event) int { return a.at.Compare(b.at) })
	cur := 0
	for _, e := range events {
		cur += e.delta
		open = max(open, cur)
	}
	slices.SortFunc(opens, func(a, b time.Time) int { return a.Compare(b) })
	for i, j := 0, 0; i < len(opens); i++ {
		for opens[i].Sub(opens[j]) >= time.Second {
			j++
		}
		perSecond = max(perSecond, i-j+1)
	}
	return open, perSecond
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// TestLiveTunnel runs each package manager found on PATH through the
// engine's CONNECT tunnel against the real public registries, with the
// environment jobs.TunnelEnv builds for a tunnel Job (§8.6 of the step-3
// spec). For every client it installs a fixture with the profile's hosts,
// then again with one host removed, and records the upstream bytes the
// install sent (the sizing input for the upload caps) and what the client
// prints on the refused CONNECT. The last subtest runs the claude CLI with
// the tunnel environment against a loopback fake broker: the model path
// must bypass the proxy. Absent clients are skipped and listed. Opt-in:
// MAUL_LIVE_TUNNEL=1.
func TestLiveTunnel(t *testing.T) {
	if os.Getenv(liveTunnelEnv) != "1" {
		t.Skipf("set %s=1 to run the live tunnel client fixtures (reaches the public package registries)",
			liveTunnelEnv)
	}
	var (
		mu      sync.Mutex
		rows    []measurement
		skipped []string
	)
	record := func(m measurement) {
		mu.Lock()
		defer mu.Unlock()
		rows = append(rows, m)
	}
	for _, c := range liveClients() {
		t.Run(c.name, func(t *testing.T) {
			version, ok := toolVersion(c.tool)
			if c.version != "" {
				version += ", " + c.version
			}
			if !ok {
				mu.Lock()
				skipped = append(skipped, c.name)
				mu.Unlock()
				t.Skipf("%s is not on PATH (or does not run)", c.tool)
			}
			t.Run("install", func(t *testing.T) { record(liveInstall(t, c, version, false)) })
			t.Run("bulk", func(t *testing.T) { record(liveInstall(t, c, version, true)) })
			t.Run("refused", func(t *testing.T) { record(liveRefused(t, c, version)) })
		})
	}
	t.Run("pnpm-workspace", func(t *testing.T) {
		version, ok := toolVersion("pnpm")
		if !ok {
			mu.Lock()
			skipped = append(skipped, "pnpm-workspace")
			mu.Unlock()
			t.Skip("pnpm is not on PATH")
		}
		for _, m := range livePnpmWorkspace(t, version) {
			record(m)
		}
	})
	t.Run("claude", func(t *testing.T) {
		version, ok := toolVersion("claude")
		if !ok {
			mu.Lock()
			skipped = append(skipped, "claude")
			mu.Unlock()
			t.Skip("claude is not on PATH")
		}
		record(liveClaude(t, version))
	})
	t.Logf("live tunnel results (bytes are TLS bytes on the wire; up = client to upstream):\n%s",
		renderMeasurements(rows))
	if len(skipped) > 0 {
		t.Logf("skipped (not on PATH): %s", strings.Join(skipped, ", "))
	}
}

// liveInstall runs c's fixture with every host listed: it must succeed
// through the tunnel only.
func liveInstall(t *testing.T, c liveClient, version string, bulk bool) measurement {
	r := newLiveTunnelRun(t, c.hosts, []string{c.preset}, "http://broker.maul.invalid:8080", bulk)
	dir := t.TempDir()
	argv, extra := c.fixture(t, r, dir, bulk)
	start := time.Now()
	out, err := r.run(dir, extra, argv...)
	m := r.measure()
	m.client, m.version, m.run, m.elapsed, m.ok = c.name, version, "install", time.Since(start), err == nil
	if bulk {
		m.run = "bulk"
	}
	if err != nil {
		t.Errorf("%s through the tunnel: %v\n%s\naudit:\n%s", c.name, err, out, r.log.String())
		return m
	}
	if m.tunnels == 0 {
		t.Errorf("%s succeeded without a tunnel: it did not use the proxy\n%s", c.name, out)
	}
	for _, h := range m.hosts {
		if !slices.Contains(c.hosts, h) {
			t.Errorf("%s tunnelled to %s, outside the profile", c.name, h)
		}
	}
	if m.refused > 0 {
		t.Logf("%s: refusals during a successful install: %v", c.name, m.refusedOutcomes)
	}
	return m
}

// liveRefused runs c's fixture with one host removed (or only the decoy
// listed): it must fail, the proxy must refuse the missing host with
// not_allowed, and the output must show what the §2.8 table says.
func liveRefused(t *testing.T, c liveClient, version string) measurement {
	hosts := []string{liveDecoyHost}
	if c.drop != "" {
		hosts = slices.DeleteFunc(slices.Clone(c.hosts), func(h string) bool { return h == c.drop })
	}
	refusedHost := c.drop
	if refusedHost == "" {
		refusedHost = c.hosts[0]
	}
	r := newLiveTunnelRun(t, hosts, []string{c.preset}, "http://broker.maul.invalid:8080", false)
	dir := t.TempDir()
	argv, extra := c.fixture(t, r, dir, false)
	start := time.Now()
	out, err := r.run(dir, extra, argv...)
	m := r.measure()
	m.client, m.version, m.run, m.elapsed, m.ok = c.name, version, "refused "+refusedHost, time.Since(start), err == nil
	m.refusedOutput = evidence(out, c.evidence)
	if err == nil {
		t.Errorf("%s succeeded without %s listed\n%s", c.name, refusedHost, out)
	}
	if !slices.Contains(m.refusedOutcomes, "not_allowed "+refusedHost) {
		t.Errorf("%s: no not_allowed refusal of %s; refusals %v\naudit:\n%s", c.name, refusedHost,
			m.refusedOutcomes, r.log.String())
	}
	phrase := "egress-proxy: " + refusedHost + " is not in profile live's hosts"
	hasCode, hasPhrase := strings.Contains(out, "403"), strings.Contains(out, phrase)
	got := showsNeither
	switch {
	case hasCode && hasPhrase:
		got = showsCodeAndPhrase
	case hasCode:
		got = showsCodeOnly
	}
	if got != c.expect {
		t.Errorf("%s's refused output shows %s, want %s (§2.8 table)\n%s", c.name, got, c.expect, out)
	}
	t.Logf("%s refused output (%s):\n%s", c.name, got, strings.Join(m.refusedOutput, "\n"))
	return m
}

// evidence is the lines of out that re matches, deduplicated, each clipped.
func evidence(out string, re *regexp.Regexp) []string {
	var lines []string
	for l := range strings.Lines(out) {
		l = strings.TrimSpace(l)
		if l == "" || !re.MatchString(l) || slices.Contains(lines, l) {
			continue
		}
		if len(l) > 240 {
			l = l[:240] + "…"
		}
		lines = append(lines, l)
		if len(lines) == 4 {
			break
		}
	}
	return lines
}

// toolVersion is the first line of tool --version, or false when the tool
// is absent or does not run (a version-manager shim with no version set).
func toolVersion(tool string) (string, bool) {
	path, err := exec.LookPath(tool)
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", false
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(first), true
}

// liveYarn is the Yarn Berry release corepack prepares for the yarn
// fixture.
const liveYarn = "yarn@4.9.2"

// pythonInterpreter is python3's path, for uv's --python.
func pythonInterpreter(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH")
	}
	return p
}

// rustupHome keeps a rustup-managed cargo working under the fixture's HOME.
func rustupHome() []string {
	if v := os.Getenv("RUSTUP_HOME"); v != "" {
		return []string{"RUSTUP_HOME=" + v}
	}
	if h, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(h, ".rustup")); err == nil {
			return []string{"RUSTUP_HOME=" + filepath.Join(h, ".rustup")}
		}
	}
	return nil
}

// liveClients are the fixtures, one per client preset (npm covers npm and
// pnpm). Each fixture has a small form (one package, the refused run's too)
// and a bulk form (a realistic dependency tree: the sizing input that
// matters, since request headers grow with the number of packages).
func liveClients() []liveClient {
	npmHosts := []string{"registry.npmjs.org"}
	pyHosts := []string{"files.pythonhosted.org", "pypi.org"}
	pyPkgs := func(bulk bool) []string {
		if bulk {
			return []string{"requests==2.32.3", "flask==3.0.3", "boto3==1.35.36"}
		}
		return []string{"six==1.16.0"}
	}
	npmPkgs := func(bulk bool) []string {
		if bulk {
			return []string{"express@4.21.2", "eslint@9.12.0"}
		}
		return []string{"left-pad@1.3.0"}
	}
	packageJSON := func(t *testing.T, dir, extra string) {
		write(t, filepath.Join(dir, "package.json"), `{"name":"live","version":"0.0.0","private":true`+extra+"}\n")
	}
	return []liveClient{
		{
			name: "pip", tool: "python3", preset: netprofile.ClientPip, hosts: pyHosts,
			drop: "files.pythonhosted.org", expect: showsCodeAndPhrase,
			evidence: regexp.MustCompile(`(?i)egress-proxy|403|ProxyError|Tunnel connection failed`),
			fixture: func(_ *testing.T, _ *liveTunnelRun, dir string, bulk bool) ([]string, []string) {
				argv := []string{"python3", "-m", "pip", "download", "--no-cache-dir", "-d", filepath.Join(dir, "out")}
				if !bulk {
					argv = append(argv, "--no-deps")
				}
				return append(argv, pyPkgs(bulk)...), nil
			},
		},
		{
			name: "uv", tool: "uv", preset: netprofile.ClientUV, hosts: pyHosts,
			drop: "files.pythonhosted.org", expect: showsNeither,
			evidence: regexp.MustCompile(`(?i)error|tunnel|egress-proxy|403`),
			fixture: func(t *testing.T, _ *liveTunnelRun, dir string, bulk bool) ([]string, []string) {
				return append([]string{"uv", "pip", "install", "--python", pythonInterpreter(t), "--no-cache",
					"--target", filepath.Join(dir, "out")}, pyPkgs(bulk)...), nil
			},
		},
		{
			name: "npm", tool: "npm", preset: netprofile.ClientNPM, hosts: npmHosts, expect: showsCodeAndPhrase,
			evidence: regexp.MustCompile(`(?i)egress-proxy|403|ERR|tunnel`),
			fixture: func(t *testing.T, _ *liveTunnelRun, dir string, bulk bool) ([]string, []string) {
				if !bulk {
					return []string{"npm", "pack", "left-pad@1.3.0"}, nil
				}
				packageJSON(t, dir, "")
				return append([]string{"npm", "install"}, npmPkgs(bulk)...), nil
			},
		},
		{
			name: "pnpm", tool: "pnpm", preset: netprofile.ClientNPM, hosts: npmHosts, expect: showsNeither,
			evidence: regexp.MustCompile(`(?i)egress-proxy|403|ERR|tunnel`),
			fixture: func(t *testing.T, _ *liveTunnelRun, dir string, bulk bool) ([]string, []string) {
				packageJSON(t, dir, "")
				return append([]string{"pnpm", "add"}, npmPkgs(bulk)...), nil
			},
		},
		{
			name: "yarn", tool: "corepack", version: liveYarn, preset: netprofile.ClientYarn,
			hosts: []string{"registry.yarnpkg.com"}, expect: showsCodeOnly,
			evidence: regexp.MustCompile(`(?i)egress-proxy|403|YN0|tunnel`),
			fixture: func(t *testing.T, r *liveTunnelRun, dir string, bulk bool) ([]string, []string) {
				packageJSON(t, dir, `,"packageManager":"`+liveYarn+`"`)
				corepackHome := filepath.Join(r.home, "corepack")
				prepareYarn(t, corepackHome)
				// Yarn Berry's default registry, registry.yarnpkg.com, is the
				// profile's host (fact 5).
				return append([]string{"corepack", "yarn", "add"}, npmPkgs(bulk)...),
					[]string{"COREPACK_HOME=" + corepackHome, "COREPACK_ENABLE_NETWORK=0",
						"YARN_ENABLE_GLOBAL_CACHE=false"}
			},
		},
		{
			name: "cargo", tool: "cargo", preset: netprofile.ClientCargo,
			hosts: []string{"index.crates.io", "static.crates.io"}, drop: "static.crates.io", expect: showsCodeOnly,
			evidence: regexp.MustCompile(`(?i)egress-proxy|403|CONNECT|error|tunnel`),
			fixture: func(t *testing.T, r *liveTunnelRun, dir string, bulk bool) ([]string, []string) {
				deps := "itoa = \"=1.0.11\"\n"
				if bulk {
					deps = "tokio = { version = \"=1.40.0\", features = [\"full\"] }\n" +
						"serde = { version = \"=1.0.210\", features = [\"derive\"] }\nserde_json = \"=1.0.128\"\n"
				}
				write(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname = \"live\"\nversion = \"0.1.0\"\n"+
					"edition = \"2021\"\n\n[dependencies]\n"+deps)
				write(t, filepath.Join(dir, "src", "lib.rs"), "")
				return []string{"cargo", "fetch"}, append(rustupHome(), "CARGO_HOME="+filepath.Join(r.home, "cargo"))
			},
		},
		{
			name: "dotnet", tool: "dotnet", preset: netprofile.ClientNuGet, hosts: []string{"api.nuget.org"},
			expect:   showsCodeOnly,
			evidence: regexp.MustCompile(`(?i)egress-proxy|403|NU1|error|tunnel`),
			fixture: func(t *testing.T, r *liveTunnelRun, dir string, bulk bool) ([]string, []string) {
				ref := `<PackageReference Include="Newtonsoft.Json" Version="13.0.3" />`
				if bulk {
					ref = `<PackageReference Include="Microsoft.Extensions.Hosting" Version="8.0.1" />`
				}
				write(t, filepath.Join(dir, "live.csproj"), `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><TargetFramework>net8.0</TargetFramework></PropertyGroup>
  <ItemGroup>`+ref+`</ItemGroup>
</Project>
`)
				write(t, filepath.Join(dir, "nuget.config"), `<?xml version="1.0" encoding="utf-8"?>
<configuration><packageSources><clear />
<add key="nuget.org" value="https://api.nuget.org/v3/index.json" protocolVersion="3" />
</packageSources></configuration>
`)
				return []string{"dotnet", "restore"}, []string{"NUGET_PACKAGES=" + filepath.Join(r.home, "nuget")}
			},
		},
		{
			name: "maven", tool: "mvn", preset: netprofile.ClientJVM, hosts: []string{"repo.maven.apache.org"},
			expect:   showsCodeAndPhrase,
			evidence: regexp.MustCompile(`(?i)egress-proxy|403|tunnel|ERROR`),
			fixture: func(_ *testing.T, r *liveTunnelRun, _ string, bulk bool) ([]string, []string) {
				artifact, transitive := "org.apache.commons:commons-lang3:3.14.0", "false"
				if bulk {
					artifact, transitive = "org.springframework.boot:spring-boot-starter-web:3.3.4", "true"
				}
				return []string{"mvn", "-B", "-Dmaven.repo.local=" + filepath.Join(r.home, "m2"),
					"dependency:get", "-Dartifact=" + artifact, "-Dtransitive=" + transitive}, nil
			},
		},
		{
			name: "gradle", tool: "gradle", preset: netprofile.ClientJVM, hosts: []string{"repo.maven.apache.org"},
			expect:   showsCodeAndPhrase,
			evidence: regexp.MustCompile(`(?i)egress-proxy|403|tunnel|Could not`),
			fixture: func(t *testing.T, r *liveTunnelRun, dir string, bulk bool) ([]string, []string) {
				dep := "org.apache.commons:commons-lang3:3.14.0"
				if bulk {
					dep = "org.springframework.boot:spring-boot-starter-web:3.3.4"
				}
				write(t, filepath.Join(dir, "settings.gradle"), "rootProject.name = 'live'\n")
				write(t, filepath.Join(dir, "build.gradle"), "plugins { id 'java' }\n"+
					"repositories { mavenCentral() }\n"+
					"dependencies { implementation '"+dep+"' }\n")
				return []string{"gradle", "--no-daemon", "-q", "dependencies", "--configuration", "compileClasspath",
						"--refresh-dependencies"},
					[]string{"GRADLE_USER_HOME=" + filepath.Join(r.home, "gradle")}
			},
		},
	}
}

// prepareYarn downloads liveYarn into corepackHome over the developer's
// own network, standing in for the image build that bakes it (a tunnel
// Job's corepack runs with COREPACK_ENABLE_NETWORK=0 or from the image).
func prepareYarn(t *testing.T, corepackHome string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(corepackHome, "v1")); err == nil {
		return
	}
	cmd := exec.Command("corepack", "install", "-g", liveYarn)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "COREPACK_HOME=" + corepackHome,
		"COREPACK_ENABLE_DOWNLOAD_PROMPT=0"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("corepack install %s: %v\n%s", liveYarn, err, out)
	}
}

// livePnpmWorkspace pins which of the environment and a repository's
// pnpm-workspace.yaml wins for pnpm 11's proxy settings (fact 4): a
// workspace httpsProxy pointing at a closed port, and a workspace noProxy
// naming the registry.
//
// Observed on pnpm 11.1.2 and 11.28.6: the environment's httpsProxy wins
// (the install goes through the tunnel), but the file's noProxy is
// honoured, so pnpm dials the registry directly. In a pod that dial is
// dropped by the NetworkPolicy, so a repository can break its own install
// this way but cannot reach a host the profile does not list. On this
// workstation the direct dial succeeds, which is how the test tells.
func livePnpmWorkspace(t *testing.T, version string) []measurement {
	var out []measurement
	for _, v := range []struct {
		name, yaml string
		// tunnel is the pinned outcome: true when the environment wins.
		tunnel bool
	}{
		{"workspace httpsProxy", "httpsProxy: http://127.0.0.1:9\n", true},
		{"workspace noProxy", "noProxy: registry.npmjs.org\n", false},
	} {
		t.Run(v.name, func(t *testing.T) {
			r := newLiveTunnelRun(t, []string{"registry.npmjs.org"}, []string{netprofile.ClientNPM},
				"http://broker.maul.invalid:8080", false)
			dir := t.TempDir()
			write(t, filepath.Join(dir, "package.json"), `{"name":"live","version":"0.0.0","private":true}`+"\n")
			write(t, filepath.Join(dir, "pnpm-workspace.yaml"), v.yaml)
			start := time.Now()
			res, err := r.run(dir, nil, "pnpm", "add", "left-pad@1.3.0")
			m := r.measure()
			m.client, m.version, m.ok = "pnpm", version, err == nil
			m.run, m.elapsed = v.name, time.Since(start)
			winner := "environment wins (through the tunnel)"
			switch {
			case err != nil:
				winner = "workspace file wins (install failed)"
				m.refusedOutput = evidence(res, regexp.MustCompile(`(?i)ERR|proxy|ECONNREFUSED`))
			case m.tunnels == 0:
				winner = "workspace file wins (went direct, which a pod's NetworkPolicy drops)"
			}
			m.refusedOutput = append([]string{winner}, m.refusedOutput...)
			t.Logf("pnpm %s, %s: %s\n%s", version, v.name, winner, res)
			if got := err == nil && m.tunnels > 0; got != v.tunnel {
				t.Errorf("pnpm %s with a %s: %s; the pinned outcome changed (tunnel %t), so the preset's "+
					"docs must change too", version, v.name, winner, v.tunnel)
			}
			if !v.tunnel && err != nil {
				t.Errorf("pnpm %s with a %s failed outright: %v\n%s", version, v.name, err, res)
			}
			out = append(out, m)
		})
	}
	return out
}

// fakeBroker is a loopback stand-in for the egress broker's anthropic
// route: it answers the Messages API with one fixed text reply.
type fakeBroker struct {
	srv   *httptest.Server
	calls atomic.Int64
	paths sync.Map
}

const fakeBrokerReply = "maul-live-tunnel-ok"

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	b := &fakeBroker{}
	// The broker serves Anthropic under /anthropic (provider.Env's route).
	b.srv = httptest.NewServer(http.StripPrefix("/anthropic", http.HandlerFunc(b.serve)))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBroker) serve(w http.ResponseWriter, r *http.Request) {
	b.calls.Add(1)
	b.paths.Store(r.Method+" "+r.URL.Path, true)
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/v1/messages/count_tokens"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":10}`)
	case strings.HasSuffix(r.URL.Path, "/v1/messages"):
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		msg := map[string]any{"id": "msg_live", "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 1}}
		if !req.Stream {
			msg["content"] = []any{map[string]string{"type": "text", "text": fakeBrokerReply}}
			msg["stop_reason"] = "end_turn"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(msg)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []struct {
			name string
			data any
		}{
			{"message_start", map[string]any{"type": "message_start", "message": msg}},
			{"content_block_start", map[string]any{"type": "content_block_start", "index": 0,
				"content_block": map[string]string{"type": "text", "text": ""}}},
			{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]string{"type": "text_delta", "text": fakeBrokerReply}}},
			{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
			{"message_delta", map[string]any{"type": "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
				"usage": map[string]int{"output_tokens": 1}}},
			{"message_stop", map[string]any{"type": "message_stop"}},
		}
		for _, e := range events {
			data, _ := json.Marshal(e.data)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, data)
		}
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}
}

// liveClaude runs the claude CLI with a tunnel Job's environment and the
// brokered provider environment pointing at a loopback fake broker: the
// model call must reach the broker directly (NO_PROXY names it), and the
// proxy may see only refusals of model-API hosts, hard-denied (fact 2).
func liveClaude(t *testing.T, version string) measurement {
	b := newFakeBroker(t)
	r := newLiveTunnelRun(t, []string{"pypi.org"}, nil, b.srv.URL, false)
	brokered := provider.Env(provider.Config{Name: provider.Anthropic, BrokerURL: b.srv.URL}, nil)
	extra := make([]string, 0, 3+len(brokered))
	extra = append(extra, provider.PlaceholderAuthEnv+"="+provider.PlaceholderAuthToken,
		"CLAUDE_CONFIG_DIR="+filepath.Join(r.home, ".claude"), "DISABLE_AUTOUPDATER=1")
	for k, v := range brokered {
		extra = append(extra, k+"="+v)
	}
	start := time.Now()
	out, err := r.run(t.TempDir(), extra, "claude", "-p", "Reply with the word ok.", "--max-turns", "1")
	m := r.measure()
	m.client, m.version, m.run, m.elapsed, m.ok = "claude", version, "model via fake broker", time.Since(start), err == nil
	m.refusedOutput = m.refusedOutcomes
	if err != nil || !strings.Contains(out, fakeBrokerReply) {
		t.Errorf("claude -p through the tunnel environment: %v\n%s", err, out)
	}
	if b.calls.Load() == 0 {
		t.Errorf("the fake broker saw no request: the model path did not bypass the proxy")
	}
	if m.tunnels > 0 {
		t.Errorf("claude opened %d tunnels (%v); a brokered pod's CLI should open none", m.tunnels, m.hosts)
	}
	for _, o := range m.refusedOutcomes {
		outcome, host, _ := strings.Cut(o, " ")
		if !strings.HasPrefix(outcome, "hard_denied") || netprofile.DeniedHost(host, netprofile.DenyContext{}) == nil {
			t.Errorf("claude: proxy refusal %q is not a hard-denied model host", o)
		}
	}
	var paths []string
	b.paths.Range(func(k, _ any) bool { paths = append(paths, k.(string)); return true })
	slices.Sort(paths)
	t.Logf("claude %s: broker requests %v; proxy refusals %v", version, paths, m.refusedOutcomes)
	return m
}

// renderMeasurements is the markdown table the PR records.
func renderMeasurements(rows []measurement) string {
	var b strings.Builder
	b.WriteString("| client | version | run | ok | tunnels | peak open | peak new/s | up total | up max/tunnel | " +
		"down total | within 1 MiB/8 MiB | refusals | client output on refusal |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, m := range rows {
		within := m.upMax <= provisionalPerTunnel && m.upSum <= provisionalPerSource
		fmt.Fprintf(&b, "| %s | %s | %s | %t | %d | %d | %d | %d | %d | %d | %t | %s | %s |\n", m.client,
			mdCell(m.version), m.run, m.ok, m.tunnels, m.peakOpen, m.peakPerSecond, m.upSum, m.upMax, m.downSum,
			within,
			mdCell(counted(m.refusedOutcomes)), mdCell(strings.Join(m.refusedOutput, " / ")))
	}
	return b.String()
}

// counted joins ss with each repeated value once, as "value ×n".
func counted(ss []string) string {
	var order []string
	n := map[string]int{}
	for _, s := range ss {
		if n[s] == 0 {
			order = append(order, s)
		}
		n[s]++
	}
	for i, s := range order {
		if n[s] > 1 {
			order[i] = fmt.Sprintf("%s ×%d", s, n[s])
		}
	}
	return strings.Join(order, "; ")
}

func mdCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

// TestMeasureCountsRefusalsAfterTheOpenLine: a tunnel refused after its
// "tunnel opened" line (here a client that closes before its hello, which
// is bad_hello) closes with msg "refused". measure must count that as the
// close of the opened tunnel, or it waits out its deadline and fails a
// client run the proxy handled correctly. Not a live test: it runs the
// engine over the loopback tunnel harness, so it runs in CI.
func TestMeasureCountsRefusalsAfterTheOpenLine(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	c, br := h.connect(t, "web", "CONNECT pkg.registry.dev:443 HTTP/1.1")
	if r := readRaw(t, br); r.statusLine != "HTTP/1.1 200 Connection established" {
		t.Fatalf("CONNECT = %q", r.statusLine)
	}
	_ = c.Close()
	// One refusal before any open line too, which closes nothing.
	c2, br2 := h.connect(t, "web", "CONNECT not-listed.example.com:443 HTTP/1.1")
	if r := readRaw(t, br2); r.statusLine == "HTTP/1.1 200 Connection established" {
		t.Fatalf("CONNECT to an unlisted host = %q", r.statusLine)
	}
	_ = c2.Close()
	h.closeLine(t, 2)
	if n := len(h.linesWith(t, "tunnel opened")); n != 1 {
		t.Fatalf("%d open lines, want 1:\n%s", n, h.log.String())
	}

	start := time.Now()
	m := (&liveTunnelRun{t: t, log: h.log}).measure()
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("measure waited %s for a tunnel already closed", waited)
	}
	if m.tunnels != 0 || m.refused != 2 {
		t.Errorf("tunnels %d refused %d, want 0 and 2", m.tunnels, m.refused)
	}
	if !slices.Contains(m.refusedOutcomes, OutcomeBadHello+" pkg.registry.dev") {
		t.Errorf("refusals %v, want bad_hello pkg.registry.dev", m.refusedOutcomes)
	}
	if m.peakOpen != 1 {
		t.Errorf("peak open %d, want 1", m.peakOpen)
	}
}

// TestClosesOpenedTunnel pins which audit lines close an opened tunnel.
func TestClosesOpenedTunnel(t *testing.T) {
	tests := []struct {
		name string
		line map[string]any
		want bool
	}{
		{"forwarded", map[string]any{"route": "tunnel", "msg": "tunnel", "upstream_ip": "192.0.2.1"}, true},
		{"refused after the dial", map[string]any{"route": "tunnel", "msg": "refused", "upstream_ip": "192.0.2.1"},
			true},
		{"refused before the dial", map[string]any{"route": "tunnel", "msg": "refused", "upstream_ip": ""}, false},
		{"refused, no address", map[string]any{"route": "tunnel", "msg": "refused"}, false},
		{"opened", map[string]any{"route": "tunnel", "msg": "tunnel opened", "upstream_ip": "192.0.2.1"}, false},
		{"progress", map[string]any{"route": "tunnel", "msg": "tunnel progress"}, false},
		{"other route", map[string]any{"route": "goproxy", "msg": "refused", "upstream_ip": "192.0.2.1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := closesOpenedTunnel(tt.line); got != tt.want {
				t.Errorf("closesOpenedTunnel(%v) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}
