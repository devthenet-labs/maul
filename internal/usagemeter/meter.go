// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagemeter

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/model"
	"github.com/bitwise-media-group/patchy/internal/usagewire"
)

// Defaults: how long a controller keeps asking after a run settles, and how
// often.
const (
	DefaultWait = 2 * time.Minute
	DefaultPoll = 10 * time.Second
)

// Meter settles runs' usage against the broker. A nil Meter is off: On is
// false and no controller calls Settle.
type Meter struct {
	Fetcher Fetcher
	// Models is the registry measured ids are priced against; nil takes
	// model.Builtins().
	Models []model.Model
	// Wait bounds how long a run's usage is waited for, from the first
	// look in this process; Poll is the interval between looks. Zero takes
	// the defaults.
	Wait, Poll time.Duration
	Now        func() time.Time
	Log        *slog.Logger

	mu    sync.Mutex
	first map[types.UID]time.Time
}

// On reports whether measurement is configured.
func (m *Meter) On() bool { return m != nil && m.Fetcher != nil }

// Run is what Settle needs of a settled run.
type Run struct {
	// UID keys the wait.
	UID types.UID
	// Job is the run's Job name, empty when none was launched.
	Job string
	// Created is no later than the run's pod's creation: the Job's
	// creation time, or the run's own when the Job is gone (earlier still,
	// so the check stays sound, only stricter).
	Created time.Time
	// Harness is the harness the controller launched the run on (never
	// the pod's own report of it).
	Harness string
}

// Brokered reports whether a harness's model traffic goes through the
// broker: brokered claude only.
func Brokered(harness string) bool { return harness == model.HarnessClaude }

// Outcome is a run's settled usage.
type Outcome struct {
	Source   v1alpha1.UsageSource
	Measured *v1alpha1.MeasuredUsage
}

// Apply writes o onto a run's usage and marks it final.
func Apply(u *v1alpha1.UsageSummary, o Outcome) {
	u.Source, u.Measured, u.Final = o.Source, o.Measured, true
}

// Settle returns run's settled usage, or nil and how long to wait before
// looking again.
func (m *Meter) Settle(ctx context.Context, run Run) (*Outcome, time.Duration) {
	if run.Job == "" || !Brokered(run.Harness) {
		return &Outcome{Source: v1alpha1.UsageSourceReported}, 0
	}
	now := m.now()
	waited := now.Sub(m.firstLook(run.UID, now)) >= m.wait()
	resp, err := m.Fetcher.Fetch(ctx, run.Job)
	if err != nil {
		m.log().LogAttrs(ctx, slog.LevelWarn, "egress broker usage unavailable",
			slog.String("job", run.Job), slog.Bool("giving_up", waited), slog.Any("error", err))
	}
	out, again := Decide(resp, err, run.Created, waited)
	if again {
		return nil, m.poll()
	}
	if out.Measured != nil {
		out.Measured = Price(m.models(), Pods(resp, run.Created), out.Measured.BrokerBootTime)
	}
	m.forget(run.UID)
	return &out, 0
}

// Decide is Settle's verdict over one answer from the broker (or the error
// in its place): the outcome, or again when the run should be looked at
// later. created is no later than the pod's creation; waited is whether the
// wait is over. Outcome.Measured carries only the boot time; Price fills it.
func Decide(resp usagewire.Response, err error, created time.Time, waited bool) (Outcome, bool) {
	if err != nil {
		if waited {
			return Outcome{Source: v1alpha1.UsageSourceReported}, false
		}
		return Outcome{}, true
	}
	pods := Pods(resp, created)
	if len(pods) == 0 {
		// No entry: nothing went through the broker, or it was restarted
		// or forgot the pod. Either way it has no figure to offer.
		return Outcome{Source: v1alpha1.UsageSourceReported}, false
	}
	inflight := false
	for _, p := range pods {
		inflight = inflight || p.Inflight > 0
	}
	if inflight && !waited {
		return Outcome{}, true
	}
	source := v1alpha1.UsageSourceMeasured
	if inflight || !resp.BootTime.Before(created) {
		source = v1alpha1.UsageSourcePartial
	}
	boot := resp.BootTime
	return Outcome{Source: source, Measured: &v1alpha1.MeasuredUsage{BrokerBootTime: metaTime(boot)}}, false
}

// Pods is the answer's entries a run can own: those first seen no earlier
// than its Job's creation. An entry older than that is another pod's that
// once had the same name.
func Pods(resp usagewire.Response, created time.Time) []usagewire.Pod {
	var out []usagewire.Pod
	for _, p := range resp.Pods {
		if !p.FirstSeen.Before(created) {
			out = append(out, p)
		}
	}
	return out
}

func (m *Meter) firstLook(uid types.UID, now time.Time) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.first == nil {
		m.first = map[types.UID]time.Time{}
	}
	t, ok := m.first[uid]
	if !ok {
		t = now
		m.first[uid] = t
	}
	return t
}

func (m *Meter) forget(uid types.UID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.first, uid)
}

func (m *Meter) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Meter) wait() time.Duration {
	if m.Wait > 0 {
		return m.Wait
	}
	return DefaultWait
}

func (m *Meter) poll() time.Duration {
	if m.Poll > 0 {
		return m.Poll
	}
	return DefaultPoll
}

func (m *Meter) models() []model.Model {
	if m.Models != nil {
		return m.Models
	}
	return model.Builtins()
}

func (m *Meter) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.DiscardHandler)
}
