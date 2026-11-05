// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeProc writes a proc filesystem of the given status files, by pid.
func fakeProc(t *testing.T, entries map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for pid, status := range entries {
		if err := os.MkdirAll(filepath.Join(dir, pid), 0o700); err != nil {
			t.Fatal(err)
		}
		if status == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, pid, "status"), []byte(status), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func status(state string, real, effective int) string {
	return fmt.Sprintf("Name:\tx\nUmask:\t0022\nState:\t%s\nTgid:\t1\nUid:\t%d\t%d\t%d\t%d\nGid:\t0\t0\t0\t0\n",
		state, real, effective, effective, effective)
}

// TestSurvivors: every live process of the UID but the shim itself and
// pid 1, by real or effective UID; zombies, other UIDs and entries that
// are not processes are not.
func TestSurvivors(t *testing.T) {
	const uid = 65540
	proc := fakeProc(t, map[string]string{
		"1":     status("S (sleeping)", uid, uid),
		"7":     status("S (sleeping)", uid, uid),
		"8":     status("R (running)", uid, uid),
		"9":     status("Z (zombie)", uid, uid),
		"10":    status("X (dead)", uid, uid),
		"11":    status("S (sleeping)", 65541, 65541),
		"12":    status("S (sleeping)", 0, 0),
		"13":    status("S (sleeping)", 65541, uid),
		"14":    status("S (sleeping)", uid, 65541),
		"15":    "", // gone between the listing and the read
		"16":    status("D (disk sleep)", uid, uid),
		"17":    "Name:\tx\nState:\tS (sleeping)\n", // no Uid line
		"self":  status("S (sleeping)", uid, uid),
		"0x1f":  status("S (sleeping)", uid, uid),
		"sys":   "",
		"00018": status("S (sleeping)", uid, uid),
	})
	sw := sweeper{proc: proc, uid: uid, self: 7}
	got, err := sw.survivors()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []int{8, 13, 14, 16}; !slices.Equal(got, want) {
		t.Errorf("survivors %v, want %v", got, want)
	}
}

// TestSweep: the sweep kills every survivor until none is left, and says
// so when some never die.
func TestSweep(t *testing.T) {
	const uid = 65540
	proc := fakeProc(t, map[string]string{
		"20": status("S (sleeping)", uid, uid),
		"21": status("S (sleeping)", uid, uid),
	})
	var killed []int
	sw := sweeper{proc: proc, uid: uid, self: 1, rounds: 5, kill: func(pid int) error {
		killed = append(killed, pid)
		return os.RemoveAll(filepath.Join(proc, fmt.Sprint(pid)))
	}}
	if err := sw.run(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	slices.Sort(killed)
	if !slices.Equal(killed, []int{20, 21}) {
		t.Errorf("killed %v", killed)
	}

	proc = fakeProc(t, map[string]string{"22": status("S (sleeping)", uid, uid)})
	sw = sweeper{proc: proc, uid: uid, self: 1, rounds: 3, kill: func(int) error { return nil }}
	if err := sw.run(); err == nil {
		t.Error("a process that never dies was swept clean")
	}

	sw = sweeper{proc: filepath.Join(t.TempDir(), "none"), uid: uid, self: 1, rounds: 3,
		kill: func(int) error { return nil }}
	if err := sw.run(); err == nil {
		t.Error("an unreadable proc filesystem was swept clean")
	}
}
