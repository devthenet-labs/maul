// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package playwright

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedImports are the only non-stdlib imports: the grammar compiled and
// the slice-1 placeholder rules it substitutes by (both pure, pinned by
// their own tests).
var allowedImports = map[string]bool{
	"github.com/devthenet-labs/maul/internal/checks":     true,
	"github.com/devthenet-labs/maul/internal/acceptance": true,
}

// TestPureImports keeps the compiler shareable by the tools relay, the
// controllers and agent-runner: the standard library, internal/checks and
// internal/acceptance, nothing else.
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
			if strings.Contains(first, ".") && !allowedImports[path] {
				t.Errorf("%s imports %s: package playwright imports only the standard library, "+
					"internal/checks and internal/acceptance", name, path)
			}
		}
	}
}
