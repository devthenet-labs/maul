// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
)

// The cloudflare-tunnel recipe's golden tree: the ConfigMap the check
// finds the tunnel by is the one `setup previews` writes, so the two
// cannot drift.
const (
	goldenTunnelDir    = "../previewsetup/testdata/golden/cloudflare-tunnel-k3s/"
	goldenTunnelSuffix = "acme-previews.dev"
	goldenTunnelID     = "6ff42ae2-765d-4adf-8112-31c55c1551ef"
	otherTunnelID      = "0a1b2c3d-0000-4000-8000-000000000001"
	// cfAddr is in Cloudflare's 104.16.0.0/13.
	cfAddr = "104.16.132.229"
)

// cnameWorld adds CNAME answers to an iResolver: a host not in cnames is
// its own canonical name, as net.Resolver reports one with no CNAME.
type cnameWorld struct {
	iResolver
	cnames map[string]string
}

func (r cnameWorld) LookupCNAME(_ context.Context, host string) (string, error) {
	if c, ok := r.cnames[host]; ok {
		return c + ".", nil
	}
	if _, ok := r.iResolver[host]; ok {
		return host + ".", nil
	}
	return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// goldenObjects are the golden manifest's documents of kind, decoded.
func goldenObjects[T any](t *testing.T, file, kind string) []T {
	t.Helper()
	raw, err := os.ReadFile(goldenTunnelDir + file)
	if err != nil {
		t.Fatal(err)
	}
	var out []T
	for doc := range bytes.SplitSeq(raw, []byte("\n---\n")) {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Kind != kind {
			continue
		}
		var o T
		if err := yaml.Unmarshal(doc, &o); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		t.Fatalf("no %s in %s", kind, file)
	}
	return out
}

// tunnelWorld is the install world served by the cloudflare-tunnel recipe:
// no gateway Ingress, the gateway's Service ClusterIP, and the recipe's
// cloudflared ConfigMap (routing this world's suffix) and credentials
// Secret, which must never be read.
func tunnelWorld(t *testing.T) *installWorld {
	t.Helper()
	w := newInstallWorld()
	w.drop("patchy-preview-gateway", &networkingv1.Ingress{})
	w.objs = append(w.objs, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "patchy-preview-gateway"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	})
	cm := goldenObjects[corev1.ConfigMap](t, "manifests/cloudflared.yaml", "ConfigMap")[0]
	cm.Data["config.yaml"] = strings.ReplaceAll(cm.Data["config.yaml"], goldenTunnelSuffix, iSuffix)
	w.objs = append(w.objs, &cm, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cm.Namespace,
		Name: "cloudflared-credentials"}})
	w.resolver[ProbeProject+"-0."+iSuffix] = []string{cfAddr}
	w.cnames = map[string]string{}
	return w
}

// TestInstallTunnel: with the cloudflare-tunnel recipe's cloudflared, the
// DNS line names the tunnel instead of the generic "publishes no address"
// SKIP, and passes only for a record that points at that tunnel.
func TestInstallTunnel(t *testing.T) {
	probe := ProbeProject + "-0." + iSuffix
	tests := map[string]struct {
		change func(*installWorld)
		status checkreport.Status
		want   []string
	}{
		"proxied record": {
			status: checkreport.Skip,
			want: []string{"Cloudflare's proxy (" + cfAddr + ")", "Cloudflare Tunnel " + goldenTunnelID,
				"cloudflared in patchy-cloudflared/cloudflared", "proxied; the target is hidden", "preview-auth-host proves routing"},
		},
		"CNAME to the tunnel": {
			change: func(w *installWorld) { w.cnames[probe] = goldenTunnelID + ".cfargotunnel.com" },
			status: checkreport.Pass,
			want:   []string{"is a CNAME to " + goldenTunnelID + ".cfargotunnel.com"},
		},
		"CNAME to another tunnel": {
			change: func(w *installWorld) { w.cnames[probe] = otherTunnelID + ".cfargotunnel.com" },
			status: checkreport.Warn,
			want: []string{"serves tunnel " + goldenTunnelID,
				"cloudflared tunnel route dns --overwrite-dns " + goldenTunnelID + " '*." + iSuffix + "'"},
		},
		"record elsewhere": {
			change: func(w *installWorld) { w.resolver[probe] = []string{"198.51.100.7"} },
			status: checkreport.Warn,
			want:   []string{"neither Cloudflare's proxy nor a CNAME to " + goldenTunnelID},
		},
		"private record elsewhere": {
			change: func(w *installWorld) { w.resolver[probe] = []string{"10.0.0.7"} },
			status: checkreport.Skip,
			want:   []string{"split-horizon"},
		},
		"no record": {
			change: func(w *installWorld) { delete(w.resolver, probe) },
			status: checkreport.Fail,
			want:   []string{"does not resolve", "a proxied CNAME to " + goldenTunnelID + ".cfargotunnel.com"},
		},
		"another host suffix's cloudflared": {
			change: func(w *installWorld) {
				w.setting("patchy-preview-controller", keyPreviewHostSuffix, "other-previews.test")
				w.setting("patchy-preview-auth-config", keyAuthHostSuffix, "other-previews.test")
				w.resolver[ProbeProject+"-0.other-previews.test"] = []string{cfAddr}
			},
			status: checkreport.Skip,
			want:   []string{"Cloudflare's proxy", "no cloudflared from `patchy setup previews"},
		},
		"no cloudflared, CNAME to a tunnel": {
			change: func(w *installWorld) {
				w.drop("cloudflared", &corev1.ConfigMap{})
				w.cnames[probe] = otherTunnelID + ".cfargotunnel.com"
			},
			status: checkreport.Skip,
			want:   []string{"Cloudflare Tunnel " + otherTunnelID, "which tunnel serves this install is unknown"},
		},
		"no cloudflared, not Cloudflare": {
			change: func(w *installWorld) {
				w.drop("cloudflared", &corev1.ConfigMap{})
				w.resolver[probe] = []string{"198.51.100.7"}
			},
			status: checkreport.Skip,
			want:   []string{"publishes no address"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := tunnelWorld(t)
			if tc.change != nil {
				tc.change(w)
			}
			r := w.run(t, interceptor.Funcs{})
			expect(t, r.Checks, CheckPreviewDNS, tc.status, tc.want...)
		})
	}
}

// TestInstallTunnelSignInHost: a sign-in host of its own gets the same
// tunnel verdict, by its own record.
func TestInstallTunnelSignInHost(t *testing.T) {
	w := tunnelWorld(t)
	w.setting("patchy-preview-auth-config", keyAuthGatewaySignIn, "login.acme.test")
	w.resolver["login.acme.test"] = []string{"198.51.100.9"}
	signInLine := func(r Report) Line {
		t.Helper()
		var got []Line
		for _, l := range r.Checks {
			if l.Name == CheckPreviewDNS && strings.HasPrefix(l.Reason, "the sign-in host login.acme.test ") {
				got = append(got, l)
			}
		}
		if len(got) != 1 {
			t.Fatalf("%d sign-in DNS lines:\n%s", len(got), dump(r.Checks))
		}
		return got[0]
	}
	l := signInLine(w.run(t, interceptor.Funcs{}))
	if l.Status != checkreport.Warn || !strings.Contains(l.Reason, "resolves to 198.51.100.9") ||
		!strings.Contains(l.Reason, "'login.acme.test'") {
		t.Errorf("sign-in host elsewhere: %s %s", l.Status, l.Reason)
	}
	w.cnames["login.acme.test"] = goldenTunnelID + ".cfargotunnel.com"
	l = signInLine(w.run(t, interceptor.Funcs{}))
	if l.Status != checkreport.Pass || !strings.Contains(l.Reason, "is a CNAME to "+goldenTunnelID) {
		t.Errorf("sign-in host on the tunnel: %s %s", l.Status, l.Reason)
	}
}

// TestPublishedAddressesNamedOnce: a published address is named once in
// the DNS line, not as the edge's name and again as its address; a
// published host name keeps its name beside its addresses.
func TestPublishedAddressesNamedOnce(t *testing.T) {
	for value, want := range map[string]string{
		iGateway:            "edge previewAuth.gateway.publishedAddresses (" + iGateway + ")",
		"lb.acme-edge.test": "edge previewAuth.gateway.publishedAddresses lb.acme-edge.test (" + iGateway + ")",
	} {
		t.Run(value, func(t *testing.T) {
			w := newInstallWorld()
			for _, o := range w.objs {
				if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == "patchy-preview-auth-config" {
					cm.Annotations = map[string]string{publishedAnnotation: value}
				}
			}
			w.resolver["lb.acme-edge.test"] = []string{iGateway}
			r := w.run(t, interceptor.Funcs{})
			expect(t, r.Checks, CheckPreviewDNS, checkreport.Pass, want)
			for _, l := range r.Checks {
				if l.Name == CheckPreviewDNS && strings.Count(l.Reason, iGateway) != 1 {
					t.Errorf("%s names %s %d times", l.Reason, iGateway, strings.Count(l.Reason, iGateway))
				}
			}
		})
	}
}

// TestJudgeTunnelDNSNeverPassesUnproven (seeded): whatever the record, the
// addresses and the cluster hold, the tunnel verdict passes only when the
// recipe's tunnel is known and the record is a visible CNAME to exactly
// that tunnel (a proxied record, which shows only Cloudflare's addresses,
// hides its target, and a CNAME to anything else is no proof); a known
// tunnel always
// gives a verdict, and with no tunnel and no Cloudflare in sight there is
// none.
func TestJudgeTunnelDNSNeverPassesUnproven(t *testing.T) {
	ids := []string{"", goldenTunnelID, otherTunnelID}
	addrPool := []string{cfAddr, "2606:4700::6810:84e5", "198.51.100.7", "10.0.0.7", "100.100.1.2", "not-an-ip"}
	cnamePool := []string{"", goldenTunnelID + ".cfargotunnel.com", otherTunnelID + ".cfargotunnel.com",
		"cfargotunnel.com", "x.cfargotunnel.com", goldenTunnelID + ".cfargotunnel.com.evil.test", "lb.acme.test"}
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		var in tunnelDNSInput
		in.host, in.record = "patchy-probe-0."+iSuffix, "*."+iSuffix
		in.tunnel.id = ids[r.Intn(len(ids))]
		if in.tunnel.id != "" {
			in.tunnel.where = "cloudflared in ns/cloudflared"
		}
		in.cname = cnamePool[r.Intn(len(cnamePool))]
		for range r.Intn(4) {
			in.addrs = append(in.addrs, addrPool[r.Intn(len(addrPool))])
		}
		args[0] = reflect.ValueOf(in)
	}
	prop := func(in tunnelDNSInput) bool {
		status, reason, ok := judgeTunnelDNS(in)
		exact := in.tunnel.id != "" && in.cname == in.tunnel.id+".cfargotunnel.com"
		switch {
		case status == checkreport.Pass && in.tunnel.id == "":
			t.Logf("PASS with no known tunnel: %+v: %s", in, reason)
			return false
		case status == checkreport.Pass && !exact:
			t.Logf("PASS without proof: %+v: %s", in, reason)
			return false
		case in.tunnel.id != "" && !ok:
			t.Logf("no verdict with a known tunnel: %+v", in)
			return false
		case ok && reason == "":
			t.Logf("a verdict with no reason: %+v", in)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestCloudflareProxied: Cloudflare's v4 and v6 ranges, nothing else, and
// never an empty answer.
func TestCloudflareProxied(t *testing.T) {
	for addrs, want := range map[string]bool{
		"":                                   false,
		cfAddr:                               true,
		"172.67.1.1,2606:4700::1":            true,
		"::ffff:104.16.0.1":                  true,
		cfAddr + ",198.51.100.7":             false,
		"10.0.0.1":                           false,
		"garbage":                            false,
		"2001:db8::1":                        false,
		fmt.Sprintf("%s,%s", cfAddr, cfAddr): true,
	} {
		var list []string
		if addrs != "" {
			list = strings.Split(addrs, ",")
		}
		if got := cloudflareProxied(list); got != want {
			t.Errorf("cloudflareProxied(%q) = %v, want %v", addrs, got, want)
		}
	}
}
