// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// The configuration keys of preview sign-in the check reads: the relay's own
// ConfigMap (the chart's preview-auth component) and preview-controller's
// switch.
const (
	keyAuthRequired    = "PATCHY_PREVIEW_AUTH_REQUIRED"
	keyAuthIssuer      = "PATCHY_PREVIEW_AUTH_ISSUER"
	keyAuthDexIssuer   = "PATCHY_PREVIEW_AUTH_DEX_ISSUER_URL"
	keyAuthDexClientID = "PATCHY_PREVIEW_AUTH_DEX_CLIENT_ID"
	// The preview gateway's (previewAuth.gateway), and preview-controller's
	// legacy slot Ingress switch, which together say which path serves
	// preview hosts (previewauth.ProbeModeFor).
	keyAuthGateway          = "PATCHY_PREVIEW_AUTH_GATEWAY"
	keyAuthGatewaySignIn    = "PATCHY_PREVIEW_AUTH_GATEWAY_SIGN_IN_HOST"
	keyAuthGatewayInbound   = "PATCHY_PREVIEW_AUTH_GATEWAY_INBOUND_CIDRS"
	keyPreviewLegacyIngress = "PATCHY_PREVIEW_LEGACY_INGRESS"
)

// placeholderLabel is the host label of the chart's placeholder Ingress
// (placeholder.<suffix>), which carries slot 0's sign-in set once sign-in is
// required. No Preview is ever named placeholder.
const placeholderLabel = "placeholder"

// httpTimeout bounds one request of the preview sign-in checks.
const httpTimeout = 10 * time.Second

// bodyLimit bounds what the checks read of any answer.
const bodyLimit = 64 << 10

// HTTPDoer sends one request and returns its answer without following
// redirects; *http.Client with CheckRedirect returning
// http.ErrUseLastResponse is one.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewHTTPDoer is the HTTPDoer the CLI uses: no cookies, no redirects
// followed, the system's roots, and a per-request timeout.
func NewHTTPDoer() HTTPDoer {
	return &http.Client{
		Timeout:       httpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// previewAuth checks preview sign-in, when the chart turned it on: that the
// relay answers at its issuer, that Dex knows the relay's one redirect URI,
// and, once sign-in is required, that every Ready preview host of the
// Project (and the placeholder) answers an unauthenticated request with the
// ALB's redirect to the relay for its own slot's client. Off, it reports
// nothing.
func (c *Checker) previewAuth(ctx context.Context, s Settings, suffix string) {
	required := s.Preview.Bool(keyAuthRequired)
	if !s.Auth.Found() && !required {
		return
	}
	issuer := s.Auth.Data[keyAuthIssuer]
	switch {
	case s.Auth.Err != nil:
		c.add(CheckPreviewAuth, checkreport.Skip, "cannot read the relay's Settings: %v", s.Auth.Err)
		return
	case !s.Auth.Found():
		c.add(CheckPreviewAuth, checkreport.Fail, "preview-controller requires sign-in (%s), but there is no "+
			"preview-auth ConfigMap in namespace %s: is previewAuth.enabled set in the patchy chart?",
			keyAuthRequired, c.cfg.Namespace)
		return
	case !previewauth.ValidIssuer(issuer):
		c.add(CheckPreviewAuth, checkreport.Fail, "%s sets %s to %q, which is not an https origin", s.Auth.From,
			keyAuthIssuer, issuer)
		return
	}
	c.relayDiscovery(ctx, issuer)
	c.dexRedirect(ctx, s, issuer)
	if mode := s.probeMode(); mode != previewauth.ProbeALB {
		c.gatewayHosts(ctx, s, issuer, suffix, mode, required)
		c.gatewayAllowlist(ctx, s, suffix)
		return
	}
	c.previewAuthHosts(ctx, issuer, suffix, required)
}

// relayDiscovery checks the relay answers its own discovery document with
// its issuer. The preview load balancer calls the relay's token endpoint
// from dynamic public addresses, so the relay must answer from anywhere.
func (c *Checker) relayDiscovery(ctx context.Context, issuer string) {
	status, body, _, err := c.httpGet(ctx, issuer+"/.well-known/openid-configuration")
	if err != nil {
		c.add(CheckPreviewAuth, checkreport.Fail, "the relay at %s does not answer: %v (it must be reachable "+
			"from the internet: the preview load balancer calls its token endpoint from dynamic addresses)",
			issuer, err)
		return
	}
	var doc struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	switch {
	case status != http.StatusOK:
		c.add(CheckPreviewAuth, checkreport.Fail, "%s/.well-known/openid-configuration answered %d, not the "+
			"relay's discovery document: does the edge route %s to the preview-auth Service?", issuer, status,
			strings.TrimPrefix(issuer, "https://"))
	case json.Unmarshal(body, &doc) != nil || doc.Issuer != issuer || doc.AuthorizationEndpoint != issuer+"/authorize":
		c.add(CheckPreviewAuth, checkreport.Fail, "%s/.well-known/openid-configuration is not the relay's "+
			"discovery document for issuer %s (it names issuer %q)", issuer, issuer, doc.Issuer)
	default:
		c.add(CheckPreviewAuth, checkreport.Pass, "the relay answers at %s with its discovery document", issuer)
	}
}

// dexMaxHops bounds the redirects the Dex check follows on Dex's own host.
const dexMaxHops = 5

// dexRedirect checks that Dex has the relay's static client with the
// relay's one redirect URI, <issuer>/dex/callback, registered: the one
// per-install IdP setup. Dex's client list is not readable without its
// storage, so the check asks Dex's authorization endpoint the way a
// sign-in would, without a browser session. Dex with a single connector
// answers any client_id there with a redirect to its own /auth/<connector>
// (live Dex v2.45.1 does, for registered and unknown clients alike), and only
// that endpoint judges the client, so the check follows redirects that stay
// on the authorization endpoint's origin (host compared without case, the
// scheme's default port made explicit), at most dexMaxHops of them, and
// judges the answer it ends on: a redirect to another host (the connector's
// upstream) or a sign-in page passes, Dex's error pages for an unknown client
// or an unregistered redirect URI fail, and a redirect loop, too many hops or
// a redirect to another scheme or port on Dex's own host skip. It never follows a redirect off Dex's host, so
// nobody signs in; Dex may keep the unused request until it expires.
func (c *Checker) dexRedirect(ctx context.Context, s Settings, issuer string) {
	dexIssuer, clientID := s.Auth.Data[keyAuthDexIssuer], s.Auth.Data[keyAuthDexClientID]
	redirect := issuer + "/dex/callback"
	if dexIssuer == "" || clientID == "" {
		c.add(CheckPreviewAuthDex, checkreport.Fail, "%s sets no %s or %s", s.Auth.From, keyAuthDexIssuer,
			keyAuthDexClientID)
		return
	}
	status, body, _, err := c.httpGet(ctx, strings.TrimSuffix(dexIssuer, "/")+"/.well-known/openid-configuration")
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	switch {
	case err != nil:
		c.add(CheckPreviewAuthDex, checkreport.Skip, "cannot reach Dex at %s: %v", dexIssuer, err)
		return
	case status != http.StatusOK || json.Unmarshal(body, &doc) != nil || doc.AuthorizationEndpoint == "":
		c.add(CheckPreviewAuthDex, checkreport.Fail, "%s/.well-known/openid-configuration answered %d without "+
			"an authorization endpoint: is %s Dex's issuer?", dexIssuer, status, dexIssuer)
		return
	}
	authURL, err := url.Parse(doc.AuthorizationEndpoint)
	if err != nil || authURL.Scheme != "https" || authURL.Host == "" {
		c.add(CheckPreviewAuthDex, checkreport.Fail, "Dex's authorization endpoint %q is not an https URL",
			doc.AuthorizationEndpoint)
		return
	}
	authURL.RawQuery = url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {"openid"}, "state": {"patchy-check-project"}}.Encode()
	next := authURL
	seen := map[string]bool{}
	for hop := 0; ; hop++ {
		seen[next.String()] = true
		status, body, location, err := c.httpGet(ctx, next.String())
		if err != nil {
			c.add(CheckPreviewAuthDex, checkreport.Skip, "cannot reach Dex's authorization endpoint at %s: %v",
				next.Redacted(), err)
			return
		}
		if status >= 300 && status < 400 && location != "" {
			to, ok := c.dexFollow(dexIssuer, clientID, redirect, authURL, next, location, status, hop, seen)
			if !ok {
				return
			}
			next = to
			continue
		}
		c.judgeDex(dexIssuer, clientID, redirect, status, pageText(body))
		return
	}
}

// dexFollow judges a redirect the Dex check met at next: the URL to ask
// next, or false once it has added the check's line (a redirect to another
// host passes; an unreadable Location, another scheme or port on Dex's own
// host, a loop or too many hops skip).
func (c *Checker) dexFollow(dexIssuer, clientID, redirect string, authURL, next *url.URL, location string, status,
	hop int, seen map[string]bool,
) (*url.URL, bool) {
	to, err := next.Parse(location)
	if err != nil {
		c.add(CheckPreviewAuthDex, checkreport.Skip, "Dex answered %d with an unreadable Location %q",
			status, location)
		return nil, false
	}
	sameHost, sameOrigin := compareOrigin(to, authURL)
	switch {
	case !sameHost:
		c.add(CheckPreviewAuthDex, checkreport.Pass, "Dex at %s accepts client %s with redirect URI %s "+
			"(it answered %d, on to %s://%s)", dexIssuer, clientID, redirect, status, to.Scheme, to.Host)
		return nil, false
	case !sameOrigin:
		c.add(CheckPreviewAuthDex, checkreport.Skip, "Dex redirected to another scheme or port on its "+
			"own host (%s://%s), which says nothing about the client", to.Scheme, to.Host)
		return nil, false
	}
	// The same origin however spelled: ask it as the endpoint is spelled.
	to.Scheme, to.Host = authURL.Scheme, authURL.Host
	switch {
	case seen[to.String()]:
		c.add(CheckPreviewAuthDex, checkreport.Skip, "Dex's authorization endpoint is a redirect loop "+
			"(%s leads back to %s), which says nothing about the client", next.Path, to.Path)
		return nil, false
	case hop >= dexMaxHops:
		c.add(CheckPreviewAuthDex, checkreport.Skip, "Dex's authorization endpoint answered more than %d "+
			"redirects on its own host (the last to %s), which says nothing about the client", dexMaxHops,
			to.Path)
		return nil, false
	}
	return to, true
}

// compareOrigin reports whether to names base's host (without case) and
// whether it is base's origin too: the same scheme and the same port, the
// scheme's default made explicit.
func compareOrigin(to, base *url.URL) (sameHost, sameOrigin bool) {
	sameHost = strings.EqualFold(to.Hostname(), base.Hostname())
	return sameHost, sameHost && strings.EqualFold(to.Scheme, base.Scheme) && effectivePort(to) == effectivePort(base)
}

// effectivePort is u's port, or its scheme's default when it names none.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// judgeDex judges the answer the Dex check ended on that is not a redirect.
func (c *Checker) judgeDex(dexIssuer, clientID, redirect string, status int, page string) {
	switch {
	case status == http.StatusOK:
		c.add(CheckPreviewAuthDex, checkreport.Pass, "Dex at %s accepts client %s with redirect URI %s "+
			"(it answered %d)", dexIssuer, clientID, redirect, status)
	case strings.Contains(page, "redirect_uri"):
		c.add(CheckPreviewAuthDex, checkreport.Fail, "Dex refuses redirect URI %s for client %s (%q): register "+
			"it on that static client, the only per-install IdP setup", redirect, clientID,
			excerpt(page, "redirect_uri"))
	case status == http.StatusNotFound || strings.Contains(page, "client_id"):
		c.add(CheckPreviewAuthDex, checkreport.Fail, "Dex has no client %s (%d %q): add the static client with "+
			"redirect URI %s", clientID, status, excerpt(page, "client_id"), redirect)
	case status >= 400 && status < 500:
		c.add(CheckPreviewAuthDex, checkreport.Fail, "Dex refused a sign-in for client %s with redirect URI %s: "+
			"%d %q", clientID, redirect, status, excerpt(page, ""))
	default:
		c.add(CheckPreviewAuthDex, checkreport.Skip, "Dex's authorization endpoint answered %d %q, which "+
			"says nothing about the client", status, excerpt(page, ""))
	}
}

// previewAuthHosts probes the placeholder host and every Ready Preview of
// the Project without credentials, as the relay's own probe does: each must
// answer with the ALB's redirect to the relay's /authorize for its own
// slot's client and callback. Before sign-in is required (the permit stage)
// nothing is expected to redirect yet.
func (c *Checker) previewAuthHosts(ctx context.Context, issuer, suffix string, required bool) {
	if !required {
		c.add(CheckPreviewAuthHost, checkreport.Skip, "preview-controller does not require sign-in yet "+
			"(previewAuth.stage=permit), so no preview host redirects to the relay")
		return
	}
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		c.add(CheckPreviewAuthHost, checkreport.Skip, "preview-controller's host suffix %q is unusable: %v",
			suffix, err)
		return
	}
	type host struct {
		label string
		slot  int
	}
	hosts := []host{{label: placeholderLabel, slot: 0}}
	var list v1alpha1.PreviewList
	if err := c.cfg.Reader.List(ctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		c.add(CheckPreviewAuthHost, checkreport.Skip, "cannot list the Previews in namespace %s (%v), so only "+
			"the placeholder host is probed", c.cfg.Namespace, err)
	}
	for i := range list.Items {
		p := &list.Items[i]
		if c.probes(p) {
			hosts = append(hosts, host{label: p.Spec.HostLabel, slot: int(*p.Status.Slot)})
		}
	}
	for _, h := range hosts {
		name := h.label + "." + suffix
		status, _, location, err := c.httpGet(ctx, "https://"+name+"/")
		switch {
		case err != nil && timedOut(err):
			c.add(CheckPreviewAuthHost, checkreport.Skip, "no answer from %s within %s: the preview load "+
				"balancer admits only the chart's preview.inboundCIDRs, so run this from an admitted address",
				name, httpTimeout)
		case err != nil:
			c.add(CheckPreviewAuthHost, checkreport.Fail, "%s: %v", name, err)
		case previewauth.JudgeProbe(status, location, issuer, h.slot, h.label, cb) == nil:
			c.add(CheckPreviewAuthHost, checkreport.Pass, "%s (slot %d) answers without credentials with the "+
				"redirect to %s/authorize for client %s", name, h.slot, issuer, previewauth.ClientID(h.slot))
		default:
			c.add(CheckPreviewAuthHost, checkreport.Fail, "%s (slot %d) answered %d (Location %q) without "+
				"credentials, not the redirect to %s/authorize for client %s: the load balancer serves it without "+
				"sign-in (its rules may be stale: compare them with aws elbv2 describe-rules)", name, h.slot, status,
				location, issuer, previewauth.ClientID(h.slot))
		}
	}
}

// httpGet GETs u with the configured doer, returning the status, at most
// bodyLimit bytes of the body and the Location header.
func (c *Checker) httpGet(ctx context.Context, u string) (int, []byte, string, error) {
	status, body, h, err := c.httpDo(ctx, u, nil)
	return status, body, h.Get("Location"), err
}

// httpDo GETs u with the configured doer and the headers hdr, returning the
// status, at most bodyLimit bytes of the body and the headers.
func (c *Checker) httpDo(ctx context.Context, u string, hdr http.Header) (int, []byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, http.Header{}, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return 0, nil, http.Header{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // a read-only probe; the body has been read
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, nil, http.Header{}, fmt.Errorf("read the answer: %w", err)
	}
	return resp.StatusCode, body, resp.Header, nil
}

// pageText is a page's text: tags dropped, whitespace collapsed.
func pageText(body []byte) string {
	var b strings.Builder
	in := false
	for _, c := range string(body) {
		switch {
		case c == '<':
			in = true
			b.WriteByte(' ')
		case c == '>':
			in = false
		case !in:
			b.WriteRune(c)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// excerptRunes bounds a quoted excerpt.
const excerptRunes = 160

// excerpt quotes text from the sentence holding key (from the start when
// key is empty or absent), at most excerptRunes runes of it.
func excerpt(text, key string) string {
	if i := strings.Index(text, key); key != "" && i >= 0 {
		start := strings.LastIndexAny(text[:i], ".!?") + 1
		text = strings.TrimSpace(text[start:])
	}
	if r := []rune(text); len(r) > excerptRunes {
		return string(r[:excerptRunes]) + "..."
	}
	return text
}
