// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/usagewire"
)

// UserCredentialMode is the per-user model credentials switch (design L3,
// chart egressBroker.userCredentials.mode).
type UserCredentialMode string

// User credential modes.
const (
	// UserCredentialsOff is the broker as it was: the org credential pays
	// for every request and no caller pod is read for it. The default.
	UserCredentialsOff UserCredentialMode = "off"
	// UserCredentialsAttribute reads each caller pod's attribution and
	// records the person in the audit line, but the org credential still
	// pays for every request.
	UserCredentialsAttribute UserCredentialMode = "attribute"
	// UserCredentialsEnforce makes a run attributed to a person use that
	// person's own credential or nothing: a request it cannot resolve is
	// refused with a 403 naming why, never sent with the org credential. A
	// run attributed to the org keeps the org credential.
	UserCredentialsEnforce UserCredentialMode = "enforce"
)

// ParseUserCredentialMode reads the switch; empty is off.
func ParseUserCredentialMode(s string) (UserCredentialMode, error) {
	switch m := UserCredentialMode(strings.TrimSpace(s)); m {
	case "", UserCredentialsOff:
		return UserCredentialsOff, nil
	case UserCredentialsAttribute, UserCredentialsEnforce:
		return m, nil
	}
	return "", fmt.Errorf("user credentials mode %q is not %s, %s or %s",
		s, UserCredentialsOff, UserCredentialsAttribute, UserCredentialsEnforce)
}

// on reports whether the mode reads caller pods' attribution at all.
func (m UserCredentialMode) on() bool {
	return m == UserCredentialsAttribute || m == UserCredentialsEnforce
}

// DefaultMaxSecretAge caps how long a person's credential is used from the
// broker's own copy before it is read from the API server again, whatever
// the Secret informer says: revocation reaches the broker within it even if
// the informer's watch has stalled (design revision 1: at most 60 seconds,
// independent of the verdict TTL).
const DefaultMaxSecretAge = time.Minute

// UserCredentialConfig configures per-user model credentials.
type UserCredentialConfig struct {
	// Mode is the switch; the zero value is off.
	Mode UserCredentialMode
	// Store is where the records and Secrets are read from; required
	// unless off.
	Store CredentialStore
	// OrgCredentialIsPersonal acknowledges that the org route's credential
	// is a setup-token (a person's Claude subscription) that will keep
	// paying for every run with no human behind it. Without it, any mode
	// but off is refused while the anthropic route uses one: the
	// subscription guard.
	OrgCredentialIsPersonal bool
	// MaxSecretAge overrides DefaultMaxSecretAge (tests).
	MaxSecretAge time.Duration
	// Health tunes the health probe; its zero value takes the defaults.
	Health HealthConfig
}

// CredentialStore is the port the broker reads people's credentials
// through (design L3). The records and the Secrets' versions come from
// informers, so a request costs no API call; a Secret's data is read live,
// at most once per DefaultMaxSecretAge per credential while its version
// holds. Names are always derived (v1alpha1.ModelCredentialName and
// ModelCredentialSecretName), never taken from a caller.
type CredentialStore interface {
	// Synced reports whether both informers have completed their first
	// list; until then a missing record proves nothing.
	Synced() bool
	// Record is the ModelCredential named name in the release namespace.
	Record(name string) (*v1alpha1.ModelCredential, bool)
	// SecretVersion is the resourceVersion of the credential Secret named
	// name, as the informer last saw it.
	SecretVersion(name string) (string, bool)
	// ReadSecret reads the credential Secret named name from the API
	// server: nil and no error when it does not exist.
	ReadSecret(ctx context.Context, name string) (*corev1.Secret, error)
	// PatchStatus merges patch into the status of the record named name,
	// on the condition that the record is still at resourceVersion when
	// that is not empty (a conflict otherwise), and returns the record's
	// new resourceVersion.
	PatchStatus(ctx context.Context, name, resourceVersion string, patch CredentialStatusPatch) (string, error)
	// Notify registers fn, called with a record's name whenever the record
	// or its Secret is added, changed or deleted.
	Notify(fn func(record string))
}

// CredentialStatusPatch is the record status fields one broker write
// sets; a nil field is left as it is. The broker never writes the
// fingerprint (the status server's, by admission policy).
type CredentialStatusPatch struct {
	Health         *v1alpha1.ModelCredentialHealth
	SecretVersion  *string
	LastUsedAt     *time.Time
	LastRejectedAt *time.Time
}

// The reasons a request is refused under enforce: each distinct, so the
// audit line, the counters and the agent's error say which link of the
// chain broke.
const (
	reasonAttributionUnverified = "attribution_unverified"
	reasonAttributionMissing    = "attribution_missing"
	reasonAttributionInvalid    = "attribution_invalid"
	reasonCredentialsUnsynced   = "credentials_unsynced"
	reasonCredentialMissing     = "credential_missing"
	reasonCredentialMismatch    = "credential_mismatch"
	reasonCredentialUnavailable = "credential_unavailable"
	reasonSecretMissing         = "credential_secret_missing"
	reasonSecretInvalid         = "credential_secret_invalid"
	reasonCredentialRejected    = "credential_rejected"
)

// refusalMessages are the caller-safe explanations, keyed by reason. None
// names a person, a Secret or any part of a credential.
var refusalMessages = map[string]string{
	reasonAttributionUnverified: "the caller pod could not be verified, so whose credential pays is unknown",
	reasonAttributionMissing:    "the caller pod carries no attributed-to annotation",
	reasonAttributionInvalid:    "the caller pod's attributed-to annotation is not in its grammar",
	reasonCredentialsUnsynced:   "the credential store is still loading; retry",
	reasonCredentialMissing:     "the run's attributed person has no model credential registered",
	reasonCredentialMismatch:    "the registered credential record does not match the run's attributed person",
	reasonCredentialUnavailable: "the person's credential could not be read; retry",
	reasonSecretMissing:         "the run's attributed person has no stored credential",
	reasonSecretInvalid:         "the person's stored credential is malformed",
	reasonCredentialRejected:    "the provider rejected the person's credential; they must store a new one",
}

// Audit values of the user attribute when the pod gave no person.
const (
	auditUserUnverified = "unverified"
	auditUserNone       = "none"
	auditUserInvalid    = "invalid"
	// auditCredentialOrg is the credential attribute of a request the org
	// credential paid for.
	auditCredentialOrg = "org"
)

// credFacts is everything the decision reads, gathered by the resolver
// (only as far as the decision needs: nothing past the attribution unless
// enforce names a person).
type credFacts struct {
	mode        UserCredentialMode
	provider    v1alpha1.ModelCredentialProvider
	podVerified bool
	// attribution is the pod's attributed-to annotation, "" when absent.
	attribution string
	synced      bool
	record      *v1alpha1.ModelCredential
	// secretUnavailable is a Secret read the API server did not answer.
	secretUnavailable bool
	// secret is the person's Secret as read, nil when there is none.
	secret *secretFacts
}

// secretFacts is a credential Secret as the decision sees it.
type secretFacts struct {
	version string
	// valid is whether it passed validateSecret; credential is then its
	// value.
	valid       bool
	credential  string
	fingerprint string
}

// credDecision is what a request carries upstream, or why it may not.
type credDecision struct {
	// user is the audit line's user attribute: the attributed-to value, or
	// one of the auditUser* values.
	user string
	// refusal is set when the request must not be forwarded.
	refusal *rejection
	// org is whether the org credential pays; otherwise the person's own,
	// below.
	org        bool
	record     string
	kind       v1alpha1.ModelCredentialKind
	credential string
	// fingerprint is the credential's v1alpha1.ModelCredentialFingerprint,
	// the audit line's credential attribute.
	fingerprint string
}

// decide is the per-user credential decision (design L3, revision 1),
// pure so its invariants are properties: off and attribute always pay with
// the org credential and never refuse; enforce pays for an org-attributed
// run with the org credential and for a person's with that person's own
// valid, unrejected credential, and otherwise refuses with a distinct
// reason, never falling back to the org's.
func decide(f credFacts) credDecision {
	d := credDecision{user: auditUser(f), org: true}
	if f.mode != UserCredentialsEnforce {
		return d
	}
	refuse := func(reason string) credDecision {
		status := http.StatusForbidden
		kind := "permission_error"
		if reason == reasonCredentialsUnsynced || reason == reasonCredentialUnavailable {
			status, kind = http.StatusServiceUnavailable, "api_error"
		}
		d.org, d.refusal = false, &rejection{status: status, kind: kind, reason: reason,
			msg: fmt.Sprintf("egress broker: %s (%s)", refusalMessages[reason], reason)}
		return d
	}
	if !f.podVerified {
		return refuse(reasonAttributionUnverified)
	}
	if f.attribution == "" {
		return refuse(reasonAttributionMissing)
	}
	id, err := attribution.ParseValue(f.attribution)
	switch {
	case err != nil:
		return refuse(reasonAttributionInvalid)
	case id == 0:
		return d
	case !f.synced:
		return refuse(reasonCredentialsUnsynced)
	}
	owner := v1alpha1.ModelCredentialOwner(id)
	r := f.record
	switch {
	case r == nil:
		return refuse(reasonCredentialMissing)
	case r.Name != v1alpha1.ModelCredentialName(owner, f.provider) || r.Spec.Owner != owner ||
		r.Spec.Provider != f.provider || !knownKind(r.Spec.Kind):
		return refuse(reasonCredentialMismatch)
	case f.secretUnavailable:
		return refuse(reasonCredentialUnavailable)
	case f.secret == nil:
		return refuse(reasonSecretMissing)
	case !f.secret.valid:
		return refuse(reasonSecretInvalid)
	case r.Status.HealthFor(f.secret.version) == v1alpha1.ModelCredentialRejected:
		return refuse(reasonCredentialRejected)
	}
	d.org, d.record, d.kind = false, r.Name, r.Spec.Kind
	d.credential, d.fingerprint = f.secret.credential, f.secret.fingerprint
	return d
}

// auditUser is whom the audit line names: the pod's attribution when it is
// verified and in the grammar, else why there is none; "" while off.
func auditUser(f credFacts) string {
	switch {
	case !f.mode.on():
		return ""
	case !f.podVerified:
		return auditUserUnverified
	case f.attribution == "":
		return auditUserNone
	}
	if _, err := attribution.ParseValue(f.attribution); err != nil {
		return auditUserInvalid
	}
	return f.attribution
}

// knownKind reports whether kind is one the broker can attach.
func knownKind(kind v1alpha1.ModelCredentialKind) bool {
	return kind == v1alpha1.ModelCredentialAPIKey || kind == v1alpha1.ModelCredentialOAuthToken
}

// validateSecret holds a credential Secret to the store's contract before
// any byte of it is used: named for owner and provider, labelled with the
// owner's hash (so a Secret copied or renamed into place under another
// person's name is refused), Opaque, exactly the one data key, and a value
// that, trimmed of surrounding whitespace, is 1 to
// ModelCredentialSecretMaxBytes of printable ASCII with no space (a
// credential is a single token: nothing in it can split or inject a
// header). It returns the trimmed credential.
func validateSecret(owner string, provider v1alpha1.ModelCredentialProvider, s *corev1.Secret) (string, bool) {
	if s == nil || s.Name != v1alpha1.ModelCredentialSecretName(owner, provider) ||
		s.Labels[v1alpha1.LabelCredentialOwner] != v1alpha1.ModelCredentialOwnerHash(owner) ||
		(s.Type != "" && s.Type != corev1.SecretTypeOpaque) || len(s.Data) != 1 {
		return "", false
	}
	raw, ok := s.Data[v1alpha1.ModelCredentialSecretKey]
	if !ok || len(raw) > v1alpha1.ModelCredentialSecretMaxBytes {
		return "", false
	}
	cred := strings.TrimSpace(string(raw))
	if cred == "" {
		return "", false
	}
	for i := 0; i < len(cred); i++ {
		if c := cred[i]; c < 0x21 || c > 0x7e {
			return "", false
		}
	}
	return cred, true
}

// cachedSecret is the broker's copy of one person's validated credential.
type cachedSecret struct {
	version string
	fetched time.Time
	facts   secretFacts
}

// userCredentials resolves each request's credential (design L3): the
// pod's attribution (read with the verdict), then under enforce the
// person's record and Secret, keeping a Secret only while the informer
// reports the version it was read at and for at most maxAge.
type userCredentials struct {
	mode     UserCredentialMode
	provider v1alpha1.ModelCredentialProvider
	store    CredentialStore
	maxAge   time.Duration
	health   *credentialHealth
	now      func() time.Time

	mu      sync.Mutex
	secrets map[string]cachedSecret
}

// resolve decides one request's credential from its caller's identity.
func (u *userCredentials) resolve(ctx context.Context, id identity) credDecision {
	f := credFacts{mode: u.mode, provider: u.provider, podVerified: id.podVerified, attribution: id.attributedTo}
	if u.mode == UserCredentialsEnforce && id.podVerified {
		if gid, err := attribution.ParseValue(id.attributedTo); err == nil && gid > 0 {
			u.gather(ctx, v1alpha1.ModelCredentialOwner(gid), &f)
		}
	}
	d := decide(f)
	if !d.org && d.refusal == nil {
		u.health.used(d.record)
	}
	return d
}

// gather reads the person's record and Secret into f.
func (u *userCredentials) gather(ctx context.Context, owner string, f *credFacts) {
	if f.synced = u.store.Synced(); !f.synced {
		return
	}
	rec, ok := u.store.Record(v1alpha1.ModelCredentialName(owner, u.provider))
	if !ok {
		return
	}
	f.record = rec
	f.secret, f.secretUnavailable = u.secret(ctx, owner)
}

// secret returns the person's Secret facts: the cached copy while the
// informer still reports its version and it is younger than maxAge, else a
// live read. (nil, false) is no Secret; (nil, true) a read that failed.
func (u *userCredentials) secret(ctx context.Context, owner string) (*secretFacts, bool) {
	name := v1alpha1.ModelCredentialSecretName(owner, u.provider)
	version, ok := u.store.SecretVersion(name)
	now := u.now()
	u.mu.Lock()
	c, cached := u.secrets[name]
	if !ok {
		delete(u.secrets, name)
	}
	u.mu.Unlock()
	if !ok {
		return nil, false
	}
	if cached && c.version == version && now.Sub(c.fetched) < u.maxAge {
		f := c.facts
		return &f, false
	}
	s, err := u.store.ReadSecret(ctx, name)
	switch {
	case err != nil:
		return nil, true
	case s == nil:
		u.mu.Lock()
		delete(u.secrets, name)
		u.mu.Unlock()
		return nil, false
	}
	f := secretFacts{version: s.ResourceVersion}
	f.credential, f.valid = validateSecret(owner, u.provider, s)
	if f.valid {
		f.fingerprint = v1alpha1.ModelCredentialFingerprint([]byte(f.credential))
	}
	u.mu.Lock()
	u.secrets[name] = cachedSecret{version: s.ResourceVersion, fetched: now, facts: f}
	u.mu.Unlock()
	return &f, false
}

// attached is the person's credential a request carries in its context to
// the route's CredentialFunc.
type attached struct {
	kind       v1alpha1.ModelCredentialKind
	credential string
}

type attachedKey struct{}

// withAttached puts a person's credential in ctx for the anthropic route's
// CredentialFunc to send instead of the org credential.
func withAttached(ctx context.Context, a *attached) context.Context {
	return context.WithValue(ctx, attachedKey{}, a)
}

// attachedFrom is the person's credential a request carries, nil for the
// org credential.
func attachedFrom(ctx context.Context) *attached {
	a, _ := ctx.Value(attachedKey{}).(*attached)
	return a
}

// usageKind is the usagewire credential kind of a person's credential.
func usageKind(kind v1alpha1.ModelCredentialKind) string {
	if kind == v1alpha1.ModelCredentialOAuthToken {
		return usagewire.CredentialOAuthToken
	}
	return usagewire.CredentialAPIKey
}

// validate rejects a per-user credential config the broker cannot serve:
// any mode but off needs a store and exactly the anthropic route (v1
// carries Anthropic API keys and setup-tokens only; Bedrock, Vertex and
// Foundry use the broker's one cloud identity), and the subscription guard
// refuses a setup-token org credential unless it is acknowledged.
func (u UserCredentialConfig) validate(c Config) error {
	switch u.Mode {
	case UserCredentialsOff:
		return nil
	case UserCredentialsAttribute, UserCredentialsEnforce:
	default:
		return fmt.Errorf("broker: user credentials mode %q is not off, attribute or enforce", u.Mode)
	}
	if u.Store == nil {
		return errors.New("broker: per-user credentials need a credential store")
	}
	org, ok := c.Upstreams["anthropic"]
	if !ok || len(c.Upstreams) != 1 {
		return errors.New("broker: per-user credentials are Anthropic API keys and setup-tokens only; " +
			"the anthropic route must be the only route")
	}
	if org.CredentialKind == usagewire.CredentialOAuthToken && !u.OrgCredentialIsPersonal {
		return errors.New("broker: per-user credentials are refused while the org credential is a " +
			"setup-token (a person's Claude subscription would pay for every run with no human behind it); " +
			"use an org API key, or acknowledge it with --org-credential-is-personal=acknowledged")
	}
	return nil
}
