// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

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

// TestImports keeps the tools container's relay small and auditable: the
// standard library, the check grammar, its Playwright compiler and the
// wire it speaks, the exec shim's client, the gateway's header hygiene and
// the preview routing rule. No Kubernetes client, no credential store, nothing that could
// reach beyond the slot on its own.
func TestImports(t *testing.T) {
	allowed := []string{
		"github.com/devthenet-labs/maul/api/v1alpha1",
		"github.com/devthenet-labs/maul/internal/acceptance",
		"github.com/devthenet-labs/maul/internal/checks",
		"github.com/devthenet-labs/maul/internal/checks/playwright",
		"github.com/devthenet-labs/maul/internal/previewauth",
		"github.com/devthenet-labs/maul/internal/toolexec",
		"github.com/devthenet-labs/maul/internal/toolwire",
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
