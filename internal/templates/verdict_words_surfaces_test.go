// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The status page's evaluation surfaces: every file that renders an intent
// (ui/src/intents.ts's INTENTS_VIEW_FILES), the module they share, their
// wire types and the demo data they are built against.
var statusPageSurfaces = []string{
	"src/intents.ts",
	"src/types.ts",
	"src/mock/intents.ts",
	"src/components/IntentsBoard.tsx",
	"src/components/IntentTimeline.tsx",
	"src/components/RunPanel.tsx",
	"src/components/LiveOutput.tsx",
	"src/components/PlainText.tsx",
	"src/components/IntentsArea.tsx",
	"src/components/EvaluationEvidence.tsx",
}

// TestStatusPageNeverOverstates (W6): the status page words an evaluation
// as the comment does, so none of its sources, comments included, uses a
// word overstating says maul looked at an outcome itself.
func TestStatusPageNeverOverstates(t *testing.T) {
	ui := filepath.Join("..", "web", "ui")
	views, err := os.ReadFile(filepath.Join(ui, "src", "intents.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range statusPageSurfaces {
		if strings.HasSuffix(f, ".tsx") && !strings.Contains(string(views), `"`+f+`"`) {
			t.Errorf("%s is not in INTENTS_VIEW_FILES: keep this list to it", f)
		}
		src, err := os.ReadFile(filepath.Join(ui, f))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if bad := overstates(line); len(bad) > 0 {
				t.Errorf("%s:%d says %q: %s", f, i+1, bad, strings.TrimSpace(line))
			}
		}
	}
}

// renderAllowed are the CLI render's literals that use an overstating word
// of something other than an evaluation: a runner image's signature, which
// cosign verifies.
var renderAllowed = map[string]bool{
	"verified": true,
	"not verified (the operator allows unsigned images)": true,
}

// TestCLIRenderNeverOverstates (W6): every string literal of the maul
// CLI's describe and get views (cmd/maul/internal/render) is held to the
// same words.
func TestCLIRenderNeverOverstates(t *testing.T) {
	dir := filepath.Join("..", "..", "cmd", "maul", "internal", "render")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no render sources in %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	literals := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			literals++
			if bad := overstates(s); len(bad) > 0 && !renderAllowed[s] {
				t.Errorf("%s says %q: %q", fset.Position(lit.Pos()), bad, s)
			}
			return true
		})
	}
	if literals < 50 {
		t.Fatalf("read %d literals from %s: the scan is broken", literals, dir)
	}
}

// TestSurfaceScanBites: the scans above find what they look for.
func TestSurfaceScanBites(t *testing.T) {
	for _, s := range []string{`ready: "maul checked the approved outcomes",`,
		`unconfirmed: "maul could not confirm every outcome",`, `Field("Result", "verified")`} {
		if len(overstates(s)) == 0 {
			t.Errorf("overstates(%q) found nothing", s)
		}
	}
}
