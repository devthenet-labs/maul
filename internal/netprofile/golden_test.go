// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestStepOneParseGolden pins what Parse makes of step 1's profile JSON, as
// the chart renders it today: registries (step 2) are additive, so a step-1
// profile must parse to exactly the same value, field for field, and
// re-encode to exactly the same JSON. The golden was written by the step-1
// code, before Profile gained its registries.
func TestStepOneParseGolden(t *testing.T) {
	const stepOne = `[
	  {"name":"go","route":"go","port":8090,"host":"github.com",
	   "repositories":["devthenet-labs/patchy-preview-demo","devthenet-labs/patchy-target"],
	   "modules":["github.com/google/uuid","golang.org/x/"],
	   "bakedModCache":"/opt/go/pkg/mod","allowPseudoVersions":false},
	  {"name":"alpha","route":"go","port":8091,
	   "repositories":["acme/(api|web)"],"modules":["example.com/m"],
	   "bakedModCache":"","allowPseudoVersions":true}]`
	set, err := Parse([]byte(stepOne))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(set.Profiles(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "step1_parse.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Parse of step 1's JSON changed:\n got: %s\nwant: %s", got, want)
	}
}
