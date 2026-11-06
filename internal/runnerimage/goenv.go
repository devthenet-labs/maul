// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerimage

import (
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// goEnvNames are the Go toolchain variables an image config may bake that
// a program run outside the image's own environment needs to find the
// image's toolchain and its module cache (SPEC-S2 S2-13 step 0): the
// agents tool pod's exec shim passes a program none of the image's
// environment, so a Go image with no network profile builds against its
// baked GOMODCACHE only when it is named.
var goEnvNames = []string{"GOMODCACHE", "GOPATH", "GOROOT"}

// MaxGoEnvValue bounds one recorded value (the Repository schema's bound).
const MaxGoEnvValue = 4096

// GoEnvName reports a variable GoEnv records.
func GoEnvName(name string) bool { return slices.Contains(goEnvNames, name) }

// GoEnv reads an image config's Env ("KEY=VALUE" entries; the last of a
// name wins, as in the runtime) for the Go toolchain's directories: each
// of goEnvNames whose value is one absolute directory other than /, of
// printable characters with no list separator, within MaxGoEnvValue,
// recorded cleaned. Anything else is left out, never rejected: the image
// stays accepted, and a program that needed the value fails to build (a
// run error, never a verdict). nil when none is recorded.
func GoEnv(env []string) map[string]string {
	var out map[string]string
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !GoEnvName(name) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		if v, ok := goDir(value); ok {
			out[name] = v
		} else {
			delete(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// goDir is v as one clean absolute directory other than /, or false.
func goDir(v string) (string, bool) {
	if !filepath.IsAbs(v) || strings.ContainsRune(v, filepath.ListSeparator) ||
		strings.IndexFunc(v, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return "", false
	}
	v = filepath.Clean(v)
	if v == "/" || len(v) > MaxGoEnvValue {
		return "", false
	}
	return v, true
}
