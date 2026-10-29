// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// acceptancePkg is the one non-stdlib import: the slice-1 grammar this one
// builds on (itself stdlib-only, pinned by its own test).
const acceptancePkg = "github.com/devthenet-labs/maul/internal/acceptance"

// TestPureImports keeps the saved-check grammar shareable by
// intent-controller, preview-controller, the tools relay and agent-runner:
// it may import the standard library and internal/acceptance, nothing
// else.
func TestPureImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") && path != acceptancePkg {
				t.Errorf("%s imports %s: package checks imports only the standard library and %s",
					name, path, acceptancePkg)
			}
		}
	}
}
