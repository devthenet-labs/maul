// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// ProbeResult is what one health probe learned of a credential.
type ProbeResult int

// Probe results.
const (
	// ProbeInconclusive is any answer that is not a verdict on the
	// credential (a network error, a 429, a 5xx, a 403): retried later.
	ProbeInconclusive ProbeResult = iota
	// ProbeValid is the provider accepting the credential.
	ProbeValid
	// ProbeRejected is the provider refusing it as a credential (a 401).
	ProbeRejected
)

// ProbeFunc asks the provider whether credential, of kind, is accepted.
type ProbeFunc func(ctx context.Context, kind v1alpha1.ModelCredentialKind, credential string) ProbeResult

// Health probe defaults.
const (
	// DefaultProbeInterval is the least time between two probes of the
	// same stored credential (the same Secret version): a run's 401s ask
	// for a recheck, and this is how often one is honoured. A new version
	// is probed at once.
	DefaultProbeInterval = 10 * time.Minute
	// DefaultProbeSpacing is the least time between any two probes, so a
	// broker restart or many people storing keys at once never turns into
	// a burst against the provider.
	DefaultProbeSpacing = 2 * time.Second
	// DefaultUsedEvery is the granularity of status.lastUsedAt: at most one
	// write per credential per period.
	DefaultUsedEvery = time.Hour
	// probeRetry and probeAttempts bound an inconclusive probe's retries.
	probeRetry    = time.Minute
	probeAttempts = 5
	// verdictRetry is how soon a refused verdict write is tried again.
	verdictRetry = 5 * time.Second
	// syncRetry is how soon a record queued before the store's first sync
	// is judged again: the informers' initial Add events arrive before the
	// store is marked synced, and dropping them would leave every record
	// stored while the broker was down unjudged until it next changed.
	syncRetry = time.Second
)

// HealthConfig tunes the health probe.
type HealthConfig struct {
	// Probe overrides every route's probe (tests); nil probes each
	// provider's own route (AnthropicProbe, OpenAIProbe).
	Probe ProbeFunc
	// ProbeModel is the model an oauth-token probe asks for one token of;
	// empty takes DefaultProbeModel.
	ProbeModel string
	// Interval, Spacing and UsedEvery override the defaults above.
	Interval, Spacing, UsedEvery time.Duration
}

func (h HealthConfig) withDefaults() HealthConfig {
	if h.Interval <= 0 {
		h.Interval = DefaultProbeInterval
	}
	if h.Spacing <= 0 {
		h.Spacing = DefaultProbeSpacing
	}
	if h.UsedEvery <= 0 {
		h.UsedEvery = DefaultUsedEvery
	}
	if h.ProbeModel == "" {
		h.ProbeModel = DefaultProbeModel
	}
	return h
}

// probeMark is the last probe of one credential.
type probeMark struct {
	version string
	at      time.Time
}

// healthVerdict is a probe's finding not yet written: the Secret version it is
// for, the health found and when, and the credential's fingerprint for
// the audit line. A write refused for a changed record keeps it, so the
// write is retried without probing the provider again.
type healthVerdict struct {
	version, fingerprint string
	health               v1alpha1.ModelCredentialHealth
	at                   time.Time
}

// credentialHealth keeps each of one provider's records' status.health true
// to the stored credential (design revision 1): health comes only from probes the broker
// issues itself, never from agent traffic; any change of the Secret resets
// it to Unknown; and a verdict is written by a patch conditional on the
// record's resourceVersion, and only while the Secret it probed is still at
// the version it probed (re-read live just before the write), so a verdict
// on a replaced credential never lands. A run's 401 asks for a recheck,
// which is throttled. Readiness never depends on any of it.
type credentialHealth struct {
	cfg      HealthConfig
	store    CredentialStore
	provider v1alpha1.ModelCredentialProvider
	now      func() time.Time
	log      *slog.Logger
	wake     chan struct{}

	mu sync.Mutex
	// due is each queued record's not-before time.
	due      map[string]time.Time
	force    map[string]bool
	attempts map[string]int
	probed   map[string]probeMark
	verdicts map[string]healthVerdict
	// used is a pending lastUsedAt write; usedWritten when the last one
	// was queued.
	usedPending map[string]time.Time
	usedWritten map[string]time.Time
	lastProbe   time.Time
}

func newCredentialHealth(cfg HealthConfig, store CredentialStore, provider v1alpha1.ModelCredentialProvider,
	now func() time.Time, log *slog.Logger,
) *credentialHealth {
	h := &credentialHealth{
		cfg: cfg.withDefaults(), store: store, provider: provider, now: now, log: log,
		wake: make(chan struct{}, 1),
		due:  map[string]time.Time{}, force: map[string]bool{}, attempts: map[string]int{},
		probed: map[string]probeMark{}, verdicts: map[string]healthVerdict{},
		usedPending: map[string]time.Time{}, usedWritten: map[string]time.Time{},
	}
	suffix := "-" + string(provider)
	store.Notify(func(record string) {
		// The store notifies every provider's records; each provider's
		// health judges only its own.
		if strings.HasSuffix(record, suffix) {
			h.enqueue(record)
		}
	})
	return h
}

// enqueue asks for record to be judged as soon as it may be.
func (h *credentialHealth) enqueue(record string) {
	h.mu.Lock()
	h.queueLocked(record, h.now())
	h.mu.Unlock()
}

// recheck asks for record's credential to be probed again even if it has
// a health: the provider just refused it on a run's request. It is
// throttled to one probe per Interval per Secret version.
func (h *credentialHealth) recheck(record string) {
	h.mu.Lock()
	h.force[record] = true
	h.queueLocked(record, h.now())
	h.mu.Unlock()
}

// used records that record's credential was just attached to a request;
// lastUsedAt is written at most once per UsedEvery.
func (h *credentialHealth) used(record string) {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if last, ok := h.usedWritten[record]; ok && now.Sub(last) < h.cfg.UsedEvery {
		return
	}
	h.usedWritten[record] = now
	h.usedPending[record] = now
	h.queueLocked(record, now)
}

// queueLocked queues record no later than at.
func (h *credentialHealth) queueLocked(record string, at time.Time) {
	if cur, ok := h.due[record]; !ok || at.Before(cur) {
		h.due[record] = at
	}
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// run judges queued records until ctx ends.
func (h *credentialHealth) run(ctx context.Context) {
	for {
		next := h.step(ctx)
		wait := time.Second
		if !next.IsZero() {
			wait = min(max(next.Sub(h.now()), 10*time.Millisecond), time.Minute)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-h.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// step judges every record that is due, in name order, and returns when
// the next one will be (zero when none is queued).
func (h *credentialHealth) step(ctx context.Context) time.Time {
	now := h.now()
	h.mu.Lock()
	var ready []string
	for name, at := range h.due {
		if !at.After(now) {
			ready = append(ready, name)
			delete(h.due, name)
		}
	}
	h.mu.Unlock()
	slices.Sort(ready)
	for _, name := range ready {
		if after := h.judge(ctx, name); after > 0 {
			h.mu.Lock()
			h.queueLocked(name, h.now().Add(after))
			h.mu.Unlock()
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var next time.Time
	for _, at := range slices.Collect(maps.Values(h.due)) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next
}

// judge brings one record's status up to date: its pending lastUsedAt,
// then, when its health does not speak for the stored credential (or a
// recheck was asked for), a reset to Unknown and a probe. It returns how
// long to wait before judging it again, 0 for not at all.
func (h *credentialHealth) judge(ctx context.Context, name string) time.Duration {
	if !h.store.Synced() {
		return syncRetry
	}
	rec, ok := h.store.Record(name)
	if !ok {
		h.forget(name)
		return 0
	}
	version := rec.ResourceVersion
	if v, ok := h.flushUsed(ctx, name); ok {
		version = v
	}
	secret := secretNameOf(name)
	current, ok := h.store.SecretVersion(secret)
	if !ok {
		return 0
	}
	h.mu.Lock()
	force := h.force[name]
	mark := h.probed[name]
	pending, held := h.verdicts[name]
	if held && pending.version != current {
		delete(h.verdicts, name)
		held = false
	}
	h.mu.Unlock()
	if held {
		return h.write(ctx, name, version, secret, pending)
	}
	if rec.Status.HealthFor(current) != v1alpha1.ModelCredentialUnknown && !force {
		return 0
	}
	now := h.now()
	if mark.version == current && now.Sub(mark.at) < h.cfg.Interval {
		// Probed this very credential lately: a recheck waits its turn.
		return mark.at.Add(h.cfg.Interval).Sub(now)
	}
	h.mu.Lock()
	gap := h.lastProbe.Add(h.cfg.Spacing).Sub(now)
	h.mu.Unlock()
	if gap > 0 {
		return gap
	}
	return h.probe(ctx, rec, version, secret)
}

// probe resets the record's health to Unknown if it speaks for another
// Secret version, probes the stored credential and writes the verdict
// under both conditions.
func (h *credentialHealth) probe(ctx context.Context, rec *v1alpha1.ModelCredential,
	version, secret string,
) time.Duration {
	name := rec.Name
	s, err := h.store.ReadSecret(ctx, secret)
	if err != nil {
		return probeRetry
	}
	if s == nil {
		return 0
	}
	cred, ok := validateSecret(rec.Spec.Owner, h.provider, s)
	if !ok || rec.Spec.Provider != h.provider || !v1alpha1.ModelCredentialKindAllowed(h.provider, rec.Spec.Kind) {
		h.log.LogAttrs(ctx, slog.LevelWarn, "credential not probed: stored credential is malformed",
			slog.String("record", name))
		h.done(name)
		return 0
	}
	probed := s.ResourceVersion
	if rec.Status.SecretVersion != probed && rec.Status.Health != "" &&
		rec.Status.Health != v1alpha1.ModelCredentialUnknown {
		unknown := v1alpha1.ModelCredentialUnknown
		v, err := h.store.PatchStatus(ctx, name, version,
			CredentialStatusPatch{Health: &unknown, SecretVersion: &probed})
		if err != nil {
			h.log.LogAttrs(ctx, slog.LevelInfo, "credential health reset not written; retrying",
				slog.String("record", name), slog.Any("error", err))
			return probeRetry
		}
		version = v
		h.audit(ctx, name, unknown, probed, v1alpha1.ModelCredentialFingerprint([]byte(cred)))
	}
	now := h.now()
	h.mu.Lock()
	h.lastProbe = now
	h.probed[name] = probeMark{version: probed, at: now}
	delete(h.force, name)
	h.mu.Unlock()

	v := healthVerdict{version: probed, at: now, fingerprint: v1alpha1.ModelCredentialFingerprint([]byte(cred))}
	switch h.cfg.Probe(ctx, rec.Spec.Kind, cred) {
	case ProbeValid:
		v.health = v1alpha1.ModelCredentialValid
	case ProbeRejected:
		v.health = v1alpha1.ModelCredentialRejected
	default:
		return h.retry(name)
	}
	return h.write(ctx, name, version, secret, v)
}

// write is the conditional verdict write: the Secret must still be at the
// version probed (re-read live), and the record still at the
// resourceVersion read, so a status-server reset in between is a conflict.
// A refused write keeps the verdict for the next pass.
func (h *credentialHealth) write(ctx context.Context, name, version, secret string, v healthVerdict) time.Duration {
	s, err := h.store.ReadSecret(ctx, secret)
	if err != nil {
		h.hold(name, v)
		return probeRetry
	}
	if s == nil || s.ResourceVersion != v.version {
		// Replaced (or deleted) while probing: judge the new one.
		h.mu.Lock()
		delete(h.probed, name)
		delete(h.verdicts, name)
		h.mu.Unlock()
		return time.Nanosecond
	}
	patch := CredentialStatusPatch{Health: &v.health, SecretVersion: &v.version}
	if v.health == v1alpha1.ModelCredentialRejected {
		patch.LastRejectedAt = &v.at
	}
	if _, err := h.store.PatchStatus(ctx, name, version, patch); err != nil {
		h.log.LogAttrs(ctx, slog.LevelInfo, "credential health not written; retrying",
			slog.String("record", name), slog.Any("error", err))
		h.hold(name, v)
		return verdictRetry
	}
	h.mu.Lock()
	delete(h.verdicts, name)
	h.mu.Unlock()
	h.done(name)
	h.audit(ctx, name, v.health, v.version, v.fingerprint)
	return 0
}

// hold keeps a verdict whose write was refused.
func (h *credentialHealth) hold(name string, v healthVerdict) {
	h.mu.Lock()
	h.verdicts[name] = v
	h.mu.Unlock()
}

// retry counts an inconclusive probe and says when to try again; past
// probeAttempts it gives up until the next change or recheck.
func (h *credentialHealth) retry(name string) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.probed, name)
	h.attempts[name]++
	if h.attempts[name] >= probeAttempts {
		delete(h.attempts, name)
		return 0
	}
	return probeRetry * time.Duration(h.attempts[name])
}

// done clears a record's retry count.
func (h *credentialHealth) done(name string) {
	h.mu.Lock()
	delete(h.attempts, name)
	h.mu.Unlock()
}

// forget drops everything kept for a record that is gone.
func (h *credentialHealth) forget(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.force, name)
	delete(h.attempts, name)
	delete(h.probed, name)
	delete(h.verdicts, name)
	delete(h.usedPending, name)
	delete(h.usedWritten, name)
}

// flushUsed writes a pending lastUsedAt and returns the record's new
// resourceVersion when it wrote one.
func (h *credentialHealth) flushUsed(ctx context.Context, name string) (string, bool) {
	h.mu.Lock()
	at, ok := h.usedPending[name]
	delete(h.usedPending, name)
	h.mu.Unlock()
	if !ok {
		return "", false
	}
	v, err := h.store.PatchStatus(ctx, name, "", CredentialStatusPatch{LastUsedAt: &at})
	if err != nil {
		h.log.LogAttrs(ctx, slog.LevelInfo, "credential lastUsedAt not written",
			slog.String("record", name), slog.Any("error", err))
		return "", false
	}
	return v, true
}

// audit logs one health write: the record, the health, the Secret version
// and the credential's fingerprint, never the credential.
func (h *credentialHealth) audit(ctx context.Context, name string, health v1alpha1.ModelCredentialHealth,
	version, fingerprint string,
) {
	h.log.LogAttrs(ctx, slog.LevelInfo, "credential health",
		slog.String("record", name), slog.String("health", string(health)),
		slog.String("secret_version", version), slog.String("credential", fingerprint))
}

// secretNameOf is the credential Secret's name for a record's.
func secretNameOf(record string) string { return "cred-" + record }
