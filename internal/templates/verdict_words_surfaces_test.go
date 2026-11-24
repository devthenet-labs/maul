// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// statusPageModules are the status page's evaluation surfaces beside its
// intents views: the module they share, their wire types and the demo data
// they are built against.
var statusPageModules = []string{"src/intents.ts", "src/types.ts", "src/mock/intents.ts"}

// intentsViewFiles are ui/src/intents.ts's INTENTS_VIEW_FILES: every
// component that renders an intent.
func intentsViewFiles(t *testing.T, src string) []string {
	t.Helper()
	start := strings.Index(src, "export const INTENTS_VIEW_FILES = [")
	if start < 0 {
		t.Fatal("ui/src/intents.ts has no INTENTS_VIEW_FILES")
	}
	body := src[start : start+strings.Index(src[start:], "];")]
	files := regexp.MustCompile(`"(src/[^"]+)"`).FindAllStringSubmatch(body, -1)
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f[1])
	}
	if !slices.Contains(out, "src/components/EvaluationEvidence.tsx") {
		t.Fatalf("INTENTS_VIEW_FILES read as %v: the parser is broken", out)
	}
	return out
}

// TestStatusPageNeverOverstates (W6): the status page words an evaluation
// as the comment does, so none of its intents views (every file of
// INTENTS_VIEW_FILES) or the modules beside them, comments included, uses
// a word overstating says maul looked at an outcome itself.
func TestStatusPageNeverOverstates(t *testing.T) {
	ui := filepath.Join("..", "web", "ui")
	views, err := os.ReadFile(filepath.Join(ui, "src", "intents.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(intentsViewFiles(t, string(views)), statusPageModules...) {
		src, err := os.ReadFile(filepath.Join(ui, f))
		if err != nil {
			t.Fatal(err)
		}
		settings := f == projectSettingsView
		for i, line := range strings.Split(string(src), "\n") {
			if f == "src/types.ts" {
				settings = inProjectSettingsDecl(line, settings)
			}
			if bad := overstates(withoutSaveConfirmation(line, settings)); len(bad) > 0 {
				t.Errorf("%s:%d says %q: %s", f, i+1, bad, strings.TrimSpace(line))
			}
		}
	}
}

// projectSettingsView is the Project settings page, an intents view since
// #467: it renders a Project's settings, not an evaluation.
const projectSettingsView = "src/components/ProjectSettingsView.tsx"

// saveConfirmation is the settings page's own sense of "confirm": an
// approver confirming what a save does (ProjectSettingsConfirmation, the
// "confirm-required" reason, "Confirm and save"). It says nothing of an
// evaluation, so it is taken out of the settings page and its wire types
// before the scan; every other overstating word there still counts.
var saveConfirmation = regexp.MustCompile(`(?i)\b(set)?confirm(s|ed|ing|ation(s|Text)?|-required)?\b|` +
	`ProjectSettingsConfirmation`)

func withoutSaveConfirmation(line string, settings bool) string {
	if !settings {
		return line
	}
	return saveConfirmation.ReplaceAllString(line, "")
}

// inProjectSettingsDecl tracks whether a line of types.ts is inside a
// ProjectSettings* declaration (an interface to its closing brace, a type
// alias to its semicolon), given whether the line before was.
func inProjectSettingsDecl(line string, was bool) bool {
	if strings.HasPrefix(line, "export interface ProjectSettings") ||
		strings.HasPrefix(line, "export type ProjectSettings") {
		return true
	}
	if was && (strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "// ----")) {
		return false
	}
	return was
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

// TestSaveConfirmationIsNotAVerdict: the settings page's save confirmation
// is taken out of its scan, and nothing else is: an overstating word beside
// it, or "confirm" anywhere but the settings page, still counts.
func TestSaveConfirmationIsNotAVerdict(t *testing.T) {
	for _, line := range []string{`{busy ? "Saving…" : armed ? "Click again to confirm and save" : "Confirm and save"}`,
		`confirm: ProjectSettingsConfirmation[];`, `// "confirm-required".`, `setConfirm(outcome.result.confirm);`} {
		if bad := overstates(withoutSaveConfirmation(line, true)); len(bad) > 0 {
			t.Errorf("%q still says %q", line, bad)
		}
	}
	for _, line := range []string{`"Confirm and save: every outcome verified"`, `maul checked the approved outcomes`} {
		if len(overstates(withoutSaveConfirmation(line, true))) == 0 {
			t.Errorf("%q passes the settings page's scan", line)
		}
	}
	if len(overstates(withoutSaveConfirmation(`outcomes confirmed`, false))) == 0 {
		t.Error(`"outcomes confirmed" passes outside the settings page`)
	}
	in := false
	for _, c := range []struct {
		line string
		want bool
	}{
		{"export interface IntentView {", false}, {"  confirmed: number;", false},
		{"export interface ProjectSettingsResult {", true}, {"  confirm: ProjectSettingsConfirmation[];", true},
		{"}", true}, {"", true}, {"export interface IntentRun {", false},
		{"export type ProjectSettingsKind =", true}, {`  | "count";`, true},
		{"// ---- usage ----", false},
	} {
		if in = inProjectSettingsDecl(c.line, in); in != c.want {
			t.Errorf("after %q in = %v, want %v", c.line, in, c.want)
		}
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
