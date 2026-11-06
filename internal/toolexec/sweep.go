// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Procfs is the proc filesystem a workspace shim's sweep reads.
const Procfs = "/proc"

// The sweep's bounds: rounds of kills sweepPause apart, about a second
// in all, before the shim gives up and calls the sandbox unclean.
const (
	sweepRounds = 100
	sweepPause  = 10 * time.Millisecond
)

// sweeper kills every process of the sandbox's UID but the shim (SPEC S2-07,
// 8a): what a program left running in a session or group of its own, a
// loopback listener above all, must not live into the next run, which may
// be the other side's comparison. The sandbox container runs nothing else
// of its UID (shareProcessNamespace is off), so every such process is a
// program's.
type sweeper struct {
	// proc is the proc filesystem; only its entries are ever killed.
	proc string
	// uid is the sandbox's, self the shim's own pid.
	uid, self int
	// rounds bounds the kill rounds; sweepRounds when zero.
	rounds int
	// kill sends SIGKILL to one process; reap collects orphans the shim
	// inherited as pid 1. Either may be nil in a test (reap) or is set to
	// the real ones by NewServer.
	kill func(pid int) error
	reap func()
}

// run kills survivors until none is left, an error when some outlive every
// round or the proc filesystem cannot be read.
func (s *sweeper) run() error {
	rounds := s.rounds
	if rounds <= 0 {
		rounds = sweepRounds
	}
	for round := 0; ; round++ {
		if s.reap != nil {
			s.reap()
		}
		pids, err := s.survivors()
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		if round == rounds {
			return fmt.Errorf("toolexec: %d processes of uid %d survive", len(pids), s.uid)
		}
		for _, pid := range pids {
			_ = s.kill(pid)
		}
		time.Sleep(sweepPause)
	}
}

// survivors lists the live processes of the sandbox's UID, by real or
// effective UID, but the shim and pid 1. A zombie is already dead, holding
// no port and running no code.
func (s *sweeper) survivors() ([]int, error) {
	entries, err := os.ReadDir(s.proc)
	if err != nil {
		return nil, fmt.Errorf("toolexec: read %s: %w", s.proc, err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == s.self || strconv.Itoa(pid) != e.Name() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.proc, e.Name(), "status"))
		if err != nil {
			continue // gone since the listing
		}
		if live, uids := parseStatus(raw); live && (uids[0] == s.uid || uids[1] == s.uid) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// parseStatus reads a /proc/<pid>/status: whether the process is live (not
// a zombie or dead) and its real and effective UIDs (-1 when absent).
func parseStatus(raw []byte) (bool, [2]int) {
	live, uids := false, [2]int{-1, -1}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(val)
		switch {
		case key == "State" && len(fields) > 0:
			live = fields[0] != "Z" && fields[0] != "X"
		case key == "Uid" && len(fields) >= 2:
			for i := range uids {
				if n, err := strconv.Atoi(fields[i]); err == nil {
					uids[i] = n
				}
			}
		}
	}
	return live, uids
}
