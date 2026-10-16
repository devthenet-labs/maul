// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// The preview gateway's checks (design §5, Revision 1), run instead of the
// legacy ALB path's while previewAuth.gateway is on.

// publishedAnnotation is where the chart records
// previewAuth.gateway.publishedAddresses on the relay's ConfigMap: the
// addresses the wildcard record should hold when the exposure publishes
// none.
const publishedAnnotation = "patchy.bitwisemedia.uk/preview-gateway-published-addresses"

// relayConfigSuffix and gatewaySuffix name the chart's objects:
// <fullname>-preview-auth-config is the relay's ConfigMap, and
// <fullname>-preview-gateway the gateway's Service, Ingress and HTTPRoute.
const (
	relayConfigSuffix = "-preview-auth-config"
	gatewaySuffix     = "-preview-gateway"
)

// gatewayAdmits is the TLS and host checks' reason for a timeout in gateway
// mode.
const gatewayAdmits = "the gateway's edge may admit only previewAuth.gateway.inboundCIDRs"

// probeMode is the path that serves preview hosts: the gateway only while
// it is on, and the legacy slot Ingress only while preview-controller
// renders it (its flag's default, true, when unset).
func (s settings) probeMode() previewauth.ProbeMode {
	legacy := true
	if v, ok := s.preview.data[keyPreviewLegacyIngress]; ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			legacy = b
		}
	}
	return previewauth.ProbeModeFor(s.auth.found() && s.auth.bool(keyAuthGateway), legacy)
}

// gatewayEdge is where the wildcard record should point for the gateway:
// the published addresses, else its exposure's status.
type gatewayEdge struct {
	name  string
	addrs []string
	// private: every address is private (RFC 1918, ULA, CGNAT and so a
	// tailnet, loopback or link-local), which public DNS rarely holds.
	private bool
	// why is set when there is nothing to compare against, and status then
	// says what that makes the DNS check.
	why    string
	status checkreport.Status
}

// gatewayEdge reads where the gateway is published: the chart's
// publishedAddresses (an override), else the gateway's Ingress or
// LoadBalancer Service status.
func (r *run) gatewayEdge(ctx context.Context, s settings) gatewayEdge {
	if v := strings.TrimSpace(s.auth.annotations[publishedAnnotation]); v != "" {
		var e gatewayEdge
		e.name = "previewAuth.gateway.publishedAddresses (" + v + ")"
		for a := range strings.SplitSeq(v, ",") {
			addrs, err := r.addresses(ctx, strings.TrimSpace(a))
			if err != nil {
				return gatewayEdge{status: checkreport.Skip, why: fmt.Sprintf("the published address %s does not "+
					"resolve: %v", a, err)}
			}
			e.addrs = append(e.addrs, addrs...)
		}
		e.private = allPrivate(e.addrs)
		return e
	}
	base, ok := strings.CutSuffix(s.auth.name, relayConfigSuffix)
	if !ok {
		return gatewayEdge{status: checkreport.Skip, why: fmt.Sprintf("the relay's ConfigMap %q is not named "+
			"<release>%s, so the gateway's exposure cannot be found", s.auth.name, relayConfigSuffix)}
	}
	key := types.NamespacedName{Namespace: r.p.Namespace, Name: base + gatewaySuffix}
	var ing networkingv1.Ingress
	switch err := r.cfg.Reader.Get(ctx, key, &ing); {
	case err == nil:
		for _, in := range ing.Status.LoadBalancer.Ingress {
			if e, ok := r.statusEdge(ctx, "Ingress "+key.String(), in.Hostname, in.IP); ok {
				return e
			}
		}
		return gatewayEdge{status: checkreport.Skip, why: fmt.Sprintf("the gateway's Ingress %s has no address "+
			"yet (an ingress controller that publishes none needs previewAuth.gateway.publishedAddresses)", key)}
	case !apierrors.IsNotFound(err):
		return gatewayEdge{status: checkreport.Skip, why: fmt.Sprintf("cannot read the gateway's Ingress %s: %v",
			key, err)}
	}
	var svc corev1.Service
	switch err := r.cfg.Reader.Get(ctx, key, &svc); {
	case err == nil && svc.Spec.Type == corev1.ServiceTypeLoadBalancer:
		for _, in := range svc.Status.LoadBalancer.Ingress {
			if e, ok := r.statusEdge(ctx, "Service "+key.String(), in.Hostname, in.IP); ok {
				return e
			}
		}
		return gatewayEdge{status: checkreport.Fail, why: fmt.Sprintf("the gateway's LoadBalancer Service %s has "+
			"no address, so its load balancer is not provisioned (the Service's events say why)", key)}
	case err != nil && !apierrors.IsNotFound(err):
		return gatewayEdge{status: checkreport.Skip, why: fmt.Sprintf("cannot read the gateway's Service %s: %v",
			key, err)}
	}
	return gatewayEdge{status: checkreport.Skip, why: "the gateway's exposure publishes no address (an HTTPRoute, " +
		"whose Gateway holds it, or a tunnel): set previewAuth.gateway.publishedAddresses to compare against"}
}

// statusEdge is an exposure's status address, resolved when a hostname.
func (r *run) statusEdge(ctx context.Context, what, hostname, ip string) (gatewayEdge, bool) {
	switch {
	case hostname != "":
		addrs, err := r.cfg.Resolver.LookupHost(ctx, hostname)
		if err != nil {
			return gatewayEdge{status: checkreport.Skip, why: fmt.Sprintf("the %s address %s does not resolve: %v",
				what, hostname, err)}, true
		}
		return gatewayEdge{name: hostname, addrs: addrs, private: allPrivate(addrs)}, true
	case ip != "":
		return gatewayEdge{name: ip, addrs: []string{ip}, private: allPrivate([]string{ip})}, true
	}
	return gatewayEdge{}, false
}

// addresses is a published address: an IP as itself, a name looked up.
func (r *run) addresses(ctx context.Context, a string) ([]string, error) {
	if _, err := netip.ParseAddr(a); err == nil {
		return []string{a}, nil
	}
	return r.cfg.Resolver.LookupHost(ctx, a)
}

// cgnat is RFC 6598 shared address space, where tailnet addresses live.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// allPrivate reports whether every address is one public DNS rarely holds.
func allPrivate(addrs []string) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return false
		}
		ip = ip.Unmap()
		if !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !cgnat.Contains(ip) {
			return false
		}
	}
	return true
}

// gatewayHost checks the preview host in gateway mode: <project>-0.<suffix>
// resolves to the gateway's edge (or, while both paths serve, to the legacy
// preview load balancer) and serves a certificate trusted for it.
func (r *run) gatewayHost(ctx context.Context, s settings, suffix string, mode previewauth.ProbeMode) {
	if suffix == "" {
		r.add(CheckPreviewDNS, "", checkreport.Skip, "preview-controller's host suffix is unknown")
		r.add(CheckPreviewTLS, "", checkreport.Skip, "preview-controller's host suffix is unknown")
		return
	}
	host := r.p.Name + "-0." + suffix
	edge := r.gatewayEdge(ctx, s)
	addrs, err := r.cfg.Resolver.LookupHost(ctx, host)
	var dnsErr *net.DNSError
	switch {
	case err != nil && errors.As(err, &dnsErr) && dnsErr.IsNotFound && edge.private:
		r.add(CheckPreviewDNS, "", checkreport.Skip, "%s does not resolve here (%v), and the gateway's edge %s is "+
			"private, which a split-horizon or tailnet resolver may hold: run this where viewers resolve it", host,
			err, edge.name)
		r.add(CheckPreviewTLS, "", checkreport.Skip, "%s does not resolve here", host)
		return
	case err != nil && errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		r.add(CheckPreviewDNS, "", checkreport.Fail, "%s does not resolve (%v): a wildcard record *.%s must point "+
			"at the preview gateway's edge%s", host, err, suffix, edgeHint(edge))
		r.add(CheckPreviewTLS, "", checkreport.Skip, "%s does not resolve", host)
		return
	case err != nil:
		r.add(CheckPreviewDNS, "", checkreport.Skip, "cannot look %s up (%v), which does not say whether the "+
			"record exists; run this again", host, err)
		r.add(CheckPreviewTLS, "", checkreport.Skip, "%s could not be looked up", host)
		return
	}
	slices.Sort(addrs)
	r.judgeGatewayDNS(ctx, host, suffix, addrs, edge, mode)
	r.previewTLS(ctx, host, suffix, gatewayAdmits)
}

// edgeHint names the gateway's edge, when known, for a reason.
func edgeHint(edge gatewayEdge) string {
	if edge.why != "" {
		return ""
	}
	return " " + edge.name + " (" + strings.Join(edge.addrs, ", ") + ")"
}

// judgeGatewayDNS compares the preview host's addresses with the gateway's
// edge and, while both paths serve, the legacy load balancer.
func (r *run) judgeGatewayDNS(ctx context.Context, host, suffix string, addrs []string, edge gatewayEdge,
	mode previewauth.ProbeMode) {
	shares := func(want []string) bool {
		return slices.ContainsFunc(addrs, func(a string) bool { return slices.Contains(want, a) })
	}
	got := strings.Join(addrs, ", ")
	if edge.why == "" && shares(edge.addrs) {
		r.add(CheckPreviewDNS, "", checkreport.Pass, "%s resolves to the preview gateway's edge %s (%s)", host,
			edge.name, got)
		return
	}
	if mode == previewauth.ProbeEither {
		if lb := r.loadBalancer(ctx); lb.why == "" && shares(lb.addrs) {
			r.add(CheckPreviewDNS, "", checkreport.Pass, "%s still resolves to the legacy preview load balancer %s "+
				"(%s): both paths sign viewers in, and the gateway serves these hosts once the record points at "+
				"its edge%s (point it back here to roll back)", host, lb.name, got, edgeHint(edge))
			return
		}
	}
	switch {
	case edge.why != "":
		r.add(CheckPreviewDNS, "", edge.status, "%s resolves to %s, but it cannot be compared with the preview "+
			"gateway's edge: %s", host, got, edge.why)
	case edge.private:
		r.add(CheckPreviewDNS, "", checkreport.Skip, "%s resolves to %s here, not to the gateway's private edge "+
			"%s (%s), which a split-horizon or tailnet resolver may hold: run this where viewers resolve it", host,
			got, edge.name, strings.Join(edge.addrs, ", "))
	default:
		r.add(CheckPreviewDNS, "", checkreport.Fail, "%s resolves to %s, but the preview gateway's edge %s is at "+
			"%s: the wildcard record *.%s must point at it", host, got, edge.name, strings.Join(edge.addrs, ", "),
			suffix)
	}
}

// navigation is how a browser opens a preview host, as far as the gateway
// tells navigations from background requests without Sec-Fetch metadata.
var navigation = http.Header{"Accept": {"text/html"}}

// gatewayHosts probes the preview hosts without credentials as the relay's
// own probe does in mode: each Ready Preview of the Project must answer
// with a redirect to sign in for its own label (the gateway's, or while
// both paths serve, either), and a host no Preview owns with the gateway's
// own 404 page, never an app's answer. While both paths serve and sign-in
// is not required on the legacy one (the permit stage), a host behind the
// ALB answers without sign-in, so only the gateway's answers pass and any
// other is a SKIP.
func (r *run) gatewayHosts(ctx context.Context, s settings, issuer, suffix string, mode previewauth.ProbeMode,
	required bool) {
	cb, err := previewauth.NewCallbacks(suffix)
	if err != nil {
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "preview-controller's host suffix %q is unusable: %v",
			suffix, err)
		return
	}
	signIn := s.auth.data[keyAuthGatewaySignIn]
	if signIn == "" {
		signIn = strings.TrimPrefix(issuer, "https://")
	}
	hosts, err := previewauth.NewHosts(suffix, signIn)
	if err != nil {
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "the gateway's hosts are unusable: %v", err)
		return
	}
	expect := previewauth.ProbeExpect{Mode: mode, Issuer: issuer, Callbacks: cb, Gateway: hosts}
	permit := mode == previewauth.ProbeEither && !required
	if permit {
		expect.Mode = previewauth.ProbeGateway
	}
	r.gatewayUnknownHost(ctx, suffix, mode)
	if mode == previewauth.ProbeEither && required {
		r.gatewayPlaceholder(ctx, expect, suffix)
	}
	for _, p := range r.readyPreviews(ctx) {
		name := p.label + "." + suffix
		status, _, h, err := r.httpDo(ctx, "https://"+name+"/", navigation)
		location := h.Get("Location")
		switch {
		case err != nil && timedOut(err):
			r.add(CheckPreviewAuthHost, "", checkreport.Skip, "no answer from %s within %s: %s, so run this from "+
				"an admitted address", name, httpTimeout, gatewayAdmits)
		case err != nil:
			r.add(CheckPreviewAuthHost, "", checkreport.Fail, "%s: %v", name, err)
		case expect.Live(status, location, p.slot, p.label) == nil:
			r.add(CheckPreviewAuthHost, "", checkreport.Pass, "%s (slot %d) answers without credentials with a "+
				"redirect to sign in for its own preview (%s)", name, p.slot, location)
		case permit:
			r.add(CheckPreviewAuthHost, "", checkreport.Skip, "%s (slot %d) answered %d without credentials, not the "+
				"gateway's redirect: sign-in is not required on the legacy ALB path yet (previewAuth.stage=permit), "+
				"which may still answer here", name, p.slot, status)
		default:
			r.add(CheckPreviewAuthHost, "", checkreport.Fail, "%s (slot %d) answered %d (Location %q) without "+
				"credentials, not a redirect to sign in at https://%s/start for %s: something other than the "+
				"preview gateway serves it", name, p.slot, status, location, signIn, p.label)
		}
	}
}

// previewHostRef is one Ready Preview's host.
type previewHostRef struct {
	label string
	slot  int
}

// readyPreviews lists the Project's Ready Previews; a failed list is a SKIP
// line and none.
func (r *run) readyPreviews(ctx context.Context) []previewHostRef {
	var list v1alpha1.PreviewList
	if err := r.cfg.Reader.List(ctx, &list, client.InNamespace(r.p.Namespace)); err != nil {
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "cannot list the Previews in namespace %s: %v",
			r.p.Namespace, err)
		return nil
	}
	var out []previewHostRef
	for i := range list.Items {
		p := &list.Items[i]
		if p.Spec.Project == r.p.Name && p.Status.Phase == v1alpha1.PreviewReady && p.Status.Slot != nil &&
			p.DeletionTimestamp == nil {
			out = append(out, previewHostRef{label: p.Spec.HostLabel, slot: int(*p.Status.Slot)})
		}
	}
	return out
}

// gatewayUnknownHost probes <project>-0.<suffix>, which no Preview owns:
// the gateway answers it with its own 404 page. While both paths serve, the
// record may still point at the legacy load balancer, so another answer is
// a SKIP rather than a FAIL.
func (r *run) gatewayUnknownHost(ctx context.Context, suffix string, mode previewauth.ProbeMode) {
	name := r.p.Name + "-0." + suffix
	status, body, h, err := r.httpDo(ctx, "https://"+name+"/", navigation)
	switch {
	case err != nil && timedOut(err):
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "no answer from %s within %s: %s, so run this from an "+
			"admitted address", name, httpTimeout, gatewayAdmits)
	case err != nil:
		r.add(CheckPreviewAuthHost, "", checkreport.Fail, "%s: %v", name, err)
	case previewauth.JudgeGatewayUnknown(status, h, body) == nil:
		r.add(CheckPreviewAuthHost, "", checkreport.Pass, "%s, which no preview owns, answers with the preview "+
			"gateway's 404 page", name)
	case previewauth.IsAllowlistRefusal(status, h, body):
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "the preview gateway refuses this address (it is "+
			"outside previewAuth.gateway.inboundCIDRs), so %s cannot be judged from here", name)
	case mode == previewauth.ProbeEither:
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "%s, which no preview owns, answered %d, not the preview "+
			"gateway's 404 page: the wildcard record may still point at the legacy preview load balancer", name,
			status)
	default:
		r.add(CheckPreviewAuthHost, "", checkreport.Fail, "%s, which no preview owns, answered %d without "+
			"credentials, not the preview gateway's 404 page: point the wildcard record *.%s at the gateway's edge, "+
			"and have the edge send every host under it to the gateway", name, status, suffix)
	}
}

// gatewayPlaceholder probes the legacy placeholder host while both paths
// serve: the ALB's redirect for slot 0 there, or the gateway's 404 (no
// Preview is named placeholder) once the record points at the gateway.
func (r *run) gatewayPlaceholder(ctx context.Context, expect previewauth.ProbeExpect, suffix string) {
	name := placeholderLabel + "." + suffix
	status, body, h, err := r.httpDo(ctx, "https://"+name+"/", navigation)
	switch {
	case err != nil && timedOut(err):
		r.add(CheckPreviewAuthHost, "", checkreport.Skip, "no answer from %s within %s: run this from an admitted "+
			"address", name, httpTimeout)
	case err != nil:
		r.add(CheckPreviewAuthHost, "", checkreport.Fail, "%s: %v", name, err)
	case expect.Live(status, h.Get("Location"), 0, placeholderLabel) == nil ||
		previewauth.JudgeGatewayUnknown(status, h, body) == nil:
		r.add(CheckPreviewAuthHost, "", checkreport.Pass, "%s answers without credentials with the legacy redirect "+
			"to the relay or the gateway's 404 page", name)
	default:
		r.add(CheckPreviewAuthHost, "", checkreport.Fail, "%s answered %d (Location %q) without credentials, "+
			"neither the redirect to %s/authorize for client %s nor the gateway's 404 page", name, status,
			h.Get("Location"), expect.Issuer, previewauth.ClientID(0))
	}
}

// gatewayAllowlist checks the gateway's inbound allowlist cannot be talked
// round: from an address it refuses, a request to a preview host naming an
// admitted address in X-Forwarded-For must still be refused. A gateway
// trusting more forwarded hops than the proxies in front of it would take
// the spoofed entry for the client. Without inboundCIDRs there is nothing to
// check; from an admitted address a spoof proves nothing.
func (r *run) gatewayAllowlist(ctx context.Context, s settings, suffix string) {
	cidrs := s.auth.list(keyAuthGatewayInbound)
	if len(cidrs) == 0 {
		r.add(CheckPreviewGatewayAllowlist, "", checkreport.Skip, "previewAuth.gateway.inboundCIDRs is empty: "+
			"every address may open the gateway's sign-in, and every preview host still requires it")
		return
	}
	prefix, err := netip.ParsePrefix(cidrs[0])
	if err != nil {
		r.add(CheckPreviewGatewayAllowlist, "", checkreport.Fail, "%s sets %s to %q, which the gateway refuses to "+
			"start with: %v", s.auth.from, keyAuthGatewayInbound, cidrs[0], err)
		return
	}
	spoof := prefix.Masked().Addr()
	if next := spoof.Next(); next.IsValid() && prefix.Contains(next) {
		spoof = next
	}
	name := r.p.Name + "-0." + suffix
	u := "https://" + name + "/"
	status, body, h, err := r.httpDo(ctx, u, navigation)
	switch {
	case err == nil && previewauth.JudgeGatewayUnknown(status, h, body) == nil:
		r.add(CheckPreviewGatewayAllowlist, "", checkreport.Skip, "the preview gateway admits this address (it is "+
			"inside previewAuth.gateway.inboundCIDRs), so a spoofed X-Forwarded-For proves nothing from here: run "+
			"this from outside them")
		return
	case err != nil && !timedOut(err):
		r.add(CheckPreviewGatewayAllowlist, "", checkreport.Skip, "%s: %v", name, err)
		return
	case err == nil && status != http.StatusForbidden:
		r.add(CheckPreviewGatewayAllowlist, "", checkreport.Skip, "%s answered %d, neither the gateway's 404 "+
			"page nor a refusal, so the allowlist cannot be judged from here", name, status)
		return
	}
	spoofed := http.Header{"Accept": {"text/html"}, "X-Forwarded-For": {spoof.String()}}
	status, body, h, err = r.httpDo(ctx, u, spoofed)
	switch {
	case err != nil || status == http.StatusForbidden:
		r.add(CheckPreviewGatewayAllowlist, "", checkreport.Pass, "this address is refused, and still is with "+
			"X-Forwarded-For %s, an address inside previewAuth.gateway.inboundCIDRs", spoof)
	default:
		r.add(CheckPreviewGatewayAllowlist, "", checkreport.Fail, "this address is refused, but %s answered %d "+
			"once X-Forwarded-For named %s, inside previewAuth.gateway.inboundCIDRs: the gateway trusts more "+
			"forwarded hops (previewAuth.gateway.forwardedHops) than there are proxies in front of it%s", name, status,
			spoof, gatewayPageNote(status, h, body))
	}
}

// gatewayPageNote says when an answer was the gateway's own 404, the proof
// the spoof got past the allowlist.
func gatewayPageNote(status int, h http.Header, body []byte) string {
	if previewauth.JudgeGatewayUnknown(status, h, body) == nil {
		return " (it answered with the gateway's 404 page, which comes only after the allowlist)"
	}
	return ""
}
