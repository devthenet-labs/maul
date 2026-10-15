// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrBadPath is wrapped by every refusal of the npm and pip path grammars.
var ErrBadPath = errors.New("path is not in the registry route's grammar")

// ErrPrerelease is wrapped by NPMVersion's refusal of a prerelease or build
// suffix the grant does not admit.
var ErrPrerelease = errors.New("prerelease and build versions are not admitted")

// npm name limits.
const (
	// MaxNPMNameLen is npm's limit on a full package name, scope included.
	MaxNPMNameLen = 214
	// maxNPMVersionLen and maxNPMPrereleaseVersionLen bound a tarball
	// version without and with a prerelease or build suffix.
	maxNPMVersionLen           = 32
	maxNPMPrereleaseVersionLen = 64
	maxNPMVersionField         = 10
)

var (
	npmScopePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._~-]{0,213}$`)
	npmBarePattern  = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,214}$`)
	// npmNumeric is one MAJOR, MINOR or PATCH field: no leading zero.
	npmNumeric = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})$`)
	// semverSuffix is a semver prerelease and/or build suffix.
	semverSuffix = regexp.MustCompile(
		`^(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
			`(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
)

// npmBareName judges the unscoped part of a name.
func npmBareName(name string) error {
	switch {
	case !npmBarePattern.MatchString(name):
		return fmt.Errorf("npm package name %q has a character outside [A-Za-z0-9._~-] or is too long", name)
	case strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_"):
		return fmt.Errorf("npm package name %q starts with . or _", name)
	case strings.EqualFold(name, "node_modules") || strings.EqualFold(name, "favicon.ico"):
		return fmt.Errorf("npm package name %q is reserved", name)
	}
	return nil
}

// NPMName parses a decoded npm package name, "name" or "@scope/name", into
// its scope ("" when unscoped) and name. The scope is lowercase
// [a-z0-9._~-]; the name admits upper case for legacy packages
// (JSONStream). Neither starts with . or _, neither is node_modules or
// favicon.ico, and the whole is at most MaxNPMNameLen bytes.
func NPMName(full string) (scope, name string, err error) {
	if len(full) > MaxNPMNameLen {
		return "", "", fmt.Errorf("npm package name %q is over %d bytes", full, MaxNPMNameLen)
	}
	if rest, ok := strings.CutPrefix(full, "@"); ok {
		var found bool
		scope, name, found = strings.Cut(rest, "/")
		if !found {
			return "", "", fmt.Errorf("npm package name %q has a scope and no name", full)
		}
		if !npmScopePattern.MatchString(scope) || strings.HasPrefix(scope, ".") || strings.HasPrefix(scope, "_") {
			return "", "", fmt.Errorf("npm scope %q is not lowercase [a-z0-9._~-] starting with a letter or digit",
				scope)
		}
		if err := npmBareName(name); err != nil {
			return "", "", err
		}
		return scope, name, nil
	}
	if err := npmBareName(full); err != nil {
		return "", "", err
	}
	return "", full, nil
}

// NPMFullName is scope and name joined back into "@scope/name" or "name".
func NPMFullName(scope, name string) string {
	if scope == "" {
		return name
	}
	return "@" + scope + "/" + name
}

// NPMVersion judges a tarball version: MAJOR.MINOR.PATCH, each a decimal of
// at most ten digits with no leading zero, at most 32 bytes in all. A
// prerelease or build suffix is refused with ErrPrerelease unless
// allowPrerelease; then it must follow semver's grammar and the whole
// version is at most 64 bytes.
func NPMVersion(v string, allowPrerelease bool) error {
	core, suffix := v, ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		core, suffix = v[:i], v[i:]
	}
	fields := strings.Split(core, ".")
	if len(fields) != 3 {
		return fmt.Errorf("%w: npm version %q is not MAJOR.MINOR.PATCH", ErrBadPath, v)
	}
	for _, f := range fields {
		if !npmNumeric.MatchString(f) || len(f) > maxNPMVersionField {
			return fmt.Errorf("%w: npm version %q has a field that is not a decimal of at most %d digits "+
				"without a leading zero", ErrBadPath, v, maxNPMVersionField)
		}
	}
	if suffix == "" {
		if len(v) > maxNPMVersionLen {
			return fmt.Errorf("%w: npm version %q is over %d bytes", ErrBadPath, v, maxNPMVersionLen)
		}
		return nil
	}
	if !allowPrerelease {
		return fmt.Errorf("%w: npm version %q", ErrPrerelease, v)
	}
	if len(v) > maxNPMPrereleaseVersionLen || !semverSuffix.MatchString(suffix) {
		return fmt.Errorf("%w: npm version %q is not semver or is over %d bytes", ErrBadPath, v,
			maxNPMPrereleaseVersionLen)
	}
	return nil
}

// NPMKind is what an npm registry path asks for.
type NPMKind string

// The npm request kinds.
const (
	NPMPackument NPMKind = "packument"
	NPMTarball   NPMKind = "tarball"
)

// NPMRequest is an npm registry path NPMPath admitted.
type NPMRequest struct {
	Kind NPMKind
	// Scope ("" when unscoped) and Name are the package.
	Scope, Name string
	// Version is the tarball's version; "" for a packument.
	Version string
	// Escaped is the path exactly as admitted, leading slash included:
	// the form forwarded upstream (a scoped packument keeps its %2f).
	Escaped string
}

// FullName is the package's "@scope/name" or "name".
func (r NPMRequest) FullName() string { return NPMFullName(r.Scope, r.Name) }

// NPMPath parses the escaped path of an npm request below a registry
// route, with the leading slash: a packument "/<name>" or
// "/@<scope>%2f<name>" (%2F too), or a tarball "/<name>/-/<name>-<v>.tgz",
// "/@<scope>/<name>/-/<name>-<v>.tgz" or
// "/@<scope>/<name>/-/@<scope>/<name>-<v>.tgz" (the file-part scope equal
// to the path's, byte for byte). The only percent-escape admitted anywhere
// is the packument's scope separator; no "..", "." or empty segment. The
// version is judged by NPMVersion under allowPrerelease. It never unescapes
// through net/url: escaped is r.URL.EscapedPath().
func NPMPath(escaped string, allowPrerelease bool) (NPMRequest, error) {
	rest, ok := strings.CutPrefix(escaped, "/")
	if !ok || rest == "" {
		return npmBad(escaped, "not an absolute path below the route")
	}
	segs := strings.Split(rest, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return npmBad(escaped, "an empty, . or .. segment")
		}
	}
	if len(segs) == 1 {
		return npmPackument(escaped, segs[0])
	}
	return npmTarball(escaped, segs, allowPrerelease)
}

// npmBad is an npm path refusal.
func npmBad(escaped, why string) (NPMRequest, error) {
	return NPMRequest{}, fmt.Errorf("%w: %q: %s", ErrBadPath, escaped, why)
}

// npmPackument parses a packument's one segment: a % only at a scoped
// name's separator.
func npmPackument(escaped, full string) (NPMRequest, error) {
	if strings.HasPrefix(full, "@") {
		scope, name, found := cutFold(full[1:], "%2f")
		if !found {
			return npmBad(escaped, "a scoped packument must be @<scope>%2f<name>")
		}
		full = "@" + scope + "/" + name
	}
	if strings.Contains(full, "%") {
		return npmBad(escaped, "a percent-escape outside the scope separator")
	}
	scope, name, err := NPMName(full)
	if err != nil {
		return npmBad(escaped, err.Error())
	}
	return NPMRequest{Kind: NPMPackument, Scope: scope, Name: name, Escaped: escaped}, nil
}

// npmTarball parses the three tarball shapes.
func npmTarball(escaped string, segs []string, allowPrerelease bool) (NPMRequest, error) {
	if strings.Contains(escaped, "%") {
		return npmBad(escaped, "a percent-escape in a tarball path")
	}
	var scope, name, file string
	switch {
	case len(segs) == 3 && segs[1] == "-": // /<name>/-/<file>
		name, file = segs[0], segs[2]
	case len(segs) == 4 && strings.HasPrefix(segs[0], "@") && segs[2] == "-": // /@s/n/-/<file>
		scope, name, file = segs[0][1:], segs[1], segs[3]
	case len(segs) == 5 && strings.HasPrefix(segs[0], "@") && segs[2] == "-": // /@s/n/-/@s/<file>
		if segs[3] != segs[0] {
			return npmBad(escaped, "the tarball's file-part scope differs from the path's")
		}
		scope, name, file = segs[0][1:], segs[1], segs[4]
	default:
		return npmBad(escaped, "not a packument or tarball path")
	}
	s, n, err := NPMName(NPMFullName(scope, name))
	if err != nil {
		return npmBad(escaped, err.Error())
	}
	version, ok := strings.CutPrefix(file, n+"-")
	if ok {
		version, ok = strings.CutSuffix(version, ".tgz")
	}
	if !ok {
		return npmBad(escaped, "the tarball file is not <name>-<version>.tgz")
	}
	if err := NPMVersion(version, allowPrerelease); err != nil {
		if errors.Is(err, ErrPrerelease) {
			return NPMRequest{}, err
		}
		return npmBad(escaped, err.Error())
	}
	return NPMRequest{Kind: NPMTarball, Scope: s, Name: n, Version: version, Escaped: escaped}, nil
}

// cutFold is strings.Cut with an ASCII case-insensitive separator.
func cutFold(s, sep string) (before, after string, found bool) {
	i := strings.Index(strings.ToLower(s), sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// PyPI name and path limits.
const (
	// MaxPyProjectLen bounds a normalized project name on a simple page
	// path.
	MaxPyProjectLen = 128
	// MaxPyFilePathLen and MaxPyFileSegments bound a file route's path.
	MaxPyFilePathLen  = 1024
	MaxPyFileSegments = 12
)

var (
	// pyNormalizedPattern is a PEP 503 normalized project name.
	pyNormalizedPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// pyNamePattern is a PEP 508 project name.
	pyNamePattern = regexp.MustCompile(`^([A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9._-]*[A-Za-z0-9])$`)
	pySeparators  = regexp.MustCompile(`[-_.]+`)
	// pep440 is PEP 440's VERSION_PATTERN (Appendix B), with a local
	// segment, matched case-insensitively.
	pep440 = regexp.MustCompile(`(?i)^v?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)*` +
		`(?:[-_.]?(?:a|b|c|rc|alpha|beta|pre|preview)[-_.]?[0-9]*)?` +
		`(?:-[0-9]+|[-_.]?(?:post|rev|r)[-_.]?[0-9]*)?` +
		`(?:[-_.]?dev[-_.]?[0-9]*)?` +
		`(?:\+[a-z0-9]+(?:[-_.][a-z0-9]+)*)?$`)
	// wheelTag is a wheel's python, abi or platform tag (dotted for
	// compressed tag sets).
	wheelTag = regexp.MustCompile(`^[A-Za-z0-9_.]+$`)
	// wheelBuild is a wheel's optional build tag: it starts with a digit.
	wheelBuild = regexp.MustCompile(`^[0-9][A-Za-z0-9_.]*$`)
)

// PyNormalize is PEP 503's normalization: runs of -, _ and . become one -,
// and the name is lowercased.
func PyNormalize(name string) string {
	return strings.ToLower(pySeparators.ReplaceAllString(name, "-"))
}

// PyNormalized reports whether name is a PEP 503 normalized project name
// of at most MaxPyProjectLen bytes: the only form a simple page path
// admits.
func PyNormalized(name string) bool {
	return len(name) <= MaxPyProjectLen && pyNormalizedPattern.MatchString(name)
}

// PyName reports whether name is a PEP 508 project name (an allowlist
// entry, compared after PyNormalize).
func PyName(name string) bool {
	return len(name) <= MaxPyProjectLen && pyNamePattern.MatchString(name)
}

// PyVersion reports whether v is a PEP 440 version (a local +segment
// admitted).
func PyVersion(v string) bool { return v != "" && len(v) <= 128 && pep440.MatchString(v) }

// PyFileKind is the kind of a distribution file.
type PyFileKind string

// The distribution file kinds.
const (
	PyWheel PyFileKind = "wheel"
	PySdist PyFileKind = "sdist"
)

// PyDist is a parsed distribution file name.
type PyDist struct {
	Kind PyFileKind
	// Project is the normalized project name; for a legacy sdist it is
	// the shortest reading whose remainder is a version (MatchesProject
	// is the allowlist's test, which needs no reading).
	Project string
	Version string
	// Metadata is a PEP 658 ".metadata" file beside the distribution.
	Metadata bool
	// base is the file name without .metadata and extension.
	base string
}

// sdistExts are the sdist archive extensions a file route serves.
var sdistExts = []string{".tar.gz", ".zip", ".tar.bz2"}

// PyProjectOfFile parses a distribution file name (decoded): a wheel by PEP
// 427 ({name}-{version}(-{build})?-{py}-{abi}-{plat}.whl), an sdist by PEP
// 625 ({name}-{version}.tar.gz) or a legacy sdist ({name}-{version} with
// .tar.gz, .zip or .tar.bz2, the name possibly holding hyphens), each
// optionally followed by .metadata. The version must be a PEP 440 version.
func PyProjectOfFile(filename string) (PyDist, error) {
	var d PyDist
	name, meta := strings.CutSuffix(filename, ".metadata")
	d.Metadata = meta
	if base, ok := strings.CutSuffix(name, ".whl"); ok {
		parts := strings.Split(base, "-")
		if len(parts) != 5 && len(parts) != 6 {
			return PyDist{}, fmt.Errorf("wheel %q does not have 5 or 6 dash-separated parts", filename)
		}
		if !PyName(parts[0]) || !PyVersion(parts[1]) {
			return PyDist{}, fmt.Errorf("wheel %q has a bad name or version", filename)
		}
		if len(parts) == 6 && !wheelBuild.MatchString(parts[2]) {
			return PyDist{}, fmt.Errorf("wheel %q has a bad build tag", filename)
		}
		for _, tag := range parts[len(parts)-3:] {
			if !wheelTag.MatchString(tag) {
				return PyDist{}, fmt.Errorf("wheel %q has a bad tag %q", filename, tag)
			}
		}
		d.Kind, d.Project, d.Version, d.base = PyWheel, PyNormalize(parts[0]), parts[1], base
		return d, nil
	}
	for _, ext := range sdistExts {
		base, ok := strings.CutSuffix(name, ext)
		if !ok {
			continue
		}
		// The shortest name whose remainder is a version: the name never
		// ends with a version-shaped part in practice (PEP 625 names have
		// no hyphen at all).
		for i := 0; i < len(base); i++ {
			if base[i] != '-' {
				continue
			}
			project, version := base[:i], base[i+1:]
			if PyName(project) && PyVersion(version) {
				d.Kind, d.Project, d.Version, d.base = PySdist, PyNormalize(project), version, base
				return d, nil
			}
		}
		return PyDist{}, fmt.Errorf("sdist %q is not <name>-<PEP 440 version>%s", filename, ext)
	}
	return PyDist{}, fmt.Errorf("%q is not a wheel or sdist (.whl, .tar.gz, .zip, .tar.bz2, optionally .metadata)",
		filename)
}

// MatchesProject reports whether the file belongs to the project (compared
// normalized): a wheel's name must normalize to it; an sdist must split at
// some dash into a name that normalizes to it and a PEP 440 version, which
// is how a legacy sdist with separators in its name is told from a
// neighbour (charset-normalizer-2.0.0.tar.gz belongs to charset-normalizer,
// never to charset, since "normalizer-2.0.0" is not a version).
func (d PyDist) MatchesProject(project string) bool {
	project = PyNormalize(project)
	if d.Kind == PyWheel {
		return d.Project == project
	}
	for i := 0; i < len(d.base); i++ {
		if d.base[i] == '-' && PyNormalize(d.base[:i]) == project && PyVersion(d.base[i+1:]) {
			return true
		}
	}
	return false
}

// PyFile is a pip file path PyFilePath admitted.
type PyFile struct {
	// Raw is the escaped path exactly as admitted (forwarded as RawPath,
	// emitted in a rewritten page's href).
	Raw string
	// Decoded is Raw with %2B and %21 decoded.
	Decoded string
	// Dist is the parsed distribution file name (the last segment).
	Dist PyDist
}

// PyFilePath judges the escaped path of a pip file (r.URL.EscapedPath(), or
// a link's escaped path on a simple page; the leading slash included): at
// most MaxPyFileSegments "/"-separated segments and MaxPyFilePathLen bytes;
// each segment one or more of [A-Za-z0-9._~!+=-] and the escapes %2B, %2b
// (+) and %21 (!), and nothing else (so %2e, %2f, %5c, %25, ";", "\", "?"
// and "#" are refused); no empty, "." or ".." segment, raw or decoded; the
// last segment a distribution file PyProjectOfFile parses. The rewriter and
// the file route both call it, so what a page can emit and what the route
// accepts are one set.
func PyFilePath(escaped string) (PyFile, error) {
	bad := func(why string) (PyFile, error) {
		return PyFile{}, fmt.Errorf("%w: %q: %s", ErrBadPath, escaped, why)
	}
	if len(escaped) > MaxPyFilePathLen {
		return bad(fmt.Sprintf("over %d bytes", MaxPyFilePathLen))
	}
	rest, ok := strings.CutPrefix(escaped, "/")
	if !ok {
		return bad("not absolute")
	}
	segs := strings.Split(rest, "/")
	if len(segs) > MaxPyFileSegments {
		return bad(fmt.Sprintf("over %d segments", MaxPyFileSegments))
	}
	decoded := make([]string, len(segs))
	for i, s := range segs {
		d, err := pyDecodeSegment(s)
		if err != nil {
			return bad(err.Error())
		}
		for _, form := range []string{s, d} {
			if form == "" || form == "." || form == ".." {
				return bad("an empty, . or .. segment")
			}
		}
		decoded[i] = d
	}
	dist, err := PyProjectOfFile(decoded[len(decoded)-1])
	if err != nil {
		return bad(err.Error())
	}
	return PyFile{Raw: escaped, Decoded: "/" + strings.Join(decoded, "/"), Dist: dist}, nil
}

// pyDecodeSegment decodes one file-path segment over the grammar's
// alphabet, refusing any byte or escape outside it.
func pyDecodeSegment(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%':
			if i+2 >= len(s) {
				return "", errors.New("a truncated percent-escape")
			}
			switch s[i+1 : i+3] {
			case "2B", "2b":
				b.WriteByte('+')
			case "21":
				b.WriteByte('!')
			default:
				return "", fmt.Errorf("the percent-escape %%%s is not admitted", s[i+1:i+3])
			}
			i += 2
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			strings.IndexByte("._~!+=-", c) >= 0:
			b.WriteByte(c)
		default:
			return "", fmt.Errorf("the byte %q is not admitted", c)
		}
	}
	return b.String(), nil
}

// PathUnder reports whether the decoded path p is prefix or below it on a
// segment boundary ("/a/b" is under "/a/" and "/a", never under "/a/bc").
func PathUnder(p, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}
