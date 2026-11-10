// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

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

// TestImports keeps the ledger's trust root pure: the standard library, the
// tool wire it reads, the CRD types it writes, and the step and check
// grammars it rereads with. No Kubernetes client, no clock, no I/O.
func TestImports(t *testing.T) {
	allowed := []string{
		"github.com/devthenet-labs/maul/api/v1alpha1",
		"github.com/devthenet-labs/maul/internal/checks",
		"github.com/devthenet-labs/maul/internal/httpstep",
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
