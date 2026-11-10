// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// placesProgram is a run check's program over the places every program at
// a side shares: HOME, TMPDIR and GOCACHE. The poison writes a file in
// each; the look prints clean only when none holds one.
const placesProgram = `package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	for _, d := range []string{os.Getenv("HOME"), os.TempDir(), os.Getenv("GOCACHE")} {
		p := filepath.Join(d, "maul-poison")
		if %s {
			fmt.Println("dirty:", p)
			return
		}
	}
	fmt.Println(%q)
}
`

// placesCheck is a library run check of placesProgram.
func placesCheck(t *testing.T, id, body, want string) string {
	t.Helper()
	return mustJSON(t, map[string]any{"id": id, "outcome": "bb", "form": "run", "run": map[string]any{
		"language": "go",
		"files":    []map[string]string{{"path": "main.go", "content": fmt.Sprintf(placesProgram, body, want)}},
		"expect":   map[string]any{"exit": 0, "stdoutContains": want},
	}})
}

// TestCheckRunsShareNoPlaces: a check run that writes into HOME, TMPDIR
// and GOCACHE leaves nothing the next check run at its side sees, at the
// head and at the base alike; else the second check's verdict would
// depend on the order the two ran in.
func TestCheckRunsShareNoPlaces(t *testing.T) {
	if testing.Short() {
		t.Skip("builds with the go command")
	}
	cfg := liveConfig(t)
	cfg.Workspace.SearchPath += ":/usr/bin:/bin"
	poison := placesCheck(t, "bb.poison", `os.MkdirAll(d, 0o700) != nil || os.WriteFile(p, nil, 0o600) != nil`,
		"poisoned")
	look := placesCheck(t, "bb.look", `func() bool { _, err := os.Lstat(p); return err == nil }()`, "clean")
	for _, side := range []toolwire.Side{"", toolwire.SideBase} {
		t.Run("side "+string(side.Recorded()), func(t *testing.T) {
			head, base := newLiveWorkspace(t), newLiveWorkspace(t)
			mine := head
			if side == toolwire.SideBase {
				mine = base
			}
			h := newHarness(t, cfg, routes{}, func(o *Options) {
				o.Head, o.Base, o.Trees, o.Key = head.client, base.client, head.trees, wsKey
				o.WorkTree = filepath.Join(mine.work, "tree")
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			h.relay.buildTrees(ctx)
			for _, c := range []struct{ name, src string }{{"the poison", poison}, {"the look", look}} {
				_, resp := h.dryRunKeyed(c.src, side)
				if resp.Refused != "" || resp.Observed == nil || resp.Observed.CheckRun == nil {
					t.Fatalf("%s: %+v", c.name, resp)
				}
				if got := resp.Observed.CheckRun.Result; got != toolwire.RunPass {
					t.Errorf("%s: %s, want a pass: %+v", c.name, got, resp.Observed.CheckRun)
				}
			}
		})
	}
}
