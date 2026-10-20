// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
	"github.com/devthenet-labs/maul/cmd/maul/internal/tailnet"
)

// iTailnetAddr is the gateway's tailnet device's address.
const iTailnetAddr = "100.101.12.58"

// tailnetWorld is the install world on the tailscale recipe: the gateway's
// ClusterIP Service exposed by the Tailscale operator, no Ingress, and a
// client on the tailnet that sees the device.
func tailnetWorld() *installWorld {
	w := newInstallWorld()
	w.drop("maul-preview-gateway", &networkingv1.Ingress{})
	w.objs = append(w.objs, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "maul", Name: "maul-preview-gateway", Annotations: map[string]string{
			tailscaleExpose: "true", tailscaleHostname: "maul-previews", tailscaleTags: "tag:maul-previews",
		}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.43.0.20"},
	})
	w.resolver[ProbeProject+"-0."+iSuffix] = []string{iTailnetAddr}
	w.tailnet = tailnetStatus(`{"BackendState": "Running", "Peer": {"k": {"HostName": "maul-previews",
		"DNSName": "maul-previews.tail1234.ts.net.", "TailscaleIPs": ["` + iTailnetAddr + `"],
		"Tags": ["tag:maul-previews"]}}}`)
	return w
}

func tailnetStatus(raw string) tailnet.StatusFunc {
	return func(context.Context) ([]byte, error) { return []byte(raw), nil }
}

// TestTailnetEdge: the tailscale recipe's gateway is found through the
// caller's Tailscale client, never a Secret (run fails on any Secret
// read): a record at its address passes; a missing or wrong record fails
// with the exact records to create; a device the client cannot see, no
// client, or no lookup at all is a SKIP saying what to do; and the chart's
// publishedAddresses still wins without asking the client.
func TestTailnetEdge(t *testing.T) {
	records := "create these records in public DNS: *." + iSuffix + ". A " + iTailnetAddr +
		"; preview-auth.acme.test. A " + iTailnetAddr
	w := tailnetWorld()
	expect(t, w.run(t, interceptor.Funcs{}).Checks, CheckPreviewDNS, checkreport.Pass,
		"resolves to the preview gateway's edge tailnet device maul-previews.tail1234.ts.net ("+iTailnetAddr+")")

	w = tailnetWorld()
	delete(w.resolver, ProbeProject+"-0."+iSuffix)
	expect(t, w.run(t, interceptor.Funcs{}).Checks, CheckPreviewDNS, checkreport.Fail, "does not resolve", records)

	w = tailnetWorld()
	w.resolver[ProbeProject+"-0."+iSuffix] = []string{"192.0.2.99"}
	expect(t, w.run(t, interceptor.Funcs{}).Checks, CheckPreviewDNS, checkreport.Fail,
		"resolves to 192.0.2.99, but the preview gateway's edge tailnet device", records)

	w = tailnetWorld()
	w.tailnet = tailnetStatus(`{"BackendState": "Stopped"}`)
	expect(t, w.run(t, interceptor.Funcs{}).Checks, CheckPreviewDNS, checkreport.Skip,
		"the tailnet device maul-previews (tag:maul-previews)", "is Stopped", "run this on a machine on the tailnet")

	w = tailnetWorld()
	w.tailnet = func(context.Context) ([]byte, error) { return nil, errors.New("no tailscale command on PATH") }
	expect(t, w.run(t, interceptor.Funcs{}).Checks, CheckPreviewDNS, checkreport.Skip, "could not be asked")

	w = tailnetWorld()
	w.tailnet = nil
	expect(t, w.run(t, interceptor.Funcs{}).Checks, CheckPreviewDNS, checkreport.Skip,
		"set previewAuth.gateway.publishedAddresses")

	w = tailnetWorld()
	asked := false
	w.tailnet = func(context.Context) ([]byte, error) { asked = true; return nil, errors.New("unused") }
	for _, o := range w.objs {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == "maul-preview-auth-config" {
			cm.Annotations = map[string]string{publishedAnnotation: iTailnetAddr}
		}
	}
	expect(t, w.run(t, interceptor.Funcs{}).Checks, CheckPreviewDNS, checkreport.Pass,
		"previewAuth.gateway.publishedAddresses")
	if asked {
		t.Error("the client was asked though publishedAddresses names the edge")
	}
}

// TestTailnetEdgeDefaults: a Service with only the expose annotation is
// the operator's <namespace>-<name> device, tagged tag:k8s.
func TestTailnetEdgeDefaults(t *testing.T) {
	w := tailnetWorld()
	for _, o := range w.objs {
		if svc, ok := o.(*corev1.Service); ok {
			svc.Annotations = map[string]string{tailscaleExpose: "true"}
		}
	}
	w.tailnet = tailnetStatus(`{"BackendState": "Running"}`)
	r := w.run(t, interceptor.Funcs{})
	expect(t, r.Checks, CheckPreviewDNS, checkreport.Skip, "the tailnet device maul-maul-preview-gateway (tag:k8s)")
	for _, l := range r.Checks {
		if strings.Contains(l.Reason, "\x1b") {
			t.Errorf("an escape reached a reason: %q", l.Reason)
		}
	}
}
