// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"regexp"
	"strings"

	"golang.org/x/mod/module"
)

// Route names the upstream a request goes to.
type Route string

// The two routes: the module proxy and the checksum database.
const (
	RouteGo    Route = "go"
	RouteSumDB Route = "sumdb"
)

// Kind is what a request asks for.
type Kind string

// The request kinds of the GOPROXY and checksum-database protocols. Latest
// is the module proxy's @latest on RouteGo and the checksum database's
// signed tree head on RouteSumDB.
const (
	KindList      Kind = "list"
	KindInfo      Kind = "info"
	KindMod       Kind = "mod"
	KindZip       Kind = "zip"
	KindLatest    Kind = "latest"
	KindSupported Kind = "supported"
	KindLookup    Kind = "lookup"
	KindTile      Kind = "tile"
)

// SumDBName is the only checksum database the proxy serves.
const SumDBName = "sum.golang.org"

// MaxPathLen bounds a request path, the listener's /go prefix included.
const MaxPathLen = 1024

// maxVersionLen bounds an escaped version.
const maxVersionLen = 128

// Request is a parsed GOPROXY or checksum-database request.
type Request struct {
	Route Route
	Kind  Kind
	// Module is the unescaped module path; "" for a kind that names none.
	Module string
	// Version is the unescaped version; "" for a kind that names none.
	Version string
	// Upstream is the validated, still-escaped path below the upstream's
	// base URL, exactly as it arrived: the forward concatenates it to the
	// fixed base and never re-encodes it from the parsed values.
	Upstream string
}

// ModuleChecked reports whether the kind names a module, so the profile's
// allowlist applies.
func (r Request) ModuleChecked() bool { return r.Module != "" }

// VersionGated reports whether the kind names one version, so the version
// gate applies: .info, .mod, .zip and the checksum database's lookup. list
// and @latest carry no version.
func (r Request) VersionGated() bool {
	switch r.Kind {
	case KindInfo, KindMod, KindZip, KindLookup:
		return true
	default:
		return false
	}
}

// PathError is a refused path. NotFound marks a path outside the protocol
// (404); every other refusal is a well-formed request the proxy will not
// forward (410). Rule names the rule, for the response body and the audit
// line.
type PathError struct {
	NotFound bool
	Rule     string
}

func (e *PathError) Error() string { return e.Rule }

func badPath(rule string) error { return &PathError{Rule: rule} }

// tilePattern is the checksum database's tile grammar below the database's
// root.
var tilePattern = regexp.MustCompile(`^tile/[0-9]+/(data|[0-9]+)/(x[0-9]{3}/)*[0-9]{3}(\.p/[0-9]+)?$`)

// ParseGoProxy parses a module-proxy path below the listener's /go/ prefix
// (no leading slash): <escmod>/@v/list, <escmod>/@v/<escver>.info|.mod|.zip
// or <escmod>/@latest. Every refusal is a *PathError for a 410.
func ParseGoProxy(p string) (Request, error) {
	if err := checkShape(p); err != nil {
		return Request{}, err
	}
	req := Request{Route: RouteGo, Upstream: p}
	var escMod, escVer string
	if mod, ok := strings.CutSuffix(p, "/@latest"); ok {
		escMod, req.Kind = mod, KindLatest
	} else {
		i := strings.LastIndex(p, "/@v/")
		if i < 0 {
			return Request{}, badPath("not a module proxy path (<module>/@v/... or <module>/@latest)")
		}
		escMod = p[:i]
		file := p[i+len("/@v/"):]
		switch {
		case file == "list":
			req.Kind = KindList
		case strings.HasSuffix(file, ".info"):
			req.Kind, escVer = KindInfo, strings.TrimSuffix(file, ".info")
		case strings.HasSuffix(file, ".mod"):
			req.Kind, escVer = KindMod, strings.TrimSuffix(file, ".mod")
		case strings.HasSuffix(file, ".zip"):
			req.Kind, escVer = KindZip, strings.TrimSuffix(file, ".zip")
		default:
			return Request{}, badPath("not a module proxy file (list, .info, .mod, .zip)")
		}
	}
	m, err := parseModule(escMod)
	if err != nil {
		return Request{}, err
	}
	req.Module = m
	if req.Kind == KindInfo || req.Kind == KindMod || req.Kind == KindZip {
		if req.Version, err = parseVersion(escVer); err != nil {
			return Request{}, err
		}
	}
	return req, nil
}

// ParseSumDB parses a checksum-database path below the listener's
// /go/sumdb/ prefix (no leading slash): <name>/supported, <name>/latest,
// <name>/lookup/<escmod>@<escver> or <name>/tile/.... A database other
// than SumDBName is a NotFound *PathError (404); every other refusal is a
// 410.
func ParseSumDB(p string) (Request, error) {
	name, rest, _ := strings.Cut(p, "/")
	if name != SumDBName {
		return Request{}, &PathError{NotFound: true, Rule: "unknown checksum database"}
	}
	if err := checkShape(p); err != nil {
		return Request{}, err
	}
	req := Request{Route: RouteSumDB, Upstream: rest}
	switch {
	case rest == "supported":
		req.Kind = KindSupported
	case rest == "latest":
		req.Kind = KindLatest
	case strings.HasPrefix(rest, "lookup/"):
		escMod, escVer, ok := strings.Cut(strings.TrimPrefix(rest, "lookup/"), "@")
		if !ok {
			return Request{}, badPath("lookup is not <module>@<version>")
		}
		m, err := parseModule(escMod)
		if err != nil {
			return Request{}, err
		}
		v, err := parseVersion(escVer)
		if err != nil {
			return Request{}, err
		}
		req.Kind, req.Module, req.Version = KindLookup, m, v
	case tilePattern.MatchString(rest):
		req.Kind = KindTile
	default:
		return Request{}, badPath("not a checksum database path (supported, latest, lookup, tile)")
	}
	return req, nil
}

// checkShape refuses an over-long path and any empty, "." or ".." element
// (so no //, /./, /../ and no trailing slash or dot segment).
func checkShape(p string) error {
	if len(p) > MaxPathLen {
		return badPath("path too long")
	}
	for elem := range strings.SplitSeq(p, "/") {
		if elem == "" || elem == "." || elem == ".." {
			return badPath("empty or dot path element")
		}
	}
	return nil
}

// moduleByteOK is the escaped module-path alphabet: module.EscapePath's
// output over modPathOK's and importPathOK's characters (lowercase, since
// an uppercase letter escapes to '!' and its lowercase).
func moduleByteOK(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("._~+!/-", c) >= 0
}

// versionByteOK is module.EscapeVersion's output alphabet: fileNameOK's
// characters without '%' (and without '/').
func versionByteOK(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("!#$&()+,-.=@[]^_{}~", c) >= 0
}

// parseModule checks an escaped module path byte by byte, then unescapes
// it. A module path must pass module.CheckPath; the prefix probes the go
// command sends while resolving an import (github.com, github.com/google)
// need only pass module.CheckImportPath.
func parseModule(esc string) (string, error) {
	if esc == "" {
		return "", badPath("empty module path")
	}
	for i := range len(esc) {
		if !moduleByteOK(esc[i]) {
			return "", badPath("module path has a character outside the escaped module-path alphabet")
		}
	}
	if m, err := module.UnescapePath(esc); err == nil {
		return m, nil
	}
	m, ok := unescape(esc)
	if !ok || module.CheckImportPath(m) != nil {
		return "", badPath("invalid module path")
	}
	return m, nil
}

// parseVersion checks an escaped version byte by byte, then unescapes it.
func parseVersion(esc string) (string, error) {
	if esc == "" || len(esc) > maxVersionLen {
		return "", badPath("version is empty or too long")
	}
	for i := range len(esc) {
		if !versionByteOK(esc[i]) {
			return "", badPath("version has a character outside the escaped version alphabet")
		}
	}
	v, err := module.UnescapeVersion(esc)
	if err != nil {
		return "", badPath("invalid version")
	}
	return v, nil
}

// unescape reverses module.EscapePath's case-encoding ("!x" is "X") without
// its module-path check, refusing an uppercase letter, a '!' not before a
// lowercase letter, and non-ASCII: the same rules as x/mod's own unescape.
func unescape(esc string) (string, bool) {
	buf := make([]byte, 0, len(esc))
	bang := false
	for i := range len(esc) {
		c := esc[i]
		switch {
		case c >= 0x80, 'A' <= c && c <= 'Z':
			return "", false
		case bang:
			if c < 'a' || c > 'z' {
				return "", false
			}
			buf = append(buf, c-'a'+'A')
			bang = false
		case c == '!':
			bang = true
		default:
			buf = append(buf, c)
		}
	}
	return string(buf), !bang
}

// VersionAllowed is the version gate: v must be a canonical semantic
// version (module.CanonicalVersion keeps +incompatible), and not a
// pseudo-version unless allowPseudo. Branch names, commit hashes, partial
// versions and free-form strings all fail.
func VersionAllowed(v string, allowPseudo bool) bool {
	if v == "" || module.CanonicalVersion(v) != v {
		return false
	}
	return allowPseudo || !module.IsPseudoVersion(v)
}
