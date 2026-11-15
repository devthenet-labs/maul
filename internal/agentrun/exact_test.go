// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
)

// TestCheckFilesKeyedAsTheRelayRecords: a check file the evaluator wrote,
// its label and role name saying nothing of exact, is keyed by the digest
// the tools container records its dry run against (checks.ParseProposal),
// never by its plain form's, so a proposal citing the digest maul-tool
// printed resolves to it.
func TestCheckFilesKeyedAsTheRelayRecords(t *testing.T) {
	const raw = `{"id":"save-beat","outcome":"save-beat","form":"browser","browser":{"steps":[{"goto":"/"},` +
		`{"fill":{"locator":{"label":"Beat name"},"text":"{{run}} a"}},{"click":{"role":"button","name":"Save"}},` +
		`{"expect":{"assert":"visible","locator":{"role":"button","name":"Vote for {{run}} a"}}}]}}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "save-beat.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := readCheckFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := checks.ParseProposal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := checks.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[checks.Digest(proposal)]; !ok || len(files) != 1 {
		t.Errorf("files keyed %v, want only the dry run's digest %s", files, checks.Digest(proposal))
	}
	if _, ok := files[checks.Digest(plain)]; ok {
		t.Error("the file is keyed by its plain form's digest, which no dry run records")
	}
}
