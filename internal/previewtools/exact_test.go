// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// labelledCheck fills the beat name the way gate 4's checks did
// (G4-B-BR-1), saves and asserts its vote button.
const labelledCheck = `{"id":"save-beat","outcome":"save-beat","form":"browser","browser":{"steps":[{"goto":"/"},` +
	`{"fill":{"locator":{"label":"Beat name"},"text":"{{run}} a"}},` +
	`{"click":{"role":"button","name":"Save"}},` +
	`{"expect":{"assert":"visible","locator":{"role":"button","name":"Vote for {{run}} a"}}}]}}`

// TestProposedNamesRunExact: a proposed check's label and role names run
// exact in its dry run and its confirmation, recorded against the digest
// of that exact form, which is what the ledger's request rereads to.
func TestProposedNamesRunExact(t *testing.T) {
	h, run := browserCheckHarness(t, func(int, string) string { return report("passed", nil, "", 0) })
	p, err := checks.ParseProposal([]byte(labelledCheck))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(checks.Digest(p), "sha256:")
	_, resp := h.call(toolwire.VerbCheckRun, `{"check":`+labelledCheck+`}`)
	if resp.Observed == nil || resp.Observed.CheckRun == nil || resp.Observed.CheckRun.Digest != digest {
		t.Fatalf("dry run %+v, want digest %s", resp, digest)
	}
	if _, resp := h.call(toolwire.VerbConfirm, `{"digests":["`+digest+`"]}`); resp.Refused != "" {
		t.Fatalf("confirm: %+v", resp)
	}
	h.finish()
	if len(run.specs) != 2 {
		t.Fatalf("specs %d, want the dry run's and the confirmation's", len(run.specs))
	}
	for _, spec := range run.specs {
		for _, want := range []string{`getByLabel("Beat name", { exact: true })`,
			`getByRole("button", { name: "Save", exact: true })`} {
			if !strings.Contains(spec, want) {
				t.Errorf("spec lacks %s:\n%s", want, spec)
			}
		}
	}
}

// TestSavedChecksReplayAsSaved: a check saved before exact matching
// replays as it was proven, every label and role name a part of the name.
func TestSavedChecksReplayAsSaved(t *testing.T) {
	cfg := baseConfig()
	cfg.Browser = true
	c := parseCheck(t, labelledCheck)
	cfg.Checks = []StoredCheck{{ID: c.ID, Digest: checks.Digest(c), Body: string(checks.Canonical(c))}}
	run := &fakeRunner{reports: func(int, string) string { return report("passed", nil, "", 0) }}
	h := newHarness(t, cfg, routes{}, func(o *Options) {
		o.Browser = &BrowserOptions{CLI: &fakeCLI{dir: t.TempDir(), script: defaultScript}, Runner: run,
			Dir: t.TempDir(), Base: frontBase}
	})
	h.relay.replay(context.Background())
	h.finish()
	if len(run.specs) == 0 {
		t.Fatal("no replay")
	}
	if strings.Contains(run.specs[0], "exact") || !strings.Contains(run.specs[0], `getByLabel("Beat name")`) {
		t.Errorf("the saved check did not replay as saved:\n%s", run.specs[0])
	}
}
