// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

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

// TestImports keeps the binary thin: the standard library, its engine and
// the exec shim's client only.
func TestImports(t *testing.T) {
	allowed := []string{
		"github.com/devthenet-labs/maul/internal/previewtools",
		"github.com/devthenet-labs/maul/internal/toolexec",
	}
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
			if strings.Contains(first, ".") && !slices.Contains(allowed, path) {
				t.Errorf("%s imports %s: preview-tools imports only the standard library and %v", name, path, allowed)
			}
		}
	}
}
