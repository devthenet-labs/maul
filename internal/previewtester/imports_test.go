// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestImports keeps the tester small and auditable: the standard library
// and the acceptance core, nothing else (no Kubernetes client, no
// credential, nothing that could reach beyond the slot on its own).
func TestImports(t *testing.T) {
	checkImports(t, ".", "github.com/devthenet-labs/maul/internal/acceptance")
}

// checkImports fails for any non-test file in dir importing a non-stdlib
// package other than allowed.
func checkImports(t *testing.T, dir string, allowed ...string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
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
			if strings.Contains(first, ".") && !slices.Contains(allowed, path) {
				t.Errorf("%s imports %s: only the standard library and %v", name, path, allowed)
			}
		}
	}
}
