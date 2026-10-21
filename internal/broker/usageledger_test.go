// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/devthenet-labs/maul/internal/usagewire"
)

const ledgerPod = testJobPod + "/uid-1"

// TestLedgerShortfallSeparated: what the responses reported and the
// worst-case shortfall charged for a cut response are kept apart per model,
// while the limits count both.
func TestLedgerShortfallSeparated(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLedger(Limits{}, func() time.Time { return now })
	l.observe(ledgerPod, "claude-sonnet-5", "anthropic", usagewire.CredentialAPIKey,
		usage{input: 100, cacheCreation: 30, cacheWrite1h: 10, cacheRead: 20, output: 5})
	l.shortfall(ledgerPod, "claude-sonnet-5", "anthropic", usagewire.CredentialAPIKey, 400)
	l.observe(ledgerPod, "us.anthropic.claude-haiku-4-5-20251001-v1:0", "anthropic", usagewire.CredentialAPIKey,
		usage{input: 7, output: 3})

	got := l.jobUsage(testJob)
	want := []usagewire.Pod{{
		Name: testJobPod, UID: "uid-1", FirstSeen: now.UTC(),
		Models: []usagewire.Model{
			{Model: "claude-haiku-4-5-20251001-v1:0", Route: "anthropic", Credential: usagewire.CredentialAPIKey,
				Input: 7, Output: 3},
			{Model: "claude-sonnet-5", Route: "anthropic", Credential: usagewire.CredentialAPIKey,
				Input: 100, Output: 5, CacheRead: 20, CacheWrite5m: 20, CacheWrite1h: 10, EstimatedShortfall: 400},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("jobUsage =\n%+v\nwant\n%+v", got, want)
	}
	if tot := l.totals(ledgerPod); tot.tokens != 100+30+20+5+400+7+3 {
		t.Errorf("limit total = %d, want observed and shortfall both counted", tot.tokens)
	}
}

// TestLedgerModelKeysBounded: a pod keeps at most usagewire.MaxModels model
// ids; an id outside the grammar and any past the cap share a reserved key
// no real id can take.
func TestLedgerModelKeysBounded(t *testing.T) {
	l := newLedger(Limits{}, time.Now)
	for i := range usagewire.MaxModels + 3 {
		l.observe(ledgerPod, fmt.Sprintf("model-%d", i), "anthropic", "", usage{input: 1})
	}
	l.observe(ledgerPod, "Bad Model\n", "anthropic", "", usage{input: 1})
	models := l.jobUsage(testJob)[0].Models
	if len(models) != usagewire.MaxModels+1 {
		t.Fatalf("models = %d, want %d kept plus the overflow key", len(models), usagewire.MaxModels+1)
	}
	byID := map[string]int64{}
	for _, m := range models {
		byID[m.Model] = m.Input
	}
	// The invalid id arrives after the cap, so it lands in the overflow.
	if byID[usagewire.ModelOther] != 4 {
		t.Errorf("overflow = %d tokens, want the 3 extra models and the invalid id (4)", byID[usagewire.ModelOther])
	}
	l2 := newLedger(Limits{}, time.Now)
	l2.observe(ledgerPod, "Bad Model\n", "anthropic", "", usage{input: 1})
	if m := l2.jobUsage(testJob)[0].Models[0].Model; m != usagewire.ModelInvalid {
		t.Errorf("invalid id keyed %q, want %q", m, usagewire.ModelInvalid)
	}
}

// TestLedgerJobUsageScope: only the named Job's pods are reported, each pod
// UID its own entry, and a read creates nothing.
func TestLedgerJobUsageScope(t *testing.T) {
	l := newLedger(Limits{}, time.Now)
	l.observe(testJobPod+"/uid-1", "m", "anthropic", "", usage{input: 1})
	l.observe(testJobPod+"/uid-2", "m", "anthropic", "", usage{input: 2})
	l.observe(testJob+"1-x7k2p/uid-3", "m", "anthropic", "", usage{input: 4}) // job "...-a11"
	l.observe("agent-pod-1", "m", "anthropic", "", usage{input: 8})
	got := l.jobUsage(testJob)
	if len(got) != 2 || got[0].UID != "uid-1" || got[1].UID != "uid-2" {
		t.Fatalf("jobUsage = %+v, want the two entries of %s", got, testJobPod)
	}
	if got := l.jobUsage("maul-ffffffffff-inv-a1"); got != nil {
		t.Errorf("unknown job = %+v, want none", got)
	}
	if n := len(l.pods); n != 4 {
		t.Errorf("ledger holds %d pods after reads, want 4", n)
	}
}

// TestLedgerInflight: a request in flight shows on the entry until it is
// released, which is what a controller waits on before calling usage final.
func TestLedgerInflight(t *testing.T) {
	l := newLedger(Limits{}, time.Now)
	release, reason := l.admit(t.Context(), ledgerPod, reservation{tokens: 0})
	if reason != "" {
		t.Fatal(reason)
	}
	if got := l.jobUsage(testJob)[0].Inflight; got != 1 {
		t.Fatalf("inflight = %d while held, want 1", got)
	}
	release()
	if got := l.jobUsage(testJob)[0].Inflight; got != 0 {
		t.Fatalf("inflight = %d after release, want 0", got)
	}
}

// ledgerOp is one observe (shortfall false) or shortfall charge.
type ledgerOp struct {
	model     int
	shortfall bool
	u         usage
}

// TestLedgerAccountingProperty (seeded): for any sequence of observed
// increments and shortfall charges over a few models, each model's observed
// fields are the sums of its observed increments, its shortfall the sum of
// its shortfall charges, and the limits' total is everything charged; the
// observed figures never include a shortfall.
func TestLedgerAccountingProperty(t *testing.T) {
	cfg := quickConfig()
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		ops := make([]ledgerOp, rnd.Intn(40))
		for i := range ops {
			cc := rnd.Int63n(1000)
			ops[i] = ledgerOp{model: rnd.Intn(3), shortfall: rnd.Intn(3) == 0, u: usage{
				input: rnd.Int63n(1000), cacheCreation: cc, cacheWrite1h: rnd.Int63n(cc + 1),
				cacheRead: rnd.Int63n(1000), output: rnd.Int63n(1000),
			}}
		}
		args[0] = reflect.ValueOf(ops)
	}
	prop := func(ops []ledgerOp) bool {
		l := newLedger(Limits{}, time.Now)
		var observed [3]usage
		var short [3]int64
		var total int64
		for _, op := range ops {
			name := fmt.Sprintf("claude-m%d", op.model)
			if op.shortfall {
				l.shortfall(ledgerPod, name, "anthropic", "", op.u.total())
				short[op.model] += op.u.total()
			} else {
				l.observe(ledgerPod, name, "anthropic", "", op.u)
				observed[op.model] = observed[op.model].mergeAdd(op.u)
			}
			total += op.u.total()
		}
		if l.totals(ledgerPod).tokens != total {
			return false
		}
		pods := l.jobUsage(testJob)
		if len(pods) == 0 {
			return total == 0
		}
		for _, m := range pods[0].Models {
			var i int
			if _, err := fmt.Sscanf(m.Model, "claude-m%d", &i); err != nil {
				return false
			}
			o := observed[i]
			if m.Input != o.input || m.Output != o.output || m.CacheRead != o.cacheRead ||
				m.CacheWrite1h != o.cacheWrite1h || m.CacheWrite5m+m.CacheWrite1h != o.cacheCreation ||
				m.EstimatedShortfall != short[i] {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}

// TestUsageScannerCacheBreakdown: the cache_creation breakdown splits the
// cache writes into their five-minute and one-hour parts; without it every
// write is a five-minute one.
func TestUsageScannerCacheBreakdown(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{" +
		`"input_tokens":10,"cache_creation_input_tokens":300,"cache_read_input_tokens":5,"output_tokens":1,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}}}` + "\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	got, complete := scanAll("text/event-stream", stream)
	if !complete || got.cacheCreation != 300 || got.cacheWrite1h != 200 || got.cacheWrite5m() != 100 ||
		got.total() != 10+300+5+9 {
		t.Fatalf("scan = %+v (5m %d) complete %v", got, got.cacheWrite5m(), complete)
	}
	plain, _ := scanAll("application/json", `{"usage":{"input_tokens":1,"cache_creation_input_tokens":40}}`)
	if plain.cacheWrite1h != 0 || plain.cacheWrite5m() != 40 {
		t.Errorf("no breakdown: %+v, want every write five-minute", plain)
	}
	// A breakdown larger than the reported total raises the total.
	odd, _ := scanAll("application/json",
		`{"usage":{"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":4,`+
			`"ephemeral_1h_input_tokens":20}}}`)
	if odd.cacheCreation != 24 || odd.cacheWrite1h != 20 {
		t.Errorf("oversized breakdown: %+v, want total 24 with 20 one-hour", odd)
	}
}
