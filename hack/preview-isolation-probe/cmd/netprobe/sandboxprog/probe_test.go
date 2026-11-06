// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A sandbox's mountinfo as k3s's kubelet shows it: its own emptyDirs by
// name, its /dev/shm a tmpfs of its own, the runtime's hosts and hostname
// files.
const headMountinfo = `1520 1400 0:345 / / ro,relatime master:1 - overlay overlay rw
1521 1520 259:2 /var/lib/kubelet/pods/u1/volumes/kubernetes.io~empty-dir/work-head /work rw - ext4 /dev/root rw
1522 1521 259:2 /var/lib/kubelet/pods/u1/volumes/kubernetes.io~empty-dir/cache-head /work/cache rw - ext4 /dev/root rw
1523 1521 259:2 /var/lib/kubelet/pods/u1/volumes/kubernetes.io~empty-dir/home-head /work/home rw - ext4 /dev/root rw
1524 1520 259:2 /var/lib/kubelet/pods/u1/volumes/kubernetes.io~empty-dir/tmp-head /tmp rw - ext4 /dev/root rw
1525 1520 259:2 /var/lib/kubelet/pods/u1/volumes/kubernetes.io~empty-dir/exec-head /run/maul-exec rw - ext4 /dev/root rw
1526 1520 259:2 /var/lib/kubelet/pods/u1/volumes/kubernetes.io~empty-dir/maul-bin /maul-bin ro - ext4 /dev/root rw
1527 1520 0:350 / /dev/shm rw - tmpfs tmpfs rw,size=16384k
1528 1520 259:2 /var/lib/kubelet/pods/u1/etc-hosts /etc/hosts rw - ext4 /dev/root rw
1529 1520 259:2 /var/lib/kubelet/pods/u1/containers/sandbox-head/0a1b /dev/termination-log rw - ext4 /dev/root rw
`

func TestJudgeMounts(t *testing.T) {
	line := func(vol, kind, at string) string {
		return "1600 1520 259:2 /var/lib/kubelet/pods/u1/volumes/kubernetes.io~" + kind + "/" + vol + " " + at +
			" ro - ext4 /dev/root rw\n"
	}
	for _, tc := range []struct {
		name, side, info, want string
	}{
		{"its own side", "head", headMountinfo, outcomeHeld},
		{"the base work volume", "head", headMountinfo + line("work-base", "empty-dir", "/x"), outcomeFailed},
		{"the base exec directory", "head", headMountinfo + line("exec-base", "empty-dir", "/x"), outcomeFailed},
		{"a pristine tree", "head", headMountinfo + line("trees-head", "empty-dir", "/x"), outcomeFailed},
		{"the tools Secret", "head", headMountinfo + line("tools-secret", "secret", "/x"), outcomeFailed},
		{"a projected token", "head", headMountinfo + line("kube-api-access-x", "projected", "/x"), outcomeFailed},
		{"a ServiceAccount mount point", "head",
			headMountinfo + "1601 1520 0:9 / /var/run/secrets/kubernetes.io/serviceaccount ro - tmpfs tmpfs ro\n",
			outcomeFailed},
		{"the other side's view of the same", "base", headMountinfo, outcomeFailed},
		// No own work volume in sight: the runtime shows no names, so the
		// row proves nothing.
		{"no names at all", "head", "1520 1400 0:345 / / ro - overlay overlay rw\n", outcomeInconclusive},
	} {
		got, detail := judgeMounts(tc.info, tc.side)
		if got != tc.want {
			t.Errorf("%s: %s (%s), want %s", tc.name, got, detail, tc.want)
		}
	}
}

// fakeProc writes a /proc with the given pid -> uid entries.
func fakeProc(t *testing.T, procs map[int]int) string {
	t.Helper()
	root := t.TempDir()
	for pid, uid := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		status := "Name:\tx\nUid:\t" + strconv.Itoa(uid) + "\t" + strconv.Itoa(uid) + "\t0\t0\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"self", "sys", "meminfo"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestForeignProcesses(t *testing.T) {
	own := fakeProc(t, map[int]int{1: 65540, 7: 65540, 9: 65540})
	if got, err := foreignProcesses(own, 65540); err != nil || len(got) != 0 {
		t.Errorf("own processes only: %v, %v", got, err)
	}
	shared := fakeProc(t, map[int]int{1: 65540, 12: 65532, 13: 65541})
	got, err := foreignProcesses(shared, 65540)
	if err != nil || !slices.Equal(got, []int{12, 13}) {
		t.Errorf("a shared PID namespace: %v, %v, want [12 13]", got, err)
	}
	if _, err := foreignProcesses(filepath.Join(own, "missing"), 65540); err == nil {
		t.Error("no /proc: want an error, never an empty pass")
	}
}

func TestForgeTargets(t *testing.T) {
	root := fakeProc(t, map[int]int{1: 65540, 12: 65532})
	// Neither fd exists in the fake: every write is refused.
	written := forge(root, []int{1, 12}, "MAUL-TOOLS: forged")
	if written != 0 {
		t.Errorf("%d writes landed in a /proc with no fds", written)
	}
	out := filepath.Join(root, "12", "fd", "1")
	if err := os.WriteFile(out, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if forge(root, []int{12}, "MAUL-TOOLS: forged") != 1 {
		t.Error("a writable fd was not written")
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "MAUL-TOOLS: forged\n") {
		t.Errorf("fd/1 holds %q", b)
	}
}

func TestShmRows(t *testing.T) {
	dir := t.TempDir()
	if got, _ := shmRow(dir, "head", "n1", "n2"); got != outcomeHeld {
		t.Errorf("an empty /dev/shm: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "maul-probe-head-n1")); err != nil {
		t.Errorf("the row planted nothing: %v", err)
	}
	// The other side's file in this side's /dev/shm: shared.
	if err := os.WriteFile(filepath.Join(dir, "maul-probe-base-n2"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := shmRow(dir, "head", "n1", "n2"); got != outcomeFailed {
		t.Errorf("the other side's file seen: %s", got)
	}
	if got, _ := shmRow(filepath.Join(dir, "missing"), "head", "n1", "n2"); got != outcomeInconclusive {
		t.Errorf("no /dev/shm to plant in: %s", got)
	}
}

func TestPlantRow(t *testing.T) {
	dir := t.TempDir()
	if got, detail := plantRow(dir, "n1"); got != outcomeHeld {
		t.Fatalf("plant: %s %s", got, detail)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Errorf("planted %d entries, want a file and two symlinks", len(entries))
	}
	if got, _ := plantRow(filepath.Join(dir, "missing"), "n1"); got != outcomeInconclusive {
		t.Errorf("no exec directory: %s", got)
	}
}

func TestLeftoverRows(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if got := goneRow(port); got != outcomeFailed {
		t.Errorf("a live listener: %s, want %s", got, outcomeFailed)
	}
	_ = l.Close()
	if got := goneRow(port); got != outcomeHeld {
		t.Errorf("a closed port: %s, want %s", got, outcomeHeld)
	}
	if awaitListening(port, 200*time.Millisecond) {
		t.Error("a closed port reported listening")
	}
}

func TestToolsAPIRows(t *testing.T) {
	serve := func(handle func(net.Conn)) int {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go handle(c)
			}
		}()
		return l.Addr().(*net.TCPAddr).Port
	}
	answer := func(status string) func(net.Conn) {
		return func(c net.Conn) {
			buf := make([]byte, 1024)
			_, _ = c.Read(buf)
			_, _ = c.Write([]byte("HTTP/1.1 " + status + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			_ = c.Close()
		}
	}
	gone, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gonePort := gone.Addr().(*net.TCPAddr).Port
	_ = gone.Close()
	for _, tc := range []struct {
		name string
		port int
		want string
	}{
		{"closed at accept, as GuardListener does", serve(func(c net.Conn) { _ = c.Close() }), outcomeBlocked},
		{"a refusal status", serve(answer("401 Unauthorized")), outcomeBlocked},
		{"served: the wall is down", serve(answer("200 OK")), outcomeReachable},
		{"redirected is served too", serve(answer("302 Found")), outcomeReachable},
		{"a server error proves nothing", serve(answer("502 Bad Gateway")), outcomeInconclusive},
		{"nothing listening proves nothing", gonePort, outcomeInconclusive},
	} {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(tc.port))
		if got := toolsAPI(addr, "deadbeef", time.Second); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestParseTargets(t *testing.T) {
	got, ok := parseTargets("artifact-server=10.43.1.4:9790;maul-egress-broker=10.43.1.2:8080")
	if !ok || len(got) != 2 || got[1].name != "maul-egress-broker" || got[1].addr != "10.43.1.2:8080" {
		t.Errorf("parseTargets: %v %v", got, ok)
	}
	for _, bad := range []string{"x", "a=b", "A=10.0.0.1:80", "a=10.0.0.1:0", "a=host:80", "a=10.0.0.1:80,"} {
		if _, ok := parseTargets(bad); ok {
			t.Errorf("parseTargets(%q) accepted", bad)
		}
	}
	if got, ok := parseTargets(""); !ok || len(got) != 0 {
		t.Errorf("an empty list: %v %v", got, ok)
	}
}

func TestDialRow(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	open := l.Addr().String()
	defer func() { _ = l.Close() }()
	gone, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := gone.Addr().String()
	_ = gone.Close()
	for _, tc := range []struct {
		name     string
		t        target
		refusals bool
		want     string
	}{
		{"denied, but connects", target{name: "x", addr: open}, false, outcomeReachable},
		{"denied, refused", target{name: "x", addr: closed}, false, outcomeInconclusive},
		{"denied, refused, refusals block", target{name: "x", addr: closed}, true, outcomeBlocked},
	} {
		if got := denyRow(tc.t, tc.refusals); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := reachRow(target{name: "x", addr: open}, time.Second); got != outcomeReached {
		t.Errorf("reach an open port: %s", got)
	}
	if got := reachRow(target{name: "x", addr: closed}, 300*time.Millisecond); got != outcomeNotReached {
		t.Errorf("reach a closed port: %s", got)
	}
}

func TestSummary(t *testing.T) {
	for _, tc := range []struct {
		rows []string
		want string
	}{
		{[]string{outcomeHeld, outcomeBlocked, outcomeReached}, outcomePass},
		{[]string{outcomeHeld, outcomeInconclusive}, outcomeInconclusive},
		{[]string{outcomeHeld, outcomeNotReached}, outcomeNotReachedSummary},
		{[]string{outcomeInconclusive, outcomeFailed}, outcomeSecurityFailure},
		{[]string{outcomeReachable}, outcomeSecurityFailure},
		{nil, outcomeInconclusive},
	} {
		var r recorder
		for _, o := range tc.rows {
			r.row("x", o, "")
		}
		if got, _ := r.summary(); got != tc.want {
			t.Errorf("%v: %s, want %s", tc.rows, got, tc.want)
		}
	}
}

// TestStdlibOnly: the program is sent as source and compiled in the
// sandbox with no module network, so it imports the standard library only.
func TestStdlibOnly(t *testing.T) {
	src, err := os.ReadFile("probe.go")
	if err != nil {
		t.Fatal(err)
	}
	inImports := false
	for line := range strings.SplitSeq(string(src), "\n") {
		switch {
		case strings.HasPrefix(line, "import ("):
			inImports = true
		case inImports && line == ")":
			inImports = false
		case inImports && strings.Contains(line, "."):
			t.Errorf("non-stdlib import %s", strings.TrimSpace(line))
		}
	}
	if len(src) > 60<<10 {
		t.Errorf("probe.go is %d bytes; an exec takes at most 64 KiB of files", len(src))
	}
}

func TestRunValidatesAndSummarises(t *testing.T) {
	reachWithin = time.Second
	summary := func(args ...string) (string, int) {
		var out strings.Builder
		code := run(args, &out)
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		return lines[len(lines)-1], code
	}
	for _, args := range [][]string{
		nil,
		{"--as", "sandbox-head"},
		{"--as", "sandbox-head", "--nonce", "ab12cd34", "--other-nonce", "ab12cd35", "--port", "47801",
			"--other-port", "47801"},
		{"--as", "sandbox-base", "--nonce", "NOT-HEX!", "--other-nonce", "ab12cd35", "--port", "47801",
			"--other-port", "47802"},
		{"--as", "tools-agents", "--deny", "x=10.0.0.1:80"},
		{"--as", "tools-agents", "--reach", "x=10.0.0.1:80"},
		{"--as", "tools-agents", "--reach", "x=10.0.0.1:80", "--deny", "y=host:80"},
		{"--as", "evaluator"},
		{"--bogus"},
	} {
		if line, code := summary(args...); code != 3 || !strings.Contains(line, `"summary","outcome":"INCONCLUSIVE"`) {
			t.Errorf("%q: exit %d, %s; want an inconclusive summary", args, code, line)
		}
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	gone, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := gone.Addr().String()
	_ = gone.Close()
	reach, deny := "artifact-server="+l.Addr().String(), "maul-egress-broker="+closed
	if line, code := summary("--as", "tools-agents", "--reach", reach, "--deny", deny,
		"--refused-is-blocked"); code != 0 || !strings.Contains(line, "PASS") {
		t.Errorf("reached and refused: exit %d, %s", code, line)
	}
	if line, code := summary("--as", "tools-agents", "--reach", reach, "--deny",
		"open="+l.Addr().String()); code != 2 || !strings.Contains(line, "SECURITY_FAILURE") {
		t.Errorf("a denied target that connects: exit %d, %s", code, line)
	}
	if line, code := summary("--as", "tools-agents", "--reach", "x="+closed, "--deny", deny); code != 4 ||
		!strings.Contains(line, "NOT_REACHED") {
		t.Errorf("an unreached control: exit %d, %s", code, line)
	}
}
