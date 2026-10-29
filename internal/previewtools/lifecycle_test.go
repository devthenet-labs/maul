// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// scoreCheckB is scoreCheck under another id: its own digest.
var scoreCheckB = strings.Replace(scoreCheck, `"score.shows"`, `"score.again"`, 1)

const webCheck = `{"id":"home.items","outcome":"home","form":"http","http":{"target":"web","steps":[
 {"request":{"method":"POST","path":"/items","json":{"n":"{{run}}"}},
  "expect":{"status":201,"bodyContains":"{{run}}"}}]}}`

type lifecycle struct {
	out  *bytes.Buffer
	term string
	o    RunOptions
}

func newLifecycle(t *testing.T, cfg Config, rt routes) *lifecycle {
	t.Helper()
	l := &lifecycle{out: &bytes.Buffer{}, term: filepath.Join(t.TempDir(), "termination-log")}
	l.o = RunOptions{
		Options: Options{
			Config: cfg, Dir: t.TempDir(), Out: l.out, Token: counterTokens(), TerminationPath: l.term,
			Client: NewClient(ClientOptions{Dial: rt.dial, Timeout: 2 * time.Second, ReadinessBackoff: 5 * time.Millisecond}),
		},
		ReadinessWindow: 2 * time.Second,
	}
	return l
}

func (l *lifecycle) verify(t *testing.T) toolwire.Ledger {
	t.Helper()
	term, err := os.ReadFile(l.term)
	if err != nil {
		t.Fatalf("termination message: %v", err)
	}
	led, err := toolwire.Verify(l.out.Bytes(), string(term))
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, l.out.String())
	}
	return led
}

func finishReason(t *testing.T, e toolwire.LedgerEntry) string {
	t.Helper()
	if e.Kind != toolwire.KindRelay || e.Verb != toolwire.VerbFinish {
		t.Fatalf("last entry %+v, want the relay's finish", e)
	}
	var args struct{ Reason string }
	if err := json.Unmarshal(e.Args, &args); err != nil {
		t.Fatal(err)
	}
	return args.Reason
}

func TestRunReplaysAloneThenFinishesAReplayRound(t *testing.T) {
	api := fakeAPI(t, func(n int) int { return []int{5, 99, 5}[n] })
	web := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	cfg := baseConfig()
	cfg.ReplayOnly = true
	cfg.Checks = []StoredCheck{storedCheck(t, scoreCheck), storedCheck(t, scoreCheckB), storedCheck(t, webCheck)}
	l := newLifecycle(t, cfg, routes{"10.0.0.1:80": api.srv.Listener.Addr().String(),
		"10.0.0.2:80": web.srv.Listener.Addr().String()})

	sum, err := Run(context.Background(), l.o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	led := l.verify(t)
	if sum != led.Summary {
		t.Errorf("Run returned %+v, the ledger closed with %+v", sum, led.Summary)
	}
	ready := led.Entries[0]
	if ready.Kind != toolwire.KindRelay || ready.Verb != toolwire.VerbHTTP ||
		!strings.Contains(string(ready.Args), "readiness") {
		t.Fatalf("first entry %+v, want the readiness wait", ready)
	}
	var waits []acceptance.TargetWait
	if err := json.Unmarshal([]byte(ready.Output), &waits); err != nil || len(waits) != 2 || !waits[0].Reached() {
		t.Errorf("readiness output %q: %v", ready.Output, err)
	}
	want := []struct {
		id     string
		result toolwire.RunResult
	}{{"score.shows", toolwire.RunPass}, {"score.again", toolwire.RunQuarantined}, {"home.items", toolwire.RunFail}}
	tokens := map[string]bool{}
	for i, w := range want {
		e := led.Entries[1+i]
		if e.Kind != toolwire.KindRelay || e.Verb != toolwire.VerbCheckRun || e.CheckRun == nil ||
			e.CheckRun.Mode != toolwire.ModeReplay || e.CheckRun.CheckID != w.id || e.CheckRun.Result != w.result {
			t.Errorf("entry %d %+v, want a replay of %s: %s", 1+i, e, w.id, w.result)
			continue
		}
		for _, a := range e.CheckRun.Attempts {
			if tokens[a.Run] {
				t.Errorf("token %q reused", a.Run)
			}
			tokens[a.Run] = true
		}
	}
	if r := finishReason(t, led.Entries[len(led.Entries)-1]); r != FinishReplayOnly {
		t.Errorf("finish reason %q", r)
	}
	if len(led.Entries) != 5 {
		t.Errorf("%d entries, want readiness, three replays and finish", len(led.Entries))
	}
}

func TestRunFinishesAtItsDeadline(t *testing.T) {
	api := fakeAPI(t, func(int) int { return 5 })
	rt := routes{"10.0.0.1:80": api.srv.Listener.Addr().String(), "10.0.0.2:80": api.srv.Listener.Addr().String()}
	l := newLifecycle(t, baseConfig(), rt)
	l.o.Timeout = 300 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l.o.Listener = ln
	start := time.Now()
	if _, err := Run(context.Background(), l.o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Run took %s past a 300ms deadline", d)
	}
	led := l.verify(t)
	if r := finishReason(t, led.Entries[len(led.Entries)-1]); r != FinishDeadline {
		t.Errorf("finish reason %q", r)
	}
}

func TestRunServesUntilTheRunnerFinishes(t *testing.T) {
	api := fakeAPI(t, func(int) int { return 5 })
	rt := routes{"10.0.0.1:80": api.srv.Listener.Addr().String(), "10.0.0.2:80": api.srv.Listener.Addr().String()}
	l := newLifecycle(t, baseConfig(), rt)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l.o.Listener = ln
	done := make(chan error, 1)
	go func() { _, err := Run(context.Background(), l.o); done <- err }()

	base := "http://" + ln.Addr().String()
	waitReady(t, base)
	post := func(verb toolwire.Verb, body string) toolwire.Response {
		resp, err := http.Post(base+toolwire.Path(verb), "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out toolwire.Response
		if err := toolwire.DecodeResponse(resp.Body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	r := post(toolwire.VerbHTTP, `{"method":"GET","path":"/api/players/p1"}`)
	if r.Observed == nil || r.Observed.Status != http.StatusOK {
		t.Fatalf("http: %+v", r)
	}
	if r = post(toolwire.VerbFinish, `{}`); r.Observed == nil || r.Observed.Summary == nil {
		t.Fatalf("finish: %+v", r)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after finish")
	}
	led := l.verify(t)
	if last := led.Entries[len(led.Entries)-1]; last.Kind != toolwire.KindRunner || last.Verb != toolwire.VerbFinish {
		t.Errorf("last entry %+v", last)
	}
}

func TestRunEndsWhenAComponentIsUnreachable(t *testing.T) {
	api := fakeAPI(t, func(int) int { return 5 })
	cfg := baseConfig()
	cfg.Checks = []StoredCheck{storedCheck(t, scoreCheck)}
	l := newLifecycle(t, cfg, routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}) // web never answers
	l.o.ReadinessWindow = 200 * time.Millisecond
	if _, err := Run(context.Background(), l.o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	led := l.verify(t)
	if len(led.Entries) != 2 {
		t.Fatalf("%d entries, want the readiness wait and finish: %+v", len(led.Entries), led.Entries)
	}
	if !strings.Contains(led.Entries[0].Output, `"web"`) {
		t.Errorf("readiness output %q does not name web", led.Entries[0].Output)
	}
	if r := finishReason(t, led.Entries[1]); r != FinishUnreachable {
		t.Errorf("finish reason %q", r)
	}
	if len(api.requests()) != 0 {
		t.Errorf("a check ran against a slot with an unreachable component")
	}
}

func waitReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + ReadyPath)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("tools never ready")
}

func TestRunFinishReasonWhenTheReplayIsCutShort(t *testing.T) {
	slow := newFakeComponent(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
		<-r.Context().Done() // never answers in time
	})
	addr := slow.srv.Listener.Addr().String()
	for _, tc := range []struct {
		name   string
		cancel bool
		want   string
	}{{"wall clock", false, FinishDeadline}, {"told to stop", true, FinishStopped}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.ReplayOnly = true
			cfg.Checks = []StoredCheck{storedCheck(t, scoreCheck)}
			l := newLifecycle(t, cfg, routes{"10.0.0.1:80": addr, "10.0.0.2:80": addr})
			l.o.Timeout = 400 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				l.o.Timeout = time.Minute
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			if _, err := Run(ctx, l.o); err != nil {
				t.Fatalf("Run: %v", err)
			}
			led := l.verify(t)
			if r := finishReason(t, led.Entries[len(led.Entries)-1]); r != tc.want {
				t.Errorf("finish reason %q, want %q", r, tc.want)
			}
		})
	}
}
