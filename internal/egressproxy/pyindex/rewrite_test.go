// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package pyindex

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"math/rand"
	"net/url"
	"path"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	xhtml "golang.org/x/net/html"
)

// seed fixes every property's generator, so the gate is deterministic.
const seed = 20261015

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

const route = "/r/ca-pypi"

// testOrigins are the registry (origin 0, files under /repo/pypi), a file
// host and a file host on another port.
var testOrigins = []Origin{{Host: "reg.example", Prefix: "/repo/pypi"}, {Host: "files.example"},
	{Host: "cdn.example:8443"}}

var testPage = mustParse("https://reg.example/repo/pypi/simple/six/")

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

// anchor is one <a> of an output page.
type anchor struct {
	href  string
	attrs map[string]string
	text  string
}

// anchors parses a page with the HTML5 parser (as pip does, roughly) and
// returns every <a> and the count of elements of each kind.
func anchors(t *testing.T, page []byte) ([]anchor, map[string]int) {
	t.Helper()
	doc, err := xhtml.Parse(bytes.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	var out []anchor
	elems := map[string]int{}
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			elems[n.Data]++
			if n.Data == "a" {
				a := anchor{attrs: map[string]string{}}
				for _, at := range n.Attr {
					if at.Key == "href" {
						a.href = at.Val
					} else {
						a.attrs[at.Key] = at.Val
					}
				}
				if n.FirstChild != nil {
					a.text = n.FirstChild.Data
				}
				out = append(out, a)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out, elems
}

// TestRewriteExamples: real index shapes.
func TestRewriteExamples(t *testing.T) {
	const sha = "sha256=" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pypiPage := `<!DOCTYPE html><html><body><h1>Links for six</h1>
<a href="https://files.pythonhosted.org/packages/d9/5a/six-1.16.0-py2.py3-none-any.whl#` + sha +
		`" data-requires-python="&gt;=2.7, !=3.0.*" data-dist-info-metadata="sha256=ab">` +
		`six-1.16.0-py2.py3-none-any.whl</a><br/>
<a href="https://files.pythonhosted.org/packages/71/39/six-1.16.0.tar.gz#md5=abc">six-1.16.0.tar.gz</a>
<a href="https://evil.example/six-1.16.0.tar.gz">x</a>
</body></html>`
	artifactoryPage := `<html><body>
<a href="../../packages/packages/aa/bb/six-1.16.0-py2.py3-none-any.whl#` + sha + `" data-yanked="">six</a>
<a href="../../../escape/six-1.16.0.tar.gz">escape</a>
<a href="../../packages/x/torch-2.3.0%2Bcu121-cp312-cp312-linux_x86_64.whl">torch</a>
<a href="../../packages/x/six-1.16.0-py3-none-any.whl.metadata">meta</a>
</body></html>`
	pypiOrigins := []Origin{{Host: "reg.example", Prefix: "/repo/pypi"}, {Host: "files.pythonhosted.org"}}
	for _, tt := range []struct {
		name      string
		page      string
		origins   []Origin
		allow     func(string) bool
		wantHrefs []string
		dropped   int
	}{
		{"pypi", pypiPage, pypiOrigins, nil, []string{
			route + "/f/1/packages/d9/5a/six-1.16.0-py2.py3-none-any.whl#" + sha,
			route + "/f/1/packages/71/39/six-1.16.0.tar.gz",
		}, 1},
		{"artifactory", artifactoryPage, testOrigins, nil, []string{
			route + "/f/0/repo/pypi/packages/packages/aa/bb/six-1.16.0-py2.py3-none-any.whl#" + sha,
			route + "/f/0/repo/pypi/packages/x/torch-2.3.0%2Bcu121-cp312-cp312-linux_x86_64.whl",
			route + "/f/0/repo/pypi/packages/x/six-1.16.0-py3-none-any.whl.metadata",
		}, 1},
		{"allowlist", artifactoryPage, testOrigins, func(p string) bool { return p == "six" }, []string{
			route + "/f/0/repo/pypi/packages/packages/aa/bb/six-1.16.0-py2.py3-none-any.whl#" + sha,
			route + "/f/0/repo/pypi/packages/x/six-1.16.0-py3-none-any.whl.metadata",
		}, 2},
		{"base href", `<base href="https://files.pythonhosted.org/p/"><a href="six-1.16.0.tar.gz">s</a>`,
			pypiOrigins, nil, []string{route + "/f/1/p/six-1.16.0.tar.gz"}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, st, err := Rewrite([]byte(tt.page), testPage, tt.origins, route, "six", tt.allow)
			if err != nil {
				t.Fatal(err)
			}
			as, _ := anchors(t, out)
			var got []string
			for _, a := range as {
				got = append(got, a.href)
			}
			if !reflect.DeepEqual(got, tt.wantHrefs) || st.Dropped != tt.dropped || st.Rewritten != len(tt.wantHrefs) {
				t.Errorf("hrefs = %q, stats %+v\nwant %q, %d dropped\n%s", got, st, tt.wantHrefs, tt.dropped, out)
			}
			if strings.Contains(string(out), "pythonhosted") || strings.Contains(string(out), "reg.example") {
				t.Errorf("an upstream host survived:\n%s", out)
			}
		})
	}
	out, _, _ := Rewrite([]byte(pypiPage), testPage, pypiOrigins, route, "six", nil)
	as, _ := anchors(t, out)
	if as[0].attrs["data-requires-python"] != ">=2.7, !=3.0.*" || as[0].attrs["data-dist-info-metadata"] != "sha256=ab" ||
		as[0].text != "six-1.16.0-py2.py3-none-any.whl" {
		t.Errorf("first anchor = %+v", as[0])
	}
	if _, _, err := Rewrite(make([]byte, MaxPageBytes+1), testPage, testOrigins, route, "six", nil); !errors.Is(err,
		ErrPageTooLarge) {
		t.Errorf("an oversized page = %v", err)
	}
	if !strings.Contains(string(out), `<meta name="pypi:repository-version" content="1.0">`) {
		t.Errorf("no repository version:\n%s", out)
	}
}

// TestJudgeAndParseFileRoute: the file route's stateless judge.
func TestJudgeAndParseFileRoute(t *testing.T) {
	for _, tt := range []struct {
		escaped string
		ok      bool
		up      string
	}{
		{"/f/0/repo/pypi/packages/six-1.16.0.tar.gz", true, "https://reg.example/repo/pypi/packages/six-1.16.0.tar.gz"},
		{"/f/1/x/torch-2.3.0%2Bcu121-cp312-cp312-linux_x86_64.whl", true,
			"https://files.example/x/torch-2.3.0%2Bcu121-cp312-cp312-linux_x86_64.whl"},
		{"/f/2/six-1.16.0.tar.gz", true, "https://cdn.example:8443/six-1.16.0.tar.gz"},
		{"/f/0/other/six-1.16.0.tar.gz", false, ""},
		{"/f/0/repo/pypix/six-1.16.0.tar.gz", false, ""},
		{"/f/0/repo/pypi/%2e%2e/six-1.16.0.tar.gz", false, ""},
		{"/f/0/repo/pypi/a%2fb/six-1.16.0.tar.gz", false, ""},
		{"/f/0/repo/pypi/a%5cb/six-1.16.0.tar.gz", false, ""},
		{"/f/0/repo/pypi/a%25b/six-1.16.0.tar.gz", false, ""},
		{"/f/0/repo/pypi/a;x=y/six-1.16.0.tar.gz", false, ""},
		{"/f/0/repo/pypi/a\\b/six-1.16.0.tar.gz", false, ""},
		{"/f/3/six-1.16.0.tar.gz", false, ""},
		{"/f/01/six-1.16.0.tar.gz", false, ""},
		{"/f/+1/six-1.16.0.tar.gz", false, ""},
		{"/f/x/six-1.16.0.tar.gz", false, ""},
		{"/f/1/readme.txt", false, ""},
		{"/g/1/six-1.16.0.tar.gz", false, ""},
	} {
		k, raw, err := ParseFileRoute(tt.escaped)
		var u *url.URL
		if err == nil {
			_, u, err = Judge(k, raw, testOrigins)
		}
		if (err == nil) != tt.ok || (tt.ok && u.String() != tt.up) {
			t.Errorf("%s = %v %v, want ok=%v %s", tt.escaped, u, err, tt.ok, tt.up)
		}
	}
}

// genLink is one generated upstream link and what the rewrite must make of
// it.
type genLink struct {
	href  string
	attrs map[string]string
	text  string
	// keep: the link passes; then want is the emitted href (fragment
	// included), file its basename, k and up its origin and upstream URL.
	keep bool
	want string
	file string
	k    int
	up   string
}

// genPage is a generated upstream page.
type genPage struct {
	links     []genLink
	allowlist bool
	page      []byte
}

const segAlphabet = "abcXYZ019._~!+=-"

func genSeg(r *rand.Rand) string {
	n := 1 + r.Intn(6)
	var b strings.Builder
	for range n {
		b.WriteByte(segAlphabet[r.Intn(len(segAlphabet))])
	}
	s := b.String()
	if strings.Trim(s, ".") == "" {
		s += "a"
	}
	return s
}

// nasty is attribute and text content an escaper must survive; it names an
// origin host on purpose, which the href property must not object to.
var nasty = []string{`"><script>x</script>`, `' onclick='x'`, `&amp;&lt;`, `reg.example`,
	`https://files.example/x`, `>=3.8`, ``, `a"b'c<d>e&f`}

func genValue(r *rand.Rand) string {
	if r.Intn(8) == 0 {
		return strings.Repeat("x", 250+r.Intn(20))
	}
	return nasty[r.Intn(len(nasty))] + nasty[r.Intn(len(nasty))]
}

var files = []string{"six-1.16.0-py2.py3-none-any.whl", "six-1.16.0.tar.gz", "six-1.16.0.zip",
	"six-1.16.0%2Bcu121-py3-none-any.whl", "six-1.16.0-py3-none-any.whl.metadata", "Six-1.16.0.tar.bz2"}

// Generate builds a page of 0-12 links of every kind.
func (genPage) Generate(r *rand.Rand, _ int) reflect.Value {
	g := genPage{allowlist: r.Intn(2) == 0}
	var page strings.Builder
	page.WriteString("<html><body>\n")
	for range r.Intn(13) {
		l := genLink{attrs: map[string]string{}, text: genValue(r), keep: true}
		dir := genSeg(r) + "/" + genSeg(r)
		file := files[r.Intn(len(files))]
		switch r.Intn(4) {
		case 0:
			l.href, l.k, l.up = "https://reg.example/repo/pypi/"+dir+"/"+file, 0, "/repo/pypi/"+dir+"/"+file
		case 1:
			l.href, l.k, l.up = "../../"+dir+"/"+file, 0, "/repo/pypi/"+dir+"/"+file
		case 2:
			l.href, l.k, l.up = "https://files.example/"+dir+"/"+file, 1, "/"+dir+"/"+file
		default:
			l.href, l.k, l.up = "https://CDN.example:8443/"+file, 2, "/"+file
		}
		mutate(r, &l, dir, file, g.allowlist)
		if l.keep {
			l.want = route + "/f/" + fmt.Sprint(l.k) + l.up
			dec, _ := url.PathUnescape(l.up)
			l.file = path.Base(dec)
		}
		switch r.Intn(4) {
		case 0:
			h := fmt.Sprintf("sha256=%064x", r.Uint64())
			l.href += "#" + h
			if l.keep {
				l.want += "#" + h
			}
		case 1:
			l.href += "#md5=" + fmt.Sprint(r.Intn(1000))
		}
		for _, k := range copiedAttrs {
			if r.Intn(2) == 0 {
				l.attrs[k] = genValue(r)
			}
		}
		page.WriteString(`<a href="` + html.EscapeString(l.href) + `"`)
		for k, v := range l.attrs {
			page.WriteString(" " + k + `="` + html.EscapeString(v) + `"`)
		}
		page.WriteString(` data-other="` + html.EscapeString(genValue(r)) + `">` + html.EscapeString(l.text) +
			"</a><br>\n")
		g.links = append(g.links, l)
	}
	page.WriteString("</body></html>\n")
	g.page = []byte(page.String())
	return reflect.ValueOf(g)
}

// mutate makes one link in three fail a rule (or, for another project's
// file, fail only under an allowlist).
func mutate(r *rand.Rand, l *genLink, dir, file string, allowlist bool) {
	// One mutation in three makes the link fail a rule.
	switch r.Intn(15) {
	case 0:
		if rest, ok := strings.CutPrefix(l.href, "https://"); ok {
			l.href, l.keep = "http://"+rest, false
		}
	case 1:
		l.href += "?x=1"
		l.keep = false
	case 2:
		l.href = "https://user:pw@files.example/" + dir + "/" + file
		l.keep = false
	case 3:
		l.href = "https://evil.example/" + dir + "/" + file
		l.keep = false
	case 4:
		l.href = "https://files.example/" + dir + "/%2e%2e/" + file
		l.keep = false
	case 5:
		l.href = "https://files.example/a%2fb/" + file
		l.keep = false
	case 6:
		l.href = "https://reg.example/elsewhere/" + file
		l.keep = false
	case 7:
		l.href = "https://files.example/" + dir + "/README.txt"
		l.keep = false
	case 8:
		l.href = "https://files.example/" + dir + "/requests-2.0.0.tar.gz"
		l.k, l.up, l.keep = 1, "/"+dir+"/requests-2.0.0.tar.gz", !allowlist
	}
}

func (g genPage) allow() func(string) bool {
	if !g.allowlist {
		return nil
	}
	return func(p string) bool { return p == "six" }
}

// singletons are the generated page's fixed elements.
var singletons = map[string]bool{"html": true, "head": true, "body": true, "meta": true, "title": true, "h1": true}

// TestRewriteProperties checks, over seeded generated pages:
//   - NoAbsoluteURLOut: every emitted href starts with the route's file
//     prefix and names no origin host (text and attributes may);
//   - LinkPreservation: every link that passes appears exactly once, in
//     order, with its path, basename and valid hash fragment;
//   - DroppedCount: rewritten plus dropped is the input's links;
//   - AttrEscaping: copied data- values and hrefs survive an HTML parser
//     unchanged, and the page holds only the elements the generator makes;
//   - HrefRoundTrip: each emitted href, judged by the file route, maps back
//     to exactly its origin and upstream path.
func TestRewriteProperties(t *testing.T) {
	prop := func(g genPage) bool {
		out, st, err := Rewrite(g.page, testPage, testOrigins, route, "six", g.allow())
		if err != nil {
			t.Logf("rewrite: %v", err)
			return false
		}
		as, elems := anchors(t, out)
		var kept []genLink
		for _, l := range g.links {
			if l.keep {
				kept = append(kept, l)
			}
		}
		if st.Rewritten+st.Dropped != len(g.links) || st.Rewritten != len(kept) || len(as) != len(kept) {
			t.Logf("stats %+v, %d anchors, %d kept of %d\n%s\n%s", st, len(as), len(kept), len(g.links), g.page, out)
			return false
		}
		for name, n := range elems {
			repeated := name == "a" || name == "br"
			if !repeated && !singletons[name] || !repeated && n != 1 {
				t.Logf("element %s x%d in\n%s", name, n, out)
				return false
			}
		}
		for i, a := range as {
			if !checkAnchor(t, a, kept[i]) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(2000)); err != nil {
		t.Error(err)
	}
}

// checkAnchor holds one emitted anchor to the link it came from.
func checkAnchor(t *testing.T, a anchor, l genLink) bool {
	if a.href != l.want || a.text != l.file {
		t.Logf("anchor %+v, want %s %s", a, l.want, l.file)
		return false
	}
	if !strings.HasPrefix(a.href, route+"/f/") {
		return false
	}
	for _, o := range testOrigins {
		if strings.Contains(strings.ToLower(a.href), strings.ToLower(o.Host)) {
			return false
		}
	}
	for k, v := range l.attrs {
		if len(v) <= maxAttrBytes && a.attrs[k] != v {
			t.Logf("attr %s = %q, want %q", k, a.attrs[k], v)
			return false
		}
		if len(v) > maxAttrBytes && a.attrs[k] != "" {
			return false
		}
	}
	for k := range a.attrs {
		if _, ok := l.attrs[k]; !ok {
			t.Logf("attr %s appeared", k)
			return false
		}
	}
	ref, _, _ := strings.Cut(strings.TrimPrefix(a.href, route), "#")
	k, raw, err := ParseFileRoute(ref)
	if err != nil {
		return false
	}
	_, u, err := Judge(k, raw, testOrigins)
	if err != nil || k != l.k || u.EscapedPath() != l.up || u.Host != strings.ToLower(testOrigins[l.k].Host) {
		t.Logf("href %s judged %d %v %v, want %d %s", a.href, k, u, err, l.k, l.up)
		return false
	}
	return true
}
