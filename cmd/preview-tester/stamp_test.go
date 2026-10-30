// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stampScript is the release step that pins this binary's image, and the
// slot tool pod's three, into the chart (.github/workflows/helm.yaml).
const stampScript = "../../hack/stamp-preview-images.sh"

const stampValues = `# head comment
image:
    repository: ghcr.io/devthenet-labs/maul

preview:
    # the tester, by digest only
    verification:
        enabled: false
        testerImage: "" # x-maul-preview-tester-image
        tools:
            image: "" # x-maul-preview-tools-image
            browserImage: "" # x-maul-preview-browser-image
            toolboxImage: "" # x-maul-preview-toolbox-image
`

// stampKeys are the images the script stamps: the name it is called with,
// the values line it rewrites and that line's marker.
var stampKeys = []struct{ name, key, marker string }{
	{"tester", "testerImage", "# x-maul-preview-tester-image"},
	{"tools", "image", "# x-maul-preview-tools-image"},
	{"browser", "browserImage", "# x-maul-preview-browser-image"},
	{"toolbox", "toolboxImage", "# x-maul-preview-toolbox-image"},
}

// stamp runs the script on a copy of values and returns the result.
func stamp(t *testing.T, values string, args ...string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(stampScript, append([]string{path}, args...)...).CombinedOutput()
	if err != nil {
		t.Logf("%s", out)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return string(got), err
}

// TestStampScript: the release stamp writes each image by digest into its
// one marked line, leaves every other byte alone, and refuses anything it
// cannot stamp exactly.
func TestStampScript(t *testing.T) {
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("yq not on PATH")
	}
	digest := "sha256:" + strings.Repeat("0a", 32)
	for _, k := range stampKeys {
		t.Run(k.name, func(t *testing.T) {
			repo := "ghcr.io/devthenet-labs/maul/preview-" + k.name
			got, err := stamp(t, stampValues, k.name, repo, digest)
			if err != nil {
				t.Fatalf("stamp: %v", err)
			}
			want := strings.Replace(stampValues, k.key+`: "" `+k.marker,
				k.key+`: "`+repo+"@"+digest+`" `+k.marker, 1)
			if got != want {
				t.Errorf("stamped values:\n%s\nwant:\n%s", got, want)
			}

			for name, tc := range map[string]struct{ values, repo, digest string }{
				"tag, not digest":   {stampValues, repo, "v0.14.0"},
				"short digest":      {stampValues, repo, "sha256:abc"},
				"repository syntax": {stampValues, repo + "|x", digest},
				"no marker":         {strings.Replace(stampValues, " "+k.marker, "", 1), repo, digest},
				"two markers": {stampValues + "other:\n    " + k.key + `: "" ` + k.marker + "\n",
					repo, digest},
				"marker on another key": {strings.Replace(stampValues, k.key+`: "" `+k.marker,
					"elsewhere: \"\" "+k.marker, 1), repo, digest},
			} {
				t.Run(name, func(t *testing.T) {
					if _, err := stamp(t, tc.values, k.name, tc.repo, tc.digest); err == nil {
						t.Errorf("stamp accepted %s", name)
					}
				})
			}
		})
	}
	for name, args := range map[string][]string{
		"an unknown image": {"agent", "ghcr.io/devthenet-labs/maul/agent", digest},
		"no image name":    {"ghcr.io/devthenet-labs/maul/preview-tester", digest},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := stamp(t, stampValues, args...); err == nil {
				t.Errorf("stamp accepted %s", name)
			}
		})
	}
}

// TestStampChartValues: the shipped chart holds each marked line the
// release stamp rewrites, so a release never fails its publish (or ships
// unstamped) over a values edit.
func TestStampChartValues(t *testing.T) {
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("yq not on PATH")
	}
	src, err := os.ReadFile("../../charts/maul/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("0a", 32)
	for _, k := range stampKeys {
		t.Run(k.name, func(t *testing.T) {
			repo := "ghcr.io/devthenet-labs/maul/preview-" + k.name
			got, err := stamp(t, string(src), k.name, repo, digest)
			if err != nil {
				t.Fatalf("stamp of charts/maul/values.yaml: %v", err)
			}
			want := strings.Replace(string(src), k.key+`: "" `+k.marker,
				k.key+`: "`+repo+"@"+digest+`" `+k.marker, 1)
			if got != want {
				t.Error("the stamp changed more of charts/maul/values.yaml than the marked line")
			}
		})
	}
}
