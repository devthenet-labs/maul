// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func TestNPMNameExamples(t *testing.T) {
	ok := map[string][2]string{
		"left-pad":                     {"", "left-pad"},
		"JSONStream":                   {"", "JSONStream"},
		"@acme/bar":                    {"acme", "bar"},
		"@types/node":                  {"types", "node"},
		"a.b~c_d":                      {"", "a.b~c_d"},
		"@0x/lib":                      {"0x", "lib"},
		"@acme/Bar.Baz":                {"acme", "Bar.Baz"},
		"x":                            {"", "x"},
		"@a~b.c-d/e":                   {"a~b.c-d", "e"},
		"a" + strings.Repeat("b", 213): {"", "a" + strings.Repeat("b", 213)},
	}
	for in, want := range ok {
		s, n, err := NPMName(in)
		if err != nil || s != want[0] || n != want[1] {
			t.Errorf("NPMName(%q) = %q, %q, %v; want %q, %q", in, s, n, err, want[0], want[1])
		}
	}
	for _, in := range []string{
		"", "@", "@acme", "@acme/", "@/bar", "@Acme/bar", ".hidden", "_private", "@acme/.x", "@acme/_x",
		"@.acme/x", "node_modules", "favicon.ico", "Node_Modules", "a b", "a/b", "@a/b/c", "a%2fb", "a:b",
		"a" + strings.Repeat("b", 214), "@" + strings.Repeat("a", 100) + "/" + strings.Repeat("b", 114),
	} {
		if _, _, err := NPMName(in); err == nil {
			t.Errorf("NPMName(%q) admitted it", in)
		}
	}
}

func TestNPMPathExamples(t *testing.T) {
	ok := []struct {
		in   string
		want NPMRequest
	}{
		{"/left-pad", NPMRequest{Kind: NPMPackument, Name: "left-pad"}},
		{"/@acme%2fbar", NPMRequest{Kind: NPMPackument, Scope: "acme", Name: "bar"}},
		{"/@acme%2Fbar", NPMRequest{Kind: NPMPackument, Scope: "acme", Name: "bar"}},
		{"/left-pad/-/left-pad-1.3.0.tgz", NPMRequest{Kind: NPMTarball, Name: "left-pad", Version: "1.3.0"}},
		{"/@acme/bar/-/bar-2.0.10.tgz", NPMRequest{Kind: NPMTarball, Scope: "acme", Name: "bar", Version: "2.0.10"}},
		{"/@acme/bar/-/@acme/bar-2.0.10.tgz",
			NPMRequest{Kind: NPMTarball, Scope: "acme", Name: "bar", Version: "2.0.10"}},
		{"/JSONStream/-/JSONStream-1.3.5.tgz", NPMRequest{Kind: NPMTarball, Name: "JSONStream", Version: "1.3.5"}},
	}
	for _, tt := range ok {
		got, err := NPMPath(tt.in, false)
		tt.want.Escaped = tt.in
		if err != nil || got != tt.want {
			t.Errorf("NPMPath(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{
		"", "/", "left-pad", "//left-pad", "/left-pad/", "/./x", "/../x", "/x/../y", "/%2e%2e/x",
		"/@acme/bar",          // a scoped packument must escape its separator
		"/@acme%2fbar%2fbaz",  // a second separator
		"/left%2dpad",         // any other escape
		"/@acme%252fbar",      // double escape
		"/left-pad/1.3.0",     // version document
		"/left-pad/dist-tags", // dist-tags
		"/-/v1/search",        // search
		"/-/npm/v1/security/advisories/bulk", "/-/whoami", "/-/ping",
		"/left-pad/-/right-pad-1.3.0.tgz",          // another package's file
		"/left-pad/-/left-pad-1.3.0.tar.gz",        // not .tgz
		"/@acme/bar/-/@other/bar-1.0.0.tgz",        // file-part scope differs
		"/@acme/bar/-/@ACME/bar-1.0.0.tgz",         // differs byte for byte
		"/@acme%2fbar/-/bar-1.0.0.tgz",             // escape in a tarball path
		"/left-pad/-/left-pad-01.3.0.tgz",          // leading zero
		"/left-pad/-/left-pad-1.3.tgz",             // two fields
		"/left-pad/-/left-pad-1.3.0.0.tgz",         // four fields
		"/left-pad/-/left-pad-12345678901.0.0.tgz", // an 11-digit field
		"/left-pad/-/left-pad-1.3.0.tgz/x",         // trailing segment
		"/left-pad/-/left-pad-1.3.0.tgz?x=1",       // query bytes in the path
		"/node_modules/-/node_modules-1.0.0.tgz",   // reserved
		"/@acme/bar/x/bar-1.0.0.tgz",               // not the /-/ separator
	} {
		if _, err := NPMPath(in, true); err == nil {
			t.Errorf("NPMPath(%q) admitted it", in)
		}
	}
	// A prerelease is ErrPrerelease without the grant's switch, admitted
	// with it.
	const pre = "/left-pad/-/left-pad-1.3.0-rc.1.tgz"
	if _, err := NPMPath(pre, false); !errors.Is(err, ErrPrerelease) {
		t.Errorf("NPMPath(%q, false) = %v, want ErrPrerelease", pre, err)
	}
	if r, err := NPMPath(pre, true); err != nil || r.Version != "1.3.0-rc.1" {
		t.Errorf("NPMPath(%q, true) = %+v, %v", pre, r, err)
	}
}

func TestNPMVersionExamples(t *testing.T) {
	for _, v := range []string{"0.0.0", "1.2.3", "10.20.30", "9999999999.0.1"} {
		if err := NPMVersion(v, false); err != nil {
			t.Errorf("NPMVersion(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"", "1", "1.2", "01.2.3", "1.02.3", "1.2.03", "a.b.c", "1.2.3.4", "v1.2.3",
		"12345678901.0.0", "1..3", "1.2.3-", "1.2.3+", " 1.2.3"} {
		if err := NPMVersion(v, true); err == nil {
			t.Errorf("NPMVersion(%q, true) admitted it", v)
		}
	}
	for _, v := range []string{"1.2.3-rc.1", "1.2.3+sha.abc", "1.2.3-alpha-1.x+b.2", "1.0.0-0"} {
		if err := NPMVersion(v, false); !errors.Is(err, ErrPrerelease) {
			t.Errorf("NPMVersion(%q, false) = %v, want ErrPrerelease", v, err)
		}
		if err := NPMVersion(v, true); err != nil {
			t.Errorf("NPMVersion(%q, true) = %v", v, err)
		}
	}
	for _, v := range []string{"1.2.3-01", "1.2.3-a..b", "1.2.3-" + strings.Repeat("a", 60)} {
		if err := NPMVersion(v, true); err == nil {
			t.Errorf("NPMVersion(%q, true) admitted it", v)
		}
	}
}

// npmAlpha and npmScopeAlpha are the name grammars' alphabets.
const (
	npmScopeAlpha = "abcz019._~-"
	npmAlpha      = "abcz019._~-ABZ"
)

// npmPkg is a valid (scope, name) pair, drawn on the grammar's edges.
type npmPkg struct{ scope, name string }

func drawFrom(r *rand.Rand, first, rest string, max int) string {
	n := 1 + r.Intn(max)
	var b strings.Builder
	b.WriteByte(first[r.Intn(len(first))])
	for range n - 1 {
		b.WriteByte(rest[r.Intn(len(rest))])
	}
	return b.String()
}

func (npmPkg) Generate(r *rand.Rand, _ int) reflect.Value {
	var p npmPkg
	if r.Intn(2) == 0 {
		p.scope = drawFrom(r, "abz09", npmScopeAlpha, 12)
	}
	p.name = drawFrom(r, "abzAZ09", npmAlpha, 16)
	if p.name == "node_modules" || strings.EqualFold(p.name, "favicon.ico") {
		p.name = "x" + p.name
	}
	return reflect.ValueOf(p)
}

// npmVer is a strict x.y.z version.
type npmVer string

func (npmVer) Generate(r *rand.Rand, _ int) reflect.Value {
	field := func() string {
		if r.Intn(4) == 0 {
			return "0"
		}
		return fmt.Sprint(1 + r.Int63n(9999999999))
	}
	return reflect.ValueOf(npmVer(field() + "." + field() + "." + field()))
}

// TestPropertyNPMNameRoundTrip: a valid name's escaped packument path
// parses back to it, in either case of the separator escape.
func TestPropertyNPMNameRoundTrip(t *testing.T) {
	prop := func(p npmPkg, upper bool) bool {
		full := NPMFullName(p.scope, p.name)
		if s, n, err := NPMName(full); err != nil || s != p.scope || n != p.name {
			t.Logf("NPMName(%q) = %q %q %v", full, s, n, err)
			return false
		}
		path := "/" + p.name
		if p.scope != "" {
			sep := "%2f"
			if upper {
				sep = "%2F"
			}
			path = "/@" + p.scope + sep + p.name
		}
		r, err := NPMPath(path, false)
		return err == nil && r.Kind == NPMPackument && r.Scope == p.scope && r.Name == p.name && r.Escaped == path
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyNPMPathRefusesInjections: inserting a byte outside the
// alphabet, a percent-escape other than the scope separator, a ".." or an
// empty segment anywhere in a valid path is refused.
func TestPropertyNPMPathRefusesInjections(t *testing.T) {
	injections := []string{"%2e", "%2E", "%2f", "%5c", "%25", "%00", "%", "/../", "/./", "//", " ", "\\", ";",
		"?", "#", ":", "\x00", "é", "*", "!", "'", "(", "@/"}
	prop := func(p npmPkg, v npmVer, which, pos uint16, shape uint8) bool {
		paths := tarballPaths(p, string(v))
		paths = append(paths, packumentPath(p))
		base := paths[int(shape)%len(paths)]
		inj := injections[int(which)%len(injections)]
		i := 1 + int(pos)%len(base) // after the leading slash
		bad := base[:i] + inj + base[i:]
		got, err := NPMPath(bad, true)
		if err == nil {
			// The one admitted outcome: the injection formed another
			// valid name (an @/ splitting a name into scope/name never
			// does, since the scope then holds no escape); report it.
			t.Logf("NPMPath(%q) admitted %+v", bad, got)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

func packumentPath(p npmPkg) string {
	if p.scope == "" {
		return "/" + p.name
	}
	return "/@" + p.scope + "%2f" + p.name
}

// tarballPaths are every tarball shape of the package at v: bare for an
// unscoped package; scoped and scope-repeated for a scoped one.
func tarballPaths(p npmPkg, v string) []string {
	file := p.name + "-" + v + ".tgz"
	if p.scope == "" {
		return []string{"/" + p.name + "/-/" + file}
	}
	return []string{
		"/@" + p.scope + "/" + p.name + "/-/" + file,
		"/@" + p.scope + "/" + p.name + "/-/@" + p.scope + "/" + file,
	}
}

// TestPropertyNPMTarballShapes: every tarball shape of a generated
// (scope, name, version) parses back to it; a scope-repeated path whose
// file-part scope differs is refused.
func TestPropertyNPMTarballShapes(t *testing.T) {
	prop := func(p npmPkg, v npmVer, other npmPkg) bool {
		for _, path := range tarballPaths(p, string(v)) {
			r, err := NPMPath(path, false)
			if err != nil || r.Kind != NPMTarball || r.Scope != p.scope || r.Name != p.name ||
				r.Version != string(v) || r.Escaped != path {
				t.Logf("NPMPath(%q) = %+v, %v", path, r, err)
				return false
			}
		}
		if p.scope != "" && other.scope != "" && other.scope != p.scope {
			bad := "/@" + p.scope + "/" + p.name + "/-/@" + other.scope + "/" + p.name + "-" + string(v) + ".tgz"
			if _, err := NPMPath(bad, true); err == nil {
				t.Logf("NPMPath(%q) admitted a differing file-part scope", bad)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyNPMVersion: a strict x.y.z is admitted either way; any
// suffix is ErrPrerelease without the switch; leading zeros and 11-digit
// fields are refused.
func TestPropertyNPMVersion(t *testing.T) {
	prop := func(v npmVer, suffix uint8, field uint8) bool {
		if NPMVersion(string(v), false) != nil || NPMVersion(string(v), true) != nil {
			return false
		}
		sfx := []string{"-rc.1", "+build.7", "-alpha", "-0.3.7", "-x.7.z.92+meta"}[int(suffix)%5]
		if !errors.Is(NPMVersion(string(v)+sfx, false), ErrPrerelease) {
			return false
		}
		if NPMVersion(string(v)+sfx, true) != nil {
			return false
		}
		fields := strings.Split(string(v), ".")
		i := int(field) % 3
		zero := append([]string(nil), fields...)
		zero[i] = "0" + zero[i]
		long := append([]string(nil), fields...)
		long[i] = "12345678901"
		return NPMVersion(strings.Join(zero, "."), true) != nil && NPMVersion(strings.Join(long, "."), true) != nil
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

// pep503Oracle normalizes the way PEP 503 states it, written out by hand
// rather than through the package's regexp.
func pep503Oracle(name string) string {
	var b strings.Builder
	sep := false
	for _, c := range name {
		if c == '-' || c == '_' || c == '.' {
			sep = true
			continue
		}
		if sep {
			b.WriteByte('-')
			sep = false
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	if sep {
		b.WriteByte('-')
	}
	return b.String()
}

// pyProject is a valid PEP 508 project name.
type pyProject string

func (pyProject) Generate(r *rand.Rand, _ int) reflect.Value {
	const alnum = "abzAZ019"
	n := 1 + r.Intn(4)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = drawFrom(r, alnum, alnum, 6)
	}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString([]string{"-", "_", ".", "--", "._"}[r.Intn(5)])
		}
		b.WriteString(p)
	}
	return reflect.ValueOf(pyProject(b.String()))
}

func TestPropertyPyNormalize(t *testing.T) {
	prop := func(p pyProject) bool {
		n := PyNormalize(string(p))
		return n == pep503Oracle(string(p)) && PyNormalize(n) == n && PyNormalized(n) && PyName(string(p))
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

func TestPyNormalizedExamples(t *testing.T) {
	for _, n := range []string{"six", "charset-normalizer", "zope-interface", "a", "a1-b2"} {
		if !PyNormalized(n) {
			t.Errorf("PyNormalized(%q) = false", n)
		}
	}
	for _, n := range []string{"", "Six", "zope.interface", "a_b", "-a", "a-", "a--b", strings.Repeat("a", 129)} {
		if PyNormalized(n) {
			t.Errorf("PyNormalized(%q) = true", n)
		}
	}
}

func TestPyProjectOfFileExamples(t *testing.T) {
	tests := []struct {
		file, project, version string
		kind                   PyFileKind
		meta                   bool
	}{
		{"six-1.16.0-py2.py3-none-any.whl", "six", "1.16.0", PyWheel, false},
		{"six-1.16.0-py2.py3-none-any.whl.metadata", "six", "1.16.0", PyWheel, true},
		{"torch-2.3.0+cu121-cp312-cp312-linux_x86_64.whl", "torch", "2.3.0+cu121", PyWheel, false},
		{"pkg-1.0-1build-py3-none-any.whl", "pkg", "1.0", PyWheel, false},
		{"zope_interface-5.4.0-cp312-cp312-manylinux_2_17_x86_64.whl", "zope-interface", "5.4.0", PyWheel, false},
		{"charset_normalizer-3.3.2.tar.gz", "charset-normalizer", "3.3.2", PySdist, false},
		{"charset-normalizer-2.0.0.tar.gz", "charset-normalizer", "2.0.0", PySdist, false},
		{"zope.interface-5.4.0.tar.gz", "zope-interface", "5.4.0", PySdist, false},
		{"backports.zoneinfo-0.2.1.tar.gz", "backports-zoneinfo", "0.2.1", PySdist, false},
		{"python-dateutil-2.9.0.post0.tar.gz", "python-dateutil", "2.9.0.post0", PySdist, false},
		{"requests-2.31.0.zip", "requests", "2.31.0", PySdist, false},
		{"requests-2.31.0.tar.bz2", "requests", "2.31.0", PySdist, false},
		{"pkg-1!2.0rc1.post3.dev4.tar.gz", "pkg", "1!2.0rc1.post3.dev4", PySdist, false},
		{"requests-2.31.0.tar.gz.metadata", "requests", "2.31.0", PySdist, true},
	}
	for _, tt := range tests {
		d, err := PyProjectOfFile(tt.file)
		if err != nil || d.Project != tt.project || d.Version != tt.version || d.Kind != tt.kind ||
			d.Metadata != tt.meta {
			t.Errorf("PyProjectOfFile(%q) = %+v, %v; want %s %s %s", tt.file, d, err, tt.project, tt.version, tt.kind)
			continue
		}
		if !d.MatchesProject(tt.project) {
			t.Errorf("%q does not match its own project %q", tt.file, tt.project)
		}
	}
	// A legacy sdist belongs to the longer name, never its prefix.
	for file, notProject := range map[string]string{
		"charset-normalizer-2.0.0.tar.gz":    "charset",
		"python-dateutil-2.9.0.post0.tar.gz": "python",
		"backports.zoneinfo-0.2.1.tar.gz":    "backports",
		"six-1.16.0-py2.py3-none-any.whl":    "sixx",
	} {
		d, err := PyProjectOfFile(file)
		if err != nil || d.MatchesProject(notProject) {
			t.Errorf("%q matched %q (err %v)", file, notProject, err)
		}
	}
	// A legacy sdist with more than one <name>-<version> reading is
	// refused, never attributed: PEP 440 admits 1-2 (1.post2), v1-2.dev0
	// and 1rc1-2, so foo-1-2.tar.gz is as much version 2 of foo-1 as
	// version 1-2 of foo, and either guess lets one project's file pass an
	// allowlist naming the other.
	for _, f := range []string{"foo-1-2.tar.gz", "foo-v1-2.dev0.tar.gz", "foo-1rc1-2.zip", "foo-1-2.tar.bz2",
		"foo-1-2.tar.gz.metadata", "foo-bar-1-2-3.tar.gz"} {
		if d, err := PyProjectOfFile(f); err == nil {
			t.Errorf("PyProjectOfFile(%q) = %+v, want an ambiguity refusal", f, d)
		}
		if f, err := PyFilePath("/packages/source/f/foo-1/" + f); err == nil {
			t.Errorf("PyFilePath admitted ambiguous %q as %+v", f.Raw, f.Dist)
		}
	}
	// A PyDist reached any other way still matches neither reading.
	amb := PyDist{Kind: PySdist, Project: "foo", Version: "1-2", base: "foo-1-2"}
	if amb.MatchesProject("foo") || amb.MatchesProject("foo-1") {
		t.Error("an ambiguous legacy sdist matched a project")
	}
	for _, f := range []string{"", "six", "six.whl", "six-1.0.whl", "six-x-py3-none-any.whl", "six-1.0.exe",
		"six-1.0-py3-none-any-x-y.whl", "-1.0.tar.gz", "six-.tar.gz", "six-notaversion.tar.gz",
		"six-1.0-py3-none-any.whl.metadata.metadata", "six-1.0-x!-none-any.whl", "six-1.0-abc-py3-none-any.whl"} {
		if d, err := PyProjectOfFile(f); err == nil {
			t.Errorf("PyProjectOfFile(%q) = %+v, want a refusal", f, d)
		}
	}
}

// pyVersion is a PEP 440 version, with optional pre, post, dev and local
// parts.
type pyVersion string

func (pyVersion) Generate(r *rand.Rand, _ int) reflect.Value {
	v := fmt.Sprintf("%d.%d", r.Intn(40), r.Intn(40))
	if r.Intn(2) == 0 {
		v += fmt.Sprintf(".%d", r.Intn(40))
	}
	for _, part := range []string{"rc1", ".post2", ".dev3", "+cu121"} {
		if r.Intn(4) == 0 {
			v += part
		}
	}
	return reflect.ValueOf(pyVersion(v))
}

// TestPropertyPyProjectOfFile: the wheel and PEP 625 sdist names of a
// generated (project, version) map back to the normalized project and the
// version, and match the project.
func TestPropertyPyProjectOfFile(t *testing.T) {
	prop := func(p pyProject, v pyVersion, meta bool) bool {
		wheelName := strings.ReplaceAll(PyNormalize(string(p)), "-", "_")
		files := []string{
			wheelName + "-" + string(v) + "-py3-none-any.whl",
			wheelName + "-" + string(v) + "-1-cp312-cp312-manylinux_2_17_x86_64.whl",
			wheelName + "-" + string(v) + ".tar.gz",
		}
		for _, f := range files {
			if meta {
				f += ".metadata"
			}
			d, err := PyProjectOfFile(f)
			if err != nil || d.Project != PyNormalize(string(p)) || d.Version != string(v) || d.Metadata != meta ||
				!d.MatchesProject(string(p)) {
				t.Logf("PyProjectOfFile(%q) = %+v, %v", f, d, err)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyPyLegacySdistAmbiguity: for any project p, the legacy sdist
// <p>-<k>-<n> (version n of the project p-k, and equally version k-n, i.e.
// k.post n, of p) is refused, and a PyDist carrying it matches neither
// project, so a grant naming p never admits a sibling's file.
func TestPropertyPyLegacySdistAmbiguity(t *testing.T) {
	prop := func(p pyProject, k, n uint16, ext uint8) bool {
		legacy := PyNormalize(string(p))
		base := fmt.Sprintf("%s-%d-%d", legacy, k, n)
		f := base + sdistExts[int(ext)%len(sdistExts)]
		if d, err := PyProjectOfFile(f); err == nil {
			t.Logf("PyProjectOfFile(%q) = %+v, want a refusal", f, d)
			return false
		}
		d := PyDist{Kind: PySdist, Project: legacy, base: base}
		return !d.MatchesProject(legacy) && !d.MatchesProject(fmt.Sprintf("%s-%d", legacy, k))
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

func TestPyFilePathExamples(t *testing.T) {
	ok := []struct{ in, want string }{
		{"/packages/ab/cd/six-1.16.0-py2.py3-none-any.whl", ""},
		{"/whl/cu121/torch-2.3.0%2Bcu121-cp312-cp312-linux_x86_64.whl",
			"/whl/cu121/torch-2.3.0+cu121-cp312-cp312-linux_x86_64.whl"},
		{"/whl/torch-2.3.0%2bcu121-cp312-cp312-linux_x86_64.whl",
			"/whl/torch-2.3.0+cu121-cp312-cp312-linux_x86_64.whl"},
		{"/p/pkg-1%212.0.tar.gz", "/p/pkg-1!2.0.tar.gz"},
		{"/artifactory/api/pypi/pypi-virtual/packages/x/requests-2.31.0.tar.gz", ""},
		{"/a=b~c/six-1.16.0-py2.py3-none-any.whl.metadata", ""},
	}
	for _, tt := range ok {
		in, want := tt.in, tt.want
		if want == "" {
			want = in // nothing to decode
		}
		f, err := PyFilePath(in)
		if err != nil || f.Decoded != want || f.Raw != in {
			t.Errorf("PyFilePath(%q) = %+v, %v; want decoded %q", in, f, err, want)
		}
	}
	for _, in := range []string{
		"", "six-1.16.0.tar.gz", "/", "//six-1.16.0.tar.gz", "/a//six-1.16.0.tar.gz", "/./six-1.16.0.tar.gz",
		"/../six-1.16.0.tar.gz", "/a/%2e%2e/six-1.16.0.tar.gz", "/a/%2E/six-1.16.0.tar.gz",
		"/a%2fb/six-1.16.0.tar.gz", "/a%5cb/six-1.16.0.tar.gz", "/a%25/six-1.16.0.tar.gz",
		"/a;x=y/six-1.16.0.tar.gz", "/a\\b/six-1.16.0.tar.gz", "/a?b/six-1.16.0.tar.gz", "/a#b/six-1.16.0.tar.gz",
		"/a b/six-1.16.0.tar.gz", "/a/six-1.16.0.tar.gz%", "/a/six-1.16.0.tar.gz%2", "/a/six.exe",
		"/a/b/c/d/e/f/g/h/i/j/k/l/six-1.16.0.tar.gz", // 13 segments
		"/" + strings.Repeat("a", 1010) + "/six-1.16.0.tar.gz",
		"/a/six-1.16.0.tar.gz/", "/a/%41/six-1.16.0.tar.gz", "/a/é/six-1.16.0.tar.gz",
	} {
		if f, err := PyFilePath(in); err == nil {
			t.Errorf("PyFilePath(%q) = %+v, want a refusal", in, f)
		}
	}
}

// pySegChars are the file-path grammar's literal bytes; pySegEscapes its
// admitted escapes.
const pySegChars = "Aaz09._~!+=-"

var pySegEscapes = []string{"%2B", "%2b", "%21"}

// pyPath is a valid escaped file path: 0-10 directory segments over the
// grammar, then a wheel or sdist file name.
type pyPath string

func (pyPath) Generate(r *rand.Rand, size int) reflect.Value {
	n := r.Intn(11)
	segs := make([]string, 0, n+1)
	for range n {
		var b strings.Builder
		m := 1 + r.Intn(6)
		for range m {
			if r.Intn(6) == 0 {
				b.WriteString(pySegEscapes[r.Intn(len(pySegEscapes))])
			} else {
				b.WriteByte(pySegChars[r.Intn(len(pySegChars))])
			}
		}
		s := b.String()
		if s == "." || s == ".." {
			s += "x"
		}
		segs = append(segs, s)
	}
	p := string(pyProject("").Generate(r, size).Interface().(pyProject))
	v := string(pyVersion("").Generate(r, size).Interface().(pyVersion))
	file := strings.ReplaceAll(PyNormalize(p), "-", "_") + "-" + strings.ReplaceAll(v, "+", "%2B") + ".tar.gz"
	segs = append(segs, file)
	return reflect.ValueOf(pyPath("/" + strings.Join(segs, "/")))
}

// TestPropertyPyFilePath: generated paths over the grammar are admitted
// and round-trip (the raw form comes back unchanged); inserting a refused
// escape or byte, or an empty, "." or ".." segment, at any position is
// refused; the raw and decoded checks agree.
func TestPropertyPyFilePath(t *testing.T) {
	refused := []string{"%2e", "%2E", "%2f", "%2F", "%5c", "%5C", "%25", ";", "\\", "?", "#", " ", "%", "%0",
		"\x00", "é", "@", "*", "/./", "/../", "//", "/%2e%2e/", "/%2E/"}
	prop := func(p pyPath, which, pos uint16) bool {
		f, err := PyFilePath(string(p))
		if err != nil || f.Raw != string(p) {
			t.Logf("PyFilePath(%q) = %+v, %v", p, f, err)
			return false
		}
		// The decoded form differs only by + and !; re-escaping them
		// gives the raw form back, up to the escape's case.
		re := strings.NewReplacer("+", "%2B", "!", "%21").Replace(f.Decoded)
		norm := strings.NewReplacer("%2b", "%2B", "+", "%2B", "!", "%21").Replace(string(p))
		if re != norm {
			t.Logf("decoded %q does not re-escape to %q", f.Decoded, p)
			return false
		}
		// Insert a refused piece into a directory segment (not the file
		// name, whose own grammar may refuse it for another reason, which
		// would prove nothing).
		dir := strings.LastIndex(string(p), "/")
		if dir < 1 {
			return true
		}
		inj := refused[int(which)%len(refused)]
		i := 1 + int(pos)%dir
		bad := string(p)[:i] + inj + string(p)[i:]
		if _, err := PyFilePath(bad); err == nil {
			t.Logf("PyFilePath(%q) admitted an injected %q", bad, inj)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

func TestPathUnder(t *testing.T) {
	tests := []struct {
		p, prefix string
		want      bool
	}{
		{"/a/b/c.whl", "/a/b/", true},
		{"/a/b/c.whl", "/a/b", true},
		{"/a/bc/c.whl", "/a/b/", false},
		{"/a/b", "/a/b/", true},
		{"/x/c.whl", "/", true},
		{"/a", "/a/b", false},
	}
	for _, tt := range tests {
		if got := PathUnder(tt.p, tt.prefix); got != tt.want {
			t.Errorf("PathUnder(%q, %q) = %v, want %v", tt.p, tt.prefix, got, tt.want)
		}
	}
}
