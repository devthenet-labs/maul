// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package maultool

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestImports keeps maul-tool small enough to audit:
// it may import the standard library and the tool wire, and
// nothing else.
func TestImports(t *testing.T) {
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
			if strings.Contains(first, ".") && path != "github.com/devthenet-labs/maul/internal/toolwire" {
				t.Errorf("%s imports %s: package maultool imports only the standard library and internal/toolwire", name, path)
			}
		}
	}
}
