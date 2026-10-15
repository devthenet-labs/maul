// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"golang.org/x/mod/module"
)

// seed is every property's seed, so the gate is deterministic.
const seed = 20261014

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// The generators' alphabets: modPathOK's characters (letters of both cases,
// digits, -, ., _, ~), importPathOK's (adding +), and fileNameOK's without
// % and space for versions.
const (
	lowerDigits  = "abcdefghijklmnopqrstuvwxyz0123456789"
	modPathChars = lowerDigits + "ABCDEFGHIJKLMNOPQRSTUVWXYZ-._~"
	versionChars = lowerDigits + "ABCDEFGHIJKLMNOPQRSTUVWXYZ!#$&()+,-.=@[]^_{}~"
)

func randString(r *rand.Rand, chars string, minLen, maxLen int) string {
	n := minLen + r.Intn(maxLen-minLen+1)
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return string(b)
}

// genModule is a module path module.CheckPath accepts: a dotted lowercase
// host, then 0-3 elements drawn from modPathOK's characters.
type genModule string

func (genModule) Generate(r *rand.Rand, _ int) reflect.Value {
	for {
		host := randString(r, lowerDigits, 1, 8) + "." + randString(r, "abcdefghijklmnopqrstuvwxyz", 2, 4)
		elems := make([]string, 1, 4)
		elems[0] = host
		for range r.Intn(4) {
			elems = append(elems, randString(r, modPathChars, 1, 10))
		}
		m := strings.Join(elems, "/")
		if module.CheckPath(m) == nil {
			return reflect.ValueOf(genModule(m))
		}
	}
}

// genImportPath is an import path (a prefix probe) module.CheckImportPath
// accepts, drawing from importPathOK's characters (adding +).
type genImportPath string

func (genImportPath) Generate(r *rand.Rand, _ int) reflect.Value {
	for {
		elems := make([]string, 1, 3)
		elems[0] = randString(r, lowerDigits+".", 1, 10)
		for range r.Intn(3) {
			elems = append(elems, randString(r, modPathChars+"+", 1, 10))
		}
		p := strings.Join(elems, "/")
		if module.CheckImportPath(p) == nil {
			return reflect.ValueOf(genImportPath(p))
		}
	}
}

// genVersion is a version module.EscapeVersion accepts: a canonical
// version, a pseudo-version, or a free-form string of fileNameOK's
// characters.
type genVersion string

func (genVersion) Generate(r *rand.Rand, _ int) reflect.Value {
	for {
		var v string
		switch r.Intn(4) {
		case 0:
			v = "v" + randString(r, "0123456789", 1, 2) + "." + randString(r, "0123456789", 1, 2) + "." +
				randString(r, "0123456789", 1, 2)
		case 1:
			v = "v0.0.0-20240101000000-" + randString(r, "0123456789abcdef", 12, 12)
		case 2:
			v = "v2.0.0+incompatible"
		default:
			v = randString(r, versionChars, 1, 40)
		}
		if _, err := module.EscapeVersion(v); err == nil {
			return reflect.ValueOf(genVersion(v))
		}
	}
}

// isGone reports whether err is a refusal answered 410 (a PathError that is
// not NotFound).
func isGone(err error) bool {
	var pe *PathError
	return errors.As(err, &pe) && !pe.NotFound
}

// TestPropertyGoProxyRoundTrip: every generated module and version, escaped
// as the go command escapes them, parses back to themselves, for each
// versioned kind, and Module never contains /@v/.
func TestPropertyGoProxyRoundTrip(t *testing.T) {
	prop := func(m genModule, v genVersion) bool {
		em, err := module.EscapePath(string(m))
		if err != nil {
			return false
		}
		ev, err := module.EscapeVersion(string(v))
		if err != nil {
			return false
		}
		for ext, kind := range map[string]Kind{".info": KindInfo, ".mod": KindMod, ".zip": KindZip} {
			p := em + "/@v/" + ev + ext
			got, err := ParseGoProxy(p)
			if err != nil {
				if len(ev) > maxVersionLen && isGone(err) {
					continue
				}
				t.Logf("ParseGoProxy(%q): %v", p, err)
				return false
			}
			if got.Module != string(m) || got.Version != string(v) || got.Kind != kind ||
				got.Upstream != p || strings.Contains(got.Module, "/@v/") {
				t.Logf("ParseGoProxy(%q) = %+v", p, got)
				return false
			}
		}
		for suffix, kind := range map[string]Kind{"/@v/list": KindList, "/@latest": KindLatest} {
			got, err := ParseGoProxy(em + suffix)
			if err != nil || got.Module != string(m) || got.Kind != kind || got.Version != "" {
				t.Logf("ParseGoProxy(%q) = %+v, %v", em+suffix, got, err)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(3000)); err != nil {
		t.Error(err)
	}
}

// TestPropertySumDBLookupRoundTrip: the checksum database's lookup path of
// every generated module and version parses back to them.
func TestPropertySumDBLookupRoundTrip(t *testing.T) {
	prop := func(m genModule, v genVersion) bool {
		em, _ := module.EscapePath(string(m))
		ev, _ := module.EscapeVersion(string(v))
		p := SumDBName + "/lookup/" + em + "@" + ev
		got, err := ParseSumDB(p)
		if len(ev) > maxVersionLen {
			return isGone(err)
		}
		return err == nil && got.Kind == KindLookup && got.Module == string(m) && got.Version == string(v) &&
			got.Upstream == "lookup/"+em+"@"+ev
	}
	if err := quick.Check(prop, quickConfig(3000)); err != nil {
		t.Error(err)
	}
}

// TestPropertyImportPathProbes: the prefix probes the go command sends
// while resolving an import (import paths, not necessarily module paths,
// + included) parse.
func TestPropertyImportPathProbes(t *testing.T) {
	prop := func(p genImportPath) bool {
		esc, ok := escapeCase(string(p))
		if !ok {
			return true
		}
		got, err := ParseGoProxy(esc + "/@v/v1.0.0.info")
		return err == nil && got.Module == string(p)
	}
	if err := quick.Check(prop, quickConfig(3000)); err != nil {
		t.Error(err)
	}
}

// escapeCase is module.EscapePath's case-encoding without its module-path
// check.
func escapeCase(s string) (string, bool) {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 0x80:
			return "", false
		case 'A' <= c && c <= 'Z':
			b.WriteByte('!')
			b.WriteRune(c + 'a' - 'A')
		default:
			b.WriteRune(c)
		}
	}
	return b.String(), true
}

// TestPropertyForeignBytesRefused: a byte outside the escaped alphabets,
// a percent sign, a dot segment or an empty element anywhere in a valid
// path is refused with 410, never parsed and never a 404.
func TestPropertyForeignBytesRefused(t *testing.T) {
	// Outside both alphabets, except A and # (version characters).
	inject := []string{"%", "%2e", " ", "\x00", "\n", "\\", "?", "#", "\"", "<", ">", "|", "`", "'", "*", ":", ";",
		"é", "\x7f", "A", "/../", "/./", "//", "/.."}
	prop := func(m genModule, pick, at uint16) bool {
		em, _ := module.EscapePath(string(m))
		const file = "/@v/v1.2.3.info"
		p := em + file
		var mutated string
		switch bad := inject[int(pick)%len(inject)]; bad {
		case "/../", "/./", "//":
			// Replace one of the path's slashes, so the dot or empty
			// element stands alone.
			var slashes []int
			for i := range len(p) {
				if p[i] == '/' {
					slashes = append(slashes, i)
				}
			}
			i := slashes[int(at)%len(slashes)]
			mutated = p[:i] + bad + p[i+1:]
		case "/..":
			mutated = p + bad
		case "A", "#":
			// Only the module part: a version may carry these.
			i := int(at) % (len(em) + 1)
			mutated = em[:i] + bad + em[i:] + file
		default:
			i := int(at) % (len(p) + 1)
			mutated = p[:i] + bad + p[i:]
		}
		_, err := ParseGoProxy(mutated)
		if err == nil {
			t.Logf("ParseGoProxy(%q) accepted", mutated)
			return false
		}
		return isGone(err)
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Error(err)
	}
}

func TestParseGoProxyExamples(t *testing.T) {
	tests := []struct {
		path    string
		want    Request
		wantErr bool
	}{
		{path: "github.com/prometheus/client_golang/@v/list",
			want: Request{Route: RouteGo, Kind: KindList, Module: "github.com/prometheus/client_golang"}},
		{path: "github.com/prometheus/client_golang/@v/v1.20.0.info",
			want: Request{Route: RouteGo, Kind: KindInfo, Module: "github.com/prometheus/client_golang",
				Version: "v1.20.0"}},
		{path: "github.com/!burnt!sushi/toml/@v/v1.3.2.mod",
			want: Request{Route: RouteGo, Kind: KindMod, Module: "github.com/BurntSushi/toml", Version: "v1.3.2"}},
		{path: "github.com/google/uuid/@v/v1.6.0.zip",
			want: Request{Route: RouteGo, Kind: KindZip, Module: "github.com/google/uuid", Version: "v1.6.0"}},
		{path: "github.com/google/uuid/@latest",
			want: Request{Route: RouteGo, Kind: KindLatest, Module: "github.com/google/uuid"}},
		// The go command's prefix probes: valid import paths, not modules.
		{path: "github.com/@v/v1.6.0.info",
			want: Request{Route: RouteGo, Kind: KindInfo, Module: "github.com", Version: "v1.6.0"}},
		{path: "gopkg.in/yaml.v3/@v/v3.0.1.mod",
			want: Request{Route: RouteGo, Kind: KindMod, Module: "gopkg.in/yaml.v3", Version: "v3.0.1"}},
		{path: "example.com/m/@v/v2.0.0+incompatible.info",
			want: Request{Route: RouteGo, Kind: KindInfo, Module: "example.com/m", Version: "v2.0.0+incompatible"}},
		{path: "example.com/m/@v/main.info",
			want: Request{Route: RouteGo, Kind: KindInfo, Module: "example.com/m", Version: "main"}},
		{path: "github.com/BurntSushi/toml/@v/list", wantErr: true}, // unescaped capital
		{path: "github.com/!/@v/list", wantErr: true},               // '!' before no letter
		{path: "github.com/google/uuid/@v/", wantErr: true},
		{path: "github.com/google/uuid/@v/v1.6.0", wantErr: true},
		{path: "github.com/google/uuid/@v/v1.6.0.tar", wantErr: true},
		{path: "github.com/google/uuid", wantErr: true},
		{path: "github.com/google/uuid/", wantErr: true},
		{path: "github.com/google/uuid/@v/v1/2.info", wantErr: true},
		{path: "github.com/google/uuid/@v/" + strings.Repeat("1", maxVersionLen+1) + ".info", wantErr: true},
		{path: "/github.com/google/uuid/@v/list", wantErr: true},
		{path: "github.com/./uuid/@v/list", wantErr: true},
		{path: "github.com/google/uuid/@v/v1.6.0.info/..", wantErr: true},
		{path: "@v/list", wantErr: true},
		{path: strings.Repeat("a", MaxPathLen) + ".com/@v/list", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseGoProxy(tt.path)
		if tt.wantErr {
			if !isGone(err) {
				t.Errorf("ParseGoProxy(%q) = %+v, %v; want a 410 refusal", tt.path, got, err)
			}
			continue
		}
		tt.want.Upstream = tt.path
		if err != nil || got != tt.want {
			t.Errorf("ParseGoProxy(%q) = %+v, %v; want %+v", tt.path, got, err, tt.want)
		}
	}
}

func TestParseSumDBExamples(t *testing.T) {
	tests := []struct {
		path     string
		kind     Kind
		module   string
		version  string
		notFound bool
		gone     bool
	}{
		{path: "sum.golang.org/supported", kind: KindSupported},
		{path: "sum.golang.org/latest", kind: KindLatest},
		{path: "sum.golang.org/lookup/github.com/google/uuid@v1.6.0", kind: KindLookup,
			module: "github.com/google/uuid", version: "v1.6.0"},
		{path: "sum.golang.org/lookup/github.com/!burnt!sushi/toml@v1.3.2", kind: KindLookup,
			module: "github.com/BurntSushi/toml", version: "v1.3.2"},
		{path: "sum.golang.org/tile/8/0/001", kind: KindTile},
		{path: "sum.golang.org/tile/8/0/x001/234", kind: KindTile},
		{path: "sum.golang.org/tile/8/1/x001/x002/345.p/17", kind: KindTile},
		{path: "sum.golang.org/tile/8/data/x123/456", kind: KindTile},
		{path: "sum.golang.org/tile/8/data/000.p/1", kind: KindTile},
		{path: "sum.golang.org/tile/8/0/01", gone: true},
		{path: "sum.golang.org/tile/8/0/0001", gone: true},
		{path: "sum.golang.org/tile/8/x/001", gone: true},
		{path: "sum.golang.org/tile/8/0/x01/001", gone: true},
		{path: "sum.golang.org/tile/8/0/001.p/", gone: true},
		{path: "sum.golang.org/tile/8/0/001.p", gone: true},
		{path: "sum.golang.org/tile/a/0/001", gone: true},
		{path: "sum.golang.org/lookup/github.com/google/uuid", gone: true},
		{path: "sum.golang.org/lookup/github.com/google/uuid@", gone: true},
		{path: "sum.golang.org/lookup/GitHub.com/google/uuid@v1.6.0", gone: true},
		{path: "sum.golang.org/lookup", gone: true},
		{path: "sum.golang.org/other", gone: true},
		{path: "sum.golang.org", gone: true},
		{path: "sum.golang.org/latest/", gone: true},
		{path: "sum.golang.org/../latest", gone: true},
		{path: "sum.example.org/supported", notFound: true},
		{path: "sum.golang.org.evil/supported", notFound: true},
		{path: "", notFound: true},
	}
	for _, tt := range tests {
		got, err := ParseSumDB(tt.path)
		var pe *PathError
		switch {
		case tt.notFound:
			if !errors.As(err, &pe) || !pe.NotFound {
				t.Errorf("ParseSumDB(%q) = %+v, %v; want a 404 refusal", tt.path, got, err)
			}
		case tt.gone:
			if !isGone(err) {
				t.Errorf("ParseSumDB(%q) = %+v, %v; want a 410 refusal", tt.path, got, err)
			}
		default:
			if err != nil || got.Route != RouteSumDB || got.Kind != tt.kind || got.Module != tt.module ||
				got.Version != tt.version {
				t.Errorf("ParseSumDB(%q) = %+v, %v; want %s %s@%s", tt.path, got, err, tt.kind, tt.module, tt.version)
			}
			if want := strings.TrimPrefix(tt.path, SumDBName+"/"); got.Upstream != want {
				t.Errorf("ParseSumDB(%q).Upstream = %q, want %q", tt.path, got.Upstream, want)
			}
		}
	}
}

// TestPropertyVersionGate: the gate admits v iff it is its own canonical
// form and (pseudo-versions are allowed or v is not one).
func TestPropertyVersionGate(t *testing.T) {
	prop := func(v genVersion, allowPseudo bool) bool {
		s := string(v)
		want := module.CanonicalVersion(s) == s && (allowPseudo || !module.IsPseudoVersion(s))
		return VersionAllowed(s, allowPseudo) == want
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Error(err)
	}
}

func TestVersionGateExamples(t *testing.T) {
	tests := []struct {
		v           string
		strict, any bool // admitted with pseudo-versions off, and on
	}{
		{"v1.6.0", true, true},
		{"v2.0.0+incompatible", true, true},
		{"v1.6.0-rc.1", true, true},
		{"v0.0.0-20240101000000-abcdefabcdef", false, true},
		{"v1.2.4-0.20240101000000-abcdefabcdef", false, true},
		{"main", false, false},
		{"abcdef1", false, false},
		{"v1.6", false, false},
		{"v1", false, false},
		{"latest", false, false},
		{"v1.6.0+meta", false, false},
		{"", false, false},
	}
	for _, tt := range tests {
		if got := VersionAllowed(tt.v, false); got != tt.strict {
			t.Errorf("VersionAllowed(%q, false) = %v, want %v", tt.v, got, tt.strict)
		}
		if got := VersionAllowed(tt.v, true); got != tt.any {
			t.Errorf("VersionAllowed(%q, true) = %v, want %v", tt.v, got, tt.any)
		}
	}
}
