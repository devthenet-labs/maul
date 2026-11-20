// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/cmd/maul/internal/ghapp"
	"github.com/devthenet-labs/maul/cmd/maul/internal/projectsetup"
)

// TestSetupProjectNextGolden pins the next: block `setup project` ends
// with: one organization, several accounts on two hosts, and the Project
// on stdout.
func TestSetupProjectNextGolden(t *testing.T) {
	one := projectsetup.Input{Name: "overdub", Namespace: "maul", IntentRepo: "overdubstudio/intents",
		Repos: []string{"overdubstudio/app"}, Approvers: []string{"alice"}}
	multi := projectsetup.Input{Name: "shop", Namespace: "apps", IntentRepo: "acme/intents",
		Repos:     []string{"acme/shop", "web=https://ghe.example.com/team/web", "lib=Acme/lib", "ui=widgets/ui"},
		Approvers: []string{"alice", "bob"}, TriggerLabel: "shop", ApproveLabel: "ship-it"}
	// A GitHub Enterprise Server on its own port: check project reads it
	// only with GH_HOST and an enterprise token, and setup github-app
	// cannot create its App.
	enterprise := projectsetup.Input{Name: "web", Namespace: "maul",
		IntentRepo: "https://ghe.example.com:8443/team/intents",
		Repos:      []string{"https://ghe.example.com:8443/team/web"}, Approvers: []string{"alice"}}
	for _, tt := range []struct {
		name string
		in   projectsetup.Input
		file string
	}{
		{"setup-project-next-one.txt", one, "overdub.project.yaml"},
		{"setup-project-next-multi.txt", multi, "my projects/shop.project.yaml"},
		{"setup-project-next-stdout.txt", one, "-"},
		{"setup-project-next-enterprise.txt", enterprise, "web.project.yaml"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := projectsetup.Plan(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, tt.name, []byte(projectNext(p, tt.file)))
		})
	}
}

// TestSetupProject runs the command: it writes the manifest projectsetup
// renders, says what it wrote and what is next, and replaces an existing
// file only with --force.
func TestSetupProject(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "overdub.project.yaml")
	args := []string{"setup", "project", "overdub", "--intent-repo", "overdubstudio/intents", "--repo",
		"overdubstudio/app", "--approver", "alice", "-o", out}
	stdout, stderr, err := execSetup(t, nil, args...)
	if err != nil {
		t.Fatalf("setup project: %v\n%s", err, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "projectsetup", "testdata", "project-one.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the file is\n%s\nwant\n%s", got, want)
	}
	wrote := "maul: wrote Project maul/overdub to " + out + " (trigger label maul:overdub, approve label " +
		"maul:approved; intent-controller creates both)\n"
	if !strings.HasPrefix(stderr, wrote) {
		t.Errorf("stderr does not start with %q:\n%s", wrote, stderr)
	}
	if !strings.Contains(stderr, "  3. kubectl apply -f "+out+"\n") {
		t.Errorf("stderr does not name the file to apply:\n%s", stderr)
	}

	// Again, as a later change is made: refused, then replaced with --force.
	args[len(args)-3] = "bob"
	if _, _, err := execSetup(t, nil, args...); !errors.Is(err, ghapp.ErrExists) {
		t.Fatalf("a second run = %v, want %v", err, ghapp.ErrExists)
	}
	if _, stderr, err := execSetup(t, nil, append(args, "--force")...); err != nil {
		t.Fatalf("--force: %v\n%s", err, stderr)
	}
	if got, _ := os.ReadFile(out); !strings.Contains(string(got), "- bob\n") {
		t.Errorf("--force did not replace the file:\n%s", got)
	}
}

// TestSetupProjectStdout: -o - writes the manifest to stdout, and -n names
// its namespace.
func TestSetupProjectStdout(t *testing.T) {
	stdout, stderr, err := execSetup(t, nil, "setup", "project", "overdub", "--intent-repo",
		"overdubstudio/intents", "--repo", "overdubstudio/app", "--approver", "alice", "-n", "tools", "-o", "-")
	if err != nil {
		t.Fatalf("setup project: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "kind: Project\n") || !strings.Contains(stdout, "  namespace: tools\n") {
		t.Errorf("stdout is not the Project in tools:\n%s", stdout)
	}
	if !strings.Contains(stderr, "maul: wrote Project tools/overdub to stdout") ||
		!strings.Contains(stderr, "maul check project overdub -n tools") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

// TestSetupProjectRefuses: a usage error, and no file, for what the CRD
// would refuse or the command cannot read.
func TestSetupProjectRefuses(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no name", []string{"--intent-repo", "o/i", "--repo", "o/a", "--approver", "al"}, "accepts 1 arg"},
		{"name too long", []string{strings.Repeat("a", 26), "--intent-repo", "o/i", "--repo", "o/a", "--approver",
			"al"}, "at most 25"},
		{"no intent repository", []string{"p", "--repo", "o/a", "--approver", "al"}, "--intent-repo"},
		{"bad key", []string{"p", "--intent-repo", "o/i", "--repo", "o/My.App", "--approver", "al"},
			"write --repo <short>=owner/repo"},
		{"namespace", []string{"p", "--intent-repo", "o/i", "--repo", "o/a", "--approver", "al", "-n", "A"}, "-n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			args := append([]string{"setup", "project"}, tt.args...)
			_, _, err := execSetup(t, nil, append(args, "-o", filepath.Join(dir, "p.yaml"))...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			// cobra's own argument count error keeps its exit code, as for
			// every other command.
			if tt.name != "no name" && exitCode(err) != ExitUsage {
				t.Errorf("exit code = %d, want the usage error's", exitCode(err))
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("a refused run wrote %v", entries)
			}
		})
	}
}
