// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeRelay stands in for the agents tool pod's relay: it requires the key
// on every call (when keyed), answers files ls, and answers each exec with
// the canned program output for its --as mode and side.
type fakeRelay struct {
	mu        sync.Mutex
	keyless   bool              // accept a call without the key: the gate a run must catch
	busy      int               // answer this many calls "unavailable" first
	refuseAll string            // refuse every keyed call with this code
	outputs   map[string]string // "<as>@<side>" -> untrusted output
	execs     []map[string]any  // the exec bodies received, in order
	keys      []string          // the keys received, in order
}

func (f *fakeRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Header.Get("X-Maul-Tools-Key")
	f.keys = append(f.keys, key)
	if key != testKey && !f.keyless {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"refused": "invalid-request", "reason": "no key"})
		return
	}
	if f.busy > 0 {
		f.busy--
		_ = json.NewEncoder(w).Encode(map[string]any{"refused": "unavailable", "reason": "busy"})
		return
	}
	if f.refuseAll != "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"refused": f.refuseAll})
		return
	}
	switch r.URL.Path {
	case "/v1/files":
		_ = json.NewEncoder(w).Encode(map[string]any{"seq": 1, "untrusted": "go.mod\n"})
	case "/v1/exec":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.execs = append(f.execs, body)
		argv, _ := body["argv"].([]any)
		as := ""
		for i, a := range argv {
			if a == "--as" && i+1 < len(argv) {
				as, _ = argv[i+1].(string)
			}
		}
		side, _ := body["side"].(string)
		out, ok := f.outputs[as+"@"+side]
		if !ok {
			out = `{"name":"x","outcome":"held"}` + "\n" + `{"name":"summary","outcome":"PASS"}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"seq": 2, "observed": map[string]any{"exit": 0},
			"untrusted": out})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// relayAddr serves f and returns its <ip>:<port>.
func relayAddr(t *testing.T, f http.Handler) string {
	t.Helper()
	s := httptest.NewServer(f)
	t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "http://")
}

func agentsTestConfig(t *testing.T, addr string) agentsConfig {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return agentsConfig{
		tool: addr, key: testKey, nonce: "00112233445566778899aabbccddeeff", goCmd: "go",
		reach: "artifact-server=" + l.Addr().String(), deny: "maul-egress-broker=10.43.1.2:8080",
		reachWithin: 2 * time.Second, retryWithin: 2 * time.Second,
	}
}

// collect runs the driver and returns its rows by name and its summary.
func collect(t *testing.T, cfg agentsConfig) (map[string]string, string, int) {
	t.Helper()
	rows := map[string]string{}
	outcome, code := runAgents(cfg, func(r result) { rows[r.Name] = r.Outcome })
	return rows, outcome, code
}

func TestAgentsDriverPasses(t *testing.T) {
	f := &fakeRelay{}
	cfg := agentsTestConfig(t, relayAddr(t, f))
	rows, outcome, code := collect(t, cfg)
	if outcome != outcomePass || code != 0 {
		t.Fatalf("summary %s (%d), rows %v", outcome, code, rows)
	}
	for _, want := range []string{"tool-pod", "key-control", "key-missing", "key-wrong", "key-malformed",
		"head-1/summary", "base/summary", "head-2/summary", "network/summary"} {
		if _, ok := rows[want]; !ok {
			t.Errorf("no row %s; have %v", want, slices.Sorted(mapKeys(rows)))
		}
	}
	checkExecs(t, f.execs)
	// No key, a well-formed key that is not this evaluation's, a malformed one.
	var wrong []string
	for _, k := range f.keys {
		if k != testKey {
			wrong = append(wrong, k)
		}
	}
	if len(wrong) != 3 || wrong[0] != "" || len(wrong[1]) != 64 || wrong[2] != "not-a-key" {
		t.Errorf("the refused calls carried %q", wrong)
	}
}

// checkExecs: the program runs head, base, head, then the network from the
// base (so each side's exec directory, planted by its last side run, is
// dialled once more), each a go run of its one file, each run planting
// under its own nonce and looking for the other side's last.
func checkExecs(t *testing.T, execs []map[string]any) {
	t.Helper()
	order := make([]string, 0, len(execs))
	for _, e := range execs {
		argv, _ := e["argv"].([]any)
		side, _ := e["side"].(string)
		order = append(order, argv[4].(string)+"@"+side)
		if argv[0] != "go" || argv[1] != "run" || argv[2] != agentsProgramFile {
			t.Errorf("argv %v does not go run the program", argv)
		}
		files, _ := e["files"].([]any)
		if len(files) != 1 {
			t.Fatalf("files %v", files)
		}
		if file := files[0].(map[string]any); file["name"] != agentsProgramFile || file["content"] == "" {
			t.Errorf("file %v", file)
		}
	}
	want := []string{"sandbox-head@head", "sandbox-base@base", "sandbox-head@head", "tools-agents@base"}
	if !slices.Equal(order, want) {
		t.Fatalf("exec order %v, want %v", order, want)
	}
	nonces := func(i int) (n, o string) {
		argv, _ := execs[i]["argv"].([]any)
		for j, a := range argv {
			switch a {
			case "--nonce":
				n = argv[j+1].(string)
			case "--other-nonce":
				o = argv[j+1].(string)
			}
		}
		return n, o
	}
	h1, _ := nonces(0)
	b, bOther := nonces(1)
	_, h2Other := nonces(2)
	if bOther != h1 || h2Other != b || h1 == b {
		t.Errorf("nonces: head-1 %s, base %s (other %s), head-2 other %s", h1, b, bOther, h2Other)
	}
}

func TestAgentsDriverFailsAKeylessRelay(t *testing.T) {
	cfg := agentsTestConfig(t, relayAddr(t, &fakeRelay{keyless: true}))
	rows, outcome, code := collect(t, cfg)
	if outcome != outcomeSecurityFailure || code != 2 || rows["key-missing"] != "REACHABLE" {
		t.Errorf("summary %s (%d), rows %v", outcome, code, rows)
	}
}

func TestAgentsDriverPropagatesAProgramFailure(t *testing.T) {
	for _, tc := range []struct {
		step, as, side, out, want string
	}{
		{"base", "sandbox-base", "base", `{"name":"dev-shm","outcome":"BROKEN"}` + "\n" +
			`{"name":"summary","outcome":"SECURITY_FAILURE"}`, outcomeSecurityFailure},
		{"network", "tools-agents", "base", `{"name":"artifact-server","outcome":"NOT_REACHED"}` + "\n" +
			`{"name":"summary","outcome":"NOT_REACHED"}`, outcomeNotReached},
		{"head-1", "sandbox-head", "head", "go: cannot find GOROOT", outcomeInconclusive},
	} {
		f := &fakeRelay{outputs: map[string]string{tc.as + "@" + tc.side: tc.out}}
		cfg := agentsTestConfig(t, relayAddr(t, f))
		rows, outcome, _ := collect(t, cfg)
		if outcome != tc.want {
			t.Errorf("%s: summary %s, want %s; rows %v", tc.step, outcome, tc.want, rows)
		}
	}
}

func TestAgentsDriverRetriesABusyRelay(t *testing.T) {
	cfg := agentsTestConfig(t, relayAddr(t, &fakeRelay{busy: 2}))
	if rows, outcome, _ := collect(t, cfg); outcome != outcomePass {
		t.Errorf("summary %s, rows %v", outcome, rows)
	}
}

// A relay that refuses the control proves nothing about the key: a
// keyless refusal of the same call might be that refusal.
func TestAgentsDriverNeedsTheControl(t *testing.T) {
	cfg := agentsTestConfig(t, relayAddr(t, &fakeRelay{refuseAll: "finished"}))
	rows, outcome, _ := collect(t, cfg)
	if outcome != outcomeInconclusive || rows["key-control"] != outcomeInconclusive {
		t.Errorf("summary %s, rows %v", outcome, rows)
	}
	if _, ok := rows["key-missing"]; ok {
		t.Errorf("the key rows ran without a control: %v", rows)
	}
}

func TestAgentsDriverOtherToolPod(t *testing.T) {
	// The other tool pod takes this evaluation's key: the key binds nothing.
	other := relayAddr(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"seq": 1})
	}))
	cfg := agentsTestConfig(t, relayAddr(t, &fakeRelay{}))
	cfg.other = other
	rows, outcome, _ := collect(t, cfg)
	if outcome != outcomeSecurityFailure || rows["other-tool-pod-own-key"] != "REACHABLE" {
		t.Errorf("summary %s, rows %v", outcome, rows)
	}
	// A relay holding another key refuses ours.
	f := &fakeRelay{}
	cfg.other = relayAddr(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Maul-Tools-Key", "")
		f.ServeHTTP(w, r)
	}))
	if rows, outcome, _ := collect(t, cfg); outcome != outcomePass || rows["other-tool-pod-own-key"] != "blocked" {
		t.Errorf("summary %s, rows %v", outcome, rows)
	}
}

func TestAgentsTargetsFromEnv(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "tools-key")
	if err := os.WriteFile(keyFile, []byte(testKey+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"PROBE_TOOL_POD": "10.42.1.10", "PROBE_TOOLS_KEY_FILE": keyFile,
		"PROBE_NONCE": "00112233445566778899aabbccddeeff",
		"PROBE_REACH": "artifact-server=10.43.1.4:9790", "PROBE_DENY": "maul-egress-broker=10.43.1.2:8080",
	}
	get := func(k string) string { return env[k] }
	cfg, ok := agentsConfigFrom(get, false)
	if !ok || cfg.tool != "10.42.1.10:8090" || cfg.key != testKey || cfg.goCmd != "go" {
		t.Fatalf("config %+v %v", cfg, ok)
	}
	for _, bad := range []map[string]string{
		{"PROBE_TOOL_POD": "maul-tools"}, {"PROBE_TOOLS_KEY_FILE": "/nope"}, {"PROBE_NONCE": "short"},
		{"PROBE_REACH": ""}, {"PROBE_DENY": "x"}, {"PROBE_OTHER_TOOL_POD": "host"}, {"PROBE_GO": "go run"},
	} {
		saved := map[string]string{}
		for k, v := range bad {
			saved[k] = env[k]
			env[k] = v
		}
		if _, ok := agentsConfigFrom(get, false); ok {
			t.Errorf("%v accepted", bad)
		}
		for k, v := range saved {
			env[k] = v
		}
	}
}

// TestAgentsProgramCompilesAsTheSandboxDoes: the embedded program builds
// with only the standard library, outside any module, with no module
// network, as `go run probe.go` does in the sandbox.
func TestAgentsProgramCompilesAsTheSandboxDoes(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	src, err := os.ReadFile(filepath.Join("sandboxprog", "probe.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(src) != agentsProgram {
		t.Fatal("the embedded program is not sandboxprog/probe.go")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, agentsProgramFile), src, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goBin, "build", "-o", filepath.Join(dir, "probe"), agentsProgramFile)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local", "GO111MODULE=on")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", agentsProgramFile, err, out)
	}
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
