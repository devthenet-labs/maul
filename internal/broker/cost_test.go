// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/devthenet-labs/maul/internal/model"
	"github.com/devthenet-labs/maul/internal/provider"
)

// costSeed fixes the pricing properties, so the gate stays deterministic.
const costSeed = 20261016

func TestPricer(t *testing.T) {
	p := newPricer(model.Builtins())
	for _, tc := range []struct {
		name string
		wire string
		u    usage
		want int64
	}{
		// Sonnet 5: $3 in, $0.30 cache read, $15 out per million tokens.
		{"input", "claude-sonnet-5", usage{input: 1_000_000}, 3_000_000},
		{"output", "claude-sonnet-5", usage{output: 1000}, 15_000},
		{"cache read", "claude-sonnet-5", usage{cacheRead: 1_000_000}, 300_000},
		{"five-minute write", "claude-sonnet-5", usage{cacheCreation: 1_000_000}, 3_750_000},
		{"one-hour write", "claude-sonnet-5", usage{cacheCreation: 1_000_000, cacheWrite1h: 1_000_000}, 6_000_000},
		{"dated id", "claude-sonnet-5-20260514", usage{output: 1000}, 15_000},
		{"canonical id", "anthropic/claude-sonnet-5", usage{output: 1000}, 15_000},
		// The shared function rounds to the micro-USD, as a run's own
		// figure is: one cache read (0.3) is 0, two (0.6) are 1.
		{"fraction down", "claude-sonnet-5", usage{cacheRead: 1}, 0},
		{"fraction up", "claude-sonnet-5", usage{cacheRead: 2}, 1},
		{"nothing", "claude-sonnet-5", usage{}, 0},
	} {
		if got := p.cost(tc.wire, tc.u); got != tc.want {
			t.Errorf("%s: cost(%q, %+v) = %d, want %d", tc.name, tc.wire, tc.u, got, tc.want)
		}
	}
	if got := p.outputCost("claude-sonnet-5", 1000); got != 15_000 {
		t.Errorf("outputCost(sonnet, 1000) = %d, want 15000", got)
	}
	if got := p.outputCost("claude-sonnet-5", 0); got != 0 {
		t.Errorf("outputCost of nothing = %d", got)
	}
	if got := p.outputCost("claude-sonnet-5", 1<<62); got != maxChargeMicroUSD {
		t.Errorf("a caller-controlled max_tokens prices at %d, want the cap %d", got, maxChargeMicroUSD)
	}
	// An id the registry cannot price is never free: it prices at the
	// ceiling, at least the dearest known model.
	opus := p.cost("claude-opus-5", usage{output: 1000})
	if got := p.cost("claude-mystery-9", usage{output: 1000}); got < opus || got == 0 {
		t.Errorf("an unknown model's output prices at %d, want at least opus's %d", got, opus)
	}
	if got := newPricer(nil).cost("claude-sonnet-5", usage{output: 1}); got != maxChargeMicroUSD {
		t.Errorf("with nothing priced, a charge is %d, want the cap", got)
	}
}

// TestPricerProperties: an unknown model never prices below any priced
// model for the same usage (fail safe), and adding tokens of any class never
// lowers a price.
func TestPricerProperties(t *testing.T) {
	p := newPricer(model.Builtins())
	cfg := &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(costSeed))}
	bound := func(v uint32) int64 { return int64(v % 50_000_000) }
	gen := func(a, b, c, d, e uint32) usage {
		u := usage{input: bound(a), cacheCreation: bound(b), cacheRead: bound(c), output: bound(d)}
		u.cacheWrite1h = min(bound(e), u.cacheCreation)
		return u
	}
	ceiling := func(a, b, c, d, e uint32) bool {
		u := gen(a, b, c, d, e)
		unknown := p.cost("not-a-model", u)
		for _, m := range model.Builtins() {
			if m.InputUSD != nil && m.OutputUSD != nil && p.cost(m.ID, u) > unknown {
				return false
			}
		}
		return true
	}
	if err := quick.Check(ceiling, cfg); err != nil {
		t.Error(err)
	}
	monotone := func(a, b, c, d, e, more uint32, class uint8) bool {
		u := gen(a, b, c, d, e)
		v := u
		n := int64(more % 1_000_000)
		switch class % 4 {
		case 0:
			v.input += n
		case 1:
			v.cacheCreation += n
		case 2:
			v.cacheRead += n
		default:
			v.output += n
		}
		return p.cost("claude-sonnet-5", v) >= p.cost("claude-sonnet-5", u)
	}
	if err := quick.Check(monotone, cfg); err != nil {
		t.Error(err)
	}
}

// TestLedgerCostLimits: under the cost unit a pod is refused once what it
// spent plus what is reserved would pass its cap, the cap is the smaller of
// --cost-per-pod and its budget annotation, the hourly ceiling spans pods,
// and every refusal names the limit in micro-USD under the prefix the
// in-pod runtime maps to budget_exceeded.
func TestLedgerCostLimits(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLedger(Limits{Unit: UnitCost, CostPerPod: 1000, CostPerHour: 2500}, func() time.Time { return now })

	held, reason := l.admit(t.Context(), "p", reservation{cost: 600})
	if reason != "" {
		t.Fatal(reason)
	}
	// The outstanding reservation counts: 600 + 600 passes 1000.
	if _, reason := l.admit(t.Context(), "p", reservation{cost: 600}); reason !=
		provider.LimitMessagePrefix+": cost per pod (1000 micro-USD) reached" {
		t.Fatalf("over the reservation: reason = %q", reason)
	}
	held()
	if got := l.observe("p", "claude-sonnet-5", "anthropic", "", usage{output: 50}); got != 750 {
		t.Fatalf("observe charged %d micro-USD, want 750", got)
	}
	if got := l.totals("p").cost; got != 750 {
		t.Fatalf("totals cost = %d, want 750", got)
	}
	if _, reason := l.admit(t.Context(), "p", reservation{cost: 300}); !strings.Contains(reason, "cost per pod") {
		t.Fatalf("750 spent + 300 reserved under 1000: reason = %q", reason)
	}
	release, reason := l.admit(t.Context(), "p", reservation{cost: 200})
	if reason != "" {
		t.Fatalf("750 + 200 fits 1000: %s", reason)
	}
	release()

	// A budget annotation lowers the cap; one above it does not raise it.
	if _, reason := l.admit(t.Context(), "p", reservation{budget: 500}); !strings.Contains(reason,
		"cost per pod (500 micro-USD)") {
		t.Fatalf("budget 500 after 750 spent: reason = %q", reason)
	}
	if _, reason := l.admit(t.Context(), "p", reservation{cost: 300, budget: 1_000_000}); !strings.Contains(reason,
		"cost per pod (1000 micro-USD)") {
		t.Fatalf("a budget above the cap raised it: reason = %q", reason)
	}

	// A shortfall is charged at the output price.
	if got := l.shortfall("q", "claude-sonnet-5", "anthropic", "", 100); got != 1500 {
		t.Fatalf("shortfall charged %d micro-USD, want 1500", got)
	}
	// 750 + 1500 = 2250 this hour: 300 more passes the 2500 ceiling.
	if _, reason := l.admit(t.Context(), "r", reservation{cost: 300}); reason !=
		provider.LimitMessagePrefix+": broker-wide hourly cost ceiling (2500 micro-USD) reached" {
		t.Fatalf("hourly cost ceiling: reason = %q", reason)
	}
	now = now.Add(61 * time.Minute)
	release, reason = l.admit(t.Context(), "r", reservation{cost: 300})
	if reason != "" {
		t.Fatalf("after the hour: %s", reason)
	}
	release()
}

// TestLedgerTokenUnitIgnoresCost: under the token unit (the default) cost
// is counted, for the audit line, but never refuses a request.
func TestLedgerTokenUnitIgnoresCost(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLedger(Limits{Unit: UnitTokens}, func() time.Time { return now })
	l.observe("p", "claude-opus-5", "anthropic", "", usage{output: 10_000_000})
	release, reason := l.admit(t.Context(), "p", reservation{cost: maxChargeMicroUSD, budget: 1})
	if reason != "" {
		t.Fatalf("token unit refused on cost: %s", reason)
	}
	release()
	if l.totals("p").cost == 0 {
		t.Error("cost was not counted under the token unit")
	}
}

// budgetPod is testJobPod (uid-1, the "jobpod" token's) with a budget
// annotation.
func budgetPod(budget string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: testJobPod, Namespace: testNamespace, UID: types.UID("uid-1"),
	}}
	if budget != "" {
		pod.Annotations = map[string]string{provider.BudgetAnnotation: budget}
	}
	return pod
}

// costServer is a cost-unit broker over upstream whose clientset holds pod
// (nil: none) and counts its pod reads.
func costServer(t *testing.T, upstream string, limits Limits, pod *corev1.Pod,
	gets *atomic.Int64) (*Server, *syncBuffer, *fake.Clientset) {
	t.Helper()
	cs := fakeReviews(nil)
	if pod != nil {
		if err := cs.Tracker().Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != testNamespace {
			t.Errorf("pod read in namespace %q, want only the agents namespace", action.GetNamespace())
		}
		gets.Add(1)
		return false, nil, nil
	})
	var buf syncBuffer
	s, err := New(cs, Config{
		AgentNamespace: testNamespace, AgentServiceAccount: testSA, Limits: limits,
		Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, upstream)}},
	}, slog.New(slog.NewJSONHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, &buf, cs
}

// postAs sends a Messages request for sonnet with max_tokens 1000 under the
// sub's token.
func postAs(h http.Handler, sub string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","max_tokens":1000}`))
	req.Header.Set(TokenHeader, tok(sub))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCostPerPodWithBudgetAnnotation: under the cost unit the broker reads
// the caller pod's budget annotation once per verdict, holds the pod to the
// smaller of it and --cost-per-pod, reserves the request's output bound at
// the model's output price, and reports the spend and the cap in micro-USD.
func TestCostPerPodWithBudgetAnnotation(t *testing.T) {
	var hits, gets atomic.Int64
	up := countingUpstream(t, &hits, "text/event-stream", sseUsage(5000, 7, true))
	s, buf, _ := costServer(t, up.URL, Limits{Unit: UnitCost, CostPerPod: 1_000_000}, budgetPod("20000"), &gets)
	h := s.Handler()

	if rec := postAs(h, "jobpod"); rec.Code != http.StatusOK {
		t.Fatalf("first request: %d: %s", rec.Code, rec.Body.String())
	}
	// message_start: 5000 in (15000) + 10 written (37.5) + 20 read (6) + 1
	// out (15) = 15058.5, rounded up; message_delta: 6 more out (90).
	line := lastAudit(t, buf)
	for key, want := range map[string]float64{
		"cost_micro_usd": 15149, "pod_cost_micro_usd": 15149, "pod_cost_limit_micro_usd": 20000,
	} {
		if line[key] != want {
			t.Errorf("audit %s = %v, want %v", key, line[key], want)
		}
	}
	// 15149 spent + 15000 reserved (1000 output tokens at $15/M) passes the
	// annotation's 20000, though not --cost-per-pod.
	rec := postAs(h, "jobpod")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if msg := errorMessage(t, rec.Body.Bytes()); msg !=
		provider.LimitMessagePrefix+": cost per pod (20000 micro-USD) reached" {
		t.Errorf("message = %q", msg)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
	if gets.Load() != 1 {
		t.Errorf("pod reads = %d, want 1 (cached with the verdict)", gets.Load())
	}
}

// TestSmallBudgetStageNotStarved (S1 bug 6): a $1 checkreview pod making
// ten small sonnet calls, each about $0.011 as the live reviewer's were, is
// refused none of them at the stage's output bound
// (provider.CheckReviewMaxOutputTokens), though the reservation rule is
// unchanged. At the CLI's default bound of 64000 tokens ($0.96 reserved per
// request) the same pod is refused its fifth call having spent under $0.05:
// what starved the reviewer live.
func TestSmallBudgetStageNotStarved(t *testing.T) {
	post := func(h http.Handler, maxTokens int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-5","max_tokens":`+strconv.Itoa(maxTokens)+`}`))
		req.Header.Set(TokenHeader, tok("jobpod"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, tc := range []struct {
		name      string
		maxTokens int
		served    int // calls served before the first refusal, of ten
	}{
		{"the stage's bound", provider.CheckReviewMaxOutputTokens, 10},
		{"the CLI's default bound", 64000, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits, gets atomic.Int64
			// 2000 input, 10 written, 20 read and 300 output tokens:
			// 10544 micro-USD a call.
			up := countingUpstream(t, &hits, "text/event-stream", sseUsage(2000, 300, true))
			s, buf, _ := costServer(t, up.URL, Limits{Unit: UnitCost, CostPerPod: 10_000_000},
				budgetPod("1000000"), &gets)
			h := s.Handler()
			served := 0
			for i := range 10 {
				rec := post(h, tc.maxTokens)
				if rec.Code != http.StatusOK {
					if rec.Code != http.StatusTooManyRequests {
						t.Fatalf("call %d: %d: %s", i+1, rec.Code, rec.Body.String())
					}
					break
				}
				served++
			}
			if served != tc.served {
				t.Errorf("served %d calls before a refusal, want %d", served, tc.served)
			}
			if tc.served == 10 {
				if spent := lastAudit(t, buf)["pod_cost_micro_usd"]; spent != float64(105440) {
					t.Errorf("pod spent %v micro-USD, want 105440", spent)
				}
			}
		})
	}
}

// TestBudgetFallsBack: a budget the broker cannot honour — the token's pod
// UID is not the pod's (the name now belongs to another pod), the annotation
// is absent or malformed, the pod is gone — falls back to --cost-per-pod,
// never to unlimited and never to another pod's budget.
func TestBudgetFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		sub  string
		pod  *corev1.Pod
	}{
		{"another pod's uid", "jobpod-recycled", budgetPod("20000")},
		{"no annotation", "jobpod", budgetPod("")},
		{"malformed", "jobpod", budgetPod("20000usd")},
		{"zero", "jobpod", budgetPod("0")},
		{"pod gone", "jobpod", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits, gets atomic.Int64
			up := countingUpstream(t, &hits, "text/event-stream", sseUsage(5000, 7, true))
			s, buf, _ := costServer(t, up.URL, Limits{Unit: UnitCost, CostPerPod: 40_000}, tc.pod, &gets)
			if rec := postAs(s.Handler(), tc.sub); rec.Code != http.StatusOK {
				t.Fatalf("%d: %s", rec.Code, rec.Body.String())
			}
			if got := lastAudit(t, buf)["pod_cost_limit_micro_usd"]; got != float64(40_000) {
				t.Errorf("cost limit = %v, want --cost-per-pod 40000", got)
			}
		})
	}
}

// TestBudgetReadFailureNotCached: a pod read the API server did not answer
// refuses the request as an unavailable review (503) and caches nothing, so
// the next request reads again rather than running under a budget the
// broker never saw.
func TestBudgetReadFailureNotCached(t *testing.T) {
	var hits, gets atomic.Int64
	up := countingUpstream(t, &hits, "text/event-stream", sseUsage(5000, 7, true))
	s, _, cs := costServer(t, up.URL, Limits{Unit: UnitCost, CostPerPod: 40_000}, budgetPod("20000"), &gets)
	var failing atomic.Bool
	failing.Store(true)
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, errors.New("apiserver unreachable")
		}
		return false, nil, nil
	})
	h := s.Handler()
	if rec := postAs(h, "jobpod"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("read failure: %d, want 503: %s", rec.Code, rec.Body.String())
	}
	failing.Store(false)
	if rec := postAs(h, "jobpod"); rec.Code != http.StatusOK {
		t.Fatalf("after recovery: %d: %s", rec.Code, rec.Body.String())
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
}

// TestTokenUnitReadsNoPods: under the token unit (the default) the broker
// reads no pod at all, its only Kubernetes access staying TokenReview.
func TestTokenUnitReadsNoPods(t *testing.T) {
	var hits, gets atomic.Int64
	up := countingUpstream(t, &hits, "text/event-stream", sseUsage(5000, 7, true))
	s, buf, _ := costServer(t, up.URL, Limits{}, budgetPod("1"), &gets)
	if rec := postAs(s.Handler(), "jobpod"); rec.Code != http.StatusOK {
		t.Fatalf("%d: %s", rec.Code, rec.Body.String())
	}
	if gets.Load() != 0 {
		t.Errorf("pod reads = %d under the token unit, want 0", gets.Load())
	}
	line := lastAudit(t, buf)
	if line["cost_micro_usd"] != float64(15149) || line["pod_cost_limit_micro_usd"] != float64(0) {
		t.Errorf("audit cost = %v, limit = %v; want 15149 counted and no limit", line["cost_micro_usd"],
			line["pod_cost_limit_micro_usd"])
	}
}
