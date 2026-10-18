// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"net"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// gatewayLB is the gateway's Ingress address in gatewayWorld.
const (
	gatewayLB   = "k8s-edge-4567.us-east-1.elb.amazonaws.com"
	gatewayAddr = "192.0.2.10"
)

// gatewayPage is a page the gateway renders itself on a preview host.
func gatewayPage(status int, body string) answer {
	h := http.Header{}
	h.Set("Content-Security-Policy", previewauth.PageCSP)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	return answer{status: status, header: h, body: body}
}

// gateway404 is the gateway's answer for a host no Preview owns.
func gateway404() answer {
	return gatewayPage(http.StatusNotFound, "<title>"+previewauth.NoPreviewTitle+"</title>")
}

// gatewayRefused is the gateway's answer to a client outside its allowlist.
func gatewayRefused() answer {
	return gatewayPage(http.StatusForbidden, "<title>Refused</title><p>"+previewauth.NotOnAllowlistMessage+"</p>")
}

// gatewayRedirect is the gateway's redirect of a navigation to label's host.
func gatewayRedirect(label string) answer {
	return answer{status: http.StatusFound, location: "https://preview-auth.acme.test/start?h=" + label +
		"&s=pa1.t.1.sealed"}
}

// gatewayWorld is signInWorld with the preview gateway on and the legacy
// slot Ingress off: the gateway's Ingress has an address the wildcard
// record holds, the inbound allowlist is set, and every host answers as the
// gateway would to an address inside it.
func gatewayWorld(t *testing.T) (*world, *fakeWeb) {
	w, web := signInWorld(t)
	w.setting(previewAuth, keyAuthGateway, "true")
	w.setting(previewAuth, keyAuthGatewayInbound, "203.0.113.0/24")
	w.setting(previewController, keyPreviewLegacyIngress, "false")
	w.objs = append(w.objs, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "patchy-preview-gateway"},
		Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{
			Ingress: []networkingv1.IngressLoadBalancerIngress{{Hostname: gatewayLB}},
		}},
	})
	w.resolver[gatewayLB] = []string{gatewayAddr}
	w.resolver["shop-0."+suffix] = []string{gatewayAddr}
	delete(web.answers, "placeholder."+suffix+"/")
	web.answers["shop-0."+suffix+"/"] = gateway404()
	web.answers["shop-7."+suffix+"/"] = gatewayRedirect("shop-7")
	return w, web
}

// onlyLine is the report's only line of check, failing the test without
// exactly one.
func onlyLine(t *testing.T, r Report, check string) Check {
	t.Helper()
	var out []Check
	for _, c := range r.Checks {
		if c.Name == check {
			out = append(out, c)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d %s lines:\n%s", len(out), check, dump(r))
	}
	return out[0]
}

// lines are the report's lines of check.
func lines(r Report, check string) []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Name == check {
			out = append(out, c)
		}
	}
	return out
}

// TestGatewayAllPass: in gateway mode the wildcard record points at the
// gateway's edge, the host no Preview owns is the gateway's 404, the
// Project's Ready Preview redirects to the gateway's /start, the Dex check
// is the relay's own, and, from inside the allowlist, the spoof check
// skips. Every host is asked as a navigation.
func TestGatewayAllPass(t *testing.T) {
	w, web := gatewayWorld(t)
	r := w.runWeb(web)
	if n := r.Failed(); n != 0 {
		t.Fatalf("Failed = %d:\n%s", n, dump(r))
	}
	if c := onlyLine(t, r, CheckPreviewDNS); c.Status != checkreport.Pass || !strings.Contains(c.Reason, gatewayLB) {
		t.Errorf("DNS: %s %s", c.Status, c.Reason)
	}
	if c := onlyLine(t, r, CheckPreviewAuthDex); c.Status != checkreport.Pass ||
		!strings.Contains(c.Reason, relayIssuer+"/dex/callback") {
		t.Errorf("Dex: %s %s", c.Status, c.Reason)
	}
	hosts := lines(r, CheckPreviewAuthHost)
	if len(hosts) != 2 || hosts[0].Status != checkreport.Pass || hosts[1].Status != checkreport.Pass ||
		!strings.Contains(hosts[0].Reason, "shop-0."+suffix+", which no preview owns") ||
		!strings.Contains(hosts[1].Reason, "shop-7."+suffix+" (slot 1)") {
		t.Errorf("hosts:\n%s", dump(Report{Checks: hosts}))
	}
	if c := onlyLine(t, r, CheckPreviewGatewayAllowlist); c.Status != checkreport.Skip ||
		!strings.Contains(c.Reason, "run this from outside") {
		t.Errorf("allowlist: %s %s", c.Status, c.Reason)
	}
	for _, u := range web.asked {
		if strings.HasPrefix(u.Host, "placeholder.") || strings.HasPrefix(u.Host, "shop-8.") {
			t.Errorf("probed %s", u)
		}
	}
}

// navWeb records whether every preview-host request was a navigation.
type navWeb struct {
	*fakeWeb
	plain []string
}

func (n *navWeb) Do(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Host, "."+suffix) && req.Header.Get("Accept") != "text/html" {
		n.plain = append(n.plain, req.URL.Host)
	}
	return n.fakeWeb.Do(req)
}

// TestGatewayAsksAsANavigation: the gateway redirects only a top-level
// navigation to sign in, so every preview host is asked with an HTML
// Accept.
func TestGatewayAsksAsANavigation(t *testing.T) {
	w, web := gatewayWorld(t)
	n := &navWeb{fakeWeb: web}
	w.runWeb(n)
	if len(n.plain) != 0 {
		t.Errorf("asked without an HTML Accept: %v", n.plain)
	}
}

// TestGatewayAllowlist: from outside the allowlist, a spoofed
// X-Forwarded-For naming an address inside it must still be refused.
func TestGatewayAllowlist(t *testing.T) {
	tests := map[string]struct {
		plain, spoofed answer
		status         checkreport.Status
		want           string
	}{
		"refused by the gateway, spoof refused": {gatewayRefused(), gatewayRefused(), checkreport.Pass,
			"X-Forwarded-For 203.0.113.1"},
		"dropped at the edge, spoof dropped": {answer{err: timeoutErr{}}, answer{err: timeoutErr{}},
			checkreport.Pass, "still is"},
		"refused by the edge, spoof refused": {answer{status: http.StatusForbidden}, answer{status: 403},
			checkreport.Pass, "still is"},
		"spoof admitted": {gatewayRefused(), gateway404(), checkreport.Fail,
			"previewAuth.gateway.forwardedHops"},
		"spoof admitted past the edge": {answer{err: timeoutErr{}}, answer{status: http.StatusOK},
			checkreport.Fail, "answered 200"},
		"unreachable": {answer{err: &net.OpError{Op: "dial", Err: net.UnknownNetworkError("x")}}, gatewayRefused(),
			checkreport.Skip, "shop-0." + suffix},
		"something else answers": {answer{status: http.StatusOK}, gatewayRefused(), checkreport.Skip,
			"answered 200"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w, web := gatewayWorld(t)
			a := tt.plain
			a.spoofed = &tt.spoofed
			web.answers["shop-0."+suffix+"/"] = a
			r := w.runWeb(web)
			c := onlyLine(t, r, CheckPreviewGatewayAllowlist)
			if c.Status != tt.status || !strings.Contains(c.Reason, tt.want) {
				t.Errorf("%s %s, want %s with %q", c.Status, c.Reason, tt.status, tt.want)
			}
		})
	}
	w, web := gatewayWorld(t)
	w.setting(previewAuth, keyAuthGatewayInbound, "")
	if c := onlyLine(t, w.runWeb(web), CheckPreviewGatewayAllowlist); c.Status != checkreport.Skip ||
		!strings.Contains(c.Reason, "inboundCIDRs is empty") {
		t.Errorf("no allowlist: %s %s", c.Status, c.Reason)
	}
}

// TestGatewayFromOutsideTheAllowlist: run from outside inboundCIDRs, as
// the allowlist check asks, the gateway refuses every host before it routes
// one. That is the allowlist working, so the allowlist check passes and every
// host line skips instead of failing, whichever path the probe expects.
func TestGatewayFromOutsideTheAllowlist(t *testing.T) {
	tests := map[string]func(*world, *fakeWeb){
		"gateway only": func(*world, *fakeWeb) {},
		"both serve, sign-in required": func(w *world, f *fakeWeb) {
			w.setting(previewController, keyPreviewLegacyIngress, "true")
			f.answers["placeholder."+suffix+"/"] = gatewayRefused()
		},
		"both serve, the permit stage": func(w *world, _ *fakeWeb) {
			w.setting(previewController, keyPreviewLegacyIngress, "true")
			w.setting(previewController, keyAuthRequired, "")
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			w, web := gatewayWorld(t)
			web.answers["shop-0."+suffix+"/"] = gatewayRefused()
			web.answers["shop-7."+suffix+"/"] = gatewayRefused()
			change(w, web)
			r := w.runWeb(web)
			if n := r.Failed(); n != 0 {
				t.Fatalf("Failed = %d:\n%s", n, dump(r))
			}
			if c := onlyLine(t, r, CheckPreviewGatewayAllowlist); c.Status != checkreport.Pass {
				t.Errorf("allowlist: %s %s", c.Status, c.Reason)
			}
			hosts := lines(r, CheckPreviewAuthHost)
			var live bool
			for _, c := range hosts {
				if c.Status != checkreport.Skip || !strings.Contains(c.Reason, "inboundCIDRs") {
					t.Errorf("host: %s %s", c.Status, c.Reason)
				}
				live = live || strings.Contains(c.Reason, "shop-7."+suffix+" (slot 1)")
			}
			if !live {
				t.Errorf("no line for the Ready preview:\n%s", dump(Report{Checks: hosts}))
			}
		})
	}
}

// TestGatewayFailures: each part of the gateway path that is wrong fails or
// skips its own line, with a reason that says what to fix.
func TestGatewayFailures(t *testing.T) {
	published := func(addrs string) func(*world, *fakeWeb) {
		return func(w *world, _ *fakeWeb) {
			for _, o := range w.objs {
				if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == "patchy-preview-auth-config" {
					cm.Annotations = map[string]string{publishedAnnotation: addrs}
				}
			}
		}
	}
	noIngress := func(w *world) {
		w.drop(func(o client.Object) bool { return o.GetName() == "patchy-preview-gateway" })
	}
	either := func(w *world) { w.setting(previewController, keyPreviewLegacyIngress, "true") }
	tests := map[string]struct {
		change func(*world, *fakeWeb)
		check  string
		status checkreport.Status
		want   []string
	}{
		"DNS missing": {
			change: func(w *world, _ *fakeWeb) { delete(w.resolver, "shop-0."+suffix) },
			check:  CheckPreviewDNS, status: checkreport.Fail, want: []string{"does not resolve",
				"preview gateway's edge " + gatewayLB},
		},
		"DNS at the legacy load balancer": {
			change: func(w *world, _ *fakeWeb) { w.resolver["shop-0."+suffix] = []string{"198.51.100.7"} },
			check:  CheckPreviewDNS, status: checkreport.Fail, want: []string{"must point at it", gatewayAddr},
		},
		"DNS at the legacy load balancer while both serve": {
			change: func(w *world, _ *fakeWeb) {
				either(w)
				w.resolver["shop-0."+suffix] = []string{"198.51.100.7"}
			},
			check: CheckPreviewDNS, status: checkreport.Pass, want: []string{"legacy preview load balancer " + lbHost,
				"roll back"},
		},
		"published addresses win over the Ingress": {
			change: published("192.0.2.77"),
			check:  CheckPreviewDNS, status: checkreport.Fail,
			want: []string{"previewAuth.gateway.publishedAddresses is at 192.0.2.77"},
		},
		"published private address, not in this DNS": {
			change: func(w *world, f *fakeWeb) {
				published("100.100.1.2")(w, f)
				delete(w.resolver, "shop-0."+suffix)
			},
			check: CheckPreviewDNS, status: checkreport.Skip, want: []string{"private", "tailnet"},
		},
		"published private address, public answer here": {
			change: published("10.0.0.5"),
			check:  CheckPreviewDNS, status: checkreport.Skip, want: []string{"private edge", "10.0.0.5"},
		},
		"published address matches": {
			change: func(w *world, f *fakeWeb) {
				published("100.100.1.2")(w, f)
				w.resolver["shop-0."+suffix] = []string{"100.100.1.2"}
			},
			check: CheckPreviewDNS, status: checkreport.Pass, want: []string{"publishedAddresses"},
		},
		"LoadBalancer Service": {
			change: func(w *world, _ *fakeWeb) {
				noIngress(w)
				w.objs = append(w.objs, &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "patchy-preview-gateway"},
					Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
					Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{{IP: gatewayAddr}}}},
				})
			},
			check: CheckPreviewDNS, status: checkreport.Pass, want: []string{"edge " + gatewayAddr},
		},
		"LoadBalancer Service not provisioned": {
			change: func(w *world, _ *fakeWeb) {
				noIngress(w)
				w.objs = append(w.objs, &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "patchy-preview-gateway"},
					Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
				})
			},
			check: CheckPreviewDNS, status: checkreport.Fail, want: []string{"not provisioned"},
		},
		"no published address (a tunnel)": {
			change: func(w *world, _ *fakeWeb) { noIngress(w) },
			check:  CheckPreviewDNS, status: checkreport.Skip, want: []string{"publishedAddresses", "tunnel"},
		},
		"Ingress without an address": {
			change: func(w *world, _ *fakeWeb) {
				for _, o := range w.objs {
					if ing, ok := o.(*networkingv1.Ingress); ok && ing.Name == "patchy-preview-gateway" {
						ing.Status = networkingv1.IngressStatus{}
					}
				}
			},
			check: CheckPreviewDNS, status: checkreport.Skip, want: []string{"no address yet"},
		},
		"the app answers a live host": {
			change: func(_ *world, f *fakeWeb) { f.answers["shop-7."+suffix+"/"] = answer{status: 200} },
			check:  CheckPreviewAuthHost, status: checkreport.Fail, want: []string{"shop-7." + suffix,
				"answered 200", "/start"},
		},
		"the ALB still answers a live host": {
			change: func(_ *world, f *fakeWeb) { f.answers["shop-7."+suffix+"/"] = probeRedirect("shop-7", 1) },
			check:  CheckPreviewAuthHost, status: checkreport.Fail, want: []string{"shop-7." + suffix},
		},
		"the ALB answers a live host while both serve": {
			change: func(w *world, f *fakeWeb) {
				either(w)
				f.answers["placeholder."+suffix+"/"] = probeRedirect("placeholder", 0)
				f.answers["shop-7."+suffix+"/"] = probeRedirect("shop-7", 1)
			},
			check: CheckPreviewAuthHost, status: checkreport.Pass, want: []string{"shop-7." + suffix},
		},
		"the unknown host is an app": {
			change: func(_ *world, f *fakeWeb) { f.answers["shop-0."+suffix+"/"] = answer{status: 200} },
			check:  CheckPreviewAuthHost, status: checkreport.Fail, want: []string{"no preview owns",
				"not the preview gateway's 404"},
		},
		"the unknown host is another 404": {
			change: func(_ *world, f *fakeWeb) { f.answers["shop-0."+suffix+"/"] = answer{status: 404} },
			check:  CheckPreviewAuthHost, status: checkreport.Fail, want: []string{"answered 404"},
		},
		"the unknown host is the ALB's while both serve": {
			change: func(w *world, f *fakeWeb) {
				either(w)
				f.answers["placeholder."+suffix+"/"] = probeRedirect("placeholder", 0)
				f.answers["shop-0."+suffix+"/"] = answer{status: 404}
			},
			check: CheckPreviewAuthHost, status: checkreport.Skip, want: []string{"legacy preview load balancer"},
		},
		"the permit stage while both serve": {
			change: func(w *world, f *fakeWeb) {
				either(w)
				w.setting(previewController, keyAuthRequired, "")
				f.answers["shop-7."+suffix+"/"] = answer{status: 200}
			},
			check: CheckPreviewAuthHost, status: checkreport.Skip, want: []string{"previewAuth.stage=permit"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			w, web := gatewayWorld(t)
			tt.change(w, web)
			r := w.runWeb(web)
			var found bool
			for _, c := range lines(r, tt.check) {
				if c.Status != tt.status {
					continue
				}
				ok := true
				for _, s := range tt.want {
					ok = ok && strings.Contains(c.Reason, s)
				}
				found = found || ok
			}
			if !found {
				t.Errorf("no %s %s line with %q:\n%s", tt.check, tt.status, tt.want, dump(r))
			}
		})
	}
}

// TestGatewayLegacyModeUnchanged: with the gateway on but the legacy slot
// Ingress still serving and nothing pointing at the gateway yet, an install
// mid-migration passes on the legacy path's answers.
func TestGatewayLegacyModeUnchanged(t *testing.T) {
	w, web := gatewayWorld(t)
	w.setting(previewController, keyPreviewLegacyIngress, "")
	w.resolver["shop-0."+suffix] = []string{"198.51.100.7"}
	web.answers["placeholder."+suffix+"/"] = probeRedirect("placeholder", 0)
	web.answers["shop-7."+suffix+"/"] = probeRedirect("shop-7", 1)
	web.answers["shop-0."+suffix+"/"] = answer{status: http.StatusNotFound}
	r := w.runWeb(web)
	if n := r.Failed(); n != 0 {
		t.Fatalf("Failed = %d:\n%s", n, dump(r))
	}
}
