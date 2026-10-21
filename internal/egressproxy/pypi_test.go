// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

const (
	pyHost   = "pypi.reg.example"
	pyPrefix = "/repository/pypi-all"
	fileHost = "files.example"
	pySHA    = "sha256=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func pyRegistry() netprofile.Registry {
	return netprofile.Registry{Name: "nexus-pypi", Ecosystem: netprofile.EcosystemPyPI,
		URL: "https://" + pyHost + pyPrefix + "/simple/", Auth: netprofile.Auth{Type: netprofile.AuthBasic},
		FileHosts: []string{fileHost}}
}

// sixPage is a simple page linking six's files on the registry (relative)
// and on the file host (absolute), plus links that must be dropped.
const sixPage = `<!DOCTYPE html><html><body><h1>Links for six</h1>
<a href="../../packages/six/1.16.0/six-1.16.0-py2.py3-none-any.whl#` + pySHA + `" data-requires-python="&gt;=2.7">w</a>
<a href="https://files.example/packages/ab/six-1.16.0.tar.gz">s</a>
<a href="../../packages/torch/torch-2.3.0%2Bcu121-cp312-cp312-linux_x86_64.whl">t</a>
<a href="https://pypi.reg.example/elsewhere/six-1.16.0.tar.gz">outside</a>
<a href="https://other.example/six-1.16.0.tar.gz">other host</a>
<a href="http://files.example/six-1.16.0.tar.gz">plain</a>
</body></html>`

// pyRig is an engine with nexus-pypi granted to profile py with mod's
// grant, the registry answering reg and the file host files.
func pyRig(t *testing.T, reg, files http.HandlerFunc, mod func(*netprofile.Grant),
) (*regHarness, *recordingUpstream, *recordingUpstream) {
	t.Helper()
	w := newWorld(t)
	up := w.host(pyHost, reg)
	fh := w.host(fileHost, files)
	creds := newFakeCreds()
	c, err := BasicCredential("maul", sentinel)
	if err != nil {
		t.Fatal(err)
	}
	creds.set("nexus-pypi", c)
	r := pyRegistry()
	h := newRegHarness(t, w, creds, Config{
		Profiles: mustProfiles(t, netprofile.Profile{Name: "py", Port: 8098,
			Registries: []netprofile.Grant{grantOf(r, mod)}}),
		Registries: mustRegistries(t, r),
	}, nil, nil)
	return h, up, fh
}

func (h *regHarness) py(t *testing.T, path string) result {
	t.Helper()
	return h.do(t, "py", http.MethodGet, path, nil)
}

var basicAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte("maul:"+sentinel))

// TestPyPIPage: a simple page is fetched with the credential and
// text/html, and regenerated: links into the proxy only, the bad ones
// dropped and counted, no upstream host left in the body.
func TestPyPIPage(t *testing.T) {
	testMetrics()
	before := sum(t, "maul.egress_proxy.index.links", map[string]string{"registry": "nexus-pypi",
		"result": "dropped"})
	h, up, _ := pyRig(t, okBody(sixPage), okBody("file"), func(g *netprofile.Grant) {
		g.PackagePolicy, g.Packages = "", []string{"six"}
	})
	res := h.py(t, "/r/nexus-pypi/simple/six/")
	if res.status != http.StatusOK || res.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("page = %d %v %q", res.status, res.header, res.body)
	}
	for _, want := range []string{
		`href="/r/nexus-pypi/f/0` + pyPrefix + `/packages/six/1.16.0/six-1.16.0-py2.py3-none-any.whl#` + pySHA + `"`,
		`data-requires-python="&gt;=2.7"`,
		`href="/r/nexus-pypi/f/1/packages/ab/six-1.16.0.tar.gz"`,
	} {
		if !strings.Contains(res.body, want) {
			t.Errorf("page lacks %s:\n%s", want, res.body)
		}
	}
	for _, gone := range []string{pyHost, fileHost, "other.example", "torch", "elsewhere"} {
		if strings.Contains(res.body, gone) {
			t.Errorf("page holds %q:\n%s", gone, res.body)
		}
	}
	hit := up.Hits()[0]
	if hit.requestURI != pyPrefix+"/simple/six/" || hit.header.Get("Authorization") != basicAuth ||
		hit.header.Get("Accept") != "text/html" {
		t.Errorf("page request = %+v", hit)
	}
	l := h.lastLine(t)
	if l["kind"] != kindPage || l["package"] != "six" || l["rewritten_links"] != float64(2) ||
		l["dropped_links"] != float64(4) {
		t.Errorf("page audit line = %v", l)
	}
	if got := sum(t, "maul.egress_proxy.index.links", map[string]string{"registry": "nexus-pypi",
		"result": "dropped"}) - before; got != 4 {
		t.Errorf("dropped links counter grew by %d, want 4", got)
	}
	// Under packagePolicy registry the torch wheel of another project
	// stays: only the allowlist pins files to the page's project.
	h, _, _ = pyRig(t, okBody(sixPage), okBody("file"), func(g *netprofile.Grant) {
		g.Packages, g.PackagePolicy = nil, netprofile.PackagePolicyRegistry
	})
	if res := h.py(t, "/r/nexus-pypi/simple/six/"); !strings.Contains(res.body, "torch-2.3.0%2Bcu121") {
		t.Errorf("registry policy dropped another project's file:\n%s", res.body)
	}
}

// TestPyPIFiles: origin 0 files carry the credential and leave with the
// page's spelling (%2B included); file-host files never carry it; every
// path outside the grammar or the prefix is 403 with no contact.
func TestPyPIFiles(t *testing.T) {
	h, up, fh := pyRig(t, okBody("wheel"), okBody("sdist"), func(g *netprofile.Grant) {
		g.Packages, g.PackagePolicy = nil, netprofile.PackagePolicyRegistry
	})
	torch := pyPrefix + "/packages/torch/torch-2.3.0%2Bcu121-cp312-cp312-linux_x86_64.whl"
	for _, tt := range [][2]string{
		{"/r/nexus-pypi/f/0" + torch, torch},
		{"/r/nexus-pypi/f/0" + pyPrefix + "/p/six-1.16.0.tar.gz", pyPrefix + "/p/six-1.16.0.tar.gz"},
	} {
		path, want := tt[0], tt[1]
		res := h.py(t, path)
		hits := up.Hits()
		if res.status != http.StatusOK || hits[len(hits)-1].requestURI != want ||
			hits[len(hits)-1].header.Get("Authorization") != basicAuth {
			t.Errorf("%s = %d, reached %+v", path, res.status, hits[len(hits)-1])
		}
	}
	if l := h.lastLine(t); l["kind"] != kindFile || l["version"] != "1.16.0" || l["package"] != "six" {
		t.Errorf("file audit line = %v", l)
	}
	if res := h.py(t, "/r/nexus-pypi/f/1/packages/ab/six-1.16.0.tar.gz"); res.status != 200 || res.body != "sdist" {
		t.Errorf("file host = %d %q", res.status, res.body)
	}
	if hit := fh.Hits()[0]; hit.header.Get("Authorization") != "" || hit.requestURI != "/packages/ab/six-1.16.0.tar.gz" {
		t.Errorf("file host request = %+v", hit)
	}
	if l := h.lastLine(t); l["credential"] != string(CredentialNone) {
		t.Errorf("file host audit credential = %v", l["credential"])
	}
	before := len(up.Hits()) + len(fh.Hits())
	for _, p := range []string{
		"/f/0/other/six-1.16.0.tar.gz",
		"/f/0" + pyPrefix + "x/six-1.16.0.tar.gz",
		"/f/0" + pyPrefix + "/%2e%2e/six-1.16.0.tar.gz",
		"/f/0" + pyPrefix + "/a%2fb/six-1.16.0.tar.gz",
		"/f/0" + pyPrefix + "/a%5cb/six-1.16.0.tar.gz",
		"/f/0" + pyPrefix + "/a%25b/six-1.16.0.tar.gz",
		"/f/0" + pyPrefix + "/a;x=y/six-1.16.0.tar.gz",
		"/f/0" + pyPrefix + "/a%5Cb/six-1.16.0.tar.gz",
		"/f/2/six-1.16.0.tar.gz",
		"/f/1/readme.txt",
		"/simple/",
		"/simple/Six/",
		"/simple/six",
		"/simple/six/x/",
		"/simple/six_x/",
		"/six-1.16.0.tar.gz",
		"/",
	} {
		res := h.py(t, "/r/nexus-pypi"+p)
		if res.status != http.StatusForbidden || h.lastLine(t)["outcome"] != OutcomeBadPath {
			t.Errorf("%s = %d %v, want 403 bad_path", p, res.status, h.lastLine(t))
		}
	}
	for _, line := range []string{"GET /r/nexus-pypi/f/0" + pyPrefix + "/a\\b/six-1.16.0.tar.gz HTTP/1.1",
		"GET /r/nexus-pypi/f/0" + pyPrefix + "/../x/six-1.16.0.tar.gz HTTP/1.1"} {
		if s := raw(t, h.urls["py"], line); s != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", line, s)
		}
	}
	if n := len(up.Hits()) + len(fh.Hits()) - before; n != 0 {
		t.Errorf("refused files reached an upstream %d times", n)
	}
}

// TestPyPIAllowlist: with an allowlist, a page or a file of another
// project is 403 not_allowed with no contact.
func TestPyPIAllowlist(t *testing.T) {
	h, up, fh := pyRig(t, okBody(sixPage), okBody("x"), func(g *netprofile.Grant) {
		g.PackagePolicy, g.Packages = "", []string{"six", "Charset_Normalizer"}
	})
	for p, status := range map[string]int{
		"/r/nexus-pypi/simple/flask/":                                    403,
		"/r/nexus-pypi/f/1/x/flask-3.0.0.tar.gz":                         403,
		"/r/nexus-pypi/f/1/x/charset-normalizer-2.0.0.tar.gz":            200,
		"/r/nexus-pypi/f/1/x/charset_normalizer-2.0.0-py3-none-any.whl":  200,
		"/r/nexus-pypi/simple/charset-normalizer/":                       200,
		"/r/nexus-pypi/f/0" + pyPrefix + "/x/six-1.16.0.tar.gz.metadata": 200,
	} {
		res := h.py(t, p)
		if res.status != status {
			t.Errorf("%s = %d %q, want %d", p, res.status, res.body, status)
		}
		if status == 403 && h.lastLine(t)["outcome"] != OutcomeNotAllowed {
			t.Errorf("%s outcome = %v", p, h.lastLine(t)["outcome"])
		}
	}
	h.py(t, "/r/nexus-pypi/f/0"+pyPrefix+"/x/six-1.16.0.tar.gz.metadata")
	if l := h.lastLine(t); l["kind"] != kindMetadata {
		t.Errorf("metadata kind = %v", l["kind"])
	}
	if len(up.Hits()) != 3 || len(fh.Hits()) != 2 {
		t.Errorf("hits: registry %d, file host %d; want 2, 2", len(up.Hits()), len(fh.Hits()))
	}
}

// TestPyPIEchoAndErrors: a page echoing the credential is 502
// credential_echo; error bodies are the proxy's own; .metadata is scanned,
// a wheel is not.
func TestPyPIEchoAndErrors(t *testing.T) {
	h, _, _ := pyRig(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/simple/six/"):
			_, _ = io.WriteString(w, `<a href="../../p/six-1.0.0.tar.gz">`+r.Header.Get("Authorization")+`</a>`)
		case strings.Contains(r.URL.Path, "/simple/seven/"):
			// A file name that parses (project six-s3cr3t-TOKEN, version
			// 1234) carries the secret into an href.
			_, _ = io.WriteString(w, `<a href="../../p/six-`+sentinel+`.tar.gz">x</a>`)
		case strings.HasSuffix(r.URL.Path, ".metadata"), strings.HasSuffix(r.URL.Path, ".whl"):
			_, _ = io.WriteString(w, "Name: six "+sentinel)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "missing "+sentinel)
		}
	}, okBody("x"), nil)
	// The echo in link text never reaches the generated page (text is
	// replaced by the file name), so the page passes: the scan judges what
	// leaves.
	if res := h.py(t, "/r/nexus-pypi/simple/six/"); res.status != http.StatusOK || strings.Contains(res.body,
		sentinel) || strings.Contains(res.body, strings.TrimPrefix(basicAuth, "Basic ")) {
		t.Errorf("page = %d %q", res.status, res.body)
	}
	if res := h.py(t, "/r/nexus-pypi/simple/seven/"); res.status != http.StatusBadGateway ||
		h.lastLine(t)["outcome"] != OutcomeCredentialEcho || strings.Contains(res.body, sentinel) {
		t.Errorf("page with the secret in an href = %d %q", res.status, res.body)
	}
	if res := h.py(t, "/r/nexus-pypi/f/0"+pyPrefix+"/p/six-1.0.0-py3-none-any.whl.metadata"); res.status !=
		http.StatusBadGateway || h.lastLine(t)["outcome"] != OutcomeCredentialEcho {
		t.Errorf("echoing metadata = %d %v", res.status, h.lastLine(t))
	}
	if res := h.py(t, "/r/nexus-pypi/f/0"+pyPrefix+"/p/six-1.0.0-py3-none-any.whl"); res.status != http.StatusOK {
		t.Errorf("wheel = %d (archives are not scanned)", res.status)
	}
	res := h.py(t, "/r/nexus-pypi/simple/flask/")
	if res.status != http.StatusNotFound || strings.Contains(res.body, sentinel) || strings.Contains(res.body, "missing") {
		t.Errorf("404 page = %d %q", res.status, res.body)
	}
}

// TestPyPIEchoInPage: an echo that survives rewriting (in a copied
// attribute) is withheld.
func TestPyPIEchoInPage(t *testing.T) {
	h, _, _ := pyRig(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<a href="../../p/six-1.0.0.tar.gz" data-yanked="`+sentinel+`">x</a>`)
	}, okBody("x"), nil)
	if res := h.py(t, "/r/nexus-pypi/simple/six/"); res.status != http.StatusBadGateway ||
		h.lastLine(t)["outcome"] != OutcomeCredentialEcho || strings.Contains(res.body, sentinel) {
		t.Errorf("echoing page = %d %q", res.status, res.body)
	}
}

// TestCodeArtifactPyPI: a CodeArtifact pypi registry is sent basic auth as
// user aws.
func TestCodeArtifactPyPI(t *testing.T) {
	const host = "maul-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com"
	w := newWorld(t)
	up := w.host(host, okBody(`<a href="../../six/six-1.16.0.tar.gz">s</a>`))
	r := netprofile.Registry{Name: "ca-pypi", Ecosystem: netprofile.EcosystemPyPI,
		URL: "https://" + host + "/pypi/pypi-store/simple/", Auth: netprofile.Auth{Type: netprofile.AuthCodeArtifact,
			CodeArtifact: &netprofile.CodeArtifact{Domain: "maul-gate", DomainOwner: "377946145366",
				Region: "us-east-1"}}}
	creds := newFakeCreds()
	c, _ := CodeArtifactCredential(netprofile.EcosystemPyPI, sentinel, time.Now().Add(time.Hour))
	creds.set("ca-pypi", c)
	h := newRegHarness(t, w, creds, Config{
		Profiles: mustProfiles(t, netprofile.Profile{Name: "py", Port: 8098,
			Registries: []netprofile.Grant{grantOf(r, nil)}}),
		Registries: mustRegistries(t, r),
	}, nil, nil)
	res := h.py(t, "/r/ca-pypi/simple/six/")
	if res.status != 200 || !strings.Contains(res.body, `href="/r/ca-pypi/f/0/pypi/pypi-store/six/six-1.16.0.tar.gz"`) {
		t.Errorf("page = %d %q", res.status, res.body)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("aws:"+sentinel))
	if got := up.Hits()[0].header.Get("Authorization"); got != want {
		t.Errorf("Authorization = %q, want basic aws:<token>", got)
	}
}
