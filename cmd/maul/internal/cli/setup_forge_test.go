// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"errors"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/cmd/maul/internal/ghapp"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files under testdata")

// checkGolden compares got with testdata/name, rewriting it under -update.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s drifted:\n%s\nwant\n%s", path, got, want)
	}
}

// TestSetupNextGolden pins the next: block `setup github-app` ends with,
// for an organization's intents App, its Secret on stdout, a user App, an
// App without a slug and a findings-only App.
func TestSetupNextGolden(t *testing.T) {
	org := setupNext{owner: "acme", org: true, installURL: "https://github.com/apps/maul-acme/installations/new",
		secretFile: "maul-github-acme.secret.yaml", forgeFile: "maul-github-acme.forge.yaml", namespace: "maul",
		intents: true, serviceAccount: ghapp.DefaultIntentControllerSA}
	for _, tt := range []struct {
		name string
		edit func(*setupNext)
	}{
		{"org-intents", func(*setupNext) {}},
		{"org-stdout", func(n *setupNext) { n.secretFile = "-" }},
		{"user", func(n *setupNext) {
			n.owner, n.org, n.secretFile, n.forgeFile = "alice", false, "maul-github.secret.yaml",
				"maul-github.forge.yaml"
		}},
		{"no-slug", func(n *setupNext) {
			n.installURL, n.settingsURL = "", "https://github.com/organizations/acme/settings/apps"
		}},
		{"security", func(n *setupNext) {
			n.intents, n.serviceAccount, n.secretFile, n.forgeFile = false, "", "/tmp/my dir/s.yaml",
				"/tmp/my dir/f.yaml"
			n.namespace = "maul-system"
		}},
		{"custom-sa", func(n *setupNext) { n.serviceAccount = "platform-maul-intent-controller" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := org
			tt.edit(&n)
			checkGolden(t, "setup-next-"+tt.name+".txt", []byte(n.render()))
		})
	}
}

// TestSetupGitHubAppOwnerCheck: with --org and a token, maul asks GitHub
// whether the person is an Owner before anything else happens. An Owner
// goes on; a member is refused with no browser opened, no App created and
// no file written; no token, or an answer that settles nothing, warns and
// goes on. The token goes on that one request only, and is printed nowhere.
func TestSetupGitHubAppOwnerCheck(t *testing.T) {
	const token = "ghp_secret-token-never-printed"
	for _, tt := range []struct {
		name    string
		env     map[string]string
		status  int
		role    string
		state   string
		refused string
		stderr  string
	}{
		{"owner", map[string]string{"GH_TOKEN": token}, http.StatusOK, "admin", "active", "",
			"maul: you are an Owner of acme"},
		{"owner by GITHUB_TOKEN", map[string]string{"GITHUB_TOKEN": token}, http.StatusOK, "admin", "active", "",
			"maul: you are an Owner of acme"},
		{"member", map[string]string{"GH_TOKEN": token}, http.StatusOK, "member", "active",
			"you are a member, not an Owner, of acme: ask an Owner to run this", ""},
		{"pending owner", map[string]string{"GH_TOKEN": token}, http.StatusOK, "admin", "pending",
			"not an Owner, of acme", ""},
		{"no token", nil, 0, "", "", "", "maul: warning: you must be an Owner of acme"},
		{"not found", map[string]string{"GH_TOKEN": token}, http.StatusNotFound, "", "", "",
			"maul: warning: you must be an Owner of acme"},
		{"forbidden", map[string]string{"GITHUB_TOKEN": token}, http.StatusForbidden, "", "", "",
			"maul: warning: you must be an Owner of acme"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newFakeAppGitHub(t)
			g.memberStatus, g.memberRole, g.memberState = tt.status, tt.role, tt.state
			dir := t.TempDir()
			out := filepath.Join(dir, "s.secret.yaml")
			var started string
			open := g.browser(false, &started)
			if tt.refused != "" {
				open = neverOpen(t)
			}
			deps := g.deps(open, nil)
			deps.getenv = func(k string) string { return tt.env[k] }
			stdout, stderr, err := execSetup(t, deps, "setup", "github-app", "--org", "acme", "--intents", "-o", out,
				"--timeout", "20s")
			if tt.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tt.refused) || exitCode(err) != ExitError {
					t.Fatalf("error = %v (exit %d), want %q", err, exitCode(err), tt.refused)
				}
				if _, created := g.received(); len(created) != 0 {
					t.Errorf("GitHub was asked to create an App: %v", created)
				}
				if entries, _ := os.ReadDir(dir); len(entries) != 0 {
					t.Errorf("a refused run wrote %v", entries)
				}
			} else if err != nil {
				t.Fatalf("setup: %v\n%s", err, stderr)
			}
			if !strings.Contains(stderr, tt.stderr) {
				t.Errorf("stderr lacks %q:\n%s", tt.stderr, stderr)
			}
			if tt.env == nil && g.memberAsks() != 0 {
				t.Error("GitHub was asked about the membership without a token")
			}
			if tt.env != nil && g.memberAuth() != "Bearer "+token {
				t.Errorf("the membership request carried Authorization %q", g.memberAuth())
			}
			for _, s := range []string{stdout, stderr, fmtErr(err)} {
				if strings.Contains(s, token) {
					t.Errorf("the token was printed:\n%s", s)
				}
			}
		})
	}
}

// TestSetupGitHubAppNoOwnerCheckOnDryRun: --dry-run asks GitHub nothing.
func TestSetupGitHubAppNoOwnerCheckOnDryRun(t *testing.T) {
	g := newFakeAppGitHub(t)
	deps := g.deps(neverOpen(t), nil)
	deps.getenv = func(string) string { return "tok" }
	if _, stderr, err := execSetup(t, deps, "setup", "github-app", "--org", "acme", "--intents",
		"--dry-run"); err != nil {
		t.Fatalf("dry run: %v\n%s", err, stderr)
	}
	if g.memberAsks() != 0 {
		t.Error("--dry-run asked GitHub about the membership")
	}
}

// TestSetupGitHubAppForgeFile: beside the Secret goes the forge file: the
// Forge for the App's organization, plus the Secret reader Role and
// RoleBinding with --intents. It holds no secret, and the stderr carries
// the next: block naming both files.
func TestSetupGitHubAppForgeFile(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		sa   string
	}{
		{"intents", []string{"--intents"}, ghapp.DefaultIntentControllerSA},
		{"intents, own ServiceAccount and Forge name", []string{"--intents", "--intent-controller-sa", "x-ic",
			"--forge-name", "acme-forge"}, "x-ic"},
		{"security", []string{"--security", "--webhook-url", setupWebhook}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newFakeAppGitHub(t)
			g.owner = "Acme"
			dir := t.TempDir()
			var started string
			args := append([]string{"setup", "github-app", "--org", "acme", "-n", "maul-system", "--timeout", "20s",
				"-o", filepath.Join(dir, "maul-github-acme.secret.yaml")}, tt.args...)
			stdout, stderr, err := execSetup(t, g.deps(g.browser(false, &started), nil), args...)
			if err != nil {
				t.Fatalf("setup: %v\n%s", err, stderr)
			}
			forgeName := "acme"
			if strings.Contains(strings.Join(tt.args, " "), "--forge-name") {
				forgeName = "acme-forge"
			}
			want, err := ghapp.ForgeFile(forgeName, "maul-system", "maul-github-acme", "Acme", tt.sa)
			if err != nil {
				t.Fatal(err)
			}
			forgePath := filepath.Join(dir, "maul-github-acme.forge.yaml")
			got, err := os.ReadFile(forgePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("the forge file is\n%s\nwant\n%s", got, want)
			}
			if leaked := g.leaks(string(got) + stdout + stderr); len(leaked) > 0 {
				t.Errorf("the forge file, stdout or stderr leaks %v", leaked)
			}
			next := setupNext{owner: "Acme", org: true, installURL: g.srv.URL + "/apps/maul-acme/installations/new",
				secretFile: filepath.Join(dir, "maul-github-acme.secret.yaml"), forgeFile: forgePath,
				namespace: "maul-system", intents: tt.sa != "", serviceAccount: tt.sa}
			if !strings.Contains(stderr, next.render()) {
				t.Errorf("stderr lacks the next: block\n%s\nin\n%s", next.render(), stderr)
			}
		})
	}
}

// TestSetupGitHubAppForgeOutput: the forge file is pre-checked with the
// Secret file, so neither can fail after the App exists: an existing one
// stops the run before GitHub hears of anything, unless --force; and it can
// be neither stdout nor the Secret's own path.
func TestSetupGitHubAppForgeOutput(t *testing.T) {
	dir := t.TempDir()
	secret, forge := filepath.Join(dir, "s.yaml"), filepath.Join(dir, "f.yaml")
	if err := os.WriteFile(forge, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newFakeAppGitHub(t)
	_, _, err := execSetup(t, g.deps(neverOpen(t), nil), "setup", "github-app", "--org", "acme", "--intents",
		"-o", secret, "--forge-output", forge)
	if !errors.Is(err, ghapp.ErrExists) {
		t.Errorf("an existing forge file = %v, want ErrExists", err)
	}
	if _, created := g.received(); len(created) != 0 {
		t.Errorf("GitHub was asked to create an App: %v", created)
	}
	if _, statErr := os.Stat(secret); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("the Secret file was written")
	}

	var started string
	if _, stderr, err := execSetup(t, g.deps(g.browser(false, &started), nil), "setup", "github-app", "--org",
		"acme", "--intents", "-o", secret, "--forge-output", forge, "--force", "--timeout", "20s"); err != nil {
		t.Fatalf("--force: %v\n%s", err, stderr)
	}
	if got, _ := os.ReadFile(forge); !strings.Contains(string(got), "kind: Forge") {
		t.Errorf("--force did not replace the forge file: %q", got)
	}

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"stdout", []string{"--forge-output", "-"}, "--forge-output"},
		{"the Secret's path", []string{"-o", secret, "--forge-output", dir + "/./s.yaml"}, "--forge-output"},
		{"forge name", []string{"--forge-name", "Acme_Forge"}, "--forge-name"},
		{"service account", []string{"--intent-controller-sa", "Bad SA"}, "--intent-controller-sa"},
		{"no service account", []string{"--intent-controller-sa", ""}, "--intent-controller-sa"},
		{"secret name too long for its Role", []string{"--secret-name", strings.Repeat("a", 240)}, "--secret-name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newFakeAppGitHub(t)
			_, _, err := execSetup(t, g.deps(neverOpen(t), nil),
				append([]string{"setup", "github-app", "--org", "acme", "--intents"}, tt.args...)...)
			if exitCode(err) != ExitUsage || !strings.Contains(fmtErr(err), tt.want) {
				t.Errorf("error = %v (exit %d), want a usage error naming %s", err, exitCode(err), tt.want)
			}
		})
	}
}
