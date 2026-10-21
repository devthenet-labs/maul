// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"slices"
	"strings"

	"github.com/devthenet-labs/maul/internal/provider"
)

// reservedEnvPrefixes are the name prefixes an image may not set: maul's
// own configuration surface and every variable the trusted CLIs read, the
// claude CLI's (ANTHROPIC_, CLAUDE_) and, since a repository image may also
// run the codex CLI, codex's (CODEX_, whose CODEX_HOME would point it at the
// image's own config.toml, AGENTS.md and auth files, and OPENAI_).
// CLAUDE_CODE_ is listed for fidelity with the contract even though CLAUDE_
// already covers it.
var reservedEnvPrefixes = []string{"MAUL_", "ANTHROPIC_", "CLAUDE_", "CLAUDE_CODE_", "CODEX_", "OPENAI_"}

// proxyEnv are the proxy variables, refused in either case (and any mixed
// case, which curl and Go both honour for some of them), because a proxy set
// by the image would redirect the broker traffic.
var proxyEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY"}

// ReservedEnvName reports whether an image may not set name: it is one of
// extra (the caller passes the Job's own reserved names, which the image
// must not shadow), a provider gateway name, carries a reserved prefix, or is
// a proxy variable in any case.
func ReservedEnvName(name string, extra map[string]bool) bool {
	if extra[name] || slices.Contains(provider.GatewayEnvNames, name) {
		return true
	}
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return slices.Contains(proxyEnv, strings.ToUpper(name))
}

// CheckEnv rejects an image config Env ("KEY=VALUE" entries) that names any
// reserved variable. The message lists every offender, sorted, so the owner
// fixes the image once.
func CheckEnv(env []string, extra map[string]bool) error {
	var offenders []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if ReservedEnvName(name, extra) && !slices.Contains(offenders, name) {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	slices.Sort(offenders)
	return reject("image ENV sets `%s`; `MAUL_*`, `ANTHROPIC_*`, `CLAUDE_*`, `CODEX_*`, `OPENAI_*`, proxy "+
		"and maul-reserved variables cannot come from the image", strings.Join(offenders, "`, `"))
}
