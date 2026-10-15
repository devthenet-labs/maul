// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestProfileCallSites: Guard.Profile is asked in exactly two places, the
// remediation launch and the intent run scheduler's stage spec (a build or
// revise run), so no other Job (investigate, plan, evaluation) can ever
// request a profile. jobs.Create refuses one on those Jobs anyway; this
// keeps the request out of their code paths too. It scans every non-test
// file of internal/ and cmd/ for a call of a method named Profile.
func TestProfileCallSites(t *testing.T) {
	root := filepath.Join("..", "..")
	var found []string
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Profile" {
					rel, _ := filepath.Rel(root, path)
					found = append(found, filepath.ToSlash(rel))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(found)
	want := []string{
		"internal/controller/intent/run_controller.go",
		"internal/controller/remediation/remediation_controller.go",
	}
	if !slices.Equal(found, want) {
		t.Errorf("Profile is called from %v, want exactly %v", found, want)
	}
}
