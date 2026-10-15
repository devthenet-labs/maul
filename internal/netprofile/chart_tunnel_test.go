// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The chart's tunnel guards (patchy.egressTunnelGuard) refuse a profile host
// the binaries would refuse, so a render that passes is a profile set the
// proxy and both job controllers start with. These tests hold the chart's
// copies of the tunnel tables, and its patchy.egressDeniedTunnelHost, to
// this package's.

// TestChartTunnelDenyMatchesNetprofile: the tunnel tables the chart spells
// out are this package's, list for list, and patchy.egressDeniedTunnelHost
// reaches DeniedTunnelHost's verdict over a seeded corpus of hosts built
// from the tables' own labels, the deny table's and the forge hosts, plus
// IP forms (run through helm on a scratch chart holding the real
// _helpers.tpl, as TestDeniedHostMatchesChart does for the registry table).
func TestChartTunnelDenyMatchesNetprofile(t *testing.T) {
	for _, c := range []struct {
		helper string
		want   []string
	}{
		{"patchy.egressTunnelForgeHosts", TunnelForgeHosts},
		{"patchy.egressTunnelModelHosts", TunnelModelAPIHosts},
		{"patchy.egressControlPlaneHosts", ControlPlaneHosts},
		{"patchy.egressSpecialUseSuffixes", SpecialUseSuffixes},
		{"patchy.egressPublishHosts", PublishHosts},
		{"patchy.egressGoRouteHosts", GoRouteHosts},
		{"patchy.egressTunnelClients", Clients},
	} {
		if got := strings.Fields(chartHelper(t, c.helper)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %v, want %v", c.helper, got, c.want)
		}
	}
	// The client-to-grant pairs guard 24 refuses are ClientEcosystem's.
	guard := chartHelper(t, "patchy.egressTunnelGuard")
	for _, c := range Clients {
		eco, ok := ClientEcosystem(c)
		pair := fmt.Sprintf("%q %q", c, eco)
		if got := strings.Contains(guard, pair); got != ok {
			t.Errorf("patchy.egressTunnelGuard pairs client %s with grant ecosystem %s: %v, want %v", c, eco, got, ok)
		}
	}

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	hosts := tunnelDenyCorpus()
	verdicts := chartTunnelVerdicts(t, hosts)
	denied := 0
	for i, h := range hosts {
		err := DeniedTunnelHost(h, DenyContext{Hosts: denyCorpusExtra})
		if verdicts[i] != "" {
			denied++
		}
		if chartAdmits := verdicts[i] == ""; chartAdmits != (err == nil) {
			t.Errorf("%q: the chart says %q, the validator %v", h, verdicts[i], err)
		}
	}
	t.Logf("%d hosts, %d denied by the chart", len(hosts), denied)
	if denied == 0 || denied == len(hosts) {
		t.Errorf("the corpus is one-sided: %d of %d denied", denied, len(hosts))
	}
}

// tunnelDenyCorpus is fixed examples (the spec's, IP forms, special-use and
// look-alike names) plus seeded hosts over the tables' labels. Every IP-ish
// part stays at most 255, where the chart's dotted-quad test and
// netip.ParseAddr agree; ValidHost refuses every such host before either
// deny check runs anyway.
func tunnelDenyCorpus() []string {
	fixed := []string{
		"pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "registry.yarnpkg.com", "api.nuget.org",
		"index.crates.io", "static.crates.io", "repo.maven.apache.org", "plugins.gradle.org", "proxy.golang.org",
		"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "ghcr.io",
		"pkg.ghcr.io", "gitlab.com", "dev.azure.com", "pkgs.dev.azure.com", "x.visualstudio.com", "git.sr.ht",
		"api.anthropic.com", "claude.ai", "x.claude.com", "chatgpt.com", "api.openai.com",
		"bedrock-runtime.us-east-1.amazonaws.com", "us-east5-aiplatform.googleapis.com",
		"x.us-east5-aiplatform.googleapis.com", "abc.gr7.us-east-1.eks.amazonaws.com", "x.hcp.westeurope.azmk8s.io",
		"container.googleapis.com", "x.k8s.ondigitalocean.com", "x.linodelke.net", "kubernetes.default",
		"x.kubernetes.default", "kubernetes.default.svc", "a.b.svc", "x.cluster.local", "metadata.google.internal",
		"localhost", "x.localhost", "printer.local", "x.home.arpa", "1.0.0.10.in-addr.arpa", "x.ip6.arpa",
		"x.invalid", "a.test", "a.example", "example.org", "github.com.example.org", "upload.pypi.org",
		"www.nuget.org", "nuget.org", "ghe.corp.example", "x.gw.corp.example", "gw.corp.example.net",
		"1.2.3.4", "0x7f.1", "127.1", "2130706433", "017700000001", "0x7f000001", "1.2.3.4.", "a.1.2.3.4",
		"1.2.3", "[::1]", "::1", "x", "", "a..b", "Pypi.org", "PYPI.ORG.", "xn--p1ai", "a.xn--p1ai", "x.a1",
	}
	const generated = 3000
	out := make([]string, 0, len(fixed)+generated)
	out = append(out, fixed...)
	labels := []string{
		"github", "com", "githubusercontent", "ghcr", "io", "gitlab", "dev", "azure", "visualstudio", "sr", "ht",
		"anthropic", "claude", "ai", "openai", "bedrock-runtime", "us-east-1", "amazonaws", "eks", "azmk8s",
		"container", "googleapis", "us-east5-aiplatform", "k8s", "ondigitalocean", "linodelke", "net", "kubernetes",
		"default", "svc", "cluster", "local", "internal", "arpa", "home", "invalid", "test", "example", "upload",
		"pypi", "org", "www", "nuget", "npmjs", "registry", "files", "corp", "gw", "ghe", "x", "0x7f", "10", "1",
	}
	r := rand.New(rand.NewSource(seed))
	for range generated {
		n := 1 + r.Intn(5)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = labels[r.Intn(len(labels))]
		}
		h := strings.Join(parts, ".")
		switch r.Intn(20) {
		case 0:
			h += "."
		case 1:
			h = strings.ToUpper(h)
		case 2:
			h = strconv.Itoa(r.Intn(256)) + "." + strconv.Itoa(r.Intn(256))
		}
		out = append(out, h)
	}
	return out
}

// chartTunnelVerdicts runs each host through patchy.egressDeniedTunnelHost
// on a scratch chart holding the real _helpers.tpl, with denyCorpusExtra as
// the further denied hosts, rendered by helm once.
func chartTunnelVerdicts(t *testing.T, hosts []string) []string {
	t.Helper()
	dir := t.TempDir()
	helpers, err := os.ReadFile(filepath.Join(chartDir, "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	probe := `apiVersion: v1
kind: ConfigMap
metadata:
    name: tunnel-deny-probe
data:
    result: |
        {{- range $i, $h := .Values.hosts }}
        {{ $i }}|{{ include "patchy.egressDeniedTunnelHost" (dict "host" $h "extra" $.Values.extra) }}
        {{- end }}
`
	values, err := json.Marshal(map[string]any{"hosts": hosts, "extra": denyCorpusExtra})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"Chart.yaml":             []byte("apiVersion: v2\nname: tunnel-deny-probe\nversion: 0.0.1\n"),
		"templates/_helpers.tpl": helpers,
		"templates/probe.yaml":   []byte(probe),
		"values.json":            values,
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.CommandContext(t.Context(), "helm", "template", "probe", dir,
		"-f", filepath.Join(dir, "values.json")).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	verdicts := make([]string, len(hosts))
	seen := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		idx, why, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		i, err := strconv.Atoi(idx)
		if err != nil || i < 0 || i >= len(hosts) {
			continue
		}
		verdicts[i] = why
		seen++
	}
	if seen != len(hosts) {
		t.Fatalf("the probe chart printed %d verdicts, want %d", seen, len(hosts))
	}
	return verdicts
}
