// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
)

// The preview gateway's proxy hygiene (design §2.1 and Revision 1's
// contract). Preview code is hostile and same-origin with its own host, and
// every preview host is same-site with every other, so each rule here is
// security-critical and pinned by a seeded property.

// Hosts is the gateway's Host grammar: preview hosts <label>.<suffix> and
// the one sign-in host.
type Hosts struct {
	suffix string
	signIn string
}

// NewHosts validates the suffix and the sign-in host (both lowercase DNS
// names of two or more labels) and refuses a sign-in host under the suffix.
func NewHosts(suffix, signIn string) (Hosts, error) {
	if !validSuffix(suffix) {
		return Hosts{}, fmt.Errorf("previewauth: host suffix %q is not a lowercase DNS name of two or more labels",
			suffix)
	}
	if !validSuffix(signIn) {
		return Hosts{}, fmt.Errorf("previewauth: sign-in host %q is not a lowercase DNS name of two or more labels",
			signIn)
	}
	if signIn == suffix || strings.HasSuffix(signIn, "."+suffix) {
		return Hosts{}, fmt.Errorf("previewauth: the sign-in host %s sits under the preview host suffix %s", signIn,
			suffix)
	}
	return Hosts{suffix: suffix, signIn: signIn}, nil
}

// Suffix is the preview host suffix.
func (h Hosts) Suffix() string { return h.suffix }

// SignIn is the sign-in host.
func (h Hosts) SignIn() string { return h.signIn }

// stripPort removes an explicit ":443", the only port a Host may carry (the
// edge terminates TLS on 443, and a browser leaves the default port out).
// Any other port, an empty one or a bracketed literal is refused.
func stripPort(host string) (string, bool) {
	name, port, found := strings.Cut(host, ":")
	if !found {
		return host, true
	}
	return name, port == "443"
}

// Preview returns the label of a Host header that is exactly
// <label>.<suffix>, optionally with ":443": lowercase, no trailing dot, a
// label ValidLabel accepts, one label deep. X-Forwarded-Host is never read.
func (h Hosts) Preview(host string) (string, bool) {
	if h.suffix == "" || len(host) > maxHost+4 {
		return "", false
	}
	name, ok := stripPort(host)
	if !ok {
		return "", false
	}
	label, ok := strings.CutSuffix(name, "."+h.suffix)
	if !ok || !ValidLabel(label) || len(name) > maxHost {
		return "", false
	}
	return label, true
}

// IsSignIn reports whether a Host header is exactly the sign-in host,
// optionally with ":443".
func (h Hosts) IsSignIn(host string) bool {
	name, ok := stripPort(host)
	return ok && h.signIn != "" && name == h.signIn
}

// Origin is the preview host's origin, https://<label>.<suffix>.
func (h Hosts) Origin(label string) string { return "https://" + label + "." + h.suffix }

// HandoffURL is where the sign-in host sends a handoff code.
func (h Hosts) HandoffURL(label, code string) string {
	return h.Origin(label) + HandoffPath + "?code=" + code
}

// StartURL is where a preview host sends a browser to sign in.
func (h Hosts) StartURL(label, sealedState string) string {
	return "https://" + h.signIn + "/start?h=" + label + "&s=" + sealedState
}

// ValidReturnPath reports whether p is a path a sign-in may return to: 1 to
// 2048 bytes, starting with exactly one "/" that is not followed by "/" or
// "\", and no control character, space or non-ASCII byte. Anything else
// returns to "/".
func ValidReturnPath(p string) bool {
	if p == "" || len(p) > maxReturn || p[0] != '/' {
		return false
	}
	if len(p) > 1 && (p[1] == '/' || p[1] == '\\') {
		return false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c <= ' ' || c >= 0x7f {
			return false
		}
	}
	return true
}

// Location is the absolute URL a sign-in returns to on label's host:
// its origin and the return path with every byte that is not a URL path,
// query or escape character percent-escaped. A path ValidReturnPath refuses
// is "/". Its host is always label's, whatever the path.
func (h Hosts) Location(label, ret string) string {
	if !ValidReturnPath(ret) {
		ret = "/"
	}
	var b strings.Builder
	b.WriteString(h.Origin(label))
	for i := 0; i < len(ret); i++ {
		c := ret[i]
		// A "%" stays only where it starts an escape; a bare one would make
		// the Location unparseable.
		if c == '%' && (i+2 >= len(ret) || !isHex(ret[i+1]) || !isHex(ret[i+2])) {
			b.WriteString("%25")
			continue
		}
		if locationSafe(c) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// locationSafe is RFC 3986's unreserved, sub-delims, ":", "@", "/", "?" and
// "%": what a path and query may hold literally. "\" and "#" are escaped.
func locationSafe(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:@/?%", c) >= 0
}

// TopLevelNavigation reports whether a request is a browser's top-level
// navigation: Sec-Fetch-Mode navigate with Sec-Fetch-Dest document, or, from
// a browser that sends no Sec-Fetch headers, a GET whose Accept names
// text/html. Only these get a state cookie and a redirect to sign in; every
// other unauthenticated request gets a 401, so background fetches and
// hot-reload sockets do not start sign-in loops.
func TopLevelNavigation(method string, h http.Header) bool {
	if method != http.MethodGet {
		return false
	}
	if mode := h.Get("Sec-Fetch-Mode"); mode != "" {
		return strings.EqualFold(mode, "navigate") && strings.EqualFold(h.Get("Sec-Fetch-Dest"), "document")
	}
	return strings.Contains(strings.ToLower(strings.Join(h.Values("Accept"), ",")), "text/html")
}

// ErrCrossOrigin means a request to a preview host came from another site or
// another preview, other than as a top-level navigation, or an unsafe or
// upgrade request did not come from the host's own origin.
var ErrCrossOrigin = errors.New("previewauth: request did not come from this preview")

// CheckOrigin is the request-origin rule for every proxied request:
//
//   - Sec-Fetch-Site same-site or cross-site is refused unless the request is
//     a top-level navigation (Sec-Fetch-Mode navigate, Sec-Fetch-Dest
//     document);
//   - an Upgrade, or any method but GET and HEAD, must carry exactly one
//     Origin equal to origin, so browsers without Sec-Fetch are covered too.
//
// Together they close cross-preview CSRF and WebSocket hijacking: every
// preview host is same-site with every other, so SameSite cookies do not.
func CheckOrigin(method string, h http.Header, origin string) error {
	switch strings.ToLower(h.Get("Sec-Fetch-Site")) {
	case "same-site", "cross-site":
		if !strings.EqualFold(h.Get("Sec-Fetch-Mode"), "navigate") ||
			!strings.EqualFold(h.Get("Sec-Fetch-Dest"), "document") {
			return ErrCrossOrigin
		}
	}
	unsafe := method != http.MethodGet && method != http.MethodHead
	if unsafe || len(h.Values("Upgrade")) > 0 {
		if o := h.Values("Origin"); len(o) != 1 || o[0] != origin {
			return ErrCrossOrigin
		}
	}
	return nil
}

// IsServiceWorkerScript reports a browser fetching a service worker script,
// which a preview host refuses: a worker would outlive the viewer's session
// and sit in front of every later request to the host.
func IsServiceWorkerScript(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get("Service-Worker")), "script")
}

// GatewayCookie reports whether a cookie name is one of the gateway's or the
// relay's, however it is cased and whether or not it carries a __Host- or
// __Secure- prefix: patchy-pv... (preview host session and state) and
// patchy-pa... (relay session and login). Preview code never sees one and
// can never set one.
func GatewayCookie(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, p := range []string{"__host-", "__secure-"} {
		if rest, ok := strings.CutPrefix(n, p); ok {
			n = rest
			break
		}
	}
	return strings.HasPrefix(n, "patchy-pv") || strings.HasPrefix(n, "patchy-pa")
}

// StripCookies removes every gateway cookie (GatewayCookie) from h's Cookie
// headers and folds the rest, in order and byte for byte, into one. A pair
// with no "=" is judged by the whole pair, as a browser names it.
func StripCookies(h http.Header) {
	var keep []string
	for _, line := range h.Values("Cookie") {
		for pair := range strings.SplitSeq(line, ";") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			name, _, _ := strings.Cut(pair, "=")
			if GatewayCookie(name) {
				continue
			}
			keep = append(keep, pair)
		}
	}
	h.Del("Cookie")
	if len(keep) > 0 {
		h.Set("Cookie", strings.Join(keep, "; "))
	}
}

// identityHeader reports a request header that could assert who the viewer
// is or where the request came from. Every one is dropped from a proxied
// request; the gateway then sets X-Patchy-Viewer and the X-Forwarded-*
// headers itself. Names are judged with "_" read as "-": CGI, WSGI and Rack
// servers fold both into one variable, so X-Patchy_Viewer or X_Forwarded_For
// would reach the preview as the gateway's own header.
func identityHeader(name string) bool {
	n := strings.ReplaceAll(strings.ToLower(name), "_", "-")
	for _, p := range []string{"x-patchy-", "x-forwarded-", "x-amzn-oidc-", "x-auth-request-", "x-remote-",
		"x-webauth-", "x-original-", "cf-access-", "x-goog-authenticated-", "x-ms-client-principal"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	switch n {
	case "forwarded", "remote-user", "x-real-ip", "x-client-ip", "x-user", "x-email", "x-username",
		"cf-connecting-ip", "true-client-ip":
		return true
	}
	return false
}

// ViewerHeader carries the viewer's pairwise subject on this Preview, and is
// all a preview learns about its viewer.
const ViewerHeader = "X-Patchy-Viewer"

// ForwardedFor is the gateway's own forwarding context for a proxied
// request.
type ForwardedFor struct {
	// Viewer is the pairwise subject (KeyRing.Sub).
	Viewer string
	// ClientIP is the viewer's address as the gateway judged it.
	ClientIP string
	// Host is the preview host.
	Host string
}

// CleanRequest strips every gateway cookie and every inbound identity or
// forwarding header from a proxied request's headers, then sets the
// gateway's own: X-Patchy-Viewer, X-Forwarded-For, -Proto and -Host.
// Authorization is the preview's own and passes untouched (the gateway never
// sets one).
func CleanRequest(h http.Header, f ForwardedFor) {
	StripCookies(h)
	for name := range h {
		if identityHeader(name) {
			delete(h, name)
		}
	}
	h.Set(ViewerHeader, f.Viewer)
	if f.ClientIP != "" {
		h.Set("X-Forwarded-For", f.ClientIP)
	}
	h.Set("X-Forwarded-Proto", "https")
	h.Set("X-Forwarded-Host", f.Host)
}

// FilterSetCookies drops every upstream Set-Cookie that names a gateway
// cookie, so a hostile preview can neither shadow nor delete the session or
// a state cookie. A Set-Cookie with no "=" before its first ";" is judged by
// that whole part, as a browser names it.
func FilterSetCookies(h http.Header) {
	vals := h.Values("Set-Cookie")
	if len(vals) == 0 {
		return
	}
	var keep []string
	for _, v := range vals {
		nv, _, _ := strings.Cut(v, ";")
		name, _, _ := strings.Cut(nv, "=")
		if GatewayCookie(name) {
			continue
		}
		keep = append(keep, v)
	}
	h.Del("Set-Cookie")
	for _, v := range keep {
		h.Add("Set-Cookie", v)
	}
}

// CleanResponse applies the response half of the contract to a proxied
// response's headers:
//
//   - Set-Cookie naming a gateway cookie is dropped (FilterSetCookies);
//   - Clear-Site-Data, Access-Control-Allow-Credentials and
//     Service-Worker-Allowed are removed;
//   - Cache-Control loses public and s-maxage and gains private, and
//     no-store too unless the status is 2xx or 304, so no shared cache ever
//     keeps a viewer's response; Vary gains Cookie.
//
// Everything else passes through untouched.
func CleanResponse(status int, h http.Header) {
	FilterSetCookies(h)
	h.Del("Clear-Site-Data")
	h.Del("Access-Control-Allow-Credentials")
	h.Del("Service-Worker-Allowed")
	h.Set("Cache-Control", PrivateCacheControl(status, h.Values("Cache-Control")))
	if !varies(h, "cookie") && !varies(h, "*") {
		h.Add("Vary", "Cookie")
	}
}

// PrivateCacheControl rewrites Cache-Control values for a proxied response:
// every directive but public and s-maxage is kept in order, then private is
// ensured, and no-store unless status is 2xx or 304 (a 304 revalidates a
// 2xx).
func PrivateCacheControl(status int, values []string) string {
	var out []string
	has := map[string]bool{}
	for _, v := range values {
		for d := range strings.SplitSeq(v, ",") {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			name, _, _ := strings.Cut(d, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "public" || name == "s-maxage" {
				continue
			}
			has[name] = true
			out = append(out, d)
		}
	}
	if !has["private"] {
		out = append(out, "private")
	}
	if ok := status >= 200 && status < 300 || status == http.StatusNotModified; !ok && !has["no-store"] {
		out = append(out, "no-store")
	}
	return strings.Join(out, ", ")
}

func varies(h http.Header, token string) bool {
	for _, v := range h.Values("Vary") {
		for f := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), token) {
				return true
			}
		}
	}
	return false
}

// ReservedPath reports whether a request path is the gateway's own on a
// preview host: /.patchy itself or anything under /.patchy/, compared after
// percent-decoding (the path a server sees), as sent and once dot segments
// and repeated slashes are cleaned away, so no spelling a preview could
// normalise back to it is forwarded.
func ReservedPath(p string) bool {
	for _, q := range []string{p, path.Clean("/" + p)} {
		if q == strings.TrimSuffix(ReservedPrefix, "/") || strings.HasPrefix(q, ReservedPrefix) {
			return true
		}
	}
	return false
}
