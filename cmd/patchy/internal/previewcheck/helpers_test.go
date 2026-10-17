// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// The checks' end-to-end behaviour is driven through `check project` in
// projectcheck's tests; these pin the helpers and edges underneath.

// dump renders lines for a failure message.
func dump(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(string(l.Status) + " " + l.Name + ": " + l.Reason + "\n")
	}
	return b.String()
}

// expect fails unless exactly one line is check's, with status and every
// wanted reason fragment.
func expect(t *testing.T, lines []Line, check string, status checkreport.Status, want ...string) {
	t.Helper()
	var got []Line
	for _, l := range lines {
		if l.Name == check {
			got = append(got, l)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d %s lines:\n%s", len(got), check, dump(lines))
	}
	if got[0].Status != status {
		t.Errorf("%s = %s, want %s: %s", check, got[0].Status, status, got[0].Reason)
	}
	for _, w := range want {
		if !strings.Contains(got[0].Reason, w) {
			t.Errorf("%s reason %q lacks %q", check, got[0].Reason, w)
		}
	}
}

func TestEffectivePort(t *testing.T) {
	u := func(s string) *url.URL {
		parsed, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	ports := map[string]string{"https://x": "443", "HTTP://x": "80", "https://x:8443": "8443", "ftp://x": ""}
	for raw, want := range ports {
		if got := effectivePort(u(raw)); got != want {
			t.Errorf("effectivePort(%s) = %q, want %q", raw, got, want)
		}
	}
}

// TestJudgeDex: each shape of Dex's final answer maps onto its verdict.
func TestJudgeDex(t *testing.T) {
	cases := []struct {
		name   string
		status int
		page   string
		want   checkreport.Status
		reason string
	}{
		{"accepted", http.StatusOK, "<html>login</html>", checkreport.Pass, "accepts client"},
		{"bad redirect", http.StatusBadRequest, "Unregistered redirect_uri", checkreport.Fail, "refuses redirect URI"},
		{"unknown client", http.StatusNotFound, "not found", checkreport.Fail, "has no client"},
		{"client id named", http.StatusBadRequest, "Invalid client_id", checkreport.Fail, "has no client"},
		{"other refusal", http.StatusForbidden, "denied", checkreport.Fail, "refused a sign-in"},
		{"server error", http.StatusBadGateway, "upstream", checkreport.Skip, "says nothing about the client"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := &Checker{}
			ch.judgeDex("https://dex.example.com", "preview-auth", "https://relay/dex/callback", c.status, c.page)
			expect(t, ch.Lines(), CheckPreviewAuthDex, c.want, c.reason)
		})
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// TestPreviewAuthHostsEdges: before sign-in is required nothing is probed;
// an unusable suffix skips; a failed Preview list still probes the
// placeholder; only the Project's Ready, slotted, live Previews are probed;
// a transport error fails the host.
func TestPreviewAuthHostsEdges(t *testing.T) {
	scope := Config{Namespace: "patchy", Project: "demo"}

	ch := &Checker{cfg: scope}
	ch.previewAuthHosts(context.Background(), "https://relay.example.com", "preview.example.com", false)
	expect(t, ch.Lines(), CheckPreviewAuthHost, checkreport.Skip, "stage=permit")

	ch = &Checker{cfg: scope}
	ch.previewAuthHosts(context.Background(), "https://relay.example.com", "not a suffix", true)
	expect(t, ch.Lines(), CheckPreviewAuthHost, checkreport.Skip, "unusable")

	var probed []string
	web := doerFunc(func(req *http.Request) (*http.Response, error) {
		probed = append(probed, req.URL.Host)
		return nil, errors.New("tls: handshake failure")
	})
	failingList := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	cfg := scope
	cfg.Reader, cfg.HTTP = failingList, web
	ch = &Checker{cfg: cfg}
	ch.previewAuthHosts(context.Background(), "https://relay.example.com", "preview.example.com", true)
	lines := ch.Lines()
	if len(lines) != 2 {
		t.Fatalf("lines = %s", dump(lines))
	}
	if l := lines[0]; l.Status != checkreport.Skip || !strings.Contains(l.Reason, "only the placeholder") {
		t.Errorf("list failure line = %+v", l)
	}
	if l := lines[1]; l.Status != checkreport.Fail || !strings.Contains(l.Reason, "handshake failure") {
		t.Errorf("transport failure line = %+v", l)
	}

	slot := func(n int32) *int32 { return &n }
	preview := func(name, projectName string, phase v1alpha1.PreviewPhase, s *int32) *v1alpha1.Preview {
		return &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
			Spec:   v1alpha1.PreviewSpec{Project: projectName, HostLabel: name},
			Status: v1alpha1.PreviewStatus{Phase: phase, Slot: s}}
	}
	reader := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(
		preview("demo-1", "demo", v1alpha1.PreviewReady, slot(1)),
		preview("demo-2", "demo", v1alpha1.PreviewDeploying, slot(0)),
		preview("demo-3", "demo", v1alpha1.PreviewReady, nil),
		preview("other-1", "other", v1alpha1.PreviewReady, slot(0)),
	).Build()
	probed = nil
	cfg.Reader = reader
	ch = &Checker{cfg: cfg}
	ch.previewAuthHosts(context.Background(), "https://relay.example.com", "preview.example.com", true)
	want := []string{"placeholder.preview.example.com", "demo-1.preview.example.com"}
	if strings.Join(probed, ",") != strings.Join(want, ",") {
		t.Errorf("probed = %v, want %v", probed, want)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("body went away") }

func TestHTTPGetEdges(t *testing.T) {
	ch := &Checker{cfg: Config{HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://next"}},
			Body: io.NopCloser(failingReader{})}, nil
	})}}
	if _, _, _, err := ch.httpGet(context.Background(), "https://x/"); err == nil ||
		!strings.Contains(err.Error(), "read the answer") {
		t.Errorf("httpGet with an unreadable body = %v", err)
	}
	if _, _, _, err := ch.httpGet(context.Background(), "https://x/\x7f"); err == nil {
		t.Error("httpGet of an unparseable URL succeeded")
	}
}

// TestNewHTTPDoerNeverFollows: the CLI's doer reports a redirect as the
// answer instead of following it.
func TestNewHTTPDoerNeverFollows(t *testing.T) {
	followed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			followed = true
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	resp, err := NewHTTPDoer().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/next" || followed {
		t.Errorf("status %d, Location %q, followed %v", resp.StatusCode, resp.Header.Get("Location"), followed)
	}
}

// TestDialTLSRefused: the CLI's dialer reports a host that is not there.
func TestDialTLSRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DialTLS(ctx, "preview.invalid"); err == nil {
		t.Error("DialTLS with a cancelled context succeeded")
	}
}

// TestExcerpt quotes the sentence holding the key, bounded in runes.
func TestExcerpt(t *testing.T) {
	tests := []struct{ text, key, want string }{
		{"dex Bad Request Unregistered redirect_uri (x).", "redirect_uri",
			"dex Bad Request Unregistered redirect_uri (x)."},
		{"Title. Invalid client_id (y).", "client_id", "Invalid client_id (y)."},
		{"no key here", "client_id", "no key here"},
		{strings.Repeat("é", 200), "", strings.Repeat("é", excerptRunes) + "..."},
	}
	for _, tc := range tests {
		if got := excerpt(tc.text, tc.key); got != tc.want {
			t.Errorf("excerpt(%q, %q) = %q, want %q", tc.text, tc.key, got, tc.want)
		}
	}
	if got := pageText([]byte("<p>a\n  <b>b</b></p>\tc")); got != "a b c" {
		t.Errorf("pageText = %q", got)
	}
}

func TestAllPrivate(t *testing.T) {
	for addrs, want := range map[string]bool{
		"10.0.0.1": true, "100.64.0.7": true, "192.168.1.2,172.16.0.1": true, "fd00::1": true, "127.0.0.1": true,
		"192.0.2.10": false, "10.0.0.1,192.0.2.10": false, "": false, "not-an-ip": false,
	} {
		var list []string
		if addrs != "" {
			list = strings.Split(addrs, ",")
		}
		if got := allPrivate(list); got != want {
			t.Errorf("allPrivate(%q) = %v, want %v", addrs, got, want)
		}
	}
}
