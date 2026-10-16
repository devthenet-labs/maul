// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"bytes"
	"math/rand"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

const testSignIn = "preview-auth.example.com"

func newHosts(t testing.TB) Hosts {
	t.Helper()
	h, err := NewHosts(testSuffix, testSignIn)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestNewHosts(t *testing.T) {
	for _, tc := range []struct{ suffix, signIn string }{
		{"preview.example.com", "x.preview.example.com"},
		{"preview.example.com", "preview.example.com"},
		{"Preview.example.com", "auth.example.com"},
		{"preview.example.com", "auth"},
		{"com", "auth.example.com"},
	} {
		if _, err := NewHosts(tc.suffix, tc.signIn); err == nil {
			t.Errorf("NewHosts(%q, %q) accepted", tc.suffix, tc.signIn)
		}
	}
}

func TestHostsExamples(t *testing.T) {
	h := newHosts(t)
	for host, want := range map[string]string{
		"demo-7.preview.example.com":     "demo-7",
		"demo-7.preview.example.com:443": "demo-7",
		"a.preview.example.com":          "a",
	} {
		if got, ok := h.Preview(host); !ok || got != want {
			t.Errorf("Preview(%q) = %q, %v", host, got, ok)
		}
	}
	for _, host := range []string{
		"", "preview.example.com", ".preview.example.com", "Demo-7.preview.example.com",
		"demo-7.preview.example.com.", "demo-7.preview.example.com:8443", "demo-7.preview.example.com:",
		"a.b.preview.example.com", "xn--bcher-kva.preview.example.com", "-a.preview.example.com",
		"demo-7.preview.example.co", "demo-7.preview.example.com:443:443", "[::1]:443", "demo-7.PREVIEW.example.com",
		"preview-auth.example.com", "demo 7.preview.example.com",
	} {
		if got, ok := h.Preview(host); ok {
			t.Errorf("Preview(%q) = %q, accepted", host, got)
		}
	}
	for host, want := range map[string]bool{
		testSignIn: true, testSignIn + ":443": true, testSignIn + ".": false, "Preview-auth.example.com": false,
		testSignIn + ":80": false, "x." + testSignIn: false,
	} {
		if h.IsSignIn(host) != want {
			t.Errorf("IsSignIn(%q) = %v", host, !want)
		}
	}
	const wantHandoff = "https://demo-7.preview.example.com/.patchy/callback?code=pa1.o.1.abc"
	if got := h.HandoffURL("demo-7", "pa1.o.1.abc"); got != wantHandoff {
		t.Errorf("HandoffURL = %s", got)
	}
}

// mutateHost turns a valid preview host into a near miss, or leaves it.
func mutateHost(r *rand.Rand, label string) string {
	host := label + "." + testSuffix
	switch r.Intn(12) {
	case 0:
		return host
	case 1:
		return host + ":443"
	case 2:
		return host + "."
	case 3:
		return host + ":" + []string{"80", "8443", "", "0443", "443 "}[r.Intn(5)]
	case 4:
		i := r.Intn(len(host))
		return host[:i] + strings.ToUpper(host[i:i+1]) + host[i+1:]
	case 5:
		return randLabel(r) + "." + host
	case 6:
		return strings.TrimSuffix(host, ".com") + ".org"
	case 7:
		return host[:r.Intn(len(host))]
	case 8:
		i := r.Intn(len(host))
		const odd = "\x00 /\\@#%_."
		return host[:i] + string(odd[r.Intn(len(odd))]) + host[i:]
	case 9:
		return "x" + testSignIn
	case 10:
		return label + "-." + testSuffix
	}
	return label[:min(2, len(label))] + "--" + label + "." + testSuffix
}

// TestHostGrammarProperty: a Host is accepted exactly when it is
// <label>.<suffix> or that with ":443", for a label ValidLabel accepts, and
// the label returned is that one.
func TestHostGrammarProperty(t *testing.T) {
	h := newHosts(t)
	cfg := quickConfig(20000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		args[0] = reflect.ValueOf(mutateHost(rnd, randLabel(rnd)))
	}
	prop := func(host string) bool {
		label, ok := h.Preview(host)
		name := strings.TrimSuffix(host, ":443")
		l, under := strings.CutSuffix(name, "."+testSuffix)
		want := under && ValidLabel(l) && len(name) <= maxHost
		if ok != want {
			return false
		}
		return !ok || label == l && (host == l+"."+testSuffix || host == l+"."+testSuffix+":443")
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// randReturn is a request path-and-query of hostile shape.
func randReturn(r *rand.Rand) string {
	const alpha = "abc/\\?#%@:.;=&+ \t\r\n\x00\x7f\xc3\xa9~-_'\"<>[]{}|^`"
	var b strings.Builder
	if r.Intn(3) > 0 {
		b.WriteByte('/')
	}
	for range r.Intn(40) {
		b.WriteByte(alpha[r.Intn(len(alpha))])
	}
	return b.String()
}

// TestReturnPathProperty: whatever path a sign-in saves, the Location it
// returns to is an absolute https URL on the requesting preview host, with
// no userinfo or fragment, and a valid path built only of safe bytes is kept
// byte for byte.
func TestReturnPathProperty(t *testing.T) {
	h := newHosts(t)
	cfg := quickConfig(20000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		args[0] = reflect.ValueOf(mutateHost(rnd, randLabel(rnd)))
		args[1] = reflect.ValueOf(randReturn(rnd))
	}
	prop := func(host, ret string) bool {
		label, ok := h.Preview(host)
		if !ok {
			return true
		}
		loc := h.Location(label, ret)
		u, err := url.Parse(loc)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
			return false
		}
		reqHost, _, _ := strings.Cut(host, ":")
		if u.Host != reqHost || u.Hostname() != reqHost {
			return false
		}
		// A browser resolving the Location against any base lands on it too.
		base, _ := url.Parse("https://other." + testSuffix + "/x/y")
		if base.ResolveReference(u).Host != reqHost {
			return false
		}
		if !strings.HasPrefix(u.EscapedPath(), "/") || strings.HasPrefix(u.EscapedPath(), "//") {
			return false
		}
		for i := len("https://"); i < len(loc); i++ {
			if !locationSafe(loc[i]) {
				return false
			}
		}
		if ValidReturnPath(ret) && !strings.Contains(ret, "%") &&
			strings.IndexFunc(ret, func(c rune) bool { return !locationSafe(byte(c)) }) < 0 {
			return loc == h.Origin(label)+ret
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestReturnPathExamples(t *testing.T) {
	h := newHosts(t)
	for ret, want := range map[string]string{
		"/":                "https://a.preview.example.com/",
		"/x?y=1":           "https://a.preview.example.com/x?y=1",
		"//evil.example/x": "https://a.preview.example.com/",
		"/\\evil.example":  "https://a.preview.example.com/",
		"/a\\b":            "https://a.preview.example.com/a%5Cb",
		"/a#frag":          "https://a.preview.example.com/a%23frag",
		"/a\nb":            "https://a.preview.example.com/",
		"https://evil/":    "https://a.preview.example.com/",
		"":                 "https://a.preview.example.com/",
		// TestReturnPathProperty's shrunk counterexample: a bare "%" made the
		// Location unparseable.
		"/%":     "https://a.preview.example.com/%25",
		"/%4":    "https://a.preview.example.com/%254",
		"/%41%g": "https://a.preview.example.com/%41%25g",
	} {
		if got := h.Location("a", ret); got != want {
			t.Errorf("Location(%q) = %s, want %s", ret, got, want)
		}
	}
}

// gatewayCookieNames are spellings of the gateway's cookies a hostile
// preview might use.
var gatewayCookieNames = []string{
	"__Host-patchy-pv", "__host-patchy-pv", "__HOST-PATCHY-PV", "__Host-patchy-pv-state-0123456789abcdef",
	"patchy-pv", " __Host-patchy-pv", "__Secure-patchy-pv", "__Host-patchy-pa", "__Host-patchy-pa-login-ab",
	"PATCHY-PA", "__Host-patchy-pvx",
}

var otherCookieNames = []string{"session", "csrftoken", "patchy", "patchy-p", "__Host-app", "x-patchy-pv", "_ga",
	"pv", "my-patchy-pv"}

func randCookieName(r *rand.Rand) string {
	if r.Intn(2) == 0 {
		return gatewayCookieNames[r.Intn(len(gatewayCookieNames))]
	}
	return otherCookieNames[r.Intn(len(otherCookieNames))]
}

// identityNames are inbound headers that could assert a viewer or a source.
var identityNames = []string{
	"X-Patchy-Viewer", "x-patchy-user", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	"X-Forwarded-User", "X-Forwarded-Email", "X-Amzn-Oidc-Data", "X-Amzn-Oidc-Identity", "X-Amzn-Oidc-Accesstoken",
	"Forwarded", "X-Real-Ip", "X-Auth-Request-User", "Remote-User", "X-Remote-User", "Cf-Access-Jwt-Assertion",
	"X-Original-Forwarded-For",
}

var plainNames = []string{"Accept", "User-Agent", "Authorization", "X-Requested-With", "Content-Type", "X-Custom",
	"Accept-Language", "If-None-Match"}

// TestCleanRequestProperty: after CleanRequest, no gateway cookie and no
// inbound identity header reaches the preview; the only identity headers are
// the gateway's own values; every other header and cookie pair is kept, in
// order.
func TestCleanRequestProperty(t *testing.T) {
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		h := http.Header{}
		for range rnd.Intn(8) {
			if rnd.Intn(2) == 0 {
				name := identityNames[rnd.Intn(len(identityNames))]
				if rnd.Intn(2) == 0 {
					name = underscored(rnd, name)
				}
				h[name] = []string{randString(rnd, 1+rnd.Intn(10))}
			} else {
				h.Add(plainNames[rnd.Intn(len(plainNames))], randString(rnd, 1+rnd.Intn(10)))
			}
		}
		for range rnd.Intn(3) {
			var pairs []string
			for range 1 + rnd.Intn(4) {
				name := randCookieName(rnd)
				if rnd.Intn(8) == 0 {
					pairs = append(pairs, name) // no "=": a nameless cookie
				} else {
					pairs = append(pairs, name+"="+randToken(rnd))
				}
			}
			h.Add("Cookie", strings.Join(pairs, "; "))
		}
		args[0] = reflect.ValueOf(h)
	}
	prop := func(in http.Header) bool {
		var wantPairs []string
		for _, line := range in.Values("Cookie") {
			for p := range strings.SplitSeq(line, ";") {
				p = strings.TrimSpace(p)
				name, _, _ := strings.Cut(p, "=")
				if p != "" && !slices.Contains(gatewayCookieNames, name) &&
					!slices.Contains(gatewayCookieNames, " "+name) {
					wantPairs = append(wantPairs, p)
				}
			}
		}
		out := in.Clone()
		CleanRequest(out, ForwardedFor{Viewer: "sub-1", ClientIP: "203.0.113.7", Host: "demo." + testSuffix})
		for name, vals := range out {
			if !cleanedHeaderOK(name, vals, in) {
				return false
			}
		}
		var gotPairs []string
		if c := out.Get("Cookie"); c != "" {
			gotPairs = strings.Split(c, "; ")
		}
		for _, p := range gotPairs {
			name, _, _ := strings.Cut(p, "=")
			if GatewayCookie(name) {
				return false
			}
		}
		return slices.Equal(gotPairs, wantPairs)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// cleanedHeaderOK judges one header CleanRequest left: the gateway's own
// with its value, one Cookie line, or an untouched header that asserts
// nothing.
func cleanedHeaderOK(name string, vals []string, in http.Header) bool {
	own := map[string]string{"X-Patchy-Viewer": "sub-1", "X-Forwarded-For": "203.0.113.7",
		"X-Forwarded-Proto": "https", "X-Forwarded-Host": "demo." + testSuffix}
	if want, ok := own[name]; ok {
		return slices.Equal(vals, []string{want})
	}
	// A server that folds "_" and "-" together reads both spellings as one.
	folded := strings.ReplaceAll(strings.ToLower(name), "_", "-")
	if identityHeader(name) || slices.ContainsFunc(identityNames, func(n string) bool {
		return strings.ToLower(n) == folded
	}) {
		return false
	}
	if name == "Cookie" {
		return len(vals) == 1
	}
	return slices.Equal(vals, in[name])
}

// underscored spells some of name's "-" as "_", the spelling a CGI-style
// server folds into the same variable.
func underscored(r *rand.Rand, name string) string {
	b := []byte(name)
	for i, c := range b {
		if c == '-' && r.Intn(2) == 0 {
			b[i] = '_'
		}
	}
	if i := bytes.IndexByte(b, '-'); i >= 0 && !bytes.Contains(b, []byte("_")) {
		b[i] = '_'
	}
	return textproto.CanonicalMIMEHeaderKey(string(b))
}

// TestCleanRequestUnderscores pins the underscore spellings of identity
// headers: Go keeps them as distinct names, so they used to pass beside the
// gateway's own X-Patchy-Viewer.
func TestCleanRequestUnderscores(t *testing.T) {
	h := http.Header{}
	for _, raw := range []string{"x-patchy_viewer", "X_Forwarded_For", "x_forwarded-host", "X-Amzn-Oidc_Data",
		"Remote_User", "X_Real_IP"} {
		h[textproto.CanonicalMIMEHeaderKey(raw)] = []string{"forged"}
	}
	h.Set("X_App_Header", "kept")
	CleanRequest(h, ForwardedFor{Viewer: "sub-1", ClientIP: "203.0.113.7", Host: "demo." + testSuffix})
	for name, vals := range h {
		if slices.Contains(vals, "forged") {
			t.Errorf("%s reached the preview", name)
		}
	}
	if h.Get("X_App_Header") != "kept" || h.Get(ViewerHeader) != "sub-1" {
		t.Errorf("headers after cleaning: %v", h)
	}
}

func randToken(r *rand.Rand) string {
	const alpha = "abcdefXYZ0123=._-"
	b := make([]byte, 1+r.Intn(12))
	for i := range b {
		b[i] = alpha[r.Intn(len(alpha))]
	}
	return string(b)
}

// TestFilterSetCookiesProperty: no upstream Set-Cookie that names a gateway
// cookie survives, however cased or prefixed, and every other one does, in
// order and byte for byte.
func TestFilterSetCookiesProperty(t *testing.T) {
	attrs := []string{"", "; Path=/", "; Secure; HttpOnly", "; Max-Age=0", "; Domain=preview.example.com",
		"; SameSite=None; Secure"}
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		n := rnd.Intn(6)
		lines := make([]string, 0, n)
		for range n {
			name := randCookieName(rnd)
			line := name + "=" + randToken(rnd) + attrs[rnd.Intn(len(attrs))]
			if rnd.Intn(8) == 0 {
				line = name + attrs[rnd.Intn(len(attrs))]
			}
			lines = append(lines, line)
		}
		args[0] = reflect.ValueOf(lines)
	}
	prop := func(lines []string) bool {
		h := http.Header{"Set-Cookie": slices.Clone(lines), "Content-Type": {"text/html"}}
		FilterSetCookies(h)
		var want []string
		for _, l := range lines {
			nv, _, _ := strings.Cut(l, ";")
			name, _, _ := strings.Cut(nv, "=")
			lower := strings.ToLower(strings.TrimSpace(name))
			lower = strings.TrimPrefix(strings.TrimPrefix(lower, "__host-"), "__secure-")
			if !strings.HasPrefix(lower, "patchy-pv") && !strings.HasPrefix(lower, "patchy-pa") {
				want = append(want, l)
			}
		}
		return slices.Equal(h.Values("Set-Cookie"), want) && h.Get("Content-Type") == "text/html"
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestCheckOriginProperty: the origin rule as an independent statement. A
// request passes exactly when it is not a non-navigation from another site
// or preview, and, when it is unsafe or an upgrade, carries exactly this
// host's origin.
func TestCheckOriginProperty(t *testing.T) {
	const own = "https://demo." + testSuffix
	sites := []string{"", "same-origin", "same-site", "cross-site", "none", "Same-Site", "CROSS-SITE"}
	modes := []string{"", "navigate", "cors", "no-cors", "websocket", "same-origin", "Navigate"}
	dests := []string{"", "document", "iframe", "empty", "script", "Document"}
	origins := [][]string{nil, {own}, {"https://other." + testSuffix}, {"null"}, {own, own}, {own + ":443"},
		{"http://demo." + testSuffix}}
	methods := []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}
	cfg := quickConfig(20000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		h := http.Header{}
		set := func(k string, vals []string) {
			if v := vals[rnd.Intn(len(vals))]; v != "" {
				h.Set(k, v)
			}
		}
		set("Sec-Fetch-Site", sites)
		set("Sec-Fetch-Mode", modes)
		set("Sec-Fetch-Dest", dests)
		if o := origins[rnd.Intn(len(origins))]; o != nil {
			h["Origin"] = o
		}
		if rnd.Intn(3) == 0 {
			h.Set("Upgrade", "websocket")
		}
		args[0] = reflect.ValueOf(methods[rnd.Intn(len(methods))])
		args[1] = reflect.ValueOf(h)
	}
	prop := func(method string, h http.Header) bool {
		site := strings.ToLower(h.Get("Sec-Fetch-Site"))
		nav := strings.ToLower(h.Get("Sec-Fetch-Mode")) == "navigate" &&
			strings.ToLower(h.Get("Sec-Fetch-Dest")) == "document"
		fromOther := site == "same-site" || site == "cross-site"
		needsOrigin := (method != "GET" && method != "HEAD") || h.Get("Upgrade") != ""
		ownOrigin := len(h["Origin"]) == 1 && h["Origin"][0] == own
		want := (!fromOther || nav) && (!needsOrigin || ownOrigin)
		return (CheckOrigin(method, h, own) == nil) == want
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestCrossPreviewWebsocket pins the motivating example: another preview's
// page opening a WebSocket to this one, with and without Sec-Fetch headers.
func TestCrossPreviewWebsocket(t *testing.T) {
	const own = "https://demo." + testSuffix
	withFetch := http.Header{"Upgrade": {"websocket"}, "Origin": {"https://evil." + testSuffix},
		"Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"websocket"}, "Sec-Fetch-Dest": {"empty"}}
	if CheckOrigin("GET", withFetch, own) == nil {
		t.Error("a same-site WebSocket from another preview passed")
	}
	noFetch := http.Header{"Upgrade": {"websocket"}, "Origin": {"https://evil." + testSuffix}}
	if CheckOrigin("GET", noFetch, own) == nil {
		t.Error("a WebSocket from another preview's origin passed without Sec-Fetch")
	}
	mine := http.Header{"Upgrade": {"websocket"}, "Origin": {own}, "Sec-Fetch-Site": {"same-origin"}}
	if err := CheckOrigin("GET", mine, own); err != nil {
		t.Errorf("the preview's own WebSocket: %v", err)
	}
	post := http.Header{"Origin": {"https://evil." + testSuffix}, "Sec-Fetch-Site": {"same-site"},
		"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}}
	if CheckOrigin("POST", post, own) == nil {
		t.Error("a cross-preview form POST passed")
	}
}

// TestPrivateCacheControlProperty: a proxied response is never cacheable by a
// shared cache: private always, public and s-maxage never, no-store on a
// status other than 2xx and 304, and every other directive kept in order.
func TestPrivateCacheControlProperty(t *testing.T) {
	dirs := []string{"public", "PUBLIC", "s-maxage=600", "S-MaxAge = 5", "max-age=60", "no-cache", "immutable",
		"private", "no-store", "must-revalidate", "stale-while-revalidate=30", ""}
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		nv := rnd.Intn(3)
		vals := make([]string, 0, nv)
		for range nv {
			nd := rnd.Intn(4)
			ds := make([]string, 0, nd)
			for range nd {
				ds = append(ds, dirs[rnd.Intn(len(dirs))])
			}
			vals = append(vals, strings.Join(ds, ", "))
		}
		args[0] = reflect.ValueOf([]int{200, 204, 206, 301, 302, 304, 400, 404, 500}[rnd.Intn(9)])
		args[1] = reflect.ValueOf(vals)
	}
	prop := func(status int, vals []string) bool {
		got := strings.Split(PrivateCacheControl(status, vals), ", ")
		names := map[string]bool{}
		for _, d := range got {
			n, _, _ := strings.Cut(d, "=")
			n = strings.ToLower(strings.TrimSpace(n))
			if n == "public" || n == "s-maxage" || n == "" {
				return false
			}
			names[n] = true
		}
		ok2xx := status >= 200 && status < 300 || status == 304
		if !names["private"] || !ok2xx && !names["no-store"] {
			return false
		}
		var kept []string
		for _, v := range vals {
			for d := range strings.SplitSeq(v, ",") {
				d = strings.TrimSpace(d)
				n, _, _ := strings.Cut(d, "=")
				n = strings.ToLower(strings.TrimSpace(n))
				if d != "" && n != "public" && n != "s-maxage" {
					kept = append(kept, d)
				}
			}
		}
		return slices.Equal(got[:len(kept)], kept)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestCleanResponse(t *testing.T) {
	h := http.Header{
		"Set-Cookie":                       {"__Host-patchy-pv=x; Path=/; Secure", "app=1; Path=/"},
		"Clear-Site-Data":                  {`"cookies"`},
		"Access-Control-Allow-Credentials": {"true"},
		"Access-Control-Allow-Origin":      {"https://other." + testSuffix},
		"Service-Worker-Allowed":           {"/"},
		"Cache-Control":                    {"public, max-age=600"},
		"Vary":                             {"Accept-Encoding"},
		"X-App":                            {"kept"},
	}
	CleanResponse(200, h)
	want := http.Header{
		"Set-Cookie":                  {"app=1; Path=/"},
		"Access-Control-Allow-Origin": {"https://other." + testSuffix},
		"Cache-Control":               {"max-age=600, private"},
		"Vary":                        {"Accept-Encoding", "Cookie"},
		"X-App":                       {"kept"},
	}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("CleanResponse = %v\nwant %v", h, want)
	}
	h = http.Header{"Vary": {"*"}}
	CleanResponse(404, h)
	if h.Get("Cache-Control") != "private, no-store" || len(h.Values("Vary")) != 1 {
		t.Errorf("a 404 = %v", h)
	}
}

func TestTopLevelNavigation(t *testing.T) {
	for _, tc := range []struct {
		method string
		h      http.Header
		want   bool
	}{
		{"GET", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}}, true},
		{"GET", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}}, false},
		{"GET", http.Header{"Sec-Fetch-Mode": {"cors"}, "Accept": {"text/html"}}, false},
		{"POST", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}}, false},
		{"GET", http.Header{"Accept": {"text/html,application/xhtml+xml"}}, true},
		{"GET", http.Header{"Accept": {"application/json"}}, false},
		{"GET", http.Header{}, false},
	} {
		if got := TopLevelNavigation(tc.method, tc.h); got != tc.want {
			t.Errorf("TopLevelNavigation(%s, %v) = %v", tc.method, tc.h, got)
		}
	}
}

func TestReservedPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/.patchy": true, "/.patchy/": true, "/.patchy/callback": true, "/.patchy/healthz": true,
		"//.patchy/callback": true, "/a/../.patchy/x": true, "/./.patchy/x": true,
		"/": false, "/.patchyx": false, "/.PATCHY/callback": false, "/app/.patchy/x": false, "/patchy/": false,
	} {
		if ReservedPath(p) != want {
			t.Errorf("ReservedPath(%q) = %v", p, !want)
		}
	}
}

func TestServiceWorkerScript(t *testing.T) {
	if !IsServiceWorkerScript(http.Header{"Service-Worker": {"script"}}) ||
		IsServiceWorkerScript(http.Header{}) {
		t.Error("IsServiceWorkerScript misreads the header")
	}
}
