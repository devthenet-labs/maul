// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func traefikAnswers() Answers {
	a := goldenCases()["existing-traefik-k3s"]
	a.Normalize()
	return a
}

// compareWrite compares files against dir and writes them as accepted.
func compareWrite(t *testing.T, dir string, files []File, accept Accept) (Comparison, []string, error) {
	t.Helper()
	c, err := Compare(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	written, err := Write(c, accept)
	return c, written, err
}

func states(c Comparison) map[string]State {
	m := map[string]State{}
	for _, ch := range c.Changes {
		m[ch.File.Path] = ch.State
	}
	return m
}

// TestWriteFreshTree: a first run writes every file, the directory 0700
// and each file 0600, and a second run finds nothing to do.
func TestWriteFreshTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	files := plan(t, traefikAnswers())
	c, written, err := compareWrite(t, dir, files, Accept{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Pending() || len(written) != len(files) {
		t.Errorf("wrote %v of %d files", written, len(files))
	}
	for _, ch := range c.Changes {
		if ch.State != StateNew {
			t.Errorf("%s: %s, want new", ch.File.Path, ch.State)
		}
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("directory mode = %v, %v; want 0700", info.Mode().Perm(), err)
		}
		for _, f := range files {
			info, err := os.Stat(filepath.Join(dir, f.Path))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, %v; want 0600", f.Path, info.Mode().Perm(), err)
			}
		}
	}
	again, err := Compare(dir, plan(t, traefikAnswers()))
	if err != nil {
		t.Fatal(err)
	}
	if again.Pending() || len(again.Stale) > 0 {
		t.Errorf("a re-run with the same answers would change %v (stale %v)", states(again), again.Stale)
	}
}

// TestRerunShowsTheDiffAndWritesOnlyWhenAccepted: a changed answer is a
// diff for every non-secret file it touches, written only with --force,
// and nothing at all is written while one change is refused.
func TestRerunShowsTheDiffAndWritesOnlyWhenAccepted(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := compareWrite(t, dir, plan(t, traefikAnswers()), Accept{}); err != nil {
		t.Fatal(err)
	}
	a := traefikAnswers()
	a.Viewers.Users = append(a.Viewers.Users, "hubot")
	a.Namespace = "patchy-system" // moves the release-namespace Secret too
	files := plan(t, a)
	c, _, err := compareWrite(t, dir, files, Accept{})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Write = %v, want a ConflictError", err)
	}
	if !slices.Contains(conflict.Changed, ValuesFile) || !slices.Contains(conflict.Changed, RelayDexSecretFile) {
		t.Errorf("changed = %v", conflict.Changed)
	}
	before, _ := os.ReadFile(filepath.Join(dir, ValuesFile))
	if strings.Contains(string(before), "hubot") {
		t.Error("a refused write replaced the values")
	}
	for _, ch := range c.Changes {
		switch {
		case ch.File.Path == ValuesFile:
			if !strings.Contains(ch.Diff, "+    - hubot") || !strings.Contains(ch.Diff, "--- a/"+ValuesFile) {
				t.Errorf("values diff:\n%s", ch.Diff)
			}
		case ch.File.Secret && ch.Diff != "":
			t.Errorf("%s: a secret file was diffed", ch.File.Path)
		}
	}
	// A rotation accepts only the secret files: the values still wait.
	if _, err := Write(c, Accept{Secrets: true}); !errors.As(err, &conflict) ||
		slices.Contains(conflict.Changed, RelayDexSecretFile) {
		t.Errorf("Accept{Secrets} = %v", err)
	}
	written, err := Write(c, Accept{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(written, ValuesFile) || slices.Contains(written, DexClientSecretFile) {
		t.Errorf("written = %v", written)
	}
	after, _ := os.ReadFile(filepath.Join(dir, ValuesFile))
	if !strings.Contains(string(after), "hubot") {
		t.Error("an accepted write left the values")
	}
}

// TestStaleFilesAreReportedNotDeleted: a file an earlier edge wrote is
// named, and stays.
func TestStaleFilesAreReportedNotDeleted(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := compareWrite(t, dir, plan(t, traefikAnswers()), Accept{}); err != nil {
		t.Fatal(err)
	}
	a := traefikAnswers()
	a.Existing = ExistingEdge{GatewayParent: "gateway-system/edge"}
	c, _, err := compareWrite(t, dir, plan(t, a), Accept{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Stale, []string{CertificateFile}) {
		t.Errorf("stale = %v, want [%s]", c.Stale, CertificateFile)
	}
	if _, err := os.Stat(filepath.Join(dir, CertificateFile)); err != nil {
		t.Errorf("a stale file was deleted: %v", err)
	}
}

// TestWriteRefusesUnsafePaths: a symbolic link or a directory where a file
// goes is never written through, with or without --force.
func TestWriteRefusesUnsafePaths(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dir string){
		"symlink": func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			_ = os.WriteFile(target, []byte("x"), 0o600)
			if err := os.Symlink(target, filepath.Join(dir, ValuesFile)); err != nil {
				t.Skip(err)
			}
		},
		"symlinked directory": func(t *testing.T, dir string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(dir, "manifests")); err != nil {
				t.Skip(err)
			}
		},
		"directory": func(_ *testing.T, dir string) {
			_ = os.MkdirAll(filepath.Join(dir, NextStepsFile), 0o700)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			setup(t, dir)
			_, written, err := compareWrite(t, dir, plan(t, traefikAnswers()), Accept{All: true})
			var conflict *ConflictError
			if !errors.As(err, &conflict) || len(conflict.Unsafe) != 1 || len(written) > 0 {
				t.Errorf("Write = %v (written %v)", err, written)
			}
			if _, err := os.Stat(filepath.Join(dir, AnswersFile)); err == nil {
				t.Error("files were written beside an unsafe path")
			}
		})
	}
}
