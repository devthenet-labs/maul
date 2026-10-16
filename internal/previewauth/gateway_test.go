// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// gatewayFlow is one sign-in's tokens at t0: the state, its cookie value,
// the handoff code and the host session it buys.
type gatewayFlow struct {
	state, nonce, code, host string
	st                       State
	handoff                  Handoff
}

func runGatewayFlow(t testing.TB, r *KeyRing, v View, ret string) gatewayFlow {
	t.Helper()
	glt := DefaultGatewayLifetimes()
	st, nonce, err := NewState(t0, glt, v, ret)
	if err != nil {
		t.Fatal(err)
	}
	state, err := r.SealState(st)
	if err != nil {
		t.Fatal(err)
	}
	code, err := r.IssueHandoff(t0, glt, state, st, v, testSession(t))
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.OpenHandoff(code, t0)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := NewHostSession(t0, glt, h)
	if err != nil {
		t.Fatal(err)
	}
	host, err := r.SealHostSession(hs)
	if err != nil {
		t.Fatal(err)
	}
	return gatewayFlow{state: state, nonce: nonce, code: code, host: host, st: st, handoff: h}
}

// sampleGatewayTokens seals one valid token of each gateway kind under r.
func sampleGatewayTokens(t testing.TB, r *KeyRing) map[Kind]string {
	t.Helper()
	f := runGatewayFlow(t, r, liveView("uid-1", "intent-7", 1), "/a?b=c")
	gl, _, err := NewGatewayLogin(t0, DefaultLifetimes(), "intent-7", f.state)
	if err != nil {
		t.Fatal(err)
	}
	login, err := r.SealGatewayLogin(gl)
	if err != nil {
		t.Fatal(err)
	}
	return map[Kind]string{KindHost: f.host, KindState: f.state, KindHandoff: f.code, KindGatewayLogin: login}
}

func TestGatewayHandoff(t *testing.T) {
	r := newRing(t, 1, masterA)
	v := liveView("uid-1", "intent-7", 1)
	f := runGatewayFlow(t, r, v, "/dash?tab=2")
	h := f.handoff
	if h.Label != "intent-7" || h.Slot != 1 || h.UID != "uid-1" || h.Client != ClientID(1) {
		t.Errorf("handoff bound to %+v", h.Bound)
	}
	if h.Return != "/dash?tab=2" || h.RelaySID == "" || h.Sub != r.Sub(ClientID(1), "uid-1", "intent-7", "github:alice") {
		t.Errorf("handoff = %+v", h)
	}
	if h.StateCookie() != StateCookieName(f.state) || !strings.HasPrefix(h.StateCookie(), "__Host-patchy-pv-state-") {
		t.Errorf("state cookie %q, want %q", h.StateCookie(), StateCookieName(f.state))
	}
	if h.Expires-h.IssuedAt != int64(time.Minute/time.Second) {
		t.Errorf("handoff lives %ds", h.Expires-h.IssuedAt)
	}
	if err := h.Redeem("intent-7", f.nonce, v); err != nil {
		t.Errorf("Redeem with the right cookie: %v", err)
	}
	hs, err := r.OpenHostSession(f.host, t0)
	if err != nil {
		t.Fatal(err)
	}
	if hs.Bound != h.Bound || hs.Sub != h.Sub || hs.Identity.Username != "github:alice" ||
		hs.Expires-hs.Start != int64(15*time.Minute/time.Second) {
		t.Errorf("host session = %+v", hs)
	}
	if _, err := r.OpenHostSession(f.host, t0.Add(15*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Errorf("host session at its end: %v, want ErrExpired", err)
	}
	if _, err := r.OpenHandoff(f.code, t0.Add(time.Minute)); !errors.Is(err, ErrExpired) {
		t.Errorf("handoff after a minute: %v, want ErrExpired", err)
	}
	if HandoffKey(h) == CodeKey(Grant{JTI: h.JTI}) {
		t.Error("a handoff's ledger key equals a code's with the same JTI")
	}
}

// TestHandoffFixation: a code reaches a browser that did not start its
// sign-in (no state cookie, another sign-in's cookie, a cookie for another
// state), or another host, or the Preview changed: never redeemable.
func TestHandoffFixation(t *testing.T) {
	r := newRing(t, 1, masterA)
	v := liveView("uid-1", "intent-7", 1)
	attacker := runGatewayFlow(t, r, v, "/")
	victim := runGatewayFlow(t, r, v, "/")
	h := attacker.handoff
	for name, tc := range map[string]struct {
		label, cookie string
		view          View
		want          error
	}{
		"no state cookie":            {"intent-7", "", v, ErrStateMismatch},
		"the victim's own cookie":    {"intent-7", victim.nonce, v, ErrStateMismatch},
		"an oversized cookie":        {"intent-7", strings.Repeat("a", 200), v, ErrStateMismatch},
		"another preview host":       {"other-1", attacker.nonce, v, ErrNotBound},
		"a new Preview at the label": {"intent-7", attacker.nonce, liveView("uid-2", "intent-7", 1), ErrNotBound},
		"another slot":               {"intent-7", attacker.nonce, liveView("uid-1", "intent-7", 0), ErrNotBound},
		"the Preview is gone":        {"intent-7", attacker.nonce, View{Slot: -1}, ErrNoPreview},
	} {
		if err := h.Redeem(tc.label, tc.cookie, tc.view); !errors.Is(err, tc.want) {
			t.Errorf("%s: Redeem = %v, want %v", name, err, tc.want)
		}
	}
	if attacker.handoff.StateCookie() == victim.handoff.StateCookie() {
		t.Error("two sign-ins share a state cookie name")
	}
}

func TestStateStart(t *testing.T) {
	r := newRing(t, 1, masterA)
	v := liveView("uid-1", "intent-7", 1)
	f := runGatewayFlow(t, r, v, "/x")
	st, err := r.OpenState(f.state, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Start("intent-7", v); err != nil {
		t.Errorf("Start: %v", err)
	}
	for name, tc := range map[string]struct {
		label string
		view  View
	}{
		"another label in h": {"other-1", liveView("uid-1", "other-1", 1)},
		"replaced Preview":   {"intent-7", liveView("uid-2", "intent-7", 1)},
		"not live":           {"intent-7", View{UID: "uid-1", Label: "intent-7", Slot: 1}},
	} {
		if err := st.Start(tc.label, tc.view); !errors.Is(err, ErrNoPreview) {
			t.Errorf("%s: Start = %v", name, err)
		}
	}
	if _, err := r.OpenState(f.state, t0.Add(10*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Errorf("state after ten minutes: %v", err)
	}
	if _, _, err := NewState(t0, DefaultGatewayLifetimes(), View{Slot: -1}, "/"); !errors.Is(err, ErrNoPreview) {
		t.Errorf("NewState for no Preview: %v", err)
	}
	refused, _, err := NewState(t0, DefaultGatewayLifetimes(), v, "//evil.example/x")
	if err != nil || refused.Return != "/" {
		t.Errorf("NewState kept a refused return path: %q, %v", refused.Return, err)
	}
}

// TestStateSizeExample pins the finding: a 2048-byte return path of
// "&"-joined query pairs is valid, but JSON writes every "&" as six bytes,
// and it used to seal to over 6000 bytes, which /start refuses. It now
// starts the sign-in at "/" instead.
func TestStateSizeExample(t *testing.T) {
	r := newRing(t, 1, masterA)
	v := liveView(strings.Repeat("u", maxUID), strings.Repeat("l", 63), 1)
	ret := "/?" + strings.Repeat("a=1&", 511) + "a="
	if len(ret) != maxReturn || !ValidReturnPath(ret) {
		t.Fatalf("example is %d bytes, valid %v", len(ret), ValidReturnPath(ret))
	}
	st, _, err := NewState(t0, DefaultGatewayLifetimes(), v, ret)
	if err != nil {
		t.Fatal(err)
	}
	sealedState, err := r.SealState(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealedState) > MaxStateBytes || st.Return != "/" {
		t.Errorf("state %d bytes returning to %.20q", len(sealedState), st.Return)
	}
	// A long return path of plain bytes is kept.
	plain := "/" + strings.Repeat("a", maxReturn-1)
	if st, _, err := NewState(t0, DefaultGatewayLifetimes(), v, plain); err != nil || st.Return != plain {
		t.Errorf("a plain 2048-byte path became %.20q, %v", st.Return, err)
	} else if tok, err := r.SealState(st); err != nil || len(tok) > MaxStateBytes {
		t.Errorf("a plain 2048-byte path seals to %d bytes, %v", len(tok), err)
	}
}

// TestStateSizeProperty: whatever request path a sign-in starts at, its
// sealed state fits under MaxStateBytes, so /start never refuses it as
// oversized; a valid path whose encoding fits is kept as it is.
func TestStateSizeProperty(t *testing.T) {
	r := newRing(t, 1, masterA)
	v := liveView(strings.Repeat("u", maxUID), strings.Repeat("l", 63), 1)
	cfg := quickConfig(500)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		// Heavy in the bytes JSON escapes, up to past the length limit.
		const alpha = "&<>\"/abc=?%"
		b := make([]byte, 1, maxReturn+65)
		b[0] = '/'
		for range rnd.Intn(maxReturn + 64) {
			b = append(b, alpha[rnd.Intn(len(alpha))])
		}
		args[0] = reflect.ValueOf(string(b))
	}
	prop := func(ret string) bool {
		st, _, err := NewState(t0, DefaultGatewayLifetimes(), v, ret)
		if err != nil {
			return false
		}
		tok, err := r.SealState(st)
		if err != nil || len(tok) > MaxStateBytes {
			return false
		}
		if ValidReturnPath(ret) && jsonLen(ret) <= maxReturnJSON {
			return st.Return == ret
		}
		return st.Return == "/"
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestIssueHandoffRefusals(t *testing.T) {
	r := newRing(t, 1, masterA)
	glt := DefaultGatewayLifetimes()
	v := liveView("uid-1", "intent-7", 1)
	st, _, err := NewState(t0, glt, v, "/")
	if err != nil {
		t.Fatal(err)
	}
	state, err := r.SealState(st)
	if err != nil {
		t.Fatal(err)
	}
	old := testSession(t)
	old.ID = ""
	if _, err := r.IssueHandoff(t0, glt, state, st, v, old); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a session without an id issued a code: %v", err)
	}
	if _, err := r.IssueHandoff(t0, glt, state, st, liveView("uid-2", "intent-7", 1), testSession(t)); !errors.Is(err,
		ErrNoPreview) {
		t.Errorf("a replaced Preview issued a code: %v", err)
	}
	if _, err := r.IssueHandoff(t0.Add(13*time.Hour), glt, state, st, v, testSession(t)); !errors.Is(err, ErrExpired) {
		t.Errorf("an ended relay session issued a code: %v", err)
	}
	// A code issued close to the relay session's end ends with it, and so
	// does the host session it buys.
	sess := testSession(t)
	at := time.Unix(sess.Expires-30, 0)
	st2, _, _ := NewState(at, glt, v, "/")
	state2, _ := r.SealState(st2)
	code, err := r.IssueHandoff(at, glt, state2, st2, v, sess)
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.OpenHandoff(code, at)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := NewHostSession(at, glt, h)
	if err != nil {
		t.Fatal(err)
	}
	if h.Expires != sess.Expires || hs.Expires != sess.Expires {
		t.Errorf("code ends %d, host session %d; relay session %d", h.Expires, hs.Expires, sess.Expires)
	}
}

func TestGatewayLogin(t *testing.T) {
	r := newRing(t, 1, masterA)
	f := runGatewayFlow(t, r, liveView("uid-1", "intent-7", 1), "/")
	gl, csrf, err := NewGatewayLogin(t0, DefaultLifetimes(), "intent-7", f.state)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := r.SealGatewayLogin(gl)
	if err != nil {
		t.Fatal(err)
	}
	back, err := r.OpenGatewayLogin(tok, t0)
	if err != nil || back != gl {
		t.Fatalf("round trip: %+v, %v", back, err)
	}
	if back.CheckCSRF(csrf) != nil || back.CheckCSRF("other") == nil || back.CheckCSRF("") == nil {
		t.Error("CheckCSRF does not compare the login cookie")
	}
	if S256(back.UpstreamVerifier) != back.UpstreamChallenge() {
		t.Error("UpstreamChallenge is not S256 of the verifier")
	}
	if TokenKind(tok) != KindGatewayLogin || TokenKind(f.code) != KindHandoff || TokenKind("pa1.z.1.x") != 0 ||
		TokenKind("pa1.l") != 0 || TokenKind("") != 0 {
		t.Error("TokenKind misreads a token")
	}
}

func TestGatewayLifetimes(t *testing.T) {
	if err := DefaultGatewayLifetimes().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*GatewayLifetimes){
		func(l *GatewayLifetimes) { l.State = 0 },
		func(l *GatewayLifetimes) { l.Handoff = 6 * time.Minute },
		func(l *GatewayLifetimes) { l.HostSession = 13 * time.Hour },
		func(l *GatewayLifetimes) { l.HostSession = 90*time.Second + time.Millisecond },
	} {
		l := DefaultGatewayLifetimes()
		edit(&l)
		if l.Validate() == nil {
			t.Errorf("%+v validated", l)
		}
	}
}

func TestViewRoutes(t *testing.T) {
	slot := int32(2)
	p := &v1alpha1.Preview{}
	p.Name, p.UID = "intent-7", types.UID("uid-1")
	p.Spec.HostLabel = "intent-7"
	p.Status.Phase, p.Status.Slot = v1alpha1.PreviewDeploying, &slot
	p.Status.Serving = &v1alpha1.PreviewServing{Generation: 1, UID: "uid-1",
		Components: []v1alpha1.PreviewServingComponent{
			{Name: "web", Service: "preview-intent-7", Path: "/", Port: 80},
			{Name: "api", Service: "preview-intent-7-api", Path: "/api", Port: 80},
		}}
	v := ViewOf(p)
	if len(v.Routes) != 2 {
		t.Fatalf("routes = %+v", v.Routes)
	}
	for path, want := range map[string]string{"/": "preview-intent-7", "/api/v1": "preview-intent-7-api",
		"/apix": "preview-intent-7"} {
		got, ok := v.Pick(path)
		if !ok || got.Service != want {
			t.Errorf("Pick(%q) = %+v, want %s", path, got, want)
		}
	}
	if got := v.Routes[1].Upstream(v.Slot); got != "preview-intent-7-api.patchy-preview-2.svc:80" {
		t.Errorf("Upstream = %s", got)
	}

	for name, edit := range map[string]func(*v1alpha1.Preview){
		"another UID's snapshot": func(p *v1alpha1.Preview) { p.Status.Serving.UID = "uid-0" },
		"no snapshot":            func(p *v1alpha1.Preview) { p.Status.Serving = nil },
		"a bad Service name":     func(p *v1alpha1.Preview) { p.Status.Serving.Components[1].Service = "x.evil" },
		"a bad port":             func(p *v1alpha1.Preview) { p.Status.Serving.Components[0].Port = 0 },
		"a relative path":        func(p *v1alpha1.Preview) { p.Status.Serving.Components[0].Path = "api" },
		"not live":               func(p *v1alpha1.Preview) { p.Status.Phase = v1alpha1.PreviewFailed },
	} {
		q := p.DeepCopy()
		edit(q)
		if got := ViewOf(q).Routes; got != nil {
			t.Errorf("%s: routes %+v", name, got)
		}
	}
	if _, ok := (View{}).Pick("/"); ok {
		t.Error("a View with no routes picked one")
	}
}
