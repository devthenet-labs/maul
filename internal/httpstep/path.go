// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// MaxPathBytes bounds a request path with its query.
	MaxPathBytes = 512
	// MaxPointerBytes bounds a JSON pointer.
	MaxPointerBytes = 128
)

// CheckPath reports whether p is a request path (with an optional query)
// the tester may send to a component's Service unchanged. It is built so
// that no admitted path can name another authority or climb out of the
// component's prefix: it starts with exactly one "/"; it holds only
// printable ASCII from RFC 3986's path and query characters (so no space,
// backslash, "#" or non-ASCII); no "//", "@" or "://" anywhere; no "." or
// ".." segment; and percent escapes are well formed and never encode a
// control character, a dot, a slash or a backslash, which a server might
// decode back into one.
func CheckPath(p string) error {
	switch {
	case len(p) > MaxPathBytes:
		return fmt.Errorf("path is over %d bytes", MaxPathBytes)
	case !strings.HasPrefix(p, "/"):
		return errors.New(`path must start with "/"`)
	case strings.Contains(p, "//"):
		return errors.New(`path may not contain "//"`)
	case strings.Contains(p, "@"):
		return errors.New(`path may not contain "@"`)
	}
	for i := 0; i < len(p); i++ {
		if !pathByte(p[i]) {
			return fmt.Errorf("path may not contain %q", p[i])
		}
		if p[i] == '%' {
			if err := checkEscape(p[i:]); err != nil {
				return err
			}
		}
	}
	path, _, _ := strings.Cut(p, "?")
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return errors.New(`path may not hold a "." or ".." segment`)
		}
	}
	return nil
}

// pathByte admits RFC 3986's unreserved and sub-delimiter characters, ":",
// "/", "?" and "%". "@" (userinfo) and "#" (fragment) are left out.
func pathByte(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:/?%", b) >= 0
}

// checkEscape checks the percent escape at the start of s.
func checkEscape(s string) error {
	if len(s) < 3 || !isHex(s[1]) || !isHex(s[2]) {
		return errors.New("path holds a malformed percent escape")
	}
	n, _ := strconv.ParseUint(s[1:3], 16, 8)
	switch b := byte(n); {
	case b < 0x20, b >= 0x7f, b == '.', b == '/', b == '\\':
		return fmt.Errorf("path may not percent-encode %q", b)
	}
	return nil
}

func isHex(b byte) bool {
	return '0' <= b && b <= '9' || 'a' <= b && b <= 'f' || 'A' <= b && b <= 'F'
}

// CheckPointer reports whether p is an RFC 6901 JSON pointer: empty (the
// whole document) or "/"-separated reference tokens in which "~" appears
// only as "~0" or "~1", with no control character.
func CheckPointer(p string) error {
	switch {
	case len(p) > MaxPointerBytes:
		return fmt.Errorf("pointer is over %d bytes", MaxPointerBytes)
	case p == "":
		return nil
	case p[0] != '/':
		return fmt.Errorf(`pointer %q must be empty or start with "/"`, p)
	}
	for i := 0; i < len(p); i++ {
		switch {
		case p[i] < 0x20 || p[i] == 0x7f:
			return fmt.Errorf("pointer %q holds a control character", p)
		case p[i] == '~' && (i+1 == len(p) || (p[i+1] != '0' && p[i+1] != '1')):
			return fmt.Errorf(`pointer %q holds "~" not followed by 0 or 1`, p)
		}
	}
	return nil
}
