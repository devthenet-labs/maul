// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/mod/sumdb/note"
	modzip "golang.org/x/mod/zip"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// fakeModule is a module version a fake proxy serves.
type fakeModule struct {
	path, version string
	mod, zip      []byte
	info          string
	h1, modH1     string
}

// makeModule builds a module with one package exporting X.
func makeModule(t *testing.T, path, version string) fakeModule {
	t.Helper()
	dir := t.TempDir()
	gomod := "module " + path + "\n\ngo 1.21\n"
	write(t, filepath.Join(dir, "go.mod"), gomod)
	write(t, filepath.Join(dir, "x.go"), "package "+filepath.Base(path)+"\n\nconst X = 1\n")
	var zbuf bytes.Buffer
	mv := module.Version{Path: path, Version: version}
	if err := modzip.CreateFromDir(&zbuf, mv, dir); err != nil {
		t.Fatal(err)
	}
	zpath := filepath.Join(t.TempDir(), "m.zip")
	if err := os.WriteFile(zpath, zbuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := dirhash.HashZip(zpath, dirhash.Hash1)
	if err != nil {
		t.Fatal(err)
	}
	modH1, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(gomod)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fakeModule{path: path, version: version, mod: []byte(gomod), zip: zbuf.Bytes(),
		info: `{"Version":"` + version + `","Time":"2026-01-01T00:00:00Z"}`, h1: h1, modH1: modH1}
}

// serve answers a GOPROXY path for m below prefix, or reports false.
func (m fakeModule) serve(w http.ResponseWriter, p string) bool {
	esc, _ := module.EscapePath(m.path)
	rest, ok := strings.CutPrefix(p, "/"+esc+"/")
	if !ok {
		return false
	}
	switch rest {
	case "@v/list":
		_, _ = io.WriteString(w, m.version+"\n")
	case "@latest", "@v/" + m.version + ".info":
		_, _ = io.WriteString(w, m.info)
	case "@v/" + m.version + ".mod":
		_, _ = w.Write(m.mod)
	case "@v/" + m.version + ".zip":
		_, _ = w.Write(m.zip)
	default:
		return false
	}
	return true
}

// goProxyHandler serves mods, 404 otherwise; with auth set, it demands it.
func goProxyHandler(auth string, mods ...fakeModule) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth != "" && r.Header.Get("Authorization") != auth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := strings.TrimPrefix(r.URL.EscapedPath(), goBase)
		for _, m := range mods {
			if m.serve(w, p) {
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not found\n")
	}
}

// TestLiveGoRegistry runs the real go command against the engine with a
// basic-auth Go registry serving one private module, a fake
// proxy.golang.org serving one public module, and a fake sum.golang.org
// signing with a test key. The profile has route go and a go grant whose
// privateModules are example.com/acme/ and publicChecksumLookups false.
// The private module builds with no checksum lookup and never reaches the
// fake Google; the public one, which the registry 404s, is served by /go
// and verified through /r/<registry>/sumdb, the database proxy the go
// command chose; a private path the registry lacks never reaches the fake
// proxy.golang.org. Opt-in: PATCHY_LIVE_REGISTRY=1.
func TestLiveGoRegistry(t *testing.T) {
	skipUnlessLiveRegistry(t, "go")
	private := makeModule(t, "example.com/acme/x", "v1.0.0")
	public := makeModule(t, "example.com/pub", "v1.2.0")
	skey, vkey, err := note.GenerateKey(rand.Reader, SumDBName)
	if err != nil {
		t.Fatal(err)
	}
	ops := sumdb.NewTestServer(skey, func(path, vers string) ([]byte, error) {
		if path == public.path && vers == public.version {
			return []byte(path + " " + vers + " " + public.h1 + "\n" + path + " " + vers + "/go.mod " +
				public.modH1 + "\n"), nil
		}
		return nil, os.ErrNotExist
	})
	sumHandler := http.StripPrefix("/sumdb-base", sumdb.NewServer(ops))
	sumUp := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { sumHandler.ServeHTTP(w, r) })
	googleUp := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if !public.serve(w, r.URL.EscapedPath()) {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	w := newWorld(t)
	regUp := w.host(goHost, goProxyHandler(basicAuth, private))
	creds := newFakeCreds()
	basic, _ := BasicCredential("patchy", sentinel)
	creds.set("art-go", basic)
	reg := goRegistry(false)
	// The grant admits all of example.com/, so the registry is asked for
	// the public module too, answers 404, and the go command falls
	// through to /go.
	p := goProfile(reg, func(g *netprofile.Grant) { g.Packages = []string{"example.com/"} })
	p.Modules = netprofile.Prefixes{"example.com/pub"}
	h := newRegHarness(t, w, creds, Config{Profiles: mustProfiles(t, p), Registries: mustRegistries(t, reg)},
		googleUp, sumUp)

	g := liveGo{t: t, bin: "go", cache: t.TempDir(), path: os.Getenv("PATH")}
	gopath := t.TempDir()
	writableCleanup(t, gopath)
	env := []string{
		"GOENV=off",
		"GOPROXY=" + h.urls["gr"] + "/r/art-go," + h.urls["gr"] + "/go",
		"GOSUMDB=" + vkey,
		"GONOSUMDB=example.com/acme/",
		"GONOPROXY=patchy.invalid",
		"GOPRIVATE=patchy.invalid",
		"GOINSECURE=patchy.invalid",
		"GOAUTH=off",
		"GOTOOLCHAIN=local",
		"GOPATH=" + gopath,
		"GOMODCACHE=" + filepath.Join(gopath, "pkg", "mod"),
	}
	dir := fixtureModule(t, map[string]string{private.path: private.version, public.path: public.version},
		private.path, public.path)
	if out, err := g.run(dir, env, "build", "./..."); err != nil {
		t.Fatalf("go build: %v\n%s\naudit:\n%s", err, out, h.log.String())
	}
	gosum, _ := os.ReadFile(filepath.Join(dir, "go.sum"))
	if !strings.Contains(string(gosum), public.path+" "+public.version+" "+public.h1) {
		t.Errorf("go.sum lacks the verified public module:\n%s", gosum)
	}
	checkGoRegistryHits(t, h, regUp, googleUp, sumUp)
	// A private path the registry lacks falls through to /go, which
	// refuses it: never the fake proxy.golang.org.
	if out, err := g.run(dir, env, "mod", "download", "example.com/acme/missing@v1.0.0"); err == nil {
		t.Errorf("a missing private module downloaded:\n%s", out)
	}
	if !strings.Contains(h.log.String(), `"route":"go","kind":"info","module":"example.com/acme/missing"`) ||
		!strings.Contains(h.log.String(), `"outcome":"private_module"`) {
		t.Errorf("the /go route did not refuse the private fall-through:\n%s", h.log.String())
	}
	for _, hit := range googleUp.Hits() {
		if strings.Contains(hit.requestURI, "acme") {
			t.Errorf("the fake proxy.golang.org saw %s", hit.requestURI)
		}
	}
	t.Logf("audit log:\n%s", h.log.String())
}

// checkGoRegistryHits: Google saw no private path and no credential, the
// public module was verified through the registry route, and the registry
// served the private zip and was asked (and 404ed) the public module.
func checkGoRegistryHits(t *testing.T, h *regHarness, regUp, googleUp, sumUp *recordingUpstream) {
	t.Helper()
	for _, hit := range append(googleUp.Hits(), sumUp.Hits()...) {
		if strings.Contains(hit.requestURI, "acme") {
			t.Errorf("a private path reached Google: %s", hit.requestURI)
		}
		if hit.header.Get("Authorization") != "" {
			t.Errorf("Google was sent a credential: %s", hit.requestURI)
		}
	}
	if !strings.Contains(h.log.String(), `"route":"r/art-go","kind":"lookup","module":"example.com/pub"`) {
		t.Errorf("the public module was not verified through the registry route:\n%s", h.log.String())
	}
	sawPrivate, askedPublic := false, false
	for _, hit := range regUp.Hits() {
		sawPrivate = sawPrivate || strings.Contains(hit.requestURI, "/example.com/acme/x/@v/v1.0.0.zip")
		askedPublic = askedPublic || strings.Contains(hit.requestURI, "/example.com/pub/")
	}
	if !sawPrivate || !askedPublic {
		t.Errorf("registry hits %v: want the private zip and a (404) public request", regUp.Hits())
	}
}
