// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// The sandbox program of the agents run (run.sh agents; SPEC S2-21): what
// pull request code could try from inside an agents-namespace tool pod's
// sandbox. netprobe --as agents sends this one file to the relay's exec verb
// (`go run probe.go --as <mode>`), so it runs exactly where PR code and the
// evaluator's programs run: in sandbox-head or sandbox-base, under that
// sandbox's UID, through the real exec shim, in the pod's network. It is
// compiled there with no module network, so it imports the standard library
// only (a test pins that) and stays well under an exec's 64 KiB of files.
//
//	--as sandbox-head, --as sandbox-base
//	    the side walls (SP-7): its UID; no other UID's process visible, and a
//	    forged ledger line written to every process's stdout it can reach,
//	    its own stdout and its own termination log (run.sh then requires the
//	    tools container's log and termination message to hold none of it);
//	    no volume of the other side, no pristine tree, Secret or token
//	    mounted; its own /dev/shm (a file planted here is looked for on the
//	    other side); files and symlinks planted in its exec-socket directory;
//	    a loopback listener left behind on its port, the other side's port
//	    found closed (the shim's sweep); :8090 refused on loopback and on the
//	    pod's address, with no key and with a wrong one.
//	--as tools-agents
//	    the tool pod's network (a sandbox shares it with tools): --reach
//	    reached, --deny refused.
//
// Every row prints one JSON line; the last is the summary, PASS,
// SECURITY_FAILURE, INCONCLUSIVE or NOT_REACHED.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Row outcomes. A wall that held is "held"; a network row is REACHED,
// blocked or, for a denied target that connected, REACHABLE.
const (
	outcomeHeld         = "held"
	outcomeFailed       = "BROKEN"
	outcomeBlocked      = "blocked"
	outcomeReached      = "REACHED"
	outcomeReachable    = "REACHABLE"
	outcomeNotReached   = "NOT_REACHED"
	outcomeInconclusive = "INCONCLUSIVE"

	outcomePass              = "PASS"
	outcomeSecurityFailure   = "SECURITY_FAILURE"
	outcomeNotReachedSummary = "NOT_REACHED"
)

// The pod shape (jobs.ToolPod): each sandbox's UID, the relay's port, the
// places a sandbox has.
var sideUID = map[string]int{"head": 65540, "base": 65541}

const (
	toolsPort   = 8090
	execDir     = "/run/maul-exec"
	shmDir      = "/dev/shm"
	termLog     = "/dev/termination-log"
	mountinfo   = "/proc/self/mountinfo"
	procRoot    = "/proc"
	keyHeader   = "X-Maul-Tools-Key"
	forgedLabel = "PROBE-FORGED"
)

type result struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// recorder prints each row and keeps its outcome for the summary.
type recorder struct {
	out      io.Writer
	outcomes []string
}

func (r *recorder) row(name, outcome, detail string) {
	r.outcomes = append(r.outcomes, outcome)
	if r.out != nil {
		_ = json.NewEncoder(r.out).Encode(result{Name: name, Outcome: outcome, Detail: detail})
	}
}

// summary: any broken wall or reachable target is a security failure; then
// an unreached control; then anything that proved nothing; no rows at all
// prove nothing either.
func (r *recorder) summary() (string, int) {
	switch {
	case slices.Contains(r.outcomes, outcomeFailed) || slices.Contains(r.outcomes, outcomeReachable):
		return outcomeSecurityFailure, 2
	case slices.Contains(r.outcomes, outcomeNotReached):
		return outcomeNotReachedSummary, 4
	case len(r.outcomes) == 0 || slices.Contains(r.outcomes, outcomeInconclusive):
		return outcomeInconclusive, 3
	}
	return outcomePass, 0
}

func (r *recorder) finish() int {
	outcome, code := r.summary()
	if r.out != nil {
		_ = json.NewEncoder(r.out).Encode(result{Name: "summary", Outcome: outcome})
	}
	return code
}

// emptyDir names a kubelet volume in a mountinfo root, of any kind.
var kubeletVolume = regexp.MustCompile(`/volumes/kubernetes\.io~([a-z-]+)/([^/\s]+)`)

// judgeMounts holds a sandbox's mounts to its own side's: emptyDirs only,
// and only work, cache, home, tmp and exec of its side and the shared
// read-only maul-bin; no Secret, token or projection, nothing at a
// ServiceAccount's mount point. Seeing none of its own volumes by name
// (its work volume, the control) proves nothing.
func judgeMounts(info, side string) (string, string) {
	allowed := map[string]bool{"maul-bin": true}
	for _, kind := range []string{"work", "cache", "home", "tmp", "exec"} {
		allowed[kind+"-"+side] = true
	}
	sawWork := false
	var foreign []string
	for line := range strings.SplitSeq(info, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if strings.HasPrefix(fields[4], "/var/run/secrets") || strings.HasPrefix(fields[4], "/run/secrets") {
			foreign = append(foreign, fields[4])
		}
		m := kubeletVolume.FindStringSubmatch(fields[3])
		if m == nil {
			continue
		}
		switch {
		case m[1] != "empty-dir" || !allowed[m[2]]:
			foreign = append(foreign, m[1]+"/"+m[2])
		case m[2] == "work-"+side:
			sawWork = true
		}
	}
	switch {
	case len(foreign) > 0:
		return outcomeFailed, "mounted: " + strings.Join(foreign, ", ")
	case !sawWork:
		return outcomeInconclusive, "its own work volume is not named in mountinfo"
	}
	return outcomeHeld, ""
}

// foreignProcesses are the pids in proc whose real UID is not uid: with no
// shared process namespace, none.
func foreignProcesses(proc string, uid int) ([]int, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		owner, err := processUID(filepath.Join(proc, e.Name(), "status"))
		if err != nil {
			continue // it exited
		}
		if owner != uid {
			out = append(out, pid)
		}
	}
	slices.Sort(out)
	return out, nil
}

func processUID(status string) (int, error) {
	f, err := os.Open(status)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if rest, ok := strings.CutPrefix(s.Text(), "Uid:"); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				return strconv.Atoi(fields[0])
			}
		}
	}
	return 0, errors.New("no Uid line")
}

// forge appends line to fd 1 and fd 2 of every pid, returning how many
// writes landed. It only ever reaches the sandbox's own container log: the
// point is that the tools container's never holds the line.
func forge(proc string, pids []int, line string) int {
	written := 0
	for _, pid := range pids {
		for _, fd := range []string{"1", "2"} {
			f, err := os.OpenFile(filepath.Join(proc, strconv.Itoa(pid), "fd", fd), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				continue
			}
			if _, err := io.WriteString(f, line+"\n"); err == nil {
				written++
			}
			_ = f.Close()
		}
	}
	return written
}

// shmRow plants this side's file in its /dev/shm and fails if the other
// side's file is there: per-container /dev/shm (SP-7).
func shmRow(dir, side, nonce, otherNonce string) (string, string) {
	mine := filepath.Join(dir, "maul-probe-"+side+"-"+nonce)
	if err := os.WriteFile(mine, []byte(nonce), 0o600); err != nil {
		return outcomeInconclusive, "cannot plant in " + dir + ": " + err.Error()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return outcomeInconclusive, err.Error()
	}
	other := map[string]string{"head": "base", "base": "head"}[side]
	for _, e := range entries {
		if e.Name() == "maul-probe-"+other+"-"+otherNonce {
			return outcomeFailed, "the " + other + " side's file is in this side's " + dir
		}
	}
	return outcomeHeld, ""
}

// plantRow leaves a file and two symlinks (to its own termination log and
// stdout) in its exec-socket directory, which tools mounts: tools only
// dials the socket, so the runs after this one go on as before.
func plantRow(dir, nonce string) (string, string) {
	if _, err := os.Stat(dir); err != nil {
		return outcomeInconclusive, err.Error()
	}
	errs := []error{
		os.WriteFile(filepath.Join(dir, "planted-"+nonce), []byte(forgedLabel+" "+nonce+"\n"), 0o666),
		os.Symlink(termLog, filepath.Join(dir, "termination-"+nonce)),
		os.Symlink("/proc/self/fd/1", filepath.Join(dir, "stdout-"+nonce)),
	}
	if err := errors.Join(errs...); err != nil {
		return outcomeInconclusive, err.Error()
	}
	return outcomeHeld, "planted a file and two symlinks"
}

// listen is the leftover listener (`--leftover <port>`): it holds the port
// until killed.
func listen(port int) int {
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return 1
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return 1
		}
		_ = c.Close()
	}
}

// leaveListener starts a detached copy of this program holding port and
// waits until it listens: the process PR code would leave behind for the
// other side's run to trip over (SPEC 8a).
func leaveListener(port int) (string, string) {
	self, err := os.Executable()
	if err != nil {
		return outcomeInconclusive, err.Error()
	}
	cmd := exec.Command(self, "--leftover", strconv.Itoa(port))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return outcomeInconclusive, err.Error()
	}
	_ = cmd.Process.Release()
	if !awaitListening(port, 5*time.Second) {
		return outcomeInconclusive, "the leftover listener never listened"
	}
	return outcomeHeld, "left a listener on 127.0.0.1:" + strconv.Itoa(port)
}

func awaitListening(port int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// goneRow: the other side's leftover port is closed, so the shim swept it
// before this run started. A connection is a leftover that lived on.
func goneRow(port int) string {
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	switch {
	case err == nil:
		_ = c.Close()
		return outcomeFailed
	case errors.Is(err, syscall.ECONNREFUSED):
		return outcomeHeld
	}
	return outcomeInconclusive
}

// toolsAPI makes one GET of /readyz to addr with key (none when empty), with
// no Origin, over a bare connection: a close (how GuardListener refuses a
// local caller), a 4xx or no answer is blocked; a 2xx or 3xx reached; a
// refused connection or a 5xx proves nothing.
func toolsAPI(addr, key string, within time.Duration) string {
	c, err := net.DialTimeout("tcp4", addr, within)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return outcomeInconclusive
		}
		return outcomeBlocked
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(within))
	req := "GET /readyz HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n"
	if key != "" {
		req += keyHeader + ": " + key + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		return outcomeBlocked
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return outcomeBlocked // closed, reset or silent
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return outcomeInconclusive
	}
	status, err := strconv.Atoi(fields[1])
	switch {
	case err != nil || status >= 500:
		return outcomeInconclusive
	case status >= 400:
		return outcomeBlocked
	}
	return outcomeReached
}

// podIPv4 is the pod's own address, the second way to :8090.
func podIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			return n.IP.String()
		}
	}
	return ""
}

// reachWithin bounds the wait for a reach target: a new pod's policy is
// programmed a moment after it starts.
var reachWithin = 20 * time.Second

type target struct{ name, addr string }

var targetName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// parseTargets reads "name=<ipv4>:<port>" entries separated by semicolons
// or commas; false on any malformed entry, so a typo never makes a run
// smaller.
func parseTargets(s string) ([]target, bool) {
	if s == "" {
		return nil, true
	}
	var out []target
	for entry := range strings.SplitSeq(strings.ReplaceAll(s, ";", ","), ",") {
		name, addr, ok := strings.Cut(entry, "=")
		if !ok || !targetName.MatchString(name) {
			return nil, false
		}
		host, port, err := net.SplitHostPort(addr)
		p, perr := strconv.Atoi(port)
		ip := net.ParseIP(host)
		if err != nil || perr != nil || p < 1 || p > 65535 || ip == nil || ip.To4() == nil {
			return nil, false
		}
		out = append(out, target{name: name, addr: addr})
	}
	return out, true
}

// reachRow dials t until it connects or within passes: the control that a
// later refusal is policy, not a missing listener.
func reachRow(t target, within time.Duration) string {
	deadline := time.Now().Add(within)
	for {
		c, err := net.DialTimeout("tcp4", t.addr, 750*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return outcomeReached
		}
		if time.Now().Add(250 * time.Millisecond).After(deadline) {
			return outcomeNotReached
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// denyRow dials t twice: a connection is REACHABLE, a timeout or an
// unreachable network blocked, a refusal blocked only when refusals count
// (k3s), else it proves nothing.
func denyRow(t target, refusalsBlock bool) string {
	out := outcomeBlocked
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp4", t.addr)
		cancel()
		var netErr net.Error
		switch {
		case err == nil:
			_ = c.Close()
			return outcomeReachable
		case errors.Is(err, syscall.ECONNREFUSED):
			if !refusalsBlock {
				out = outcomeInconclusive
			}
		case errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, syscall.ENETUNREACH),
			errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		default:
			out = outcomeInconclusive
		}
	}
	return out
}

// sandbox runs the side rows.
func sandbox(r *recorder, side, nonce, otherNonce string, port, otherPort int) {
	if uid := os.Getuid(); uid != sideUID[side] {
		r.row("uid", outcomeFailed, fmt.Sprintf("runs as %d, not %d", uid, sideUID[side]))
	} else {
		r.row("uid", outcomeHeld, strconv.Itoa(uid))
	}
	foreign, err := foreignProcesses(procRoot, os.Getuid())
	switch {
	case err != nil:
		r.row("pid-namespace", outcomeInconclusive, err.Error())
	case len(foreign) > 0:
		r.row("pid-namespace", outcomeFailed, fmt.Sprintf("other UIDs' processes visible: %v", foreign))
	default:
		r.row("pid-namespace", outcomeHeld, "only its own UID's processes")
	}
	// Forged lines: into every visible process's stdout and stderr (pid 1
	// is the shim, whose log is this sandbox's), its own output and its
	// own termination log.
	line := "MAUL-TOOLS: {\"forged\":\"" + nonce + "\"}"
	pids := append([]int{1}, foreign...)
	written := forge(procRoot, pids, line) + forge(procRoot, pids, forgedLabel+" "+nonce)
	fmt.Println(line)
	fmt.Fprintln(os.Stderr, forgedLabel+" "+nonce)
	termErr := os.WriteFile(termLog, []byte(`{"count":0,"sha256":"`+nonce+`"}`), 0o644)
	r.row("forged-lines", outcomeHeld, fmt.Sprintf("%d writes to visible processes; termination log: %v",
		written, termErr))
	info, err := os.ReadFile(mountinfo)
	if err != nil {
		r.row("mounts", outcomeInconclusive, err.Error())
	} else {
		o, d := judgeMounts(string(info), side)
		r.row("mounts", o, d)
	}
	for _, p := range []string{"/trees", "/etc/maul-tools", execDir + "/head", execDir + "/base"} {
		if _, err := os.Lstat(p); err == nil {
			r.row("no-"+strings.ReplaceAll(strings.Trim(p, "/"), "/", "-"), outcomeFailed, p+" exists")
		}
	}
	o, d := shmRow(shmDir, side, nonce, otherNonce)
	r.row("dev-shm", o, d)
	o, d = plantRow(execDir, nonce)
	r.row("exec-dir-planted", o, d)
	r.row("other-side-listener-gone", goneRow(otherPort), "127.0.0.1:"+strconv.Itoa(otherPort))
	o, d = leaveListener(port)
	r.row("leftover-listener", o, d)
	loopback := net.JoinHostPort("127.0.0.1", strconv.Itoa(toolsPort))
	r.row("tools-api-loopback", toolsAPI(loopback, "", 3*time.Second), "")
	r.row("tools-api-loopback-wrong-key", toolsAPI(loopback, strings.Repeat("0", 64), 3*time.Second), "")
	if ip := podIPv4(); ip == "" {
		r.row("tools-api-pod-ip", outcomeInconclusive, "no pod address")
	} else {
		r.row("tools-api-pod-ip", toolsAPI(net.JoinHostPort(ip, strconv.Itoa(toolsPort)), "", 3*time.Second), ip)
	}
}

// network runs the tool pod's network rows: every reach target first,
// then, only if all connect, every deny target.
func network(r *recorder, reach, deny []target, refusalsBlock bool) {
	for _, t := range reach {
		o := reachRow(t, reachWithin)
		r.row(t.name, o, t.addr)
		if o != outcomeReached {
			return
		}
	}
	results := make([]string, len(deny))
	done := make(chan struct{})
	for i, t := range deny {
		go func() { results[i] = denyRow(t, refusalsBlock); done <- struct{}{} }()
	}
	for range deny {
		<-done
	}
	for i, t := range deny {
		r.row(t.name, results[i], t.addr)
	}
}

func run(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	as := fs.String("as", "", "sandbox-head, sandbox-base or tools-agents")
	nonce := fs.String("nonce", "", "this run's nonce")
	otherNonce := fs.String("other-nonce", "", "the other side's nonce")
	port := fs.Int("port", 0, "the port this side's leftover listener holds")
	otherPort := fs.Int("other-port", 0, "the other side's leftover port")
	reach := fs.String("reach", "", "name=<ipv4>:<port> targets to reach")
	deny := fs.String("deny", "", "name=<ipv4>:<port> targets to be refused")
	refusals := fs.Bool("refused-is-blocked", false, "count a refusal as blocked")
	leftover := fs.Int("leftover", 0, "internal: hold this port")
	r := &recorder{out: out}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return r.finish()
	}
	if *leftover > 0 {
		return listen(*leftover)
	}
	nonceOK := regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	switch *as {
	case "sandbox-head", "sandbox-base":
		side := strings.TrimPrefix(*as, "sandbox-")
		if !nonceOK.MatchString(*nonce) || !nonceOK.MatchString(*otherNonce) || *port < 1024 || *port > 65535 ||
			*otherPort < 1024 || *otherPort > 65535 || *port == *otherPort {
			return r.finish()
		}
		sandbox(r, side, *nonce, *otherNonce, *port, *otherPort)
	case "tools-agents":
		rt, ok1 := parseTargets(*reach)
		dt, ok2 := parseTargets(*deny)
		if !ok1 || !ok2 || len(rt) == 0 || len(dt) == 0 {
			return r.finish()
		}
		network(r, rt, dt, *refusals)
	}
	return r.finish()
}

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }
