// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	chartDir      = "../../../../charts/patchy"
	renderFixture = "../../../../hack/testdata/chart-render/intent-controller.yaml"
	// offlineAPIServer stands in for apiServerCIDR auto, whose lookup needs
	// a cluster: what a GitOps render sets instead.
	offlineAPIServer = "10.43.0.1/32"
	// testDNSCIDR is the preview.dnsCIDR EKS Auto Mode needs, which the
	// operator's own values carry (NEXT-STEPS.md says so).
	testDNSCIDR = "172.20.0.10/32"
)

// renderValues renders the chart with a's generated values layered over a
// minimal install (intent-controller on), as NEXT-STEPS.md's helm upgrade
// does, and returns the objects.
func renderValues(t *testing.T, a Answers, files []File) []*unstructured.Unstructured {
	t.Helper()
	return renderOver(t, a, files, "")
}

// renderOver is renderValues with own, the operator's own values (YAML,
// "" for none), layered before the generated file as NEXT-STEPS.md says.
func renderOver(t *testing.T, a Answers, files []File, own string) []*unstructured.Unstructured {
	t.Helper()
	dir := t.TempDir()
	values := filepath.Join(dir, ValuesFile)
	if err := os.WriteFile(values, []byte(byPath(files)[ValuesFile]), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"template", a.Release, chartDir, "-n", a.Namespace, "-f", renderFixture}
	if own != "" {
		ownFile := filepath.Join(dir, "own.yaml")
		if err := os.WriteFile(ownFile, []byte(own), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-f", ownFile)
	}
	args = append(args, "-f", values, "--set", "previewAuth.keys.renderOffline=true")
	if a.APIServerCIDR == DefaultAPIServerCIDR {
		args = append(args, "--set", "previewController.config.apiServerCIDR="+offlineAPIServer)
	}
	if a.NodeIsolation.EKSAutoMode != (EKSAutoMode{}) {
		args = append(args, "--set", "preview.dnsCIDR="+testDNSCIDR)
	}
	cmd := exec.Command("helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var objs []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode the render: %v", err)
		}
		if m != nil {
			objs = append(objs, &unstructured.Unstructured{Object: m})
		}
	}
	return objs
}

func find(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}

// ingressHosts are an Ingress's rule hosts, and the hosts its TLS covers
// with each Secret.
func ingressHosts(o *unstructured.Unstructured) (rules []string, tls map[string][]string) {
	tls = map[string][]string{}
	rs, _, _ := unstructured.NestedSlice(o.Object, "spec", "rules")
	for _, r := range rs {
		if h, ok := r.(map[string]any)["host"].(string); ok {
			rules = append(rules, h)
		}
	}
	ts, _, _ := unstructured.NestedSlice(o.Object, "spec", "tls")
	for _, e := range ts {
		m := e.(map[string]any)
		name, _ := m["secretName"].(string)
		hs, _, _ := unstructured.NestedStringSlice(m, "hosts")
		tls[name] = append(tls[name], hs...)
	}
	return rules, tls
}

// TestGoldenValuesRender renders every golden case's values with the
// chart (design §9: drift between the wizard and the chart's schema and
// render is caught here) and holds that what they render routes both
// names to the gateway or the relay with the TLS the recipe promised: the
// gateway's Service the manifests name exists, every Ingress is classed
// as answered, the Certificate's Secret is what both Ingresses name for
// the hosts it covers, and the chart's HTTPRoute sits on the Gateway
// answered.
func TestGoldenValuesRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for name, a := range goldenCases() {
		t.Run(name, func(t *testing.T) {
			a.Normalize()
			files := plan(t, a)
			objs := renderValues(t, a, files)
			if find(objs, "Service", a.GatewayService()) == nil {
				t.Fatalf("no gateway Service %s in the render: Fullname disagrees with the chart", a.GatewayService())
			}
			relay := find(objs, "Ingress", a.Fullname()+"-preview-auth")
			if relay == nil {
				t.Fatal("no relay Ingress in the render")
			}
			if a.GatewayTerminatesTLS() {
				checkGatewayTLS(t, a, objs, files)
				return
			}
			if a.Edge == EdgeCloudflareTunnel {
				checkCloudflare(t, a, objs, files)
				return
			}
			if a.Existing.GatewayParent != "" {
				checkRoute(t, a, objs, files)
				return
			}
			gw := find(objs, "Ingress", a.GatewayService())
			if gw == nil {
				t.Fatal("no gateway Ingress in the render")
			}
			for _, ing := range []*unstructured.Unstructured{gw, relay} {
				class, _, _ := unstructured.NestedString(ing.Object, "spec", "ingressClassName")
				if class != a.Existing.IngressClass {
					t.Errorf("Ingress %s class = %q, want %q", ing.GetName(), class, a.Existing.IngressClass)
				}
			}
			gwRules, gwTLS := ingressHosts(gw)
			relayRules, relayTLS := ingressHosts(relay)
			if !slices.Equal(gwRules, []string{"*." + a.HostSuffix}) || !slices.Equal(relayRules, []string{a.SignInHost}) {
				t.Errorf("rules: gateway %v, relay %v", gwRules, relayRules)
			}
			if a.EdgeTLS() == TLSEdge {
				if len(gwTLS) != 0 || len(relayTLS) != 0 {
					t.Errorf("the edge holds the certificate, yet the Ingresses carry TLS: %v %v", gwTLS, relayTLS)
				}
				return
			}
			var cert struct {
				Spec struct {
					SecretName string   `json:"secretName"`
					DNSNames   []string `json:"dnsNames"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal([]byte(byPath(files)[CertificateFile]), &cert); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(gwTLS[cert.Spec.SecretName], gwRules) ||
				!slices.Equal(relayTLS[cert.Spec.SecretName], relayRules) {
				t.Errorf("Certificate Secret %s: gateway TLS %v, relay TLS %v", cert.Spec.SecretName, gwTLS, relayTLS)
			}
			if !slices.Equal(cert.Spec.DNSNames, append(gwRules, relayRules...)) {
				t.Errorf("Certificate covers %v, the Ingresses route %v and %v", cert.Spec.DNSNames, gwRules, relayRules)
			}
		})
	}
}

// checkRoute: on a Gateway API parent the chart's HTTPRoute carries the
// previews on the answered parent, and the wizard's route carries the
// sign-in host onto the gateway's Service.
func checkRoute(t *testing.T, a Answers, objs []*unstructured.Unstructured, files []File) {
	t.Helper()
	route := find(objs, "HTTPRoute", a.GatewayService())
	if route == nil {
		t.Fatal("no HTTPRoute in the render")
	}
	ns, name, section, _ := ParseGatewayParent(a.Existing.GatewayParent)
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	want := map[string]any{"name": name, "namespace": ns}
	if section != "" {
		want["sectionName"] = section
	}
	if len(parents) != 1 || !equalMaps(parents[0].(map[string]any), want) {
		t.Errorf("HTTPRoute parentRefs = %v, want [%v]", parents, want)
	}
	hosts, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
	if !slices.Equal(hosts, []string{"*." + a.HostSuffix}) {
		t.Errorf("HTTPRoute hostnames = %v", hosts)
	}
	var signIn struct {
		Spec struct {
			Hostnames []string `json:"hostnames"`
			Rules     []struct {
				BackendRefs []struct {
					Name string `json:"name"`
					Port int    `json:"port"`
				} `json:"backendRefs"`
			} `json:"rules"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(byPath(files)[SignInRouteFile]), &signIn); err != nil {
		t.Fatal(err)
	}
	svc := find(objs, "Service", a.GatewayService())
	port, _, _ := unstructured.NestedSlice(svc.Object, "spec", "ports")
	if !slices.Equal(signIn.Spec.Hostnames, []string{a.SignInHost}) || len(signIn.Spec.Rules) != 1 ||
		len(signIn.Spec.Rules[0].BackendRefs) != 1 || signIn.Spec.Rules[0].BackendRefs[0].Name != svc.GetName() ||
		fmt.Sprint(signIn.Spec.Rules[0].BackendRefs[0].Port) != fmt.Sprint(port[0].(map[string]any)["port"]) {
		t.Errorf("sign-in route %+v does not send %s to %s:%v", signIn.Spec, a.SignInHost, svc.GetName(), port)
	}
}

// checkGatewayTLS: in the gateway-tls and tailscale recipes the gateway's
// own Service is the only exposure (no gateway Ingress or HTTPRoute), it
// serves 443 with the Certificate's Secret projected into the relay, and
// it is a LoadBalancer of its own (Local, the answered class and source
// ranges) or a ClusterIP Service the Tailscale operator exposes as the
// answered device.
func checkGatewayTLS(t *testing.T, a Answers, objs []*unstructured.Unstructured, files []File) {
	t.Helper()
	gw := a.GatewayService()
	if find(objs, "Ingress", gw) != nil || find(objs, "HTTPRoute", gw) != nil {
		t.Error("a gateway Ingress or HTTPRoute rendered beside the gateway's own TLS")
	}
	svc := find(objs, "Service", gw)
	ports, _, _ := unstructured.NestedSlice(svc.Object, "spec", "ports")
	var https bool
	for _, p := range ports {
		if fmt.Sprint(p.(map[string]any)["port"]) == "443" {
			https = true
		}
	}
	if !https {
		t.Errorf("the gateway Service serves no 443: %v", ports)
	}
	checkGatewayService(t, a, svc)
	checkGatewayCertificate(t, a, objs, files)
}

// checkGatewayService: the gateway's Service is the recipe's exposure.
func checkGatewayService(t *testing.T, a Answers, svc *unstructured.Unstructured) {
	t.Helper()
	spec, _, _ := unstructured.NestedMap(svc.Object, "spec")
	ann := svc.GetAnnotations()
	switch a.Edge {
	case EdgeGatewayTLS:
		ranges, _, _ := unstructured.NestedStringSlice(svc.Object, "spec", "loadBalancerSourceRanges")
		class, _, _ := unstructured.NestedString(svc.Object, "spec", "loadBalancerClass")
		if spec["type"] != "LoadBalancer" || spec["externalTrafficPolicy"] != "Local" ||
			class != a.LoadBalancer.Class || !slices.Equal(ranges, a.LoadBalancer.InboundCIDRs) {
			t.Errorf("gateway Service spec = %v, want a Local LoadBalancer of class %q admitting %v", spec,
				a.LoadBalancer.Class, a.LoadBalancer.InboundCIDRs)
		}
	case EdgeTailscale:
		if spec["type"] != "ClusterIP" || ann["tailscale.com/expose"] != "true" ||
			ann["tailscale.com/hostname"] != a.TailscaleHostname() || ann["tailscale.com/tags"] != a.TailscaleTag() {
			t.Errorf("gateway Service type %v, annotations %v: not the tailnet device %s (%s)", spec["type"], ann,
				a.TailscaleHostname(), a.TailscaleTag())
		}
	}
}

// checkGatewayCertificate: the Certificate covers both names, the relay
// mounts its Secret, and it names the Issuer the recipe wrote.
func checkGatewayCertificate(t *testing.T, a Answers, objs []*unstructured.Unstructured, files []File) {
	t.Helper()
	var cert struct {
		Spec struct {
			SecretName string   `json:"secretName"`
			DNSNames   []string `json:"dnsNames"`
			IssuerRef  struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"issuerRef"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(byPath(files)[CertificateFile]), &cert); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cert.Spec.DNSNames, []string{"*." + a.HostSuffix, a.SignInHost}) {
		t.Errorf("Certificate covers %v", cert.Spec.DNSNames)
	}
	relay := find(objs, "Deployment", a.Fullname()+"-preview-auth")
	if relay == nil {
		t.Fatal("no relay Deployment in the render")
	}
	if !strings.Contains(fmt.Sprint(relay.Object), "name:"+cert.Spec.SecretName) {
		t.Errorf("the relay does not mount the Certificate's Secret %s", cert.Spec.SecretName)
	}
	if issuerFile, ok := byPath(files)[IssuerFile]; ok {
		var iss struct {
			Kind     string                           `json:"kind"`
			Metadata struct{ Name, Namespace string } `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(issuerFile), &iss); err != nil {
			t.Fatal(err)
		}
		wantNS := a.Namespace
		if iss.Kind == "ClusterIssuer" {
			wantNS = ""
		}
		if cert.Spec.IssuerRef.Kind != iss.Kind || cert.Spec.IssuerRef.Name != iss.Metadata.Name ||
			iss.Metadata.Namespace != wantNS {
			t.Errorf("Certificate names %+v, the %s written is %+v", cert.Spec.IssuerRef, iss.Kind, iss.Metadata)
		}
	}
}

func equalMaps(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// previewControllerConfig is the preview-controller's ConfigMap data in a
// render.
func previewControllerConfig(t *testing.T, objs []*unstructured.Unstructured) map[string]string {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() != "ConfigMap" {
			continue
		}
		data, _, _ := unstructured.NestedStringMap(o.Object, "data")
		if _, ok := data["PATCHY_PREVIEW_HOST_SUFFIX"]; ok {
			return data
		}
	}
	t.Fatal("no preview-controller ConfigMap in the render")
	return nil
}

// TestNodeIsolationOverOperatorValues: the generated values decide which
// nodes slot Pods select even over an operator's own values that isolate
// another way. Regression: an EKS Auto Mode pair in the operator's values
// survived the layering and was merged with the generated selector, so
// slot Pods required labels no node carried.
func TestNodeIsolationOverOperatorValues(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	const own = "preview:\n  nodeIsolation:\n    eksAutoMode:\n      nodePool: theirs\n      nodeClass: theirs\n" +
		"    taintKey: example.com/theirs\n"
	generic := goldenCases()["existing-alb-eks"]
	generic.Normalize()
	cfg := previewControllerConfig(t, renderOver(t, generic, plan(t, generic), own))
	if pool, ok := cfg["PATCHY_PREVIEW_NODE_POOL"]; ok {
		t.Errorf("the operator's NodePool %q survived the generated values", pool)
	}
	if got, want := cfg["PATCHY_PREVIEW_NODE_SELECTOR"], `{"patchy.bitwisemedia.uk/previews":"true"}`; got != want {
		t.Errorf("node selector = %s, want %s", got, want)
	}
	if got := cfg["PATCHY_PREVIEW_TAINT_KEY"]; got != DefaultTaintKey {
		t.Errorf("taint key = %s, want %s", got, DefaultTaintKey)
	}

	auto := goldenCases()["existing-alb-eks-auto-mode"]
	auto.Normalize()
	cfg = previewControllerConfig(t, renderOver(t, auto, plan(t, auto), own))
	if cfg["PATCHY_PREVIEW_NODE_POOL"] != "patchy-previews" || cfg["PATCHY_PREVIEW_NODE_CLASS"] != "patchy-previews" {
		t.Errorf("Auto Mode pair = %s/%s, want patchy-previews/patchy-previews", cfg["PATCHY_PREVIEW_NODE_POOL"],
			cfg["PATCHY_PREVIEW_NODE_CLASS"])
	}
	if sel, ok := cfg["PATCHY_PREVIEW_NODE_SELECTOR"]; ok {
		t.Errorf("Auto Mode also selects %s: no preview node carries a generic label there", sel)
	}
}

// checkCloudflare: in the cloudflare-tunnel recipe the gateway has no
// exposure of its own (no Ingress, HTTPRoute, LoadBalancer or TLS), and
// cloudflared's config, Deployment and NetworkPolicy reach exactly what
// the chart renders: the gateway Service's name, namespace and port 80
// for both names, and the gateway Pods by their label and their
// listener's port, behind forwardedHops 1.
func checkCloudflare(t *testing.T, a Answers, objs []*unstructured.Unstructured, files []File) {
	t.Helper()
	gwName := a.GatewayService()
	if find(objs, "Ingress", gwName) != nil || find(objs, "HTTPRoute", gwName) != nil {
		t.Error("a gateway Ingress or HTTPRoute rendered beside the tunnel")
	}
	svc := find(objs, "Service", gwName)
	spec, _, _ := unstructured.NestedMap(svc.Object, "spec")
	ports, _, _ := unstructured.NestedSlice(svc.Object, "spec", "ports")
	if spec["type"] != "ClusterIP" || len(ports) != 1 || fmt.Sprint(ports[0].(map[string]any)["port"]) != "80" ||
		ports[0].(map[string]any)["targetPort"] != "gateway" {
		t.Errorf("gateway Service type %v, ports %v: want ClusterIP port 80 to the gateway's listener", spec["type"],
			ports)
	}
	cm := find(objs, "ConfigMap", a.Fullname()+"-preview-auth-config")
	if cm == nil {
		t.Fatal("no relay ConfigMap in the render")
	}
	hops, _, _ := unstructured.NestedString(cm.Object, "data", "PATCHY_PREVIEW_AUTH_GATEWAY_FORWARDED_HOPS")
	if hops != "1" {
		t.Errorf("the gateway's forwardedHops = %q, want 1 (Cloudflare's edge appends the viewer)", hops)
	}
	relay := find(objs, "Deployment", a.Fullname()+"-preview-auth")
	if relay == nil {
		t.Fatal("no relay Deployment in the render")
	}
	podLabels, _, _ := unstructured.NestedStringMap(relay.Object, "spec", "template", "metadata", "labels")
	if podLabels[gatewayPodLabel] != "true" {
		t.Errorf("the relay's Pods carry no %s label: cloudflared's NetworkPolicy reaches nothing", gatewayPodLabel)
	}
	if !strings.Contains(fmt.Sprint(relay.Object), fmt.Sprintf("containerPort:%d name:gateway", gatewayPodPort)) {
		t.Errorf("the relay's gateway listener is not containerPort %d", gatewayPodPort)
	}
	checkCloudflared(t, a, files)
}

// checkCloudflared: cloudflared's config sends both names to the gateway
// Service's address and 404s the rest, its NetworkPolicy reaches the
// release namespace, and every object sits in cloudflared's namespace.
func checkCloudflared(t *testing.T, a Answers, files []File) {
	t.Helper()
	gwName := a.GatewayService()
	m := cloudflaredObjects(t, byPath(files)[CloudflaredFile])
	var cfg struct {
		Tunnel  string `json:"tunnel"`
		Ingress []struct{ Hostname, Service string }
	}
	if err := yaml.Unmarshal([]byte(m["ConfigMap"].Object["data"].(map[string]any)["config.yaml"].(string)),
		&cfg); err != nil {
		t.Fatal(err)
	}
	origin := fmt.Sprintf("http://%s.%s.svc.cluster.local:80", gwName, a.Namespace)
	if cfg.Tunnel != a.Cloudflare.TunnelID || len(cfg.Ingress) != 3 ||
		cfg.Ingress[0].Hostname != "*."+a.HostSuffix || cfg.Ingress[1].Hostname != a.SignInHost ||
		cfg.Ingress[0].Service != origin || cfg.Ingress[1].Service != origin ||
		cfg.Ingress[2].Hostname != "" || cfg.Ingress[2].Service != "http_status:404" {
		t.Errorf("cloudflared config %+v does not send both names to %s and 404 the rest", cfg, origin)
	}
	np := m["NetworkPolicy"]
	if !strings.Contains(fmt.Sprint(np.Object), "kubernetes.io/metadata.name:"+a.Namespace) {
		t.Errorf("cloudflared's NetworkPolicy does not reach namespace %s", a.Namespace)
	}
	for _, o := range m {
		if ns := o.GetNamespace(); o.GetKind() != "Namespace" && ns != a.CloudflaredNamespace() {
			t.Errorf("%s %s is in namespace %q, not %s", o.GetKind(), o.GetName(), ns, a.CloudflaredNamespace())
		}
	}
}

// cloudflaredObjects decodes manifests/cloudflared.yaml, by kind.
func cloudflaredObjects(t *testing.T, data string) map[string]*unstructured.Unstructured {
	t.Helper()
	out := map[string]*unstructured.Unstructured{}
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(data), 4096)
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode %s: %v", CloudflaredFile, err)
		}
		if m != nil {
			o := &unstructured.Unstructured{Object: m}
			if out[o.GetKind()] != nil {
				t.Fatalf("%s holds two %s objects", CloudflaredFile, o.GetKind())
			}
			out[o.GetKind()] = o
		}
	}
	return out
}
