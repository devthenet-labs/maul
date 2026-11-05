// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolpod

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

// TestImports keeps the tool pod's shared rendering free of any client:
// the standard library, the core API types it renders, the CRD types and
// the relay's configuration only.
func TestImports(t *testing.T) {
	allowed := []string{
		"k8s.io/api/core/v1",
		"github.com/devthenet-labs/maul/api/v1alpha1",
		"github.com/devthenet-labs/maul/internal/previewtools",
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
				t.Errorf("%s imports %s: only the standard library and %v", name, path, allowed)
			}
		}
	}
}
