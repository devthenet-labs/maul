// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // npm's shasum field is SHA-1 by definition
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// liveRegistryEnv opts into TestLiveRegistry and TestLiveGoRegistry, which
// run the real npm, pip and go commands (mise run live-registry). They
// reach nothing beyond loopback.
const liveRegistryEnv = "PATCHY_LIVE_REGISTRY"

func skipUnlessLiveRegistry(t *testing.T, tools ...string) {
	t.Helper()
	if os.Getenv(liveRegistryEnv) != "1" {
		t.Skipf("set %s=1 to run the live registry tests (the real npm, pip and go against loopback registries)",
			liveRegistryEnv)
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH: %v", tool, err)
		}
	}
}

// npmPackage is a package the fake npm registry serves.
type npmPackage struct {
	scope, name, version string
	tgz                  []byte
}

func (p npmPackage) full() string { return netprofile.NPMFullName(p.scope, p.name) }

// tarballPath is the canonical tarball path below the registry base.
func (p npmPackage) tarballPath() string {
	return "/" + p.full() + "/-/" + p.name + "-" + p.version + ".tgz"
}

// makeTarball is a package tarball as npm pack makes it.
func makeTarball(t *testing.T, full, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"package/package.json": `{"name":"` + full + `","version":"` + version + `","main":"index.js"}`,
		"package/index.js":     "module.exports = 42;\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// npmRegistryHandler is an Artifactory-shaped npm registry demanding the
// sentinel bearer token.
func npmRegistryHandler(pkgs []npmPackage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+sentinel {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := strings.TrimPrefix(r.URL.EscapedPath(), npmBase)
		for _, pkg := range pkgs {
			if p == pkg.tarballPath() {
				_, _ = w.Write(pkg.tgz)
				return
			}
			esc := "/" + pkg.name
			if pkg.scope != "" {
				esc = "/@" + pkg.scope + "%2f" + pkg.name
			}
			if p != esc {
				continue
			}
			sha := sha512.Sum512(pkg.tgz)
			sha1sum := sha1.Sum(pkg.tgz) //nolint:gosec // npm's shasum
			doc := map[string]any{
				"name":      pkg.full(),
				"dist-tags": map[string]string{"latest": pkg.version},
				"versions": map[string]any{pkg.version: map[string]any{
					"name": pkg.full(), "version": pkg.version,
					"dist": map[string]string{
						"tarball":   "https://" + npmHost + npmBase + pkg.tarballPath(),
						"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sha[:]),
						"shasum":    hex.EncodeToString(sha1sum[:]),
					},
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

// makeWheel is a minimal pure-Python wheel.
func makeWheel(t *testing.T, project, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	info := project + "-" + version + ".dist-info/"
	files := make([][2]string, 0, 4)
	files = append(files, [][2]string{
		{project + ".py", "x = 1\n"},
		{info + "METADATA", "Metadata-Version: 2.1\nName: " + project + "\nVersion: " + version + "\n\n"},
		{info + "WHEEL", "Wheel-Version: 1.0\nGenerator: patchy-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"},
	}...)
	var record strings.Builder
	for _, f := range files {
		sum := sha256.Sum256([]byte(f[1]))
		record.WriteString(f[0] + ",sha256=" + base64.RawURLEncoding.EncodeToString(sum[:]) + "," +
			strconv.Itoa(len(f[1])) + "\n")
	}
	record.WriteString(info + "RECORD,,\n")
	files = append(files, [2]string{info + "RECORD", record.String()})
	for _, f := range files {
		w, err := zw.Create(f[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// liveRegistryRig is the engine on loopback with an npm and a pip
// registry, each demanding its own login, and a credential-free file host.
type liveRegistryRig struct {
	h       *regHarness
	npmUp   *recordingUpstream
	pyUp    *recordingUpstream
	files   *recordingUpstream
	wheel   []byte
	pkgs    []npmPackage
	proxy   string
	sixWhl  string
	outputs []string
}

func newLiveRegistryRig(t *testing.T) *liveRegistryRig {
	t.Helper()
	w := newWorld(t)
	pkgs := []npmPackage{{name: "left-pad", version: "1.3.0"}, {scope: "acme", name: "bar", version: "2.0.0"}}
	for i := range pkgs {
		pkgs[i].tgz = makeTarball(t, pkgs[i].full(), pkgs[i].version)
	}
	rig := &liveRegistryRig{pkgs: pkgs, wheel: makeWheel(t, "six", "1.16.0"),
		sixWhl: "six-1.16.0-py3-none-any.whl"}
	rig.npmUp = w.host(npmHost, npmRegistryHandler(pkgs))
	sum := sha256.Sum256(rig.wheel)
	page := `<!DOCTYPE html><html><body><a href="https://` + fileHost + `/packages/ab/` + rig.sixWhl +
		`#sha256=` + hex.EncodeToString(sum[:]) + `" data-requires-python="&gt;=3">` + rig.sixWhl + `</a></body></html>`
	rig.pyUp = w.host(pyHost, func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != basicAuth {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == pyPrefix+"/simple/six/" {
			rw.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(rw, page)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	})
	rig.files = w.host(fileHost, func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/packages/ab/"+rig.sixWhl {
			_, _ = rw.Write(rig.wheel)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	})
	creds := newFakeCreds()
	bearer, _ := BearerCredential(sentinel)
	basic, _ := BasicCredential("patchy", sentinel)
	creds.set("art-npm", bearer)
	creds.set("nexus-pypi", basic)
	npmReg, pyReg := npmRegistry(), pyRegistry()
	rig.h = newRegHarness(t, w, creds, Config{
		Profiles: mustProfiles(t, netprofile.Profile{Name: "web", Port: 8095, Registries: []netprofile.Grant{
			grantOf(npmReg, nil),
			grantOf(pyReg, func(g *netprofile.Grant) { g.PackagePolicy, g.Packages = "", []string{"six"} }),
		}}),
		Registries: mustRegistries(t, npmReg, pyReg),
	}, nil, nil)
	rig.proxy = rig.h.urls["web"]
	return rig
}

// run runs a command in dir with exactly env (plus PATH) and records its
// output for the leak check.
func (r *liveRegistryRig) run(t *testing.T, dir string, env []string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	r.outputs = append(r.outputs, string(out))
	return string(out), err
}

// TestLiveRegistry runs the real npm and pip against the engine on
// loopback, with TLS registries that demand a login only the proxy holds:
// npm install of a scoped and an unscoped package and npm ci from the
// lockfile, whose resolved URLs stay the registry's canonical ones; pip
// download through a rewritten page whose file sits on a second host; a
// hostile .npmrc and pip.conf ignored; the credential nowhere in the
// clients' debug output or the audit. Opt-in: PATCHY_LIVE_REGISTRY=1.
func TestLiveRegistry(t *testing.T) {
	skipUnlessLiveRegistry(t, "npm", "python3")
	rig := newLiveRegistryRig(t)
	t.Run("npm", rig.npm)
	t.Run("pip", rig.pip)
	for _, out := range append(rig.outputs, rig.h.log.String()) {
		for _, leak := range []string{sentinel, strings.TrimPrefix(basicAuth, "Basic ")} {
			if strings.Contains(out, leak) {
				t.Errorf("the credential appeared in:\n%s", out)
			}
		}
	}
	for _, hit := range rig.files.Hits() {
		if hit.header.Get("Authorization") != "" {
			t.Error("the file host was sent the credential")
		}
	}
	t.Logf("audit log:\n%s", rig.h.log.String())
}

// npmEnv is §5.2 of the step-2 spec verbatim, with home and cache under
// tmp.
func (r *liveRegistryRig) npmEnv(tmp string) []string {
	return []string{
		"HOME=" + tmp,
		"NPM_CONFIG_REGISTRY=" + r.proxy + "/r/art-npm/",
		"NPM_CONFIG_REPLACE_REGISTRY_HOST=always",
		"NPM_CONFIG_AUDIT=false",
		"NPM_CONFIG_FUND=false",
		"NPM_CONFIG_UPDATE_NOTIFIER=false",
		"NPM_CONFIG_IGNORE_SCRIPTS=true",
		"NPM_CONFIG_CACHE=" + filepath.Join(tmp, "npm-cache"),
		"NPM_CONFIG_STRICT_SSL=true",
	}
}

func (r *liveRegistryRig) npm(t *testing.T) {
	dir := t.TempDir()
	tmp := t.TempDir()
	write(t, filepath.Join(dir, "package.json"), `{"name":"demo","version":"1.0.0","private":true}`)
	write(t, filepath.Join(dir, ".npmrc"), "registry=https://evil.invalid/\nreplace-registry-host=never\n")
	env := r.npmEnv(tmp)
	if out, err := r.run(t, dir, env, "npm", "install", "--loglevel", "silly", "left-pad@1.3.0",
		"@acme/bar@2.0.0"); err != nil {
		t.Fatalf("npm install: %v\n%s", err, out)
	}
	lock, err := os.ReadFile(filepath.Join(dir, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.pkgs {
		want := `"resolved": "https://` + npmHost + npmBase + p.tarballPath() + `"`
		if !strings.Contains(string(lock), want) {
			t.Errorf("lockfile lacks %s:\n%s", want, lock)
		}
	}
	if strings.Contains(string(lock), "127.0.0.1") {
		t.Errorf("the lockfile names the proxy:\n%s", lock)
	}
	for _, d := range []string{filepath.Join(dir, "node_modules"), filepath.Join(tmp, "npm-cache")} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
	}
	before := len(r.npmUp.Hits())
	if out, err := r.run(t, dir, env, "npm", "ci", "--loglevel", "silly"); err != nil {
		t.Fatalf("npm ci: %v\n%s", err, out)
	}
	if len(r.npmUp.Hits()) == before {
		t.Error("npm ci fetched nothing from the registry")
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "@acme", "bar", "index.js")); err != nil {
		t.Errorf("npm ci did not install @acme/bar: %v", err)
	}
	for _, hit := range r.npmUp.Hits() {
		if !strings.HasPrefix(hit.requestURI, npmBase+"/") {
			t.Errorf("the registry saw %s", hit.requestURI)
		}
	}
}

func (r *liveRegistryRig) pip(t *testing.T) {
	dir := t.TempDir()
	tmp := t.TempDir()
	xdg := filepath.Join(tmp, "xdg")
	write(t, filepath.Join(xdg, "pip", "pip.conf"),
		"[global]\nindex-url = https://evil.invalid/simple/\nextra-index-url = https://evil2.invalid/simple/\n")
	host := strings.TrimPrefix(r.proxy, "http://")
	env := []string{
		"HOME=" + tmp,
		"XDG_CONFIG_HOME=" + xdg,
		"PIP_CONFIG_FILE=/dev/null",
		"PIP_INDEX_URL=" + r.proxy + "/r/nexus-pypi/simple/",
		"PIP_EXTRA_INDEX_URL= ",
		"PIP_FIND_LINKS= ",
		"PIP_TRUSTED_HOST=" + host,
		"PIP_ONLY_BINARY=:all:",
		"PIP_NO_INPUT=1",
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
		"PIP_CACHE_DIR=" + filepath.Join(tmp, "pip-cache"),
	}
	out, err := r.run(t, dir, env, "python3", "-m", "pip", "download", "-vvv", "--no-deps", "-d", dir, "six==1.16.0")
	if err != nil {
		t.Fatalf("pip download: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, r.sixWhl))
	if err != nil || !bytes.Equal(got, r.wheel) {
		t.Errorf("downloaded wheel = %d bytes, %v", len(got), err)
	}
	if strings.Contains(out, "evil") {
		t.Errorf("pip consulted the hostile pip.conf:\n%s", out)
	}
	if len(r.files.Hits()) != 1 {
		t.Errorf("the file host saw %d requests, want 1", len(r.files.Hits()))
	}
	// flask is not on the allowlist: refused, and pip finds nothing.
	if out, err := r.run(t, dir, env, "python3", "-m", "pip", "download", "--no-deps", "-d", dir,
		"flask"); err == nil {
		t.Errorf("pip download flask succeeded:\n%s", out)
	}
	if !strings.Contains(r.h.log.String(), `"outcome":"not_allowed","reason":"package","ecosystem":"pypi",`+
		`"registry":"nexus-pypi","package":"flask"`) {
		t.Errorf("no not_allowed line for flask:\n%s", r.h.log.String())
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
