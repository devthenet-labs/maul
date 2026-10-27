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

// stampScript is the release step that pins this binary's image into the
// chart (.github/workflows/helm.yaml).
const stampScript = "../../hack/stamp-preview-tester.sh"

const stampValues = `# head comment
image:
    repository: ghcr.io/devthenet-labs/maul

preview:
    # the tester, by digest only
    verification:
        enabled: false
        testerImage: "" # x-maul-preview-tester-image
`

// TestStampScript: the release stamp writes the image by digest into the
// one marked line, leaves every other byte alone, and refuses anything it
// cannot stamp exactly.
func TestStampScript(t *testing.T) {
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("yq not on PATH")
	}
	repo := "ghcr.io/devthenet-labs/maul/preview-tester"
	digest := "sha256:" + strings.Repeat("0a", 32)
	stamp := func(t *testing.T, values, repository, digest string) (string, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(stampScript, path, repository, digest).CombinedOutput()
		if err != nil {
			t.Logf("%s", out)
		}
		got, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatal(rerr)
		}
		return string(got), err
	}

	got, err := stamp(t, stampValues, repo, digest)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	want := strings.Replace(stampValues, `testerImage: ""`, `testerImage: "`+repo+"@"+digest+`"`, 1)
	if got != want {
		t.Errorf("stamped values:\n%s\nwant:\n%s", got, want)
	}

	for name, tc := range map[string]struct{ values, repo, digest string }{
		"tag, not digest":   {stampValues, repo, "v0.14.0"},
		"short digest":      {stampValues, repo, "sha256:abc"},
		"repository syntax": {stampValues, repo + "|x", digest},
		"no marker":         {strings.Replace(stampValues, " # x-maul-preview-tester-image", "", 1), repo, digest},
		"two markers": {stampValues + `other:
    testerImage: "" # x-maul-preview-tester-image
`, repo, digest},
		"marker on another key": {strings.Replace(stampValues, "    verification:\n", "    elsewhere:\n", 1), repo, digest},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := stamp(t, tc.values, tc.repo, tc.digest); err == nil {
				t.Errorf("stamp accepted %s", name)
			}
		})
	}
}

// TestStampChartValues: the shipped chart holds the one marked line the
// release stamp rewrites, at preview.verification.testerImage, so a release
// never fails its publish (or ships unstamped) over a values edit.
func TestStampChartValues(t *testing.T) {
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("yq not on PATH")
	}
	src, err := os.ReadFile("../../charts/maul/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	repo := "ghcr.io/devthenet-labs/maul/preview-tester"
	digest := "sha256:" + strings.Repeat("0a", 32)
	out, err := exec.Command(stampScript, path, repo, digest).CombinedOutput()
	if err != nil {
		t.Fatalf("stamp of charts/maul/values.yaml: %v\n%s", err, out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(src), `testerImage: "" # x-maul-preview-tester-image`,
		`testerImage: "`+repo+"@"+digest+`" # x-maul-preview-tester-image`, 1)
	if string(got) != want {
		t.Error("the stamp changed more of charts/maul/values.yaml than the marked line")
	}
}
