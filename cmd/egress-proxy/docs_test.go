// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/cli"
)

// egressProxyDoc is the operator page whose debugging commands this test
// keeps honest.
const egressProxyDoc = "../../docs/configuration/egress-proxy.md"

// TestDocsDebugCommands holds the commands docs/configuration/egress-proxy.md
// gives an operator without a log sink to what kubectl and this binary do:
//
//   - every `kubectl … logs` with a label selector (-l) carries --tail=-1,
//     because with a selector kubectl shows only the last 10 lines per pod
//     by default and the audit line wanted is rarely among them;
//   - every `kubectl … exec` runs /usr/local/bin/controller probe (the
//     image is distroless and its binary is /usr/local/bin/controller), and
//     each flag it passes is one the probe command defines.
//
// Both kinds must appear, so the page cannot lose its runbook unnoticed.
func TestDocsDebugCommands(t *testing.T) {
	raw, err := os.ReadFile(egressProxyDoc)
	if err != nil {
		t.Fatal(err)
	}
	probe := newProbeCmd(cli.NewOptions())
	flagPattern := regexp.MustCompile(`(?:^|\s)--([a-z][a-z0-9-]*)`)
	var logs, execs int
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\\\n", " "), "\n") {
		for _, cmd := range kubectlCommands(line) {
			fields := strings.Fields(cmd)
			switch {
			case has(fields, "logs") && has(fields, "-l"):
				logs++
				if !has(fields, "--tail=-1") {
					t.Errorf("%s: a label-selected kubectl logs without --tail=-1 shows 10 lines per pod: %s",
						egressProxyDoc, cmd)
				}
			case has(fields, "exec"):
				execs++
				_, after, ok := strings.Cut(cmd, "/usr/local/bin/controller probe")
				if !ok {
					t.Errorf("%s: kubectl exec does not run /usr/local/bin/controller probe: %s", egressProxyDoc, cmd)
					continue
				}
				for _, m := range flagPattern.FindAllStringSubmatch(after, -1) {
					if probe.Flags().Lookup(m[1]) == nil {
						t.Errorf("%s: probe has no --%s flag: %s", egressProxyDoc, m[1], cmd)
					}
				}
			}
		}
	}
	if logs == 0 || execs == 0 {
		t.Errorf("%s gives %d label-selected kubectl logs and %d kubectl exec commands; want both", egressProxyDoc,
			logs, execs)
	}
}

// kubectlCommands are the kubectl invocations on a line, each up to the
// next pipe, backtick or redirection.
func kubectlCommands(line string) []string {
	var out []string
	for {
		i := strings.Index(line, "kubectl ")
		if i < 0 {
			return out
		}
		line = line[i:]
		end := strings.IndexAny(line, "|`>;")
		if end < 0 {
			end = len(line)
		}
		out = append(out, line[:end])
		line = line[end:]
	}
}

// has reports whether fields holds s.
func has(fields []string, s string) bool {
	for _, f := range fields {
		if f == s {
			return true
		}
	}
	return false
}
