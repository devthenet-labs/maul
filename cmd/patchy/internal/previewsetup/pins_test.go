// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// renovateLine is a pin's renovate comment and the constant under it.
var renovateLine = regexp.MustCompile(`(?m)^// renovate: datasource=helm registryUrl=(\S+) depName=(\S+)\n` +
	`const (\w+) = "([^"]+)"$`)

// imageLine is an image pin's renovate comment and the constant under it.
var imageLine = regexp.MustCompile(`(?m)^// renovate: datasource=docker depName=(\S+)\n` +
	`const (\w+) = "([^"@]+)@(sha256:[a-f0-9]{64})"$`)

// pinVersion is a chart version a pin may hold: a release, never a range.
var pinVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

// TestPinsAreTracked: every pinned chart's version is a constant under a
// renovate comment naming the same chart and repository, so a renovate
// bump is the pin, and a pin without its comment would never be bumped.
func TestPinsAreTracked(t *testing.T) {
	src, err := os.ReadFile("pins.go")
	if err != nil {
		t.Fatal(err)
	}
	type tracked struct{ repo, chart, version string }
	matches := renovateLine.FindAllStringSubmatch(string(src), -1)
	found := make([]tracked, 0, len(matches))
	for _, m := range matches {
		found = append(found, tracked{m[1], m[2], m[4]})
	}
	images := imageLine.FindAllStringSubmatch(string(src), -1)
	if got := strings.Count(string(src), "// renovate:"); got != len(found)+len(images) {
		t.Errorf("%d renovate comments, %d of them over a version constant", got, len(found)+len(images))
	}
	for _, p := range ImagePins() {
		if !slices.ContainsFunc(images, func(m []string) bool {
			return m[1] == p.Repository && m[3] == p.Tag && m[4] == p.Digest
		}) {
			t.Errorf("image pin %s has no renovate comment over its tag and digest", p.Ref())
		}
		if !strings.HasPrefix(p.Repository, "docker.io/") || !pinVersion.MatchString(p.Tag) {
			t.Errorf("image pin %s: a release tag of a fully qualified repository", p.Ref())
		}
	}
	if len(images) != len(ImagePins()) {
		t.Errorf("%d tracked images, %d image pins", len(images), len(ImagePins()))
	}
	for _, p := range Pins() {
		if !slices.Contains(found, tracked{p.Repo, p.Chart, p.Version}) {
			t.Errorf("pin %s %s from %s has no renovate comment over its version", p.Chart, p.Version, p.Repo)
		}
		if !pinVersion.MatchString(p.Version) || !strings.HasPrefix(p.Repo, "https://") {
			t.Errorf("pin %+v: a release version from an https repository", p)
		}
		if !strings.HasPrefix(p.ValuesFile, "upstream/") || !strings.HasSuffix(p.ValuesFile, ".values.yaml") {
			t.Errorf("pin %s writes %s, outside upstream/", p.Chart, p.ValuesFile)
		}
	}
	if len(found) != len(Pins()) {
		t.Errorf("%d tracked versions, %d pins", len(found), len(Pins()))
	}
}

// TestUpstreamValuesNameTheirPin: each upstream values file's header
// installs exactly the pinned chart and version, from the file itself.
func TestUpstreamValuesNameTheirPin(t *testing.T) {
	for _, tc := range []struct {
		pin    Pin
		render func() ([]byte, error)
	}{{CertManager, certManagerValues}, {TailscaleOperator, tailscaleOperatorValues}} {
		data, err := tc.render()
		if err != nil {
			t.Fatal(err)
		}
		p := tc.pin
		want := fmt.Sprintf("helm upgrade --install %s %s --repo %s --version %s", p.Release, p.Chart, p.Repo,
			p.Version)
		if !strings.Contains(string(data), want) || !strings.Contains(string(data), "-f "+p.ValuesFile) {
			t.Errorf("%s header does not install its pin:\n%s", p.ValuesFile, data)
		}
	}
}

// renovateConfig is the repository's own renovate config, which extends
// the org preset (that one scans only Dockerfiles).
const renovateConfig = "../../../../.github/renovate.json5"

// pinsPath is pins.go as renovate sees it, from the repository root.
const pinsPath = "cmd/patchy/internal/previewsetup/pins.go"

// json5ToJSON reads the JSON5 subset prettier writes renovate.json5 in
// (whole-line comments, bare keys, single-quoted strings, trailing
// commas) as JSON.
func json5ToJSON(src string) []byte {
	var lines []string
	for _, l := range strings.Split(src, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines = append(lines, l)
		}
	}
	s := strings.Join(lines, "\n")
	s = regexp.MustCompile(`'((?:[^'\\]|\\.)*)'`).ReplaceAllStringFunc(s, func(q string) string {
		inner := strings.ReplaceAll(q[1:len(q)-1], `\'`, `'`)
		return `"` + strings.ReplaceAll(inner, `"`, `\"`) + `"`
	})
	s = regexp.MustCompile(`(?m)^(\s*)(\$?[A-Za-z_]\w*):`).ReplaceAllString(s, `$1"$2":`)
	return []byte(regexp.MustCompile(`,(\s*[\]}])`).ReplaceAllString(s, `$1`))
}

// renovateDep is one dependency a renovate regex manager extracts.
type renovateDep struct{ datasource, registryURL, depName, currentValue, currentDigest string }

// renovateDeps are the dependencies renovate's regex managers extract from
// pins.go: those whose file patterns match it, through their matchStrings.
func renovateDeps(t *testing.T) []renovateDep {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(renovateConfig))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		CustomManagers []struct {
			CustomType          string   `json:"customType"`
			ManagerFilePatterns []string `json:"managerFilePatterns"`
			MatchStrings        []string `json:"matchStrings"`
		} `json:"customManagers"`
	}
	if err := json.Unmarshal(json5ToJSON(string(raw)), &cfg); err != nil {
		t.Fatalf("%s: %v", renovateConfig, err)
	}
	src, err := os.ReadFile("pins.go")
	if err != nil {
		t.Fatal(err)
	}
	var deps []renovateDep
	for _, m := range cfg.CustomManagers {
		if m.CustomType != "regex" || !scansPins(m.ManagerFilePatterns) {
			continue
		}
		for _, ms := range m.MatchStrings {
			re, err := regexp.Compile(ms)
			if err != nil {
				t.Fatalf("matchString %q: %v", ms, err)
			}
			for _, sub := range re.FindAllStringSubmatch(string(src), -1) {
				g := map[string]string{}
				for i, name := range re.SubexpNames() {
					g[name] = sub[i]
				}
				deps = append(deps, renovateDep{g["datasource"], g["registryUrl"], g["depName"], g["currentValue"],
					g["currentDigest"]})
			}
		}
	}
	return deps
}

// scansPins reports whether a manager's file patterns match pins.go.
func scansPins(patterns []string) bool {
	for _, p := range patterns {
		if body, ok := strings.CutPrefix(p, "/"); ok && strings.HasSuffix(body, "/") {
			if regexp.MustCompile(strings.TrimSuffix(body, "/")).MatchString(pinsPath) {
				return true
			}
		} else if p == pinsPath {
			return true
		}
	}
	return false
}

// TestRenovateTracksPins: a renovate manager actually reads pins.go, so
// the renovate comments TestPinsAreTracked holds every pin to are bumped,
// not decoration. Each pin is matched as one helm dependency at its
// chart, repository and version, exactly as renovate's regex manager
// would extract it (the same named groups, through Go's RE2, which reads
// renovate's (?<name>) groups).
func TestRenovateTracksPins(t *testing.T) {
	deps := renovateDeps(t)
	for _, p := range Pins() {
		if !slices.Contains(deps, renovateDep{"helm", p.Repo, p.Chart, p.Version, ""}) {
			t.Errorf("no renovate manager in %s extracts pin %s %s from %s (found %v)", renovateConfig, p.Chart,
				p.Version, p.Repo, deps)
		}
	}
	for _, p := range ImagePins() {
		if !slices.Contains(deps, renovateDep{"docker", "", p.Repository, p.Tag, p.Digest}) {
			t.Errorf("no renovate manager in %s extracts image pin %s (found %v)", renovateConfig, p.Ref(), deps)
		}
	}
	if want := len(Pins()) + len(ImagePins()); len(deps) != want {
		t.Errorf("renovate extracts %d dependencies from pins.go, %d pins: %v", len(deps), want, deps)
	}
}
