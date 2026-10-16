// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
	"github.com/bitwise-media-group/patchy/internal/previewauth/fakedex"
)

// previewApp is one preview component's code: it answers with its name and
// the request it was sent, and records every request.
type previewApp struct {
	name string
	srv  *httptest.Server
	mu   sync.Mutex
	seen []*http.Request
}

func newPreviewApp(t *testing.T, name string) *previewApp {
	t.Helper()
	a := &previewApp{name: name}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.seen = append(a.seen, r.Clone(context.Background()))
		a.mu.Unlock()
		_, _ = fmt.Fprintf(w, "%s %s %s", a.name, r.Method, r.URL.RequestURI())
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *previewApp) requests() []*http.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*http.Request(nil), a.seen...)
}

// gatewayBrowser is a browser in front of the gateway: requests for the
// sign-in host and every preview host go to the gateway's plain listener
// (the edge's TLS is not under test; the cookie jar still sees https, so
// __Host- cookies behave as in a browser), and every other request to Dex.
type gatewayBrowser struct {
	t    *testing.T
	gw   string
	base http.RoundTripper
	c    *http.Client
}

func (b *gatewayBrowser) RoundTrip(r *http.Request) (*http.Response, error) {
	h := r.URL.Hostname()
	if h != signInRelayHost && !strings.HasSuffix(h, "."+signInSuffix) {
		return b.base.RoundTrip(r)
	}
	out := r.Clone(r.Context())
	out.Host = r.URL.Host
	out.URL.Scheme, out.URL.Host = "http", b.gw
	return b.base.RoundTrip(out)
}

func newGatewayBrowser(t *testing.T, gw string, base http.RoundTripper) *gatewayBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &gatewayBrowser{t: t, gw: gw, base: base}
	b.c = &http.Client{Transport: b, Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return b
}

// do sends one request, a top-level navigation unless hdr says otherwise
// (an empty value removes a header), and returns the status, Location and
// body.
func (b *gatewayBrowser) do(method, u string, hdr http.Header) (int, string, string) {
	b.t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Accept", "text/html")
	for k, v := range hdr {
		if len(v) == 0 || v[0] == "" {
			req.Header.Del(k)
			continue
		}
		req.Header[k] = v
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

func (b *gatewayBrowser) nav(u string) (int, string, string) {
	b.t.Helper()
	return b.do(http.MethodGet, u, nil)
}

// follow navigates and expects a redirect whose Location starts with
// prefix; it returns the Location.
func (b *gatewayBrowser) follow(u, prefix string) string {
	b.t.Helper()
	status, location, body := b.nav(u)
	if (status != http.StatusFound && status != http.StatusSeeOther) || !strings.HasPrefix(location, prefix) {
		b.t.Fatalf("GET %s: %d to %q (%s), want a redirect to %s...", u, status, location, body, prefix)
	}
	return location
}

// signIn opens path on label's host and follows the browser through the
// gateway's /start, Dex (only without a relay session) and the handoff back
// to the host. It returns where the handoff sent it.
func (b *gatewayBrowser) signIn(label, path string, viaDex bool) string {
	b.t.Helper()
	origin := "https://" + label + "." + signInSuffix
	start := b.follow(origin+path, "https://"+signInRelayHost+"/start?")
	next := b.follow(start, "https://")
	if viaDex {
		if strings.HasPrefix(next, origin) {
			b.t.Fatalf("/start skipped Dex without a relay session: %s", next)
		}
		next = b.follow(next, "https://"+signInRelayHost+"/dex/callback?")
		next = b.follow(next, origin+previewauth.HandoffPath+"?")
	} else if !strings.HasPrefix(next, origin+previewauth.HandoffPath+"?") {
		b.t.Fatalf("/start with a relay session sent the browser to %s", next)
	}
	return b.follow(next, origin+"/")
}

// hostCookie is the browser's host session for label's host.
func (b *gatewayBrowser) hostCookie(label string) string {
	b.t.Helper()
	u, _ := url.Parse("https://" + label + "." + signInSuffix + "/")
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == previewauth.HostSessionCookie {
			return c.Value
		}
	}
	return ""
}

// TestPreviewGatewayEndToEnd drives the shipped preview-controller and
// preview-auth binaries with the gateway on and the legacy slot Ingress
// off, as release N runs them once the wildcard record points at the
// gateway. envtest has no kube-proxy, so preview-auth is built with the
// e2edial tag and maps each slot Service's DNS name to a local listener
// (the only difference from a released binary); the test plays the
// Deployment controller and kubelet. A viewer signs in through a TLS Dex
// once per browser; the gateway routes by path to each component; a new PR
// head keeps the old revision serving while it deploys; another preview's
// host neither takes this one's session nor may reach it cross-site; an
// unbound viewer is refused; and revoking the viewer's grant, then ending
// the Intent, end the session.
func TestPreviewGatewayEndToEnd(t *testing.T) {
	cl := startCluster(t)
	ctx := context.Background()
	for _, name := range []string{"patchy-preview-0", "patchy-preview-1"} {
		if err := cl.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	dex, err := fakedex.StartTLS(signInDexClient, signInDexSecret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dex.Close)
	keysDir, caFile, secretFile := signInFiles(t, dex)
	grantPreviewViewer(t, cl, "github:alice")
	if err := cl.client.Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: signInLease, Namespace: namespace},
	}); err != nil {
		t.Fatal(err)
	}

	// Each component's code, and the dial map from its Service's name in
	// either slot to it.
	web1, api1, app2 := newPreviewApp(t, "web-1"), newPreviewApp(t, "api-1"), newPreviewApp(t, "app-2")
	dial := map[string]string{}
	for slot := range 2 {
		for svc, app := range map[string]*previewApp{"preview-shop-1": web1, "preview-shop-1-api": api1,
			"preview-shop-2": app2, "preview-shop-2-api": app2} {
			dial[fmt.Sprintf("%s.patchy-preview-%d.svc:80", svc, slot)] = app.srv.Listener.Addr().String()
		}
	}
	dialJSON, err := json.Marshal(dial)
	if err != nil {
		t.Fatal(err)
	}

	cl.controller(t, "preview-controller",
		"--preview-slot-count", "2",
		"--preview-image-prefix", "registry.example/patchy/previews/",
		"--preview-host-suffix", signInSuffix,
		"--preview-node-selector", `{"node-role.example.com/preview":"true"}`,
		"--preview-taint-key", "node-role.example.com/preview",
		"--preview-poll-interval", "1s", "--preview-legacy-ingress=false")
	gwAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.stoppableBinary(t, "preview-auth", buildTagged(t, "preview-auth", "e2edial"),
		"--listen-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"--preview-auth-issuer", signInIssuer, "--preview-auth-host-suffix", signInSuffix,
		"--preview-auth-slot-count", "2", "--preview-auth-keys-dir", keysDir,
		"--preview-auth-dex-issuer-url", dex.URL, "--preview-auth-dex-client-id", signInDexClient,
		"--preview-auth-dex-client-secret-file", secretFile, "--preview-auth-dex-ca-file", caFile,
		"--preview-auth-username-prefix", "github:", "--preview-auth-groups-prefix", "github:",
		"--preview-auth-ledger-lease", signInLease,
		"--preview-auth-gateway", "--preview-auth-gateway-listen-addr", gwAddr,
		// One second of access-review cache (and so of stream re-checks),
		// so revocation lands within the test's patience.
		"--preview-auth-review-ttl", "1s",
		// The test client is every request's one address, and the probe
		// has no real host to ask.
		"--preview-auth-rate-per-second", "0", "--preview-auth-probe-interval", "0s",
		"--e2e-gateway-dial-map", string(dialJSON))

	shop := gatewayProject(t, cl)
	in1, p1 := gatewayPreview(t, cl, shop, 1)
	pods1 := readyComponents(t, cl, p1, "a")
	_, p2 := gatewayPreview(t, cl, shop, 2)
	pods2 := readyComponents(t, cl, p2, "a")

	// alice signs in once, through Dex, and the gateway routes by path.
	dex.SignIn(map[string]any{"sub": "dex-alice", "preferred_username": "alice", "groups": []any{"acme:qa"}})
	alice := newGatewayBrowser(t, gwAddr, dex.Client().Transport)
	origin1 := "https://shop-1." + signInSuffix
	if got := alice.signIn("shop-1", "/api/hello?x=1", true); got != origin1+"/api/hello?x=1" {
		t.Fatalf("the handoff returned to %s", got)
	}
	for path, want := range map[string]string{"/api/hello?x=1": "api-1 GET /api/hello?x=1", "/": "web-1 GET /",
		"/apix": "web-1 GET /apix", "/api": "api-1 GET /api"} {
		if status, _, body := alice.nav(origin1 + path); status != http.StatusOK || body != want {
			t.Errorf("GET %s: %d %q, want %q", path, status, body, want)
		}
	}
	for _, r := range append(web1.requests(), api1.requests()...) {
		viewer := r.Header.Get("X-Patchy-Viewer")
		if viewer == "" || strings.Contains(viewer, "alice") {
			t.Errorf("the preview was sent viewer %q", viewer)
		}
		if strings.Contains(r.Header.Get("Cookie"), "patchy-p") || previewauth.ReservedPath(r.URL.Path) {
			t.Errorf("the preview was sent %s with Cookie %q", r.URL.Path, r.Header.Get("Cookie"))
		}
	}

	// The relay session signs alice in to another preview without Dex.
	before := dex.Authorizations()
	origin2 := "https://shop-2." + signInSuffix
	if got := alice.signIn("shop-2", "/", false); got != origin2+"/" || dex.Authorizations() != before {
		t.Fatalf("a second preview went to %s, with %d Dex authorizations", got, dex.Authorizations()-before)
	}

	// Cross-preview: shop-1's session is not shop-2's, and shop-2's page may
	// not reach shop-1 with shop-1's cookie.
	stranger := newGatewayBrowser(t, gwAddr, dex.Client().Transport)
	seen2 := len(app2.requests())
	status, location, _ := stranger.do(http.MethodGet, origin2+"/", http.Header{
		"Cookie": {previewauth.HostSessionCookie + "=" + alice.hostCookie("shop-1")}})
	if status != http.StatusFound || !strings.HasPrefix(location, "https://"+signInRelayHost+"/start?") ||
		len(app2.requests()) != seen2 {
		t.Errorf("shop-1's session on shop-2's host: %d %q", status, location)
	}
	seenAPI := len(api1.requests())
	for name, hdr := range map[string]http.Header{
		"a cross-preview fetch": {"Origin": {origin2}, "Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"cors"},
			"Sec-Fetch-Dest": {"empty"}, "Accept": {""}},
		"a form post without Sec-Fetch": {"Origin": {origin2}, "Sec-Fetch-Site": {""}, "Sec-Fetch-Mode": {""},
			"Sec-Fetch-Dest": {""}},
		"a WebSocket from shop-2": {"Origin": {origin2}, "Connection": {"Upgrade"}, "Upgrade": {"websocket"},
			"Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"websocket"}, "Sec-Fetch-Dest": {"websocket"}},
	} {
		method := http.MethodPost
		if hdr.Get("Upgrade") != "" {
			method = http.MethodGet
		}
		if status, _, _ := alice.do(method, origin1+"/api/transfer", hdr); status != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, status)
		}
	}
	if len(api1.requests()) != seenAPI {
		t.Error("a cross-preview request reached shop-1's API")
	}

	// mallory signs in at Dex but may view no Project: a page, no redirect.
	dex.SignIn(map[string]any{"sub": "dex-mallory", "preferred_username": "mallory", "groups": []any{"acme:x"}})
	mallory := newGatewayBrowser(t, gwAddr, dex.Client().Transport)
	start := mallory.follow(origin1+"/", "https://"+signInRelayHost+"/start?")
	cb := mallory.follow(mallory.follow(start, "https://"), "https://"+signInRelayHost+"/dex/callback?")
	if status, location, _ := mallory.nav(cb); status != http.StatusForbidden || location != "" {
		t.Errorf("an unbound viewer: %d to %q", status, location)
	}

	// A new PR head: the old revision serves while the new one deploys.
	head := strings.Repeat("c", 40)
	setHead(t, cl, in1, "https://github.com/acme/shop-web", head)
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(p1), p1); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in1), in1); err != nil {
		t.Fatal(err)
	}
	components, ok := v1alpha1.DesiredPreviewComponents(shop, in1)
	if !ok {
		t.Fatal("no components at the new head")
	}
	p1.Spec.Components = components
	if err := cl.client.Update(ctx, p1); err != nil {
		t.Fatal(err)
	}
	slot := v1alpha1.PreviewSlotNamespace(*p1.Status.Slot)
	eventually(t, "the web Deployment at the new head while Deploying", func() bool {
		var dep appsv1.Deployment
		var cur v1alpha1.Preview
		return cl.client.Get(ctx, types.NamespacedName{Namespace: slot, Name: "preview-shop-1"}, &dep) == nil &&
			strings.HasSuffix(dep.Spec.Template.Spec.Containers[0].Image, "sha-"+head) &&
			cl.client.Get(ctx, client.ObjectKeyFromObject(p1), &cur) == nil &&
			cur.Status.Phase == v1alpha1.PreviewDeploying && cur.Status.Serving != nil
	})
	for _, path := range []string{"/", "/api/hello"} {
		if status, _, body := alice.nav(origin1 + path); status != http.StatusOK || !strings.Contains(body, path) {
			t.Errorf("GET %s while deploying: %d %q", path, status, body)
		}
	}
	for _, pod := range pods1 {
		if err := cl.client.Delete(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	pods1 = readyComponents(t, cl, p1, "c")
	if status, _, body := alice.nav(origin1 + "/"); status != http.StatusOK || body != "web-1 GET /" {
		t.Errorf("GET / at the new head: %d %q", status, body)
	}

	// Revoking the grant ends the session within the access review's TTL.
	if err := cl.client.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "patchy-preview-viewers", Namespace: namespace}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice refused once her grant is gone", func() bool {
		status, _, body := alice.nav(origin2 + "/")
		return status == http.StatusForbidden && strings.Contains(body, "not a preview viewer")
	})
	if alice.hostCookie("shop-2") != "" {
		t.Error("a refused session's cookie was kept")
	}

	// Ending the Intent ends its preview: its host is the gateway's 404.
	for _, pod := range pods1 {
		if err := cl.client.Delete(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	in1.Status.Phase = v1alpha1.IntentMerged
	if err := cl.client.Status().Update(ctx, in1); err != nil {
		t.Fatal(err)
	}
	eventually(t, "shop-1 deleted", func() bool {
		return apierrors.IsNotFound(cl.client.Get(ctx, client.ObjectKeyFromObject(p1), &v1alpha1.Preview{}))
	})
	eventually(t, "shop-1's host answered with the gateway's 404", func() bool {
		status, _, body := alice.nav(origin1 + "/")
		return status == http.StatusNotFound && strings.Contains(body, previewauth.NoPreviewTitle)
	})
	var ingresses networkingv1.IngressList
	for _, ns := range []string{"patchy-preview-0", "patchy-preview-1"} {
		if err := cl.client.List(ctx, &ingresses, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		if len(ingresses.Items) != 0 {
			t.Errorf("%d Ingresses in %s without the legacy path", len(ingresses.Items), ns)
		}
	}
	for _, pod := range pods2 {
		_ = cl.client.Delete(ctx, pod)
	}
}

// gatewayProject is the shop Project: a web component at / and an API at
// /api.
func gatewayProject(t *testing.T, cl *cluster) *v1alpha1.Project {
	t.Helper()
	preview := func(leaf, path string) *v1alpha1.ProjectRepositoryPreview {
		return &v1alpha1.ProjectRepositoryPreview{ProjectPreview: v1alpha1.ProjectPreview{
			ImageRepository: "registry.example/patchy/previews/" + leaf, Port: 8080, ReadinessPath: "/healthz",
		}, Path: path}
	}
	p := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: namespace},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories: []v1alpha1.ProjectRepository{
				{Name: "web", URL: "https://github.com/acme/shop-web", Preview: preview("shop-web", "/")},
				{Name: "api", URL: "https://github.com/acme/shop-api", Preview: preview("shop-api", "/api")},
			},
		}}
	if err := cl.client.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

// gatewayPreview creates Intent shop-<n> in review with a PR on both
// repositories, and the Preview intent-controller would write for it.
func gatewayPreview(t *testing.T, cl *cluster, project *v1alpha1.Project, n int) (*v1alpha1.Intent,
	*v1alpha1.Preview) {
	t.Helper()
	ctx := context.Background()
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("shop-%d", n), Namespace: namespace},
		Spec: v1alpha1.IntentSpec{Project: "shop",
			Issue:       v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: int64(n)},
			RequestedBy: v1alpha1.IntentRequest{Login: "octocat", At: metav1.Now(), EventID: int64(n)}}}
	if err := cl.client.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentInReview
	in.Status.PullRequests = []v1alpha1.IntentPullRequest{
		{Repository: "https://github.com/acme/shop-web", Number: int64(2 * n), State: "open",
			HeadSHA: strings.Repeat("a", 40)},
		{Repository: "https://github.com/acme/shop-api", Number: int64(2*n + 1), State: "open",
			HeadSHA: strings.Repeat("b", 40)},
	}
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	components, ok := v1alpha1.DesiredPreviewComponents(project, in)
	if !ok || len(components) != 2 {
		t.Fatalf("derived %+v, %v; want two components", components, ok)
	}
	p := &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Intent",
			Name: in.Name, UID: in.UID, Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}}},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: in.Name, UID: in.UID}, Project: "shop", HostLabel: in.Name,
			Components: components, TTL: metav1.Duration{Duration: 72 * time.Hour},
		}}
	if err := cl.client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	return in, p
}

// setHead moves the Intent's pull request on repo to head.
func setHead(t *testing.T, cl *cluster, in *v1alpha1.Intent, repo, head string) {
	t.Helper()
	ctx := context.Background()
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	for i := range in.Status.PullRequests {
		if in.Status.PullRequests[i].Repository == repo {
			in.Status.PullRequests[i].HeadSHA = head
		}
	}
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
}

// readyComponents plays the Deployment controller and kubelet for every
// component of p at its current spec: once each Deployment in p's slot
// runs its component's revision, a Ready Pod of it (named for tag, its
// image ID too), until preview-controller reports p Ready with a serving
// snapshot of this generation. It returns the Pods, which the test deletes
// before replacing or ending them (envtest has no garbage collector).
func readyComponents(t *testing.T, cl *cluster, p *v1alpha1.Preview, tag string) []*corev1.Pod {
	t.Helper()
	ctx := context.Background()
	var cur v1alpha1.Preview
	eventually(t, p.Name+" in a slot", func() bool {
		return cl.client.Get(ctx, client.ObjectKeyFromObject(p), &cur) == nil && cur.Status.Slot != nil
	})
	slot := v1alpha1.PreviewSlotNamespace(*cur.Status.Slot)
	pods := make([]*corev1.Pod, 0, len(cur.Spec.Components))
	for i, c := range cur.Spec.Components {
		key := types.NamespacedName{Namespace: slot, Name: v1alpha1.PreviewComponentServiceName(&cur, i)}
		var dep appsv1.Deployment
		eventually(t, key.String()+" at "+c.Revision, func() bool {
			return cl.client.Get(ctx, key, &dep) == nil &&
				strings.HasSuffix(dep.Spec.Template.Spec.Containers[0].Image, "sha-"+c.Revision)
		})
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1, 1
		if err := cl.client.Status().Update(ctx, &dep); err != nil {
			t.Fatal(err)
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key.Name + "-" + tag, Namespace: slot,
			Labels: dep.Spec.Template.Labels}, Spec: dep.Spec.Template.Spec}
		if err := cl.client.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: c.Name, Ready: true,
			ImageID: "repo@sha256:" + c.Name + "-" + tag}}
		if err := cl.client.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
		pods = append(pods, pod)
	}
	eventually(t, p.Name+" Ready with its serving snapshot", func() bool {
		return cl.client.Get(ctx, client.ObjectKeyFromObject(p), &cur) == nil &&
			cur.Status.Phase == v1alpha1.PreviewReady && cur.Status.Serving != nil &&
			cur.Status.Serving.Generation == cur.Generation
	})
	return pods
}
