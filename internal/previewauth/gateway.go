// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/bitwise-media-group/patchy/internal/sealed"
)

// The preview gateway's sign-in (design §2, Revision 1): the gateway is its
// own relying party. A preview host with no host session seals a State, sets
// a state cookie named for it and sends the browser to the sign-in host's
// /start. The sign-in host, once it has a relay session and the access review
// passes, issues a single-use Handoff code bound to the state and sends the
// browser to the preview host's /.patchy/callback, which spends it for a
// HostSession on that host only.

const (
	// HostSessionCookie is a preview host's session cookie. __Host- pins it
	// to the one preview host that set it.
	HostSessionCookie = "__Host-patchy-pv"
	// stateCookiePrefix starts every state cookie's name; StateCookieName
	// adds a hash of the sealed state, so each sign-in in flight has its own.
	stateCookiePrefix = "__Host-patchy-pv-state-"
	// ReservedPrefix is the path prefix every preview host keeps for the
	// gateway. Nothing under it is ever forwarded to a preview.
	ReservedPrefix = "/.patchy/"
	// HandoffPath is the one path a handoff code is ever sent to.
	HandoffPath = ReservedPrefix + "callback"
	// HealthPath answers on every host, for the edge's health check.
	HealthPath = ReservedPrefix + "healthz"
	// maxReturn bounds a return path.
	maxReturn = 2048
)

// GatewayLifetimes are the gateway's token lifetimes.
type GatewayLifetimes struct {
	// State is how long a preview host's sign-in may take, end to end.
	State time.Duration
	// Handoff is a handoff code's lifetime; codes are also single-use.
	Handoff time.Duration
	// HostSession is a preview host session's absolute lifetime, and so
	// how long a change to the viewer's IdP groups can take to apply.
	HostSession time.Duration
}

// DefaultGatewayLifetimes are the design's defaults.
func DefaultGatewayLifetimes() GatewayLifetimes {
	return GatewayLifetimes{State: 10 * time.Minute, Handoff: time.Minute, HostSession: 15 * time.Minute}
}

// Validate keeps every lifetime whole seconds and within bounds.
func (l GatewayLifetimes) Validate() error {
	checks := []struct {
		name     string
		v        time.Duration
		min, max time.Duration
	}{
		{"state", l.State, time.Minute, 30 * time.Minute},
		{"handoff", l.Handoff, 10 * time.Second, 5 * time.Minute},
		{"host session", l.HostSession, time.Minute, 12 * time.Hour},
	}
	for _, c := range checks {
		if c.v < c.min || c.v > c.max || c.v%time.Second != 0 {
			return fmt.Errorf("previewauth: %s lifetime %s is not whole seconds in [%s, %s]", c.name, c.v, c.min, c.max)
		}
	}
	return nil
}

// State is what a preview host seals into the s parameter it sends to the
// sign-in host: which Preview, the hash of the state cookie's random value,
// and the path to return to.
type State struct {
	V         int    `json:"v"`
	Label     string `json:"lbl"`
	UID       string `json:"uid"`
	NonceHash string `json:"nh"`
	Return    string `json:"ret"`
	IssuedAt  int64  `json:"iat"`
	Expires   int64  `json:"exp"`
}

func (s State) valid() error {
	if s.V != payloadVersion || !ValidLabel(s.Label) || s.UID == "" || len(s.UID) > maxUID ||
		len(s.NonceHash) != 64 || s.Expires <= s.IssuedAt || !ValidReturnPath(s.Return) {
		return ErrInvalidToken
	}
	return nil
}

// NewState starts a sign-in for the live Preview v at the request path ret
// (refused paths become "/"). It returns the state to seal and the random
// value for its state cookie; the state keeps only the value's hash.
func NewState(now time.Time, lt GatewayLifetimes, v View, ret string) (State, string, error) {
	if !v.Live || !ValidLabel(v.Label) || v.UID == "" {
		return State{}, "", ErrNoPreview
	}
	nonce, err := sealed.RandomToken()
	if err != nil {
		return State{}, "", err
	}
	if !ValidReturnPath(ret) {
		ret = "/"
	}
	s := State{V: payloadVersion, Label: v.Label, UID: v.UID, NonceHash: csrfHash(nonce), Return: ret,
		IssuedAt: now.Unix(), Expires: now.Add(lt.State).Unix()}
	if err := s.valid(); err != nil {
		return State{}, "", err
	}
	return s, nonce, nil
}

// StateCookieName is the state cookie of one sealed state: the prefix plus
// the first 16 hex characters of its SHA-256, the LoginCookieName
// construction. Two sign-ins in two tabs set two cookies.
func StateCookieName(sealedState string) string { return stateCookieName(stateHash(sealedState)) }

func stateCookieName(hash string) string { return stateCookiePrefix + hash[:16] }

// stateHash is the hex SHA-256 of a sealed state.
func stateHash(sealedState string) string {
	sum := sha256.Sum256([]byte(sealedState))
	return hex.EncodeToString(sum[:])
}

// SealState seals a preview host's sign-in state.
func (r *KeyRing) SealState(s State) (string, error) {
	if err := s.valid(); err != nil {
		return "", err
	}
	return r.seal(KindState, s)
}

// OpenState opens a sign-in state that has not expired at now.
func (r *KeyRing) OpenState(token string, now time.Time) (State, error) {
	var s State
	if err := r.open(KindState, token, &s); err != nil {
		return State{}, err
	}
	if s.valid() != nil {
		return State{}, ErrInvalidToken
	}
	return s, live(now, s.Expires)
}

// Start is the sign-in host's check of a /start request: the state names the
// label in h and the same Preview (UID) that is live there now.
func (s State) Start(label string, v View) error {
	if s.Label != label || !v.Live || v.Label != label || v.UID != s.UID || v.Slot < 0 || v.Slot >= MaxSlots {
		return ErrNoPreview
	}
	return nil
}

// GatewayLogin is the sealed state parameter of a gateway sign-in through
// Dex: the preview host's sealed state to resume afterwards, the hash of the
// login cookie's CSRF value, and the relay's PKCE verifier and nonce for Dex.
type GatewayLogin struct {
	V                int    `json:"v"`
	IssuedAt         int64  `json:"iat"`
	Expires          int64  `json:"exp"`
	Label            string `json:"lbl"`
	State            string `json:"st"`
	CSRFHash         string `json:"csrf"`
	UpstreamNonce    string `json:"un"`
	UpstreamVerifier string `json:"uv"`
}

func (g GatewayLogin) valid() error {
	if g.V != payloadVersion || g.Expires <= g.IssuedAt || !ValidLabel(g.Label) || g.State == "" ||
		len(g.State) > MaxStateBytes || len(g.CSRFHash) != 64 || g.UpstreamNonce == "" ||
		!validVerifier(g.UpstreamVerifier) {
		return ErrInvalidToken
	}
	return nil
}

// NewGatewayLogin starts a sign-in through Dex for the /start request with
// the sealed state sealedState for label. It returns the login to seal and
// the random CSRF value for the login cookie.
func NewGatewayLogin(now time.Time, lt Lifetimes, label, sealedState string) (GatewayLogin, string, error) {
	csrf, err := sealed.RandomToken()
	if err != nil {
		return GatewayLogin{}, "", err
	}
	nonce, err := sealed.RandomToken()
	if err != nil {
		return GatewayLogin{}, "", err
	}
	verifier, err := sealed.RandomToken()
	if err != nil {
		return GatewayLogin{}, "", err
	}
	g := GatewayLogin{V: payloadVersion, IssuedAt: now.Unix(), Expires: now.Add(lt.Login).Unix(), Label: label,
		State: sealedState, CSRFHash: csrfHash(csrf), UpstreamNonce: nonce, UpstreamVerifier: verifier}
	if err := g.valid(); err != nil {
		return GatewayLogin{}, "", err
	}
	return g, csrf, nil
}

// CheckCSRF compares the login cookie's value with the login's hash of it.
func (g GatewayLogin) CheckCSRF(cookie string) error {
	return LoginState{CSRFHash: g.CSRFHash}.CheckCSRF(cookie)
}

// UpstreamChallenge is the PKCE S256 challenge sent to Dex.
func (g GatewayLogin) UpstreamChallenge() string { return S256(g.UpstreamVerifier) }

// SealGatewayLogin seals a gateway sign-in's login state.
func (r *KeyRing) SealGatewayLogin(g GatewayLogin) (string, error) {
	if err := g.valid(); err != nil {
		return "", err
	}
	return r.seal(KindGatewayLogin, g)
}

// OpenGatewayLogin opens a gateway login state that has not expired at now.
func (r *KeyRing) OpenGatewayLogin(token string, now time.Time) (GatewayLogin, error) {
	var g GatewayLogin
	if err := r.open(KindGatewayLogin, token, &g); err != nil {
		return GatewayLogin{}, err
	}
	if g.valid() != nil {
		return GatewayLogin{}, ErrInvalidToken
	}
	return g, live(now, g.Expires)
}

// Handoff is a handoff code's payload: the binding, the state it answers
// (its hash names the state cookie, and the state's nonce hash is what the
// cookie must hash to), the relay session that issued it, the viewer and the
// path to return to. Only the gateway ever holds it: the browser carries it
// sealed from the sign-in host to the preview host's /.patchy/callback.
type Handoff struct {
	V int `json:"v"`
	Bound
	JTI        string   `json:"jti"`
	IssuedAt   int64    `json:"iat"`
	Expires    int64    `json:"exp"`
	StateHash  string   `json:"sh"`
	NonceHash  string   `json:"nh"`
	RelaySID   string   `json:"rsid"`
	Sub        string   `json:"sub"`
	Identity   Identity `json:"id"`
	SessionEnd int64    `json:"abs"`
	Return     string   `json:"ret"`
}

func (h Handoff) valid() error {
	if h.V != payloadVersion || h.JTI == "" || h.Expires <= h.IssuedAt || h.Expires > h.SessionEnd ||
		len(h.StateHash) != 64 || len(h.NonceHash) != 64 || h.RelaySID == "" || h.Sub == "" ||
		!ValidReturnPath(h.Return) {
		return ErrInvalidToken
	}
	if err := h.validBound(); err != nil {
		return err
	}
	if h.Identity.Validate() != nil {
		return ErrInvalidToken
	}
	return nil
}

// IssueHandoff issues a handoff code for the /start request whose sealed
// state sealedState opened as st, for the live Preview v, from the relay
// session sess whose viewer's access review passed. The code expires after
// lt.Handoff, or when the relay session ends if that is sooner.
func (r *KeyRing) IssueHandoff(now time.Time, lt GatewayLifetimes, sealedState string, st State, v View,
	sess Session) (string, error) {
	if err := st.Start(st.Label, v); err != nil {
		return "", err
	}
	if sess.valid() != nil || sess.ID == "" {
		return "", ErrInvalidToken
	}
	if err := live(now, sess.Expires); err != nil {
		return "", err
	}
	jti, err := sealed.RandomToken()
	if err != nil {
		return "", err
	}
	b := Bound{Client: ClientID(v.Slot), Slot: v.Slot, UID: v.UID, Label: v.Label}
	h := Handoff{V: payloadVersion, Bound: b, JTI: jti, IssuedAt: now.Unix(),
		Expires: min(now.Add(lt.Handoff).Unix(), sess.Expires), StateHash: stateHash(sealedState),
		NonceHash: st.NonceHash, RelaySID: sess.ID, Sub: r.Sub(b.Client, b.UID, b.Label, sess.Identity.Username),
		Identity: sess.Identity, SessionEnd: sess.Expires, Return: st.Return}
	return r.SealHandoff(h)
}

// SealHandoff seals a handoff code.
func (r *KeyRing) SealHandoff(h Handoff) (string, error) {
	if err := h.valid(); err != nil {
		return "", err
	}
	return r.seal(KindHandoff, h)
}

// OpenHandoff opens a handoff code that has not expired at now.
func (r *KeyRing) OpenHandoff(token string, now time.Time) (Handoff, error) {
	var h Handoff
	if err := r.open(KindHandoff, token, &h); err != nil {
		return Handoff{}, err
	}
	if h.valid() != nil {
		return Handoff{}, ErrInvalidToken
	}
	return h, live(now, h.Expires)
}

// ErrStateMismatch means a handoff code reached a browser that did not start
// its sign-in: the state cookie it names is missing or holds another value.
// This is the login-CSRF and session-fixation defence.
var ErrStateMismatch = errors.New("previewauth: sign-in was not started by this browser on this host")

// StateCookie is the name of the state cookie a handoff code answers.
func (h Handoff) StateCookie() string { return stateCookieName(h.StateHash) }

// Redeem is the preview host's check of a handoff code before it spends it:
// the code was issued for this host's label, the state cookie it names holds
// the value whose hash the state carried, and the Preview is still the same
// one, live in the same slot.
func (h Handoff) Redeem(label, stateCookie string, v View) error {
	if h.Label != label {
		return ErrNotBound
	}
	if stateCookie == "" || len(stateCookie) > 128 ||
		subtle.ConstantTimeCompare([]byte(csrfHash(stateCookie)), []byte(h.NonceHash)) != 1 {
		return ErrStateMismatch
	}
	return h.Matches(v)
}

// HandoffKey is the single-use ledger key of an opened handoff code, kept
// apart from CodeKey's by its prefix.
func HandoffKey(h Handoff) [32]byte { return sha256.Sum256([]byte("handoff|" + h.JTI)) }

// HostSession is a preview host's session: the binding, the viewer and their
// pairwise subject, and its absolute end.
type HostSession struct {
	V int `json:"v"`
	Bound
	ID       string   `json:"jti"`
	Start    int64    `json:"ss"`
	Expires  int64    `json:"abs"`
	Sub      string   `json:"sub"`
	Identity Identity `json:"id"`
}

func (s HostSession) valid() error {
	if s.V != payloadVersion || s.ID == "" || s.Expires <= s.Start || s.Sub == "" {
		return ErrInvalidToken
	}
	if err := s.validBound(); err != nil {
		return err
	}
	if s.Identity.Validate() != nil {
		return ErrInvalidToken
	}
	return nil
}

// NewHostSession starts the host session a redeemed handoff code buys: it
// ends after lt.HostSession, or when the relay session that issued the code
// ends if that is sooner.
func NewHostSession(now time.Time, lt GatewayLifetimes, h Handoff) (HostSession, error) {
	if h.valid() != nil {
		return HostSession{}, ErrInvalidToken
	}
	id, err := sealed.RandomToken()
	if err != nil {
		return HostSession{}, err
	}
	s := HostSession{V: payloadVersion, Bound: h.Bound, ID: id, Start: now.Unix(),
		Expires: min(now.Add(lt.HostSession).Unix(), h.SessionEnd), Sub: h.Sub, Identity: h.Identity}
	if err := s.valid(); err != nil {
		return HostSession{}, err
	}
	if err := live(now, s.Expires); err != nil {
		return HostSession{}, err
	}
	return s, nil
}

// SealHostSession seals a preview host's session cookie. An identity too
// large for a cookie is ErrIdentityTooLarge.
func (r *KeyRing) SealHostSession(s HostSession) (string, error) {
	if err := s.valid(); err != nil {
		return "", err
	}
	tok, err := r.seal(KindHost, s)
	if err != nil {
		return "", err
	}
	if len(tok) > MaxSessionBytes {
		return "", ErrIdentityTooLarge
	}
	return tok, nil
}

// OpenHostSession opens a host session that has not ended at now.
func (r *KeyRing) OpenHostSession(token string, now time.Time) (HostSession, error) {
	if len(token) > MaxSessionBytes {
		return HostSession{}, ErrInvalidToken
	}
	var s HostSession
	if err := r.open(KindHost, token, &s); err != nil {
		return HostSession{}, err
	}
	if s.valid() != nil {
		return HostSession{}, ErrInvalidToken
	}
	return s, live(now, s.Expires)
}
