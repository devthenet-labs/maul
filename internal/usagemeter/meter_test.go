// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagemeter

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/model"
	"github.com/bitwise-media-group/patchy/internal/usagewire"
)

// seed keeps every property deterministic in the gate.
const seed = 20261016

const job = "patchy-0a1b2c3d4e-inv-a1"

var (
	t0      = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	created = t0.Add(time.Hour) // the Job's creation
)

func pod(inflight int64, models ...usagewire.Model) usagewire.Pod {
	return usagewire.Pod{Name: job + "-x7k2p", UID: "u1", FirstSeen: created.Add(time.Minute), Inflight: inflight,
		Models: models}
}

func sonnet(in, out int64) usagewire.Model {
	return usagewire.Model{Model: "claude-sonnet-5-20260514", Route: "anthropic",
		Credential: usagewire.CredentialAPIKey, Input: in, Output: out}
}

func TestDecide(t *testing.T) {
	booted := usagewire.Response{BootTime: t0, Pods: []usagewire.Pod{pod(0, sonnet(10, 1))}}
	restarted := usagewire.Response{BootTime: created.Add(30 * time.Second), Pods: booted.Pods}
	busy := usagewire.Response{BootTime: t0, Pods: []usagewire.Pod{pod(1, sonnet(10, 1))}}
	recycled := usagewire.Response{BootTime: t0, Pods: []usagewire.Pod{{Name: job + "-x7k2p",
		FirstSeen: created.Add(-time.Minute), Models: []usagewire.Model{sonnet(10, 1)}}}}
	unreachable := errors.New("connection refused")
	for _, tt := range []struct {
		name   string
		resp   usagewire.Response
		err    error
		waited bool
		want   v1alpha1.UsageSource
		again  bool
	}{
		{"booted before the job", booted, nil, false, v1alpha1.UsageSourceMeasured, false},
		{"restarted after the job existed", restarted, nil, false, v1alpha1.UsageSourcePartial, false},
		{"in flight, waiting", busy, nil, false, "", true},
		{"in flight, wait over", busy, nil, true, v1alpha1.UsageSourcePartial, false},
		{"unreachable, waiting", usagewire.Response{}, unreachable, false, "", true},
		{"unreachable, wait over", usagewire.Response{}, unreachable, true, v1alpha1.UsageSourceReported, false},
		{"no entry", usagewire.Response{BootTime: t0}, nil, false, v1alpha1.UsageSourceReported, false},
		{"only a recycled name's entry", recycled, nil, false, v1alpha1.UsageSourceReported, false},
		{"booted the instant the job was created", usagewire.Response{BootTime: created, Pods: booted.Pods}, nil,
			false, v1alpha1.UsageSourcePartial, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, again := Decide(tt.resp, tt.err, created, tt.waited)
			if again != tt.again || out.Source != tt.want {
				t.Fatalf("Decide = %+v again %v, want %q again %v", out, again, tt.want, tt.again)
			}
			if measured := out.Measured != nil; measured != (tt.want == v1alpha1.UsageSourceMeasured ||
				tt.want == v1alpha1.UsageSourcePartial) {
				t.Errorf("measured present = %v for source %q", measured, out.Source)
			}
		})
	}
}

// fakeFetcher answers from a script, one answer per call, repeating the last.
type fakeFetcher struct {
	answers []usagewire.Response
	errs    []error
	calls   int
	jobs    []string
}

func (f *fakeFetcher) Fetch(_ context.Context, j string) (usagewire.Response, error) {
	f.jobs = append(f.jobs, j)
	i := min(f.calls, len(f.answers)-1)
	f.calls++
	return f.answers[i], f.errs[i]
}

func TestSettle(t *testing.T) {
	clock := t0.Add(2 * time.Hour)
	f := &fakeFetcher{
		answers: []usagewire.Response{
			{BootTime: t0, Pods: []usagewire.Pod{pod(1, sonnet(10, 1))}},
			{BootTime: t0, Pods: []usagewire.Pod{pod(0, sonnet(1_000_000, 100_000))}},
		},
		errs: []error{nil, nil},
	}
	m := &Meter{Fetcher: f, Now: func() time.Time { return clock }}
	run := Run{UID: "uid", Job: job, Created: created, Harness: model.HarnessClaude}

	out, wait := m.Settle(t.Context(), run)
	if out != nil || wait != DefaultPoll {
		t.Fatalf("in flight: %+v / %v, want to look again after %v", out, wait, DefaultPoll)
	}
	clock = clock.Add(DefaultPoll)
	out, _ = m.Settle(t.Context(), run)
	if out == nil || out.Source != v1alpha1.UsageSourceMeasured || out.Measured == nil {
		t.Fatalf("settled: %+v", out)
	}
	// $3 per million input, $15 per million output.
	if got := out.Measured.CostUSD; got != "4.500000" || out.Measured.PriceTable != model.PriceTableVersion ||
		out.Measured.Pricing != v1alpha1.UsagePricingList {
		t.Errorf("measured = %+v", out.Measured)
	}
	if !out.Measured.BrokerBootTime.Time.Equal(t0) {
		t.Errorf("boot time = %v", out.Measured.BrokerBootTime)
	}
	if f.jobs[0] != job {
		t.Errorf("fetched %v", f.jobs)
	}
	if len(m.first) != 0 {
		t.Errorf("a settled run's wait is still remembered: %v", m.first)
	}

	// Not brokered, or never launched: reported at once, no fetch.
	for _, r := range []Run{
		{UID: "a", Job: job, Created: created, Harness: model.HarnessCodex},
		{UID: "b", Job: job, Created: created, Harness: model.HarnessFake},
		{UID: "c", Created: created, Harness: model.HarnessClaude},
	} {
		calls := f.calls
		if out, _ := m.Settle(t.Context(), r); out == nil || out.Source != v1alpha1.UsageSourceReported ||
			out.Measured != nil || f.calls != calls {
			t.Errorf("%+v: %+v (fetches %d)", r, out, f.calls-calls)
		}
	}
}

// TestSettleWaitBounded: a broker that never answers, or a pod whose
// requests never settle, is waited for two minutes from the first look and
// no longer.
func TestSettleWaitBounded(t *testing.T) {
	for name, f := range map[string]*fakeFetcher{
		"unreachable": {answers: []usagewire.Response{{}}, errs: []error{errors.New("refused")}},
		"in flight": {answers: []usagewire.Response{{BootTime: t0, Pods: []usagewire.Pod{pod(2, sonnet(5, 5))}}},
			errs: []error{nil}},
	} {
		t.Run(name, func(t *testing.T) {
			clock := t0.Add(2 * time.Hour)
			m := &Meter{Fetcher: f, Now: func() time.Time { return clock }}
			run := Run{UID: "uid", Job: job, Created: created, Harness: model.HarnessClaude}
			start := clock
			for clock.Sub(start) < DefaultWait {
				if out, _ := m.Settle(t.Context(), run); out != nil {
					t.Fatalf("settled after %v, before the wait ended: %+v", clock.Sub(start), out)
				}
				clock = clock.Add(DefaultPoll)
			}
			out, _ := m.Settle(t.Context(), run)
			want := map[string]v1alpha1.UsageSource{"unreachable": v1alpha1.UsageSourceReported,
				"in flight": v1alpha1.UsageSourcePartial}[name]
			if out == nil || out.Source != want {
				t.Fatalf("after the wait: %+v, want %q", out, want)
			}
		})
	}
}

func TestPrice(t *testing.T) {
	boot := metaTime(t0)
	pods := []usagewire.Pod{pod(0,
		usagewire.Model{Model: "claude-sonnet-5-20260514", Credential: usagewire.CredentialAPIKey,
			Input: 1_000_000, CacheWrite5m: 1_000_000, CacheWrite1h: 1_000_000, EstimatedShortfall: 9_999_999},
		usagewire.Model{Model: "claude-haiku-4-5", Credential: usagewire.CredentialAPIKey, Output: 1_000_000},
	)}
	got := Price(model.Builtins(), pods, boot)
	// sonnet 3 + 3.75 + 6 = 12.75; haiku output $5.
	if got.CostUSD != "17.750000" || got.Pricing != v1alpha1.UsagePricingList ||
		got.PriceTable != model.PriceTableVersion || got.EstimatedShortfallTokens != 9_999_999 ||
		got.CacheWriteTokens != 2_000_000 || got.InputTokens != 1_000_000 || got.OutputTokens != 1_000_000 {
		t.Fatalf("Price = %+v", got)
	}
	if len(got.Models) != 2 || got.Models[0].Model != "anthropic/claude-haiku-4-5" ||
		got.Models[1].Model != "anthropic/claude-sonnet-5" || got.Models[1].CostUSD != "12.750000" ||
		got.Models[1].EstimatedShortfallTokens != 9_999_999 || got.Models[1].PriceTable != model.PriceTableVersion {
		t.Errorf("models = %+v", got.Models)
	}

}

// TestPriceUnknownModel: an unknown model keeps its tokens and has no cost;
// the total, which would understate the run, is absent too.
func TestPriceUnknownModel(t *testing.T) {
	boot := metaTime(t0)
	unknown := Price(model.Builtins(), []usagewire.Pod{pod(0, sonnet(1_000_000, 0),
		usagewire.Model{Model: "claude-mystery-9", Credential: usagewire.CredentialAPIKey, Input: 7})}, boot)
	if unknown.CostUSD != "" || unknown.PriceTable != "" || unknown.InputTokens != 1_000_007 {
		t.Errorf("unknown model: %+v", unknown)
	}
	if m := unknown.Models[1]; m.Model != "claude-mystery-9" || m.CostUSD != "" || m.InputTokens != 7 {
		t.Errorf("unknown model entry: %+v", m)
	}
	if m := unknown.Models[0]; m.CostUSD != "3.000000" {
		t.Errorf("priced beside it: %+v", m)
	}

}

// TestPriceNotional: a subscription's priced figure is notional, and a
// priced model with only a shortfall cost nothing observed ("0", not
// unknown).
func TestPriceNotional(t *testing.T) {
	boot := metaTime(t0)
	sub := Price(model.Builtins(), []usagewire.Pod{pod(0, usagewire.Model{Model: "claude-opus-5",
		Credential: usagewire.CredentialOAuthToken, Output: 1_000_000})}, boot)
	if sub.Pricing != v1alpha1.UsagePricingSubscription || sub.Models[0].Pricing != v1alpha1.UsagePricingSubscription ||
		sub.CostUSD != "25.000000" || sub.Models[0].Credential != usagewire.CredentialOAuthToken {
		t.Errorf("subscription: %+v", sub)
	}

	short := Price(model.Builtins(), []usagewire.Pod{pod(0, usagewire.Model{Model: "claude-sonnet-5",
		EstimatedShortfall: 500})}, boot)
	if short.CostUSD != "0.000000" || short.EstimatedShortfallTokens != 500 {
		t.Errorf("shortfall only: %+v", short)
	}
}

// TestPriceBoundsCredential: an unexpected credential kind is recorded as
// "unknown" (priced list), never as the broker's string.
func TestPriceBoundsCredential(t *testing.T) {
	got := Price(model.Builtins(), []usagewire.Pod{pod(0, usagewire.Model{Model: "claude-sonnet-5",
		Credential: strings.Repeat("x", 100), Input: 1})}, nil)
	if m := got.Models[0]; m.Credential != "unknown" || m.Pricing != v1alpha1.UsagePricingList {
		t.Errorf("model = %+v", m)
	}
}

// TestPriceBoundsModels: however many models the pods report, the figure
// keeps at most the schema's sixteen, the rest summed under ModelOther.
func TestPriceBoundsModels(t *testing.T) {
	pods := make([]usagewire.Pod, 0, 3)
	for p := range 3 {
		models := make([]usagewire.Model, 0, 9)
		for i := range 9 {
			models = append(models, usagewire.Model{Model: fmt.Sprintf("m-%d-%d", p, i), Input: 1})
		}
		pods = append(pods, pod(0, models...))
	}
	got := Price(model.Builtins(), pods, nil)
	if len(got.Models) != maxModels || got.InputTokens != 27 {
		t.Fatalf("models = %d input %d, want %d and 27", len(got.Models), got.InputTokens, maxModels)
	}
	var other int64
	for _, m := range got.Models {
		if m.Model == usagewire.ModelOther {
			other = m.InputTokens
		}
	}
	if other != 27-(maxModels-1) {
		t.Errorf("overflow = %d", other)
	}
}

// TestPriceProperty (seeded): the totals are the sums of the models'
// counts; the shortfall never moves a cost; and when every model is priced
// the total cost is the sum of theirs.
func TestPriceProperty(t *testing.T) {
	ids := []string{"claude-sonnet-5", "claude-haiku-4-5-20251001", "claude-opus-5@20260101", "claude-unknown-1"}
	cfg := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(seed))}
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		n := rnd.Intn(6)
		models := make([]usagewire.Model, 0, n)
		for range n {
			models = append(models, usagewire.Model{
				Model: ids[rnd.Intn(len(ids))], Credential: usagewire.CredentialAPIKey,
				Input: rnd.Int63n(1e7), Output: rnd.Int63n(1e7), CacheRead: rnd.Int63n(1e7),
				CacheWrite5m: rnd.Int63n(1e7), CacheWrite1h: rnd.Int63n(1e7), EstimatedShortfall: rnd.Int63n(1e7),
			})
		}
		args[0] = reflect.ValueOf(models)
	}
	prop := func(models []usagewire.Model) bool {
		got := Price(model.Builtins(), []usagewire.Pod{pod(0, models...)}, nil)
		noShort := make([]usagewire.Model, len(models))
		for i, m := range models {
			m.EstimatedShortfall = 0
			noShort[i] = m
		}
		if Price(model.Builtins(), []usagewire.Pod{pod(0, noShort...)}, nil).CostUSD != got.CostUSD {
			return false
		}
		var in, out, read, write, short int64
		var cost float64
		allPriced := true
		for _, m := range got.Models {
			in, out, read = in+m.InputTokens, out+m.OutputTokens, read+m.CacheReadTokens
			write, short = write+m.CacheWrite5mTokens+m.CacheWrite1hTokens, short+m.EstimatedShortfallTokens
			if m.CostUSD == "" {
				allPriced = allPriced && m.InputTokens+m.OutputTokens+m.CacheReadTokens+m.CacheWrite5mTokens+
					m.CacheWrite1hTokens == 0
				continue
			}
			c, _ := strconv.ParseFloat(m.CostUSD, 64)
			cost += c
		}
		if in != got.InputTokens || out != got.OutputTokens || read != got.CacheReadTokens ||
			write != got.CacheWriteTokens || short != got.EstimatedShortfallTokens {
			return false
		}
		if !allPriced {
			return got.CostUSD == ""
		}
		if len(got.Models) == 0 {
			return got.CostUSD == ""
		}
		total, err := strconv.ParseFloat(got.CostUSD, 64)
		return err == nil && total-cost < 1e-5 && cost-total < 1e-5
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}

// TestClient: the token file is read on every call and sent as a bearer, a
// non-200 or oversized answer is an error, and a bad Job name never leaves.
func TestClient(t *testing.T) {
	var gotAuth, gotPath string
	status := http.StatusOK
	body := `{"bootTime":"2026-10-16T12:00:00Z","pods":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(srv.URL+"/", tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Fetch(t.Context(), job)
	if err != nil || !resp.BootTime.Equal(t0) || gotAuth != "Bearer first" || gotPath != usagewire.PathPrefix+job {
		t.Fatalf("fetch: %+v %v auth %q path %q", resp, err, gotAuth, gotPath)
	}
	if err := os.WriteFile(tokenFile, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(t.Context(), job); err != nil || gotAuth != "Bearer rotated" {
		t.Fatalf("rotated token not re-read: %q %v", gotAuth, err)
	}
	status = http.StatusUnauthorized
	if _, err := c.Fetch(t.Context(), job); err == nil {
		t.Error("401 accepted")
	}
	status, body = http.StatusOK, `{"pods":"`+strings.Repeat("x", maxResponse)+`"}`
	if _, err := c.Fetch(t.Context(), job); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("oversized answer: %v", err)
	}
	gotPath = ""
	if _, err := c.Fetch(t.Context(), "../etc"); err == nil || gotPath != "" {
		t.Errorf("bad job name fetched: %v %q", err, gotPath)
	}
	for _, bad := range []string{"", "ftp://x", "http://"} {
		if _, err := NewClient(bad, tokenFile); err == nil {
			t.Errorf("NewClient(%q) accepted", bad)
		}
	}
	if _, err := NewClient(srv.URL, ""); err == nil {
		t.Error("no token file accepted")
	}
}

func TestNew(t *testing.T) {
	if m, err := New(Config{}, nil); m != nil || err != nil || m.On() {
		t.Fatalf("off: %v %v", m, err)
	}
	if _, err := New(Config{On: true}, nil); err == nil {
		t.Error("on without a broker URL accepted")
	}
	m, err := New(Config{On: true, BrokerURL: "http://broker:8082", TokenFile: DefaultTokenFile}, nil)
	if err != nil || !m.On() {
		t.Fatalf("on: %v %v", m, err)
	}
}

// TestPendingDeleted: a run being deleted still waits for its usage while
// the meter is on, it launched a Job and its usage is not final (or it
// never reported one); never while the meter is off, for a run that never
// launched, or once final.
func TestPendingDeleted(t *testing.T) {
	on := &Meter{Fetcher: &fakeFetcher{}}
	ref := &v1alpha1.JobReference{Name: job}
	for _, tc := range []struct {
		name string
		m    *Meter
		ref  *v1alpha1.JobReference
		u    *v1alpha1.UsageSummary
		want bool
	}{
		{name: "no stage yet", m: on, ref: ref, want: true},
		{name: "reported, not final", m: on, ref: ref, u: &v1alpha1.UsageSummary{CostUSD: "1"}, want: true},
		{name: "final", m: on, ref: ref, u: &v1alpha1.UsageSummary{Final: true}},
		{name: "never launched", m: on},
		{name: "unnamed job", m: on, ref: &v1alpha1.JobReference{}},
		{name: "meter off", ref: ref},
	} {
		if got := tc.m.PendingDeleted(tc.ref, tc.u); got != tc.want {
			t.Errorf("%s: PendingDeleted = %v, want %v", tc.name, got, tc.want)
		}
	}
}
