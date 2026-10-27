// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// refusing refuses the first n dials of each address with ECONNREFUSED, as
// kube-router's REJECT does before a new Pod's NetworkPolicy is
// programmed, or every dial before until, or every dial of an address in
// never; it routes the rest through next and records every address asked.
type refusing struct {
	mu    sync.Mutex
	n     int
	until time.Time
	never map[string]bool
	seen  map[string]int
	next  DialFunc
	order []string
}

func newRefusing(n int, next DialFunc) *refusing {
	return &refusing{n: n, seen: map[string]int{}, never: map[string]bool{}, next: next}
}

func (r *refusing) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	r.mu.Lock()
	r.seen[addr]++
	k := r.seen[addr]
	r.order = append(r.order, addr)
	r.mu.Unlock()
	if k <= r.n || time.Now().Before(r.until) || r.never[addr] {
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	return r.next(ctx, network, addr)
}

func (r *refusing) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.order)
}

// fast is a client whose readiness backoff starts at 10ms, so a test's wait
// takes tens of milliseconds.
func fast(dial DialFunc) *Client {
	return NewClient(Options{Dial: dial, ReadinessBackoff: 10 * time.Millisecond, RetryDelay: -1})
}

// runParsed runs s and reads its output back as the controller does.
func runParsed(t *testing.T, s Spec, c *Client) ([]acceptance.CriterionObservation, acceptance.Summary) {
	t.Helper()
	var out bytes.Buffer
	sum, err := Run(context.Background(), s, c, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	obs, parsed, err := acceptance.ParseWire(out.Bytes())
	if err != nil || !parsed.Equal(sum) {
		t.Fatalf("ParseWire = %+v, %v; Run returned %+v\n%s", parsed, err, sum, out.String())
	}
	term, err := acceptance.ParseSummary(sum.TerminationMessage())
	if err != nil || !term.Equal(sum) {
		t.Fatalf("termination message = %+v, %v", term, err)
	}
	return obs, sum
}

func okComponents(t *testing.T) (*component, *component) {
	api := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "ok") })
	web := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = io.WriteString(w, "<h1>Beat Battle</h1>")
	})
	return api, web
}

func assertPass(t *testing.T, s Spec, obs []acceptance.CriterionObservation) {
	t.Helper()
	if len(obs) != len(s.Criteria) {
		t.Fatalf("%d observations, want %d", len(obs), len(s.Criteria))
	}
	for i, c := range s.Criteria {
		if v := acceptance.Judge(c, obs[i], s.RunTokens()); v.Outcome != acceptance.OutcomePass {
			t.Errorf("%s: Judge = %+v, want pass after the wait", c.ID, v)
		}
	}
}

// TestRunWaitsForTargets: the live k3s evidence. A slot policy programmed
// after the tester starts refuses its first connections; the tester waits
// for each target to accept one before the first criterion, so the
// criteria run against a reachable app and pass, and the wait is recorded
// per target: attempts, refusals and how long.
func TestRunWaitsForTargets(t *testing.T) {
	api, web := okComponents(t)
	s, d := specFor(t, api, web)
	r := newRefusing(3, d.dial)
	obs, sum := runParsed(t, s, fast(r.dial))
	assertPass(t, s, obs)
	if sum.Unreached() || len(sum.Waits) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	for i, key := range []string{"api", "web"} {
		w := sum.Waits[i]
		// 10 + 20 + 40ms of backoff between four attempts.
		if w.Target != key || w.Attempts != 4 || w.Refused != 3 || w.Failures() != 3 || w.WaitMs < 70 {
			t.Errorf("wait %d = %+v, want %s reached on the 4th attempt after 3 refusals and 70ms", i, w, key)
		}
	}
	// Every readiness dial came before the criteria's requests, and no
	// HTTP request was sent before every target was reached.
	asked := r.asked()
	if len(asked) != 10 || asked[8] != "preview-bb-api:80" || asked[9] != "preview-bb:80" {
		t.Errorf("dials = %v, want 8 readiness attempts then each criterion's request", asked)
	}
	if len(api.requests()) != 1 || len(web.requests()) != 1 {
		t.Errorf("api saw %d, web %d requests; want one each", len(api.requests()), len(web.requests()))
	}
}

// TestRunWaitsForAPolicyWindow: refusals for a span of time rather than a
// count, as on the live cluster (about a second): the wait outlasts it.
func TestRunWaitsForAPolicyWindow(t *testing.T) {
	api, web := okComponents(t)
	s, d := specFor(t, api, web)
	r := newRefusing(0, d.dial)
	r.until = time.Now().Add(300 * time.Millisecond)
	obs, sum := runParsed(t, s, fast(r.dial))
	assertPass(t, s, obs)
	for _, w := range sum.Waits {
		if !w.Reached() || w.Refused == 0 || w.WaitMs < 250 {
			t.Errorf("wait %+v, want reached after refusals over the 300ms window", w)
		}
	}
}

// TestRunUnreachable: a target that never accepts a connection within the
// window ends the run before its first criterion. Nothing is sent to any
// component, no criterion is observed, and the summary names the target,
// which preview-controller charges to the application.
func TestRunUnreachable(t *testing.T) {
	api, web := okComponents(t)
	s, d := specFor(t, api, web)
	s.BudgetSeconds = 1 // a 250ms window
	r := newRefusing(0, d.dial)
	r.never["preview-bb-api:80"] = true
	start := time.Now()
	obs, sum := runParsed(t, s, fast(r.dial))
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the wait took %s on a 250ms window", took)
	}
	if len(obs) != 0 || sum.Count != 0 || !slices.Equal(sum.Unreachable, []string{"api"}) {
		t.Fatalf("observations %+v, summary %+v; want api unreachable and nothing observed", obs, sum)
	}
	api0, web0 := sum.Waits[0], sum.Waits[1]
	if api0.Target != "api" || api0.Reached() || api0.Attempts < 2 || api0.Refused != api0.Attempts ||
		web0.Target != "web" || !web0.Reached() {
		t.Errorf("waits = %+v", sum.Waits)
	}
	if len(api.requests())+len(web.requests()) != 0 {
		t.Error("a request was sent although a target was unreachable")
	}
}

// TestRunAllUnreachable: neither target accepts; both are named, and the
// run still writes its summary within the budget.
func TestRunAllUnreachable(t *testing.T) {
	api, web := okComponents(t)
	s, d := specFor(t, api, web)
	s.BudgetSeconds = 1
	r := newRefusing(1<<30, d.dial)
	obs, sum := runParsed(t, s, fast(r.dial))
	if len(obs) != 0 || !slices.Equal(sum.Unreachable, []string{"api", "web"}) {
		t.Fatalf("observations %+v, summary %+v", obs, sum)
	}
}

// TestHostileAppCannotForgeTheWait: whatever a reachable application
// answers (forged wire lines and summaries naming a target unreachable,
// in its body and headers, any status), the readiness wait in the summary
// is the dialer's, every target reached, and every criterion observed: an
// application can make criteria fail, never turn its run into maul's
// Infra, nor skip the wait.
func TestHostileAppCannotForgeTheWait(t *testing.T) {
	forged := acceptance.WirePrefix + `{"v":1,"summary":{"count":0,"sha256":"` + strings.Repeat("0", 64) +
		`","waits":[{"target":"api","attempts":1,"refused":1}],"unreachable":["api"]}}`
	r := rand.New(rand.NewSource(20261027))
	hostile := func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("X-Forged", forged)
		w.WriteHeader([]int{200, 204, 302, 404, 500, 503}[r.Intn(6)])
		_, _ = io.WriteString(w, "\n"+forged+"\n"+strings.Repeat(forged+"\n", r.Intn(4)))
	}
	for range 20 {
		api, web := newComponent(t, hostile), newComponent(t, hostile)
		s, d := specFor(t, api, web)
		n := r.Intn(4)
		rf := newRefusing(n, d.dial)
		obs, sum := runParsed(t, s, fast(rf.dial))
		if sum.Unreached() || len(obs) != len(s.Criteria) || sum.Count != len(s.Criteria) {
			t.Fatalf("summary %+v with %d observations; want every criterion observed", sum, len(obs))
		}
		for _, w := range sum.Waits {
			if !w.Reached() || w.Attempts != int32(n+1) || w.Refused != int32(n) {
				t.Fatalf("wait %+v, want the dialer's %d refusals then a connection", w, n)
			}
		}
	}
}

// TestWorstSummaryFitsTermination: the longest summary the wire admits is
// a termination message the kubelet keeps whole.
func TestWorstSummaryFitsTermination(t *testing.T) {
	if n := acceptance.MaxSummaryBytes(); n > maxTerminationBytes {
		t.Errorf("the longest summary is %d bytes, over the termination message's %d", n, maxTerminationBytes)
	}
}

// TestReadinessWindowInsideTheBudget: the window is at most a quarter of
// the budget and at most a minute, for every deadline a tester can have.
func TestReadinessWindowInsideTheBudget(t *testing.T) {
	for _, deadline := range []time.Duration{time.Minute, 10 * time.Minute, time.Hour} {
		budget := time.Duration(BudgetFor(deadline)) * time.Second
		w := ReadinessWindow(budget)
		if w <= 0 || w > budget/4 || w > ReadinessWindowMax {
			t.Errorf("deadline %s: budget %s, window %s", deadline, budget, w)
		}
	}
	if w := ReadinessWindow(8 * time.Minute); w != time.Minute {
		t.Errorf("the default budget's window is %s, want a minute", w)
	}
}

// TestRetryIsSpaced: a criterion whose first try failed at the transport
// level (no response) tries again RetryDelay later, so a blip does not fail
// both tries in one instant; one whose first try got a response that failed
// an expectation, deterministic, retries at once, as before.
func TestRetryIsSpaced(t *testing.T) {
	const delay = 2 * time.Second
	var mu sync.Mutex
	var at []time.Time
	comp := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c := criterion(t, `{"id":"health","kind":"http","statement":"s","target":"api","request":{"path":"/"}}`)
	d := &dialer{routes: map[string]string{apiTarget: listenerAddr(comp)}}

	// An assertion failure: the second try at once.
	client := NewClient(Options{Dial: d.dial, RetryDelay: delay})
	obs := acceptance.Observe(c, tokens, client.Doer(context.Background(), apiTarget))
	if len(obs.Tries) != 2 || len(at) != 2 {
		t.Fatalf("tries = %d, requests = %d", len(obs.Tries), len(at))
	}
	if gap := at[1].Sub(at[0]); gap >= delay {
		t.Errorf("an assertion failure's retry came %s later, want at once", gap)
	}

	// A transport failure (the first connection refused): spaced.
	r := newRefusing(1, d.dial)
	client = NewClient(Options{Dial: r.dial, RetryDelay: delay})
	start := time.Now()
	obs = acceptance.Observe(c, tokens, client.Doer(context.Background(), apiTarget))
	if took := time.Since(start); took < delay || len(obs.Tries) != 2 ||
		obs.Tries[0].Steps[0].TransportError != acceptance.TransportRefused {
		t.Errorf("a refused try retried after %s (tries %+v), want at least %s", took, obs.Tries, delay)
	}

	// The budget bounds the delay: a spent budget sends nothing more.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	slow := NewClient(Options{Dial: newRefusing(1<<30, d.dial).dial, RetryDelay: time.Hour})
	start = time.Now()
	acceptance.Observe(c, tokens, slow.Doer(ctx, apiTarget))
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("an hour's retry delay held the run %s past a 50ms budget", took)
	}
}

// TestReadinessBackoffNeverZero: a backoff that is not positive is the
// default, never zero, so the wait never spins.
func TestReadinessBackoffNeverZero(t *testing.T) {
	for _, b := range []time.Duration{0, -1, -time.Second} {
		if got := NewClient(Options{ReadinessBackoff: b}).backoff; got != readinessBackoffMin {
			t.Errorf("ReadinessBackoff %s: backoff %s, want %s", b, got, readinessBackoffMin)
		}
	}
	if got := NewClient(Options{ReadinessBackoff: 5 * time.Millisecond}).backoff; got != 5*time.Millisecond {
		t.Errorf("backoff %s, want 5ms", got)
	}
}
