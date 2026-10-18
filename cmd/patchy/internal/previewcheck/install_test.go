// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"golang.org/x/net/publicsuffix"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/controller/preview"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// seed makes every property here deterministic.
const seed = 20261017

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// The install world: previews under their own registrable domain, sign-in
// and Dex on another.
const (
	iSuffix    = "acme-previews.test"
	iIssuer    = "https://preview-auth.acme.test"
	iDex       = "https://dex.acme.test"
	iGateway   = "192.0.2.10"
	iTaint     = "patchy.bitwisemedia.uk/preview-only"
	iNodeLabel = "patchy.bitwisemedia.uk/previews"
)

// iWeb answers requests by host and path.
type iWeb map[string]func(*http.Request) (int, http.Header, string)

func (w iWeb) Do(req *http.Request) (*http.Response, error) {
	f, ok := w[req.URL.Host+req.URL.Path]
	if !ok {
		return nil, fmt.Errorf("dial tcp: lookup %s: no such host", req.URL.Host)
	}
	status, h, body := f(req)
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
}

// iResolver resolves from a map; anything else is NXDOMAIN.
type iResolver map[string][]string

func (r iResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if a, ok := r[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// gatewayPage is a page the gateway renders itself.
func gatewayPage(status int, body string) (int, http.Header, string) {
	h := http.Header{}
	h.Set("Content-Security-Policy", previewauth.PageCSP)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	return status, h, body
}

// installWorld is a gateway-only install that passes every check.
type installWorld struct {
	objs     []client.Object
	web      iWeb
	resolver iResolver
	// cnames, when set, also answers CNAME lookups (tunnel_test.go).
	cnames map[string]string
	reads  *[]string
	// dial, when set, answers TLS dials instead of a certificate for
	// *.<suffix> everywhere.
	dial func(context.Context, string) (tls.ConnectionState, error)
}

func newInstallWorld() *installWorld {
	newPreview := func(name, project string, phase v1alpha1.PreviewPhase, slot int32) *v1alpha1.Preview {
		return &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
			Spec:   v1alpha1.PreviewSpec{Project: project, HostLabel: name},
			Status: v1alpha1.PreviewStatus{Phase: phase, Slot: &slot}}
	}
	relay := configMap("patchy-preview-auth-config", PreviewAuth, map[string]string{
		keyAuthIssuer: iIssuer, keyAuthDexIssuer: iDex, keyAuthDexClientID: "patchy-preview-auth",
		keyAuthGateway: "true", keyAuthHostSuffix: iSuffix,
		keyAuthUsernamePrefix: "github:", keyAuthGroupsPrefix: "github:",
	})
	w := &installWorld{
		objs: []client.Object{
			configMap("patchy-preview-controller", PreviewController, map[string]string{
				keyPreviewHostSuffix: iSuffix, keyPreviewLegacyIngress: "false",
				keyPreviewNodeSelector: `{"` + iNodeLabel + `":"true"}`, keyPreviewTaintKey: iTaint,
			}),
			relay,
			&networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "patchy-preview-gateway"},
				Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{
					Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: iGateway}},
				}},
			},
			newPreview("shop-7", "shop", v1alpha1.PreviewReady, 1),
			newPreview("blog-3", "blog", v1alpha1.PreviewReady, 2),
			newPreview("shop-8", "shop", v1alpha1.PreviewDeploying, 3),
			&rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "patchy-preview-viewers"},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "patchy-preview-viewer"},
				Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "github:acme:qa"},
					{Kind: rbacv1.UserKind, Name: "github:octocat"}},
			},
			node("preview-1", map[string]string{iNodeLabel: "true"}, taint(iTaint, "true", corev1.TaintEffectNoExecute)),
			node("worker-1", nil),
		},
		resolver: iResolver{ProbeProject + "-0." + iSuffix: {iGateway}},
		reads:    &[]string{},
	}
	redirect := func(label string) func(*http.Request) (int, http.Header, string) {
		return func(*http.Request) (int, http.Header, string) {
			return http.StatusFound, http.Header{"Location": {iIssuer + "/start?h=" + label + "&s=pa1.t.1.sealed"}}, ""
		}
	}
	w.web = iWeb{
		"preview-auth.acme.test/.well-known/openid-configuration": func(*http.Request) (int, http.Header, string) {
			return http.StatusOK, nil, `{"issuer":"` + iIssuer + `","authorization_endpoint":"` + iIssuer + `/authorize"}`
		},
		"dex.acme.test/.well-known/openid-configuration": func(*http.Request) (int, http.Header, string) {
			return http.StatusOK, nil, `{"issuer":"` + iDex + `","authorization_endpoint":"` + iDex + `/auth"}`
		},
		"dex.acme.test/auth": func(*http.Request) (int, http.Header, string) {
			return http.StatusFound, http.Header{"Location": {"https://github.com/login/oauth/authorize"}}, ""
		},
		ProbeProject + "-0." + iSuffix + "/": func(*http.Request) (int, http.Header, string) {
			return gatewayPage(http.StatusNotFound, "<title>"+previewauth.NoPreviewTitle+"</title>")
		},
		"shop-7." + iSuffix + "/": redirect("shop-7"),
		"blog-3." + iSuffix + "/": redirect("blog-3"),
	}
	return w
}

func node(name string, labels map[string]string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: corev1.NodeSpec{Taints: taints}}
}

func taint(key, value string, effect corev1.TaintEffect) corev1.Taint {
	return corev1.Taint{Key: key, Value: value, Effect: effect}
}

// setting overrides one setting of the ConfigMap named name.
func (w *installWorld) setting(name, key, value string) {
	for _, o := range w.objs {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == name {
			if value == "" {
				delete(cm.Data, key)
			} else {
				cm.Data[key] = value
			}
			return
		}
	}
	panic("no ConfigMap " + name)
}

// drop removes the object of the given type and name.
func (w *installWorld) drop(name string, kind client.Object) {
	out := w.objs[:0]
	for _, o := range w.objs {
		if o.GetName() == name && reflect.TypeOf(o) == reflect.TypeOf(kind) {
			continue
		}
		out = append(out, o)
	}
	w.objs = out
}

// run checks the install, failing the test on any Secret read; fn
// intercepts reads first when given.
func (w *installWorld) run(t *testing.T, fn interceptor.Funcs) Report {
	t.Helper()
	get, list := fn.Get, fn.List
	fn.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Secret); ok {
			t.Errorf("check previews read Secret %s", key)
		}
		if get != nil {
			return get(ctx, c, key, obj, opts...)
		}
		return c.Get(ctx, key, obj, opts...)
	}
	fn.List = func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		if _, ok := l.(*corev1.SecretList); ok {
			t.Error("check previews listed Secrets")
		}
		if list != nil {
			return list(ctx, c, l, opts...)
		}
		return c.List(ctx, l, opts...)
	}
	reader := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(w.objs...).
		WithInterceptorFuncs(fn).Build()
	dial := iDial
	if w.dial != nil {
		dial = w.dial
	}
	var res Resolver = w.resolver
	if w.cnames != nil {
		res = cnameWorld{iResolver: w.resolver, cnames: w.cnames}
	}
	return Install(context.Background(), Config{Reader: reader, Namespace: "patchy", Resolver: res,
		DialTLS: dial, HTTP: w.web})
}

// iDial answers every TLS dial with a certificate for *.<suffix>.
func iDial(context.Context, string) (tls.ConnectionState, error) {
	return tls.ConnectionState{PeerCertificates: []*x509.Certificate{{DNSNames: []string{"*." + iSuffix},
		Issuer: pkix.Name{CommonName: "Test CA"}, NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}}}, nil
}

// TestInstallAllPass: a gateway-only install with viewers and an isolated
// preview node passes every check, in the report's order; every Ready
// Preview is probed whatever its Project, and the probe host is the relay's
// own.
func TestInstallAllPass(t *testing.T) {
	r := newInstallWorld().run(t, interceptor.Funcs{})
	if n := r.Failed(); n != 0 {
		t.Fatalf("Failed = %d:\n%s", n, dump(r.Checks))
	}
	var order []string
	for _, l := range r.Checks {
		if l.Status != checkreport.Pass && l.Name != CheckPreviewGatewayAllowlist {
			t.Errorf("%s = %s: %s", l.Name, l.Status, l.Reason)
		}
		if len(order) == 0 || order[len(order)-1] != l.Name {
			order = append(order, l.Name)
		}
	}
	want := []string{CheckPreviewConfig, CheckPreviewViewers, CheckPreviewNodes, CheckPreviewDNS, CheckPreviewTLS,
		CheckPreviewAuth, CheckPreviewAuthDex, CheckPreviewAuthHost, CheckPreviewGatewayAllowlist}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if r.Namespace != "patchy" {
		t.Errorf("namespace = %q", r.Namespace)
	}
	hosts := 0
	for _, l := range r.Checks {
		if l.Name == CheckPreviewAuthHost {
			hosts++
		}
	}
	if hosts != 3 { // the probe host, shop-7 and blog-3; never the Deploying shop-8
		t.Errorf("%d host lines:\n%s", hosts, dump(r.Checks))
	}
	expect(t, r.Checks, CheckPreviewDNS, checkreport.Pass, "patchy-probe-0."+iSuffix)
	expect(t, r.Checks, CheckPreviewConfig, checkreport.Pass, "acme-previews.test is a registrable domain",
		"sign in at preview-auth.acme.test")
	expect(t, r.Checks, CheckPreviewViewers, checkreport.Pass, "RoleBinding patchy-preview-viewers binds",
		"1 team and 1 user")
	expect(t, r.Checks, CheckPreviewNodes, checkreport.Pass, "node preview-1 carries",
		iTaint+"=true:NoExecute")
	if lines := r.Lines(); len(lines) != len(r.Checks) || lines[0].Cells[0] != CheckPreviewConfig {
		t.Errorf("Lines = %+v", lines)
	}
}

// TestInstallNothingOn: with no patchy ConfigMap, preview-config fails,
// the host checks and the rest skip, and nothing is asked of the network.
func TestInstallNothingOn(t *testing.T) {
	w := &installWorld{web: iWeb{}, resolver: iResolver{}}
	r := w.run(t, interceptor.Funcs{})
	if n := r.Failed(); n != 1 {
		t.Fatalf("Failed = %d:\n%s", n, dump(r.Checks))
	}
	expect(t, r.Checks, CheckPreviewConfig, checkreport.Fail, "no preview-controller ConfigMap",
		"previewController.enabled")
	expect(t, r.Checks, CheckPreviewDNS, checkreport.Skip, "host suffix is unknown")
	expect(t, r.Checks, CheckPreviewViewers, checkreport.Skip, "sign-in is off")
	expect(t, r.Checks, CheckPreviewNodes, checkreport.Skip, "previews are off")
}

// TestInstallSignInOff: previews without sign-in fail preview-config, and
// report no sign-in line.
func TestInstallSignInOff(t *testing.T) {
	w := newInstallWorld()
	w.drop("patchy-preview-auth-config", &corev1.ConfigMap{})
	r := w.run(t, interceptor.Funcs{})
	expect(t, r.Checks, CheckPreviewConfig, checkreport.Fail, "preview sign-in is off", "previewAuth.enabled")
	for _, l := range r.Checks {
		if strings.HasPrefix(l.Name, "preview-auth") {
			t.Errorf("sign-in line with sign-in off: %s", l.Name)
		}
	}
}

// TestInstallSettingsUnreadable: a failed ConfigMap list skips the
// configuration and node checks rather than failing them.
func TestInstallSettingsUnreadable(t *testing.T) {
	forbidden := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := l.(*corev1.ConfigMapList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", errors.New("RBAC"))
		}
		return c.List(ctx, l, opts...)
	}}
	r := newInstallWorld().run(t, forbidden)
	expect(t, r.Checks, CheckPreviewConfig, checkreport.Skip, "cannot read the controllers' settings")
	expect(t, r.Checks, CheckPreviewViewers, checkreport.Skip, "cannot read the controllers' settings")
	expect(t, r.Checks, CheckPreviewNodes, checkreport.Skip, "cannot read the controllers' settings")
	if n := r.Failed(); n != 0 {
		t.Errorf("Failed = %d:\n%s", n, dump(r.Checks))
	}
}

// TestInstallConfigWarns: the legacy path still serving and a shared
// registrable domain warn without failing the run.
func TestInstallConfigWarns(t *testing.T) {
	w := newInstallWorld()
	w.setting("patchy-preview-controller", keyPreviewLegacyIngress, "")
	w.setting("patchy-preview-controller", keyAuthRequired, "true")
	w.setting("patchy-preview-controller", keyPreviewHostSuffix, "previews.acme.test")
	w.setting("patchy-preview-auth-config", keyAuthHostSuffix, "previews.acme.test")
	r := w.run(t, interceptor.Funcs{})
	expect(t, r.Checks, CheckPreviewConfig, checkreport.Warn, "both the preview gateway and the legacy slot "+
		"Ingress serve", "shares its registrable domain acme.test with preview-auth.acme.test, dex.acme.test")
}

// TestProbeProject: the install-level probe host is the relay's own.
func TestProbeProject(t *testing.T) {
	if ProbeProject+"-0" != previewauth.UnknownProbeLabel {
		t.Errorf("ProbeProject %q does not name %q", ProbeProject, previewauth.UnknownProbeLabel)
	}
}

// TestJudgeConfig: each finding maps onto its status, failures before
// warnings, every one named.
func TestJudgeConfig(t *testing.T) {
	base := configInput{suffix: iSuffix, authSuffix: iSuffix, issuer: iIssuer, dexIssuer: iDex, gateway: true}
	cases := []struct {
		name   string
		edit   func(*configInput)
		status checkreport.Status
		want   []string
	}{
		{"pass", func(*configInput) {}, checkreport.Pass, []string{"the preview gateway alone serves <label>." +
			iSuffix}},
		{"separate sign-in host", func(in *configInput) { in.signIn = "go.acme.test" }, checkreport.Pass,
			[]string{"sign in at go.acme.test"}},
		{"legacy only", func(in *configInput) { in.gateway, in.legacy, in.required = false, true, true },
			checkreport.Warn, []string{"the preview gateway is off"}},
		{"both paths", func(in *configInput) { in.legacy, in.required = true, true }, checkreport.Warn,
			[]string{"set preview.legacyIngress false"}},
		{"legacy path without sign-in", func(in *configInput) { in.legacy = true }, checkreport.Fail,
			[]string{"serves preview hosts without sign-in (previewAuth.stage permit)"}},
		{"legacy only without sign-in", func(in *configInput) { in.gateway, in.legacy = false, true },
			checkreport.Fail, []string{"without sign-in"}},
		{"nothing serves", func(in *configInput) { in.gateway = false }, checkreport.Fail,
			[]string{"nothing serves preview hosts"}},
		{"relay suffix differs", func(in *configInput) { in.authSuffix = "other.test" }, checkreport.Fail,
			[]string{"the relay's host suffix other.test is not preview-controller's " + iSuffix}},
		{"bad issuer", func(in *configInput) { in.issuer = "http://relay" }, checkreport.Fail,
			[]string{`issuer "http://relay" is not an https origin`}},
		{"sign-in under suffix", func(in *configInput) { in.signIn = "auth." + iSuffix }, checkreport.Fail,
			[]string{"sits under the preview host suffix"}},
		{"public suffix", func(in *configInput) { in.suffix, in.authSuffix = "co.uk", "co.uk" }, checkreport.Fail,
			[]string{"has no registrable domain of its own"}},
		{"shared with dex", func(in *configInput) { in.dexIssuer = "https://dex." + iSuffix + ":8443" },
			checkreport.Warn, []string{"shares its registrable domain " + iSuffix + " with dex." + iSuffix}},
		{"fail and warn", func(in *configInput) {
			in.authSuffix, in.gateway, in.legacy, in.required = "other.test", false, true, true
		}, checkreport.Fail, []string{"is not preview-controller's", "; the preview gateway is off"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.edit(&in)
			v := judgeConfig(in)
			if v.status != tc.status {
				t.Errorf("status = %s, want %s: %s", v.status, tc.status, v.reason)
			}
			for _, w := range tc.want {
				if !strings.Contains(v.reason, w) {
					t.Errorf("reason %q lacks %q", v.reason, w)
				}
			}
		})
	}
}

// TestJudgeConfigPrivateSuffix pins the property's first shrunk
// counterexample (a generator that took <label>.com for a registrable
// domain): de.com is a public suffix of its own, so z7cn7.de.com and
// y6.de.com are two sites and share nothing.
func TestJudgeConfigPrivateSuffix(t *testing.T) {
	v := judgeConfig(configInput{suffix: "z7cn7.de.com", issuer: "https://auth.m.net", signIn: "y6.de.com",
		dexIssuer: "https://dex.m.net", gateway: true})
	if v.status != checkreport.Pass {
		t.Errorf("judgeConfig = %+v, want PASS", v)
	}
}

// randLabel is a lowercase DNS label.
func randLabel(r *rand.Rand) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 1+r.Intn(8))
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	b[0] = 'a' + byte(r.Intn(26))
	return string(b)
}

// randDomain is <label>.<tld> on an ICANN TLD, so publicsuffix decides it.
func randDomain(r *rand.Rand) string {
	tlds := []string{"com", "dev", "net", "org", "io", "co.uk"}
	return randLabel(r) + "." + tlds[r.Intn(len(tlds))]
}

// TestJudgeConfigProperties (seeded): a sign-in host under the suffix
// always fails; previews on their own registrable domain, with sign-in and
// Dex on another and the gateway alone serving, always pass; and a sign-in
// host sharing the suffix's registrable domain (as publicsuffix decides it),
// outside it, never passes.
func TestJudgeConfigProperties(t *testing.T) {
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(randDomain(r))
		other := randDomain(r)
		for other == args[0].String() {
			other = randDomain(r)
		}
		args[1] = reflect.ValueOf(other)
		args[2] = reflect.ValueOf(randLabel(r))
		args[3] = reflect.ValueOf(randLabel(r))
	}
	prop := func(site, other, a, b string) bool {
		if a == b {
			b += "x"
		}
		ok := configInput{suffix: a + "." + site, issuer: "https://auth." + other, dexIssuer: "https://dex." + other,
			gateway: true}
		if v := judgeConfig(ok); v.status != checkreport.Pass {
			t.Logf("own domain: %+v -> %+v", ok, v)
			return false
		}
		under := ok
		under.signIn = b + "." + ok.suffix
		if v := judgeConfig(under); v.status != checkreport.Fail {
			t.Logf("under: %+v -> %+v", under, v)
			return false
		}
		reg, err := publicsuffix.EffectiveTLDPlusOne(ok.suffix)
		if err != nil {
			t.Logf("%s: %v", ok.suffix, err)
			return false
		}
		shared := ok
		shared.signIn = b + "." + reg
		if v := judgeConfig(shared); v.status == checkreport.Pass {
			t.Logf("shared: %+v -> %+v", shared, v)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestViewers: who may view previews, from the bindings of the chart's
// preview-viewer ClusterRole.
func TestViewers(t *testing.T) {
	forbidCRB := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := l.(*rbacv1.ClusterRoleBindingList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Group: rbacv1.GroupName,
				Resource: "clusterrolebindings"}, "", errors.New("RBAC"))
		}
		return c.List(ctx, l, opts...)
	}}
	forbidRB := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := l.(*rbacv1.RoleBindingList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"},
				"", errors.New("RBAC"))
		}
		return c.List(ctx, l, opts...)
	}}
	unusable := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "patchy-preview-viewers"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "patchy-preview-viewer"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "octocat"},
			{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: "patchy"}},
	}
	cluster := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "everyone-qa"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "patchy-preview-viewer"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "github:acme:qa"}},
	}
	other := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "unrelated"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, Name: "github:acme:qa"}},
	}
	cases := []struct {
		name   string
		edit   func(*installWorld)
		fn     interceptor.Funcs
		status checkreport.Status
		want   []string
	}{
		{"none", func(w *installWorld) { w.drop("patchy-preview-viewers", &rbacv1.RoleBinding{}) },
			interceptor.Funcs{}, checkreport.Warn, []string{"unless another role grants get on projects/previews",
				"every sign-in is refused 403", "previewAuth.viewers"}},
		{"another role only", func(w *installWorld) {
			w.drop("patchy-preview-viewers", &rbacv1.RoleBinding{})
			w.objs = append(w.objs, other)
		}, interceptor.Funcs{}, checkreport.Warn, []string{"nothing binds ClusterRole patchy-preview-viewer"}},
		{"unusable subjects", func(w *installWorld) {
			w.drop("patchy-preview-viewers", &rbacv1.RoleBinding{})
			w.objs = append(w.objs, unusable)
		}, interceptor.Funcs{}, checkreport.Warn, []string{"ServiceAccount default, User octocat never sign in"}},
		{"only the granting binding is credited", func(w *installWorld) {
			w.drop("patchy-preview-viewers", &rbacv1.RoleBinding{})
			w.objs = append(w.objs, unusable, cluster)
		}, interceptor.Funcs{}, checkreport.Pass, []string{"ClusterRoleBinding everyone-qa binds ClusterRole",
			"User octocat never sign in"}},
		{"cluster binding", func(w *installWorld) {
			w.drop("patchy-preview-viewers", &rbacv1.RoleBinding{})
			w.objs = append(w.objs, cluster)
		}, interceptor.Funcs{}, checkreport.Pass, []string{"ClusterRoleBinding everyone-qa binds", "1 team and 0 users"}},
		{"cluster bindings unreadable", func(w *installWorld) {
			w.drop("patchy-preview-viewers", &rbacv1.RoleBinding{})
		}, forbidCRB, checkreport.Skip, []string{"not every binding could be listed"}},
		{"cluster bindings unreadable but a role binding", func(*installWorld) {}, forbidCRB, checkreport.Pass,
			[]string{"RoleBinding patchy-preview-viewers"}},
		{"role bindings unreadable", func(*installWorld) {}, forbidRB, checkreport.Skip,
			[]string{"not every binding could be listed", "rolebindings"}},
		{"role bindings unreadable but a cluster binding", func(w *installWorld) {
			w.objs = append(w.objs, cluster)
		}, forbidRB, checkreport.Pass, []string{"ClusterRoleBinding everyone-qa binds"}},
		{"relay misnamed", func(w *installWorld) {
			for _, o := range w.objs {
				if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == "patchy-preview-auth-config" {
					cm.Name = "relay-settings"
				}
			}
		}, interceptor.Funcs{}, checkreport.Skip, []string{"is not named <release>-preview-auth-config"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newInstallWorld()
			tc.edit(w)
			r := w.run(t, tc.fn)
			expect(t, r.Checks, CheckPreviewViewers, tc.status, tc.want...)
		})
	}
}

// TestJudgeViewers: a subject counts once, by kind and the relay's
// prefixes; a bare prefix is no one.
func TestJudgeViewers(t *testing.T) {
	v := judgeViewers([]rbacv1.Subject{
		{Kind: rbacv1.GroupKind, Name: "github:acme:qa"},
		{Kind: rbacv1.GroupKind, Name: "github:acme:qa"},
		{Kind: rbacv1.UserKind, Name: "github:octocat"},
		{Kind: rbacv1.UserKind, Name: "gh:hubot"},
		{Kind: rbacv1.GroupKind, Name: "github:"},
	}, "github:", "github:")
	if v.teams != 1 || v.users != 1 || strings.Join(v.ignored, "|") != "Group github:|User gh:hubot" {
		t.Errorf("judgeViewers = %+v", v)
	}
	if v := judgeViewers([]rbacv1.Subject{{Kind: rbacv1.UserKind, Name: "octocat"}}, "", ""); v.users != 1 {
		t.Errorf("an empty prefix admits any name: %+v", v)
	}
}

// TestNodes: preview-nodes' verdicts over the nodes and the isolation
// settings.
func TestNodes(t *testing.T) {
	forbidden := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := l.(*corev1.NodeList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("RBAC"))
		}
		return c.List(ctx, l, opts...)
	}}
	const pc = "patchy-preview-controller"
	many := func(w *installWorld) {
		for i := range 7 {
			w.objs = append(w.objs, node(fmt.Sprintf("bare-%d", i), map[string]string{iNodeLabel: "true"}))
		}
	}
	cases := []struct {
		name   string
		edit   func(*installWorld)
		fn     interceptor.Funcs
		status checkreport.Status
		want   []string
	}{
		{"isolation off", func(w *installWorld) {
			w.setting(pc, keyPreviewNodeIsolation, preview.NodeIsolationNone)
		}, interceptor.Funcs{}, checkreport.Warn, []string{"node isolation is off"}},
		{"no taint key", func(w *installWorld) { w.setting(pc, keyPreviewTaintKey, "") }, interceptor.Funcs{},
			checkreport.Fail, []string{"sets no node selector or taint key"}},
		{"bad selector", func(w *installWorld) { w.setting(pc, keyPreviewNodeSelector, "[1]") }, interceptor.Funcs{},
			checkreport.Fail, []string{"preview node selector"}},
		{"nodes unreadable", func(*installWorld) {}, forbidden, checkreport.Skip, []string{"cannot list the nodes"}},
		{"no preview node", func(w *installWorld) { w.drop("preview-1", &corev1.Node{}) }, interceptor.Funcs{},
			checkreport.Fail, []string{"no node carries", "at registration"}},
		{"node pool launches on demand", func(w *installWorld) {
			w.drop("preview-1", &corev1.Node{})
			w.setting(pc, keyPreviewNodePool, "previews")
			w.setting(pc, keyPreviewNodeClass, "previews")
		}, interceptor.Funcs{}, checkreport.Skip, []string{"NodePool previews launches them",
			"eks.amazonaws.com/nodeclass=previews,karpenter.sh/nodepool=previews," + iNodeLabel + "=true"}},
		{"selected, not tainted", func(w *installWorld) {
			w.objs = append(w.objs, node("half", map[string]string{iNodeLabel: "true"},
				taint(iTaint, "true", corev1.TaintEffectNoSchedule)))
		}, interceptor.Funcs{}, checkreport.Fail, []string{"node half matches the node selector without the taint"}},
		{"tainted, not selected", func(w *installWorld) {
			w.objs = append(w.objs, node("typo", map[string]string{iNodeLabel: "yes"},
				taint(iTaint, "true", corev1.TaintEffectNoExecute)))
		}, interceptor.Funcs{}, checkreport.Warn, []string{"node preview-1 carries",
			"node typo carries the taint without", "another install's preview node"}},
		{"only another install's node", func(w *installWorld) {
			w.drop("preview-1", &corev1.Node{})
			w.objs = append(w.objs, node("theirs", map[string]string{iNodeLabel: "other"},
				taint(iTaint, "true", corev1.TaintEffectNoExecute)))
		}, interceptor.Funcs{}, checkreport.Fail, []string{"no node carries", "; node theirs carries the taint"}},
		{"unknown isolation mode", func(w *installWorld) {
			w.setting(pc, keyPreviewNodeIsolation, "None")
		}, interceptor.Funcs{}, checkreport.Fail, []string{`to "None", which preview-controller refuses`}},
		{"many named, bounded", many, interceptor.Funcs{}, checkreport.Fail,
			[]string{"nodes bare-0, bare-1, bare-2, bare-3, bare-4 and 2 more match"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newInstallWorld()
			tc.edit(w)
			r := w.run(t, tc.fn)
			expect(t, r.Checks, CheckPreviewNodes, tc.status, tc.want...)
		})
	}
}

// TestNodeLabelsMatchController pins the two EKS keys this check fills to
// preview-controller's own: with them, a selector naming another NodePool
// than the setting is the contradiction the controller refuses.
func TestNodeLabelsMatchController(t *testing.T) {
	for _, tc := range []struct {
		label string
		set   func(*preview.Settings)
	}{
		{nodePoolLabel, func(s *preview.Settings) { s.NodePool, s.NodeClass = "b", "c" }},
		{nodeClassLabel, func(s *preview.Settings) { s.NodePool, s.NodeClass = "p", "b" }},
	} {
		s := preview.Settings{ImagePrefix: "ghcr.io/acme/previews/", Namespace: "patchy", SlotCount: 1,
			HostSuffix: iSuffix, RolloutTimeout: time.Minute, PollInterval: time.Second, MaxRetries: 1,
			NodeSelector: map[string]string{tc.label: "a"}, TaintKey: iTaint}
		tc.set(&s)
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "contradicts") {
			t.Errorf("%s is not preview-controller's key: Validate = %v", tc.label, err)
		}
	}
}

// randNode is a node with a random subset of the selector's labels (some
// with a wrong value), unrelated labels, and maybe a taint of the key with
// a random value and effect.
func randNode(r *rand.Rand, i int, selector map[string]string, key string) corev1.Node {
	labels := map[string]string{"kubernetes.io/hostname": fmt.Sprint(i)}
	for k, v := range selector {
		switch r.Intn(4) {
		case 0:
		case 1:
			labels[k] = v + "x"
		default:
			labels[k] = v
		}
	}
	var taints []corev1.Taint
	if r.Intn(3) > 0 {
		values := []string{"true", "false", ""}
		effects := []corev1.TaintEffect{corev1.TaintEffectNoExecute, corev1.TaintEffectNoSchedule,
			corev1.TaintEffectPreferNoSchedule}
		taints = append(taints, taint(key, values[r.Intn(3)], effects[r.Intn(3)]))
	}
	if r.Intn(2) == 0 {
		taints = append(taints, taint("other", "true", corev1.TaintEffectNoExecute))
	}
	return *node(fmt.Sprintf("n%02d", i), labels, taints...)
}

// TestJudgeNodesProperties (seeded): every node lands on at most one list;
// a node lands on isolated exactly when it has every selector label and
// the key=true:NoExecute taint; a node without the taint key is never a
// preview node unless it is selected; the lists are sorted; and neither
// the nodes' order nor an unrelated label changes the verdict.
func TestJudgeNodesProperties(t *testing.T) {
	selector := map[string]string{iNodeLabel: "true", "pool": "previews"}
	cfg := quickConfig(2000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		nodes := make([]corev1.Node, r.Intn(8))
		for i := range nodes {
			nodes[i] = randNode(r, i, selector, iTaint)
		}
		args[0] = reflect.ValueOf(nodes)
		args[1] = reflect.ValueOf(r.Int63())
	}
	prop := func(nodes []corev1.Node, shuffle int64) bool {
		v := judgeNodes(selector, iTaint, nodes)
		where := map[string]int{}
		for _, list := range [][]string{v.isolated, v.untainted, v.unselected} {
			if !sortedStrings(list) {
				return false
			}
			for _, n := range list {
				where[n]++
			}
		}
		for i := range nodes {
			n := &nodes[i]
			selected := true
			for k, want := range selector {
				if n.Labels[k] != want {
					selected = false
				}
			}
			good, hasKey := false, false
			for _, tt := range n.Spec.Taints {
				if tt.Key == iTaint {
					hasKey = true
					good = good || (tt.Value == "true" && tt.Effect == corev1.TaintEffectNoExecute)
				}
			}
			if where[n.Name] > 1 || (selected && good) != containsString(v.isolated, n.Name) ||
				(!hasKey && !selected && where[n.Name] != 0) {
				t.Logf("node %s (selected %v, tainted %v): %+v", n.Name, selected, good, v)
				return false
			}
		}
		moved := append([]corev1.Node(nil), nodes...)
		rand.New(rand.NewSource(shuffle)).Shuffle(len(moved), func(i, j int) { moved[i], moved[j] = moved[j], moved[i] })
		for i := range moved {
			moved[i].Labels = mergeLabels(moved[i].Labels, map[string]string{"zone": "a"})
		}
		return reflect.DeepEqual(v, judgeNodes(selector, iTaint, moved))
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

func containsString(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// signInWorld is the install world with a sign-in host of its own,
// previewAuth.gateway.signInHost, which the chart routes through the
// gateway's exposure.
func signInWorld(signIn string) *installWorld {
	w := newInstallWorld()
	w.setting("patchy-preview-auth-config", keyAuthGatewaySignIn, signIn)
	for _, label := range []string{"shop-7", "blog-3"} {
		w.web[label+"."+iSuffix+"/"] = func(*http.Request) (int, http.Header, string) {
			return http.StatusFound, http.Header{"Location": {"https://" + signIn + "/start?h=" + label +
				"&s=pa1.t.1.sealed"}}, ""
		}
	}
	return w
}

// linesOf are the lines of one check, in order.
func linesOf(lines []Line, check string) []Line {
	var out []Line
	for _, l := range lines {
		if l.Name == check {
			out = append(out, l)
		}
	}
	return out
}

// TestInstallSignInHost: a sign-in host other than the relay's own gets
// preview-dns and preview-tls lines of its own (design §4.4: both names),
// so a missing record, a record pointing elsewhere or an untrusted
// certificate there fails the run; the relay's own host gets none, as its
// discovery already reaches it.
func TestInstallSignInHost(t *testing.T) {
	const signIn = "login.acme.test"
	trusted := func(_ context.Context, host string) (tls.ConnectionState, error) {
		names := []string{"*." + iSuffix}
		if host == signIn {
			names = []string{signIn}
		}
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{{DNSNames: names,
			Issuer: pkix.Name{CommonName: "Test CA"}, NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}}}, nil
	}
	untrusted := func(ctx context.Context, host string) (tls.ConnectionState, error) {
		if host == signIn {
			return tls.ConnectionState{}, x509.HostnameError{Certificate: &x509.Certificate{
				DNSNames: []string{"*." + iSuffix}}, Host: signIn}
		}
		return trusted(ctx, host)
	}
	cases := []struct {
		name      string
		addrs     []string // nil: NXDOMAIN
		dial      func(context.Context, string) (tls.ConnectionState, error)
		dns, cert checkreport.Status
		dnsWant   string
		certWant  string
	}{
		{"routed", []string{iGateway}, trusted, checkreport.Pass, checkreport.Pass,
			"sign-in host " + signIn + " resolves to the preview gateway's edge", "serves a trusted certificate"},
		{"no record", nil, trusted, checkreport.Fail, checkreport.Skip,
			"sign-in host " + signIn + " does not resolve", "does not resolve"},
		{"elsewhere", []string{"192.0.2.99"}, trusted, checkreport.Fail, checkreport.Pass,
			"resolves to 192.0.2.99, but the preview gateway's edge", "serves a trusted certificate"},
		{"untrusted certificate", []string{iGateway}, untrusted, checkreport.Pass, checkreport.Fail,
			"resolves to the preview gateway's edge", "must cover " + signIn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := signInWorld(signIn)
			if tc.addrs != nil {
				w.resolver[signIn] = tc.addrs
			}
			w.dial = tc.dial
			r := w.run(t, interceptor.Funcs{})
			dns, cert := linesOf(r.Checks, CheckPreviewDNS), linesOf(r.Checks, CheckPreviewTLS)
			if len(dns) != 2 || len(cert) != 2 {
				t.Fatalf("want two preview-dns and two preview-tls lines:\n%s", dump(r.Checks))
			}
			if dns[0].Status != checkreport.Pass || cert[0].Status != checkreport.Pass {
				t.Errorf("probe host lines: %+v, %+v", dns[0], cert[0])
			}
			if l := dns[1]; l.Status != tc.dns || !strings.Contains(l.Reason, tc.dnsWant) {
				t.Errorf("sign-in preview-dns = %s %q, want %s with %q", l.Status, l.Reason, tc.dns, tc.dnsWant)
			}
			if l := cert[1]; l.Status != tc.cert || !strings.Contains(l.Reason, tc.certWant) {
				t.Errorf("sign-in preview-tls = %s %q, want %s with %q", l.Status, l.Reason, tc.cert, tc.certWant)
			}
			wantFailed := 0
			for _, s := range []checkreport.Status{tc.dns, tc.cert} {
				if s == checkreport.Fail {
					wantFailed++
				}
			}
			if n := r.Failed(); n != wantFailed {
				t.Errorf("Failed = %d, want %d:\n%s", n, wantFailed, dump(r.Checks))
			}
		})
	}

	// The relay's own host as the sign-in host is no second name.
	w := signInWorld(strings.TrimPrefix(iIssuer, "https://"))
	r := w.run(t, interceptor.Funcs{})
	if dns := linesOf(r.Checks, CheckPreviewDNS); len(dns) != 1 {
		t.Errorf("%d preview-dns lines for the relay's own host:\n%s", len(dns), dump(r.Checks))
	}
	if n := r.Failed(); n != 0 {
		t.Errorf("Failed = %d:\n%s", n, dump(r.Checks))
	}
}
