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

// httpstepPkg is the one non-stdlib import: the step grammar this one
// builds on (itself stdlib-only, pinned by its own test).
const httpstepPkg = "github.com/devthenet-labs/maul/internal/httpstep"

// TestPureImports keeps the saved-check grammar shareable by
// intent-controller, preview-controller, the tools relay and agent-runner:
// it may import the standard library and internal/httpstep, nothing
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
			if strings.Contains(first, ".") && path != httpstepPkg {
				t.Errorf("%s imports %s: package checks imports only the standard library and %s",
					name, path, httpstepPkg)
			}
		}
	}
}
