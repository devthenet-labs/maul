// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"io/fs"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/semver"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// liveEnv opts into TestLiveGo, which reaches proxy.golang.org and
// sum.golang.org (mise run live-go).
const liveEnv = "MAUL_LIVE_GO"

// liveProfile admits github.com/google/uuid only.
const liveProfile = `[{"name":"go","route":"go","port":8090,"repositories":["devthenet-labs/maul-target"],
  "modules":["github.com/google/uuid"]}]`

// liveGo is the real go command and the shared pieces of its environment.
type liveGo struct {
	t      *testing.T
	bin    string
	goroot string
	cache  string // GOCACHE, shared
	path   string // PATH
}

// run runs the go command in dir with env (NAME=value) and nothing
// inherited, returning its combined output.
func (g liveGo) run(dir string, env []string, args ...string) (string, error) {
	g.t.Helper()
	cmd := exec.Command(g.bin, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + g.path, "HOME=" + dir, "GOCACHE=" + g.cache, "GOFLAGS=-mod=mod"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// profileEnv is the agent container's go environment for a profile (§4.3
// of the spec) verbatim, with gopath as the writable GOPATH.
func profileEnv(proxyURL, baked, gopath string) []string {
	goproxy := proxyURL + "/go"
	if baked != "" {
		goproxy += "," + (&url.URL{Scheme: "file", Path: path.Join(filepath.ToSlash(baked), "cache/download")}).String()
	}
	return []string{
		"GOENV=off",
		"GOPROXY=" + goproxy,
		"GOSUMDB=sum.golang.org",
		"GONOSUMDB=maul.invalid",
		"GONOPROXY=maul.invalid",
		"GOPRIVATE=maul.invalid",
		"GOINSECURE=maul.invalid",
		"GOAUTH=off",
		"GOTOOLCHAIN=local",
		"GOPATH=" + gopath,
		"GOMODCACHE=" + filepath.Join(gopath, "pkg", "mod"),
	}
}

// directEnv reaches proxy.golang.org directly: the fixtures' setup only,
// standing in for the image build that bakes a module cache.
func directEnv(modcache string) []string {
	return []string{"GOENV=off", "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org",
		"GOTOOLCHAIN=local", "GOPATH=" + filepath.Dir(filepath.Dir(modcache)), "GOMODCACHE=" + modcache}
}

// writableCleanup makes a module cache removable again when the test ends
// (the go command writes it read-only).
func writableCleanup(t *testing.T, dir string) {
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
}

// fixtureModule writes a fixture module: go.mod with the requirements and a main
// package importing them.
func fixtureModule(t *testing.T, requires map[string]string, imports ...string) string {
	t.Helper()
	dir := t.TempDir()
	gomod := "module example.com/live\n\ngo 1.21\n"
	for m, v := range requires {
		gomod += "\nrequire " + m + " " + v + "\n"
	}
	main := "package main\n\nimport (\n"
	for _, imp := range imports {
		main += "\t_ \"" + imp + "\"\n"
	}
	main += ")\n\nfunc main() {}\n"
	if len(imports) == 0 {
		main = "package main\n\nfunc main() {}\n"
	}
	for name, body := range map[string]string{"go.mod": gomod, "main.go": main} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// liveRig is the engine on loopback with real upstreams, the go command,
// and the baked module cache.
type liveRig struct {
	g     liveGo
	url   string
	baked string
	gosum []byte
	log   *lockedBuffer
}

// TestLiveGo runs the real go command through the engine against the real
// proxy.golang.org and sum.golang.org, with the profile Job's go
// environment verbatim (proxy first, the baked cache second). It codifies
// the spec's facts 1, 2, 11 and 12. Opt-in: MAUL_LIVE_GO=1.
func TestLiveGo(t *testing.T) {
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 to run the live go command test (reaches proxy.golang.org)", liveEnv)
	}
	r := newLiveRig(t)
	t.Run("download allowed, refuse denied", r.downloadAndRefuse)
	t.Run("version bump past the bake", r.bumpPastTheBake)
	t.Run("build with a baked module outside the allowlist", r.buildFromTheBake)
	t.Run("pseudo-version refused", r.pseudoVersionRefused)
	t.Run("hostile go.env", r.hostileGoEnv)
	if !strings.Contains(r.log.String(), `"kind":"supported"`) {
		t.Errorf("the go command never asked the proxy whether it serves the checksum database")
	}
	t.Logf("audit log:\n%s", r.log.String())
}

func newLiveRig(t *testing.T) *liveRig {
	t.Helper()
	bin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	g := liveGo{t: t, bin: bin, cache: t.TempDir(), path: os.Getenv("PATH")}
	out, err := exec.Command(bin, "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	g.goroot = strings.TrimSpace(string(out))

	set, err := netprofile.Parse([]byte(liveProfile))
	if err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	srv, err := New(Config{Profiles: set}, slog.New(slog.NewJSONHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := srv.Handler("go")
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// The baked cache, as an image build leaves it: read-only, holding an
	// older uuid and a module outside the allowlist. Its go.sum is the
	// fixture's, as the repository's would be.
	baked := filepath.Join(t.TempDir(), "gopath", "pkg", "mod")
	writableCleanup(t, baked)
	fixture := fixtureModule(t, map[string]string{"github.com/google/uuid": "v1.5.0", "golang.org/x/mod": "v0.20.0"},
		"github.com/google/uuid", "golang.org/x/mod/semver")
	if out, err := g.run(fixture, directEnv(baked), "mod", "tidy"); err != nil {
		t.Fatalf("baking the module cache: %v\n%s", err, out)
	}
	gosum, err := os.ReadFile(filepath.Join(fixture, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	return &liveRig{g: g, url: ts.URL, baked: baked, gosum: gosum, log: log}
}

// env is the profile environment with a fresh GOPATH, with or without the
// baked file:// entry.
func (r *liveRig) env(t *testing.T, withBaked bool) []string {
	p := filepath.Join(t.TempDir(), "go")
	writableCleanup(t, p)
	baked := ""
	if withBaked {
		baked = r.baked
	}
	return profileEnv(r.url, baked, p)
}

// module writes a fixture module carrying the baked go.sum.
func (r *liveRig) module(t *testing.T, requires map[string]string, imports ...string) string {
	dir := fixtureModule(t, requires, imports...)
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), r.gosum, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// downloadAndRefuse: an allowed module downloads; a denied one fails. A
// refusal's 410 body reaches the agent only while the proxy is the last
// GOPROXY entry: the go command reports the LAST not-found error
// (modfetch.TryProxies), so with the baked file:// entry after the proxy
// the agent sees that entry's "no such file" instead, and the reason is in
// the proxy's audit line only.
func (r *liveRig) downloadAndRefuse(t *testing.T) {
	dir := fixtureModule(t, nil)
	env := r.env(t, true)
	if out, err := r.g.run(dir, env, "mod", "download", "github.com/google/uuid@v1.6.0"); err != nil {
		t.Fatalf("download of an allowed module: %v\n%s", err, out)
	}
	if out, err := r.g.run(dir, env, "mod", "download", "rsc.io/quote@v1.5.2"); err == nil {
		t.Errorf("download of a denied module with the baked entry succeeded:\n%s", out)
	}
	out, err := r.g.run(dir, r.env(t, false), "mod", "download", "rsc.io/quote@v1.5.2")
	if err == nil || !strings.Contains(out, "410 Gone") ||
		!strings.Contains(out, "egress-proxy: module path rsc.io/quote is not allowed by profile go") {
		t.Errorf("download of a denied module: %v\n%s", err, out)
	}
}

// bumpPastTheBake: with the proxy first, @latest and -u see versions newer
// than the baked one.
func (r *liveRig) bumpPastTheBake(t *testing.T) {
	dir := r.module(t, map[string]string{"github.com/google/uuid": "v1.5.0"}, "github.com/google/uuid")
	env := r.env(t, true)
	out, err := r.g.run(dir, env, "list", "-m", "-u", "github.com/google/uuid")
	if err != nil || !regexp.MustCompile(`github.com/google/uuid v1\.5\.0 \[v1\.\d+\.\d+\]`).MatchString(out) {
		t.Errorf("go list -m -u: %v\n%s", err, out)
	}
	if out, err := r.g.run(dir, env, "get", "github.com/google/uuid@latest"); err != nil {
		t.Fatalf("go get @latest: %v\n%s", err, out)
	}
	gomod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
	m := regexp.MustCompile(`github.com/google/uuid (v\S+)`).FindStringSubmatch(string(gomod))
	if m == nil || semver.Compare(m[1], "v1.6.0") < 0 {
		t.Errorf("go.mod after go get @latest:\n%s", gomod)
	}
}

// buildFromTheBake: a baked module outside the allowlist is refused by the
// proxy and served by the baked entry, so the build succeeds.
func (r *liveRig) buildFromTheBake(t *testing.T) {
	dir := r.module(t, map[string]string{"github.com/google/uuid": "v1.5.0", "golang.org/x/mod": "v0.20.0"},
		"github.com/google/uuid", "golang.org/x/mod/semver")
	if out, err := r.g.run(dir, r.env(t, true), "build", "./..."); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	if !strings.Contains(r.log.String(), `"module":"golang.org/x/mod","version":"v0.20.0"`) {
		t.Errorf("the proxy never saw (and refused) golang.org/x/mod before the baked cache served it")
	}
}

// pseudoVersionRefused: the version gate refuses a pseudo-version of an
// allowed module.
func (r *liveRig) pseudoVersionRefused(t *testing.T) {
	dir := fixtureModule(t, nil)
	const pseudo = "github.com/google/uuid@v0.0.0-20230101000000-abcdefabcdef"
	if out, err := r.g.run(dir, r.env(t, true), "mod", "download", pseudo); err == nil {
		t.Errorf("pseudo-version download with the baked entry succeeded:\n%s", out)
	}
	out, err := r.g.run(dir, r.env(t, false), "mod", "download", pseudo)
	if err == nil || !strings.Contains(out, "410 Gone") || !strings.Contains(out, "is not a tagged release") {
		t.Errorf("pseudo-version download: %v\n%s", err, out)
	}
}

// hostileGoEnv: a $GOROOT/go.env turning checksum verification off cannot
// win over the profile's non-empty values; a new go.sum line is still
// looked up through the proxy.
func (r *liveRig) hostileGoEnv(t *testing.T) {
	root := hostileGoroot(t, r.g.goroot)
	dir := fixtureModule(t, nil)
	env := append(r.env(t, false), "GOROOT="+root)
	// The control: blanks, which a profile never sets, lose to it.
	blanks := append(slices.Clone(env), "GOPRIVATE=", "GONOSUMDB=")
	if out, err := r.g.run(dir, blanks, "env", "GONOSUMDB"); err != nil || strings.TrimSpace(out) != "*" {
		t.Fatalf("the hostile go.env is not in effect (GONOSUMDB=%q): %v", out, err)
	}
	if out, err := r.g.run(dir, env, "env", "GOPRIVATE", "GONOSUMDB"); err != nil || strings.Contains(out, "*") {
		t.Fatalf("the hostile go.env won: %v\n%s", err, out)
	}
	if out, err := r.g.run(dir, env, "get", "github.com/google/uuid@v1.4.0"); err != nil {
		t.Fatalf("go get: %v\n%s", err, out)
	}
	lookup := `"route":"sumdb","kind":"lookup","module":"github.com/google/uuid","version":"v1.4.0"`
	if !strings.Contains(r.log.String(), lookup) {
		t.Errorf("no checksum-database lookup for the new go.sum line went through the proxy")
	}
}

// hostileGoroot is a copy of goroot (symlinks) whose go.env turns checksum
// verification off for everything, as an image's $GOROOT/go.env could.
func hostileGoroot(t *testing.T, goroot string) string {
	t.Helper()
	root := t.TempDir()
	entries, err := os.ReadDir(goroot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "go.env" {
			continue
		}
		if err := os.Symlink(filepath.Join(goroot, e.Name()), filepath.Join(root, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	hostile := "GOPRIVATE=*\nGONOSUMDB=*\nGONOPROXY=*\nGOSUMDB=off\nGOPROXY=direct\nGOTOOLCHAIN=auto\n"
	if err := os.WriteFile(filepath.Join(root, "go.env"), []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}
