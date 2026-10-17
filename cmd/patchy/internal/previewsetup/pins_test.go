// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// renovateLine is a pin's renovate comment and the constant under it.
var renovateLine = regexp.MustCompile(`(?m)^// renovate: datasource=helm registryUrl=(\S+) depName=(\S+)\n` +
	`const (\w+) = "([^"]+)"$`)

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
	if got := strings.Count(string(src), "// renovate:"); got != len(found) {
		t.Errorf("%d renovate comments, %d of them over a version constant", got, len(found))
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
