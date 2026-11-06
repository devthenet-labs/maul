// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// toolPodGoldens are internal/jobs' rendered agents-namespace tool pods.
var toolPodGoldens = []string{"toolpod_default.yaml", "toolpod_profile_go_tunnel.yaml"}

// TestToolPodContract holds the inits internal/jobs renders (S2-10) to what
// this binary parses: prepare's flags and mounts, each seed's side, UID and
// mounts, and the sandboxes' tool-exec path where prepare installs it.
func TestToolPodContract(t *testing.T) {
	for _, name := range toolPodGoldens {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "jobs", "testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			var job batchv1.Job
			if err := yaml.Unmarshal(raw, &job); err != nil {
				t.Fatal(err)
			}
			pod := job.Spec.Template.Spec
			names := make([]string, 0, len(pod.InitContainers))
			var bin string
			for _, c := range pod.InitContainers {
				names = append(names, c.Name)
				switch {
				case c.Name == "prepare":
					bin = checkPrepareInit(t, c)
				case strings.HasPrefix(c.Name, "seed-"):
					checkSeedInit(t, c)
				case strings.HasPrefix(c.Name, "sandbox-"):
					if want := filepath.Join(bin, toolExecName); !slices.Equal(c.Command, []string{want}) {
						t.Errorf("%s command %v, want [%s], where prepare installs it", c.Name, c.Command, want)
					}
				}
			}
			want := []string{"prepare", "seed-head", "seed-base", "sandbox-head", "sandbox-base"}
			if !slices.Equal(names, want) {
				t.Errorf("init order %v, want %v", names, want)
			}
		})
	}
}

// checkPrepareInit parses prepare's arguments as the binary would and
// returns its bin directory.
func checkPrepareInit(t *testing.T, c corev1.Container) string {
	t.Helper()
	if len(c.Args) == 0 || c.Args[0] != prepareCommand {
		t.Fatalf("prepare args %v do not start with %s", c.Args, prepareCommand)
	}
	fl, err := parsePrepare(c.Args[1:])
	if err != nil {
		t.Fatalf("prepare args %v: %v", c.Args, err)
	}
	mounts := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = !m.ReadOnly
	}
	for _, p := range []string{filepath.Join(fl.trees, "head"), filepath.Join(fl.trees, "base"), fl.bin, "/tmp"} {
		if !mounts[p] {
			t.Errorf("prepare writes %s, which it does not mount writable (%v)", p, c.VolumeMounts)
		}
	}
	return fl.bin
}

// checkSeedInit parses a seed's arguments and holds its UID and mounts to
// the side it names.
func checkSeedInit(t *testing.T, c corev1.Container) {
	t.Helper()
	if len(c.Args) == 0 || c.Args[0] != seedCommand {
		t.Fatalf("%s args %v do not start with %s", c.Name, c.Args, seedCommand)
	}
	side, err := parseSeed(c.Args[1:])
	if err != nil {
		t.Fatalf("%s args %v: %v", c.Name, c.Args, err)
	}
	if c.Name != "seed-"+side {
		t.Errorf("%s seeds %s", c.Name, side)
	}
	if uid := c.SecurityContext.RunAsUser; uid == nil || int(*uid) != sideUIDs[side] {
		t.Errorf("%s runs as %v, want %d", c.Name, uid, sideUIDs[side])
	}
	want := map[string]bool{filepath.Join(seedTrees, side): true, seedWork: false}
	if len(c.VolumeMounts) != len(want) {
		t.Errorf("%s mounts %v, want only %v", c.Name, c.VolumeMounts, want)
	}
	for _, m := range c.VolumeMounts {
		if ro, ok := want[m.MountPath]; !ok || ro != m.ReadOnly {
			t.Errorf("%s mounts %s (read-only %v), want %v", c.Name, m.MountPath, m.ReadOnly, want)
		}
	}
}
