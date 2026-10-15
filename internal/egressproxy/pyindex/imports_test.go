// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package pyindex

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestImportSet keeps the rewriter pure: the standard library, the HTML
// tokenizer and netprofile's grammar (itself stdlib-only), nothing else.
func TestImportSet(t *testing.T) {
	allowed := map[string]bool{
		"golang.org/x/net/html": true,
		"github.com/bitwise-media-group/patchy/internal/netprofile": true,
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
			if strings.Contains(first, ".") && !allowed[path] {
				t.Errorf("%s imports %s: package pyindex is pure", name, path)
			}
		}
	}
}
