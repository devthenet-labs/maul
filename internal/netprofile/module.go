// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"fmt"
	"regexp"
	"strings"
)

// prefixPattern is the grammar of a module-path prefix: a dotted lowercase
// host element (at least two labels), then path elements of a module
// path's characters, with an optional trailing slash. It is the chart
// schema's pattern for agent.networkProfiles.*.modules items.
var prefixPattern = regexp.MustCompile(
	`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(/[A-Za-z0-9._~+-]+)*/?$`)

// toolchainHost is the Go project's host. A prefix that is its root admits
// the toolchain modules (golang.org/toolchain), which GOTOOLCHAIN=local keeps
// the go command from fetching; refusing such prefixes is the belt to that
// pair of braces.
const toolchainHost = "golang.org"

// ValidPrefix reports whether p is an acceptable module-path prefix: the
// grammar above, and neither the root of golang.org nor anything starting
// golang.org/toolchain.
func ValidPrefix(p string) error {
	if !prefixPattern.MatchString(p) {
		return fmt.Errorf("module prefix %q is not a lowercase host followed by module path elements", p)
	}
	trimmed := strings.TrimSuffix(p, "/")
	if trimmed == toolchainHost || strings.HasPrefix(trimmed, toolchainHost+"/toolchain") {
		return fmt.Errorf("module prefix %q would admit the Go toolchain modules; "+
			"name the golang.org paths needed (golang.org/x/...)", p)
	}
	return nil
}

// Prefixes is a module allowlist: each entry admits itself and every path
// below it.
type Prefixes []string

// Allows reports whether modulePath is one of the prefixes or below one on a
// path-element boundary: golang.org/x admits golang.org/x and
// golang.org/x/mod, never golang.org/xyz. A trailing slash on a prefix makes
// no difference. The comparison is exact (case-sensitive), as module paths
// are.
func (ps Prefixes) Allows(modulePath string) bool {
	if modulePath == "" {
		return false
	}
	for _, p := range ps {
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			continue
		}
		if modulePath == p || strings.HasPrefix(modulePath, p+"/") {
			return true
		}
	}
	return false
}

// String is the comma-separated list, trailing slashes dropped, for a
// refusal message.
func (ps Prefixes) String() string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, strings.TrimSuffix(p, "/"))
	}
	return strings.Join(out, ",")
}
