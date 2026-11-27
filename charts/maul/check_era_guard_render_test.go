// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCheckEraGuardRendersNothingOffline: the check-era upgrade guard is a
// lookup, and a render without a cluster (helm template, Argo CD, Flux)
// sees nothing: it never fails such a render, with the evaluator off or on,
// and never renders an object. Its fail path needs a cluster and is
// TestCheckEraUpgradeAgainstAPIServer's.
func TestCheckEraGuardRendersNothingOffline(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for name, args := range map[string][]string{"defaults": nil, "the evaluator": verificationArgs} {
		all := append([]string{"template", "maul", ".", "--namespace", "maul",
			"--show-only", "templates/check-era-guard.yaml"}, args...)
		out, err := exec.CommandContext(t.Context(), "helm", all...).CombinedOutput()
		if err != nil {
			t.Errorf("%s: offline render of the guard: %v\n%s", name, err, out)
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") && line != "---" {
				t.Errorf("%s: the guard rendered %q, want nothing", name, line)
			}
		}
	}
}
