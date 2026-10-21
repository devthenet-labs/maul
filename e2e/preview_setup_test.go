// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/previewauth/fakedex"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// The install TestPreviewSetupEndToEnd asks `maul setup previews` for. The
// sign-in host and Dex sit on a registrable domain other than the previews'
// (acme.test beside acme-previews.test), as the wizard insists.
const (
	setupSuffix    = "acme-previews.test"
	setupSignIn    = "preview-auth.maul.acme.test"
	setupDexIssuer = "https://dex.maul.acme.test"
	setupViewer    = "alice"
)

// TestPreviewSetupEndToEnd configures the preview gateway from what `maul
// setup previews` writes (preview setup design §9, W10), for the
// gateway-tls recipe, where the gateway terminates TLS itself: the wizard
// runs, helm renders its values over a minimal install exactly as
// NEXT-STEPS.md's helm upgrade would, and the rendered ConfigMaps become the
// controllers' environment as their Deployments' envFrom makes them. The
// rendered Service, viewer binding, Lease and slot namespaces are applied;
// the Dex client secret both Secret manifests carry is what the fake Dex
// knows and what the relay is given; the gateway's certificate covers
// exactly the names the generated Certificate asks for, from a test CA.
// Then a viewer named on the wizard's command line signs in over the
// gateway's TLS and is routed to a preview, and `maul check previews`
// passes every check against that edge. What the test supplies itself is
// what a cluster would: the keys Secret, the mounts' paths, loopback
// listen addresses, the Dex at its own address, a preview node, and (in an
// e2edial build of each binary) the map from the names they dial to local
// listeners.
func TestPreviewSetupEndToEnd(t *testing.T) {
	cl := startCluster(t)
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatal("helm is not on PATH: `mise run e2e` provides it")
	}
	ctx := context.Background()
	dir := t.TempDir()
	maul := buildTagged(t, "maul", "e2edial")

	// The wizard, as an operator runs it.
	token := filepath.Join(dir, "dns01-token")
	if err := os.WriteFile(token, []byte("e2e-cloudflare-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "maul-previews")
	runMaul(t, maul, "setup", "previews", "--non-interactive", "-n", namespace, "--platform", "k3s",
		"--edge", "gateway-tls", "--dns", "cloudflare", "--dns-credentials-file", token,
		"--host-suffix", setupSuffix, "--sign-in-host", setupSignIn, "--dex-issuer", setupDexIssuer,
		"--viewer-user", setupViewer, "--registry", "registry.example", "--image-path-prefix", "maul/previews",
		"-o", out)

	// Dex knows the relay's client by the secret the wizard drew, from the
	// Secret manifest meant for Dex's namespace, and registers the one
	// redirect URI NEXT-STEPS.md gives it.
	dexSecret := secretKey(t, filepath.Join(out, "secrets", "dex-maul-preview-auth-client.secret.yaml"),
		"clientSecret")
	relaySecret := secretKey(t, filepath.Join(out, "secrets", "maul-preview-auth-dex.secret.yaml"),
		"clientSecret")
	if dexSecret == "" || dexSecret != relaySecret {
		t.Fatal("the two Dex client secret manifests disagree")
	}
	dex, err := fakedex.StartTLS("maul-preview-auth", dexSecret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dex.Close)
	callback := "https://" + setupSignIn + "/dex/callback"
	if !strings.Contains(readFile(t, filepath.Join(out, "NEXT-STEPS.md")), callback) {
		t.Fatalf("NEXT-STEPS.md does not name the redirect URI %s", callback)
	}
	dex.Register(callback)

	// The chart, over the wizard's values. The fake Dex stands in for the
	// operator's, so its address replaces the issuer the wizard was given;
	// the keys Secret and the API server's address are what a GitOps render
	// sets without a cluster to look up.
	objs := helmTemplate(t, "-f", "../hack/testdata/chart-render/intent-controller.yaml",
		"-f", filepath.Join(out, "maul-previews.values.yaml"),
		"--set", "previewAuth.keys.renderOffline=true",
		"--set", "previewController.config.apiServerCIDR=10.43.0.1/32",
		"--set", "previewAuth.dex.issuerURL="+dex.URL)
	authEnv, ctlEnv := applySetupRender(t, cl, objs)

	// A preview node, labelled and tainted as NEXT-STEPS.md registers one.
	selector, taintKey := nodeIsolation(t, filepath.Join(out, "maul-previews.values.yaml"))
	if err := cl.client.Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "preview-1", Labels: selector},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: taintKey, Value: "true",
			Effect: corev1.TaintEffectNoExecute}}},
	}); err != nil {
		t.Fatal(err)
	}
	rbactest.WaitAllowed(t, cl.client, rbactest.Review("github:"+setupViewer, nil, authorizationv1.ResourceAttributes{
		Namespace: namespace, Verb: "get", Group: v1alpha1.GroupVersion.Group,
		Resource: "projects", Subresource: "previews", Name: "shop",
	}))

	// The gateway's certificate: the generated Certificate's names, from a
	// test CA, in the directory layout the chart's projected volume gives.
	ca, caKey := testCA(t)
	tlsDir := filepath.Join(dir, "gateway-tls")
	writeGatewayPair(t, objs, tlsDir, ca, caKey, certificateNames(t, filepath.Join(out, "manifests",
		"certificate.yaml")))
	keysDir, dexCAFile, _ := signInFiles(t, dex)
	secretFile := filepath.Join(dir, "dex-client-secret")
	if err := os.WriteFile(secretFile, []byte(relaySecret), 0o600); err != nil {
		t.Fatal(err)
	}

	web1, api1, app2 := newPreviewApp(t, "web-1"), newPreviewApp(t, "api-1"), newPreviewApp(t, "app-2")
	dial := map[string]string{}
	for slot := range 2 {
		for svc, app := range map[string]*previewApp{"preview-shop-1": web1, "preview-shop-1-api": api1,
			"preview-shop-2": app2, "preview-shop-2-api": app2} {
			dial[fmt.Sprintf("%s.maul-preview-%d.svc:80", svc, slot)] = app.srv.Listener.Addr().String()
		}
	}
	dialJSON, err := json.Marshal(dial)
	if err != nil {
		t.Fatal(err)
	}

	cl.serveBinaryEnv(t, "preview-controller", build(t, "preview-controller"), ctlEnv,
		"--namespace", namespace, "--preview-poll-interval", "1s")
	gwTLS := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.serveBinaryEnv(t, "preview-auth", buildTagged(t, "preview-auth", "e2edial"),
		append(authEnv, "POD_NAMESPACE="+namespace),
		"--namespace", namespace,
		"--listen-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"--preview-auth-keys-dir", keysDir,
		"--preview-auth-dex-client-secret-file", secretFile, "--preview-auth-dex-ca-file", dexCAFile,
		"--preview-auth-gateway-listen-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"--preview-auth-gateway-tls-dir", tlsDir, "--preview-auth-gateway-tls-listen-addr", gwTLS,
		// The test client is every request's one address, and the probe
		// has no real host to ask.
		"--preview-auth-rate-per-second", "0", "--preview-auth-probe-interval", "0s",
		"--e2e-gateway-dial-map", string(dialJSON))

	shop := gatewayProject(t, cl)
	in1, p1 := gatewayPreview(t, cl, shop, 1)
	pods1 := readyComponents(t, cl, p1, "a")

	// alice, the viewer the wizard was told of, signs in over the gateway's
	// own TLS and is routed by path.
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	roots.AddCert(dex.Certificate())
	base, ok := dex.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("the fake Dex's client has no *http.Transport")
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(addr); host == setupSignIn || strings.HasSuffix(host, "."+setupSuffix) {
			addr = gwTLS
		}
		return d.DialContext(ctx, network, addr)
	}
	dex.SignIn(map[string]any{"sub": "dex-alice", "preferred_username": setupViewer})
	alice := newBrowserFor(t, "", setupSignIn, setupSuffix, tr)
	origin := "https://" + in1.Name + "." + setupSuffix
	if got := alice.signIn(in1.Name, "/api/hello", true); got != origin+"/api/hello" {
		t.Fatalf("the handoff returned to %s", got)
	}
	for path, want := range map[string]string{"/api/hello": "api-1 GET /api/hello", "/": "web-1 GET /"} {
		if status, _, body := alice.nav(origin + path); status != http.StatusOK || body != want {
			t.Errorf("GET %s: %d %q, want %q", path, status, body, want)
		}
	}

	// The command NEXT-STEPS.md ends with, against that edge: every check
	// passes, but for the allowlist's, which has nothing to prove with no
	// inboundCIDRs (and from an admitted address would skip anyway).
	if !strings.Contains(readFile(t, filepath.Join(out, "NEXT-STEPS.md")), "maul check previews -n "+namespace) {
		t.Error("NEXT-STEPS.md does not end with maul check previews")
	}
	caFile := filepath.Join(dir, "check-roots.pem")
	rootsPEM := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: dex.Certificate().Raw})...)
	if err := os.WriteFile(caFile, rootsPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	dexAddr := strings.TrimPrefix(dex.URL, "https://")
	checkDial, err := json.Marshal(map[string]string{
		"maul-probe-0." + setupSuffix + ":443": gwTLS,
		setupSignIn + ":443":                   gwTLS,
		origin[len("https://"):] + ":443":      gwTLS,
		dexAddr:                                dexAddr,
	})
	if err != nil {
		t.Fatal(err)
	}
	report := runMaul(t, maul, "check", "previews", "-n", namespace, "--kubeconfig", cl.kubeconfig, "-o", "json",
		"--e2e-dial-map", string(checkDial), "--e2e-ca-file", caFile)
	var got struct {
		Checks []struct{ Name, Status, Reason string } `json:"checks"`
	}
	if err := json.Unmarshal([]byte(report), &got); err != nil {
		t.Fatalf("check previews printed %q: %v", report, err)
	}
	names := make([]string, 0, len(got.Checks))
	for _, c := range got.Checks {
		names = append(names, c.Name)
		if c.Status != "PASS" && (c.Name != "preview-gateway-allowlist" || c.Status != "SKIP") {
			t.Errorf("%s: %s: %s", c.Name, c.Status, c.Reason)
		}
	}
	for _, want := range []string{"preview-config", "preview-dns", "preview-tls", "preview-auth", "preview-auth-dex",
		"preview-auth-host", "preview-viewers", "preview-nodes"} {
		if !slices.Contains(names, want) {
			t.Errorf("check previews ran no %s check: %v", want, names)
		}
	}

	for _, pod := range pods1 {
		_ = cl.client.Delete(ctx, pod)
	}
}

// runMaul runs the CLI with args and returns its standard output, failing
// the test on a non-zero exit.
func runMaul(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("maul %s: %v\n%s\n%s", strings.Join(args[:2], " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// decodeAll decodes every YAML document in data.
func decodeAll(t *testing.T, data []byte) []*unstructured.Unstructured {
	t.Helper()
	var objs []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return objs
		} else if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if m != nil {
			objs = append(objs, &unstructured.Unstructured{Object: m})
		}
	}
}

// secretKey is one key of the Secret manifest at path, decoded.
func secretKey(t *testing.T, path, key string) string {
	t.Helper()
	var s corev1.Secret
	if err := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(readFile(t, path)), 4096).Decode(&s); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(s.Data[key])
}

// helmTemplate renders the maul chart as release maul in the release
// namespace with args.
func helmTemplate(t *testing.T, args ...string) []*unstructured.Unstructured {
	t.Helper()
	cmd := exec.Command("helm", append([]string{"template", "maul", "../charts/maul", "-n", namespace},
		args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	return decodeAll(t, out)
}

// applySetupRender creates what the previews need from the render: the
// slot namespaces, the gateway's Service (its load balancer at loopback),
// the viewer role and binding, the relay's code ledger Lease, and the two
// controllers' ConfigMaps, whose data it returns as each one's environment.
func applySetupRender(t *testing.T, cl *cluster, objs []*unstructured.Unstructured) (authEnv, ctlEnv []string) {
	t.Helper()
	ctx := context.Background()
	for _, o := range objs {
		apply := false
		switch name := o.GetName(); o.GetKind() {
		case "Namespace":
			apply = strings.HasPrefix(name, "maul-preview-")
		case "Service":
			apply = name == "maul-preview-gateway"
		case "ClusterRole":
			apply = name == "maul-preview-viewer"
		case "RoleBinding":
			apply = name == "maul-preview-viewers"
		case "Lease":
			apply = name == "maul-preview-auth-codes"
		case "ConfigMap":
			switch o.GetLabels()["app.kubernetes.io/name"] {
			case "preview-auth":
				authEnv, apply = configMapEnv(t, o), true
			case "preview-controller":
				ctlEnv, apply = configMapEnv(t, o), true
			}
		}
		if !apply {
			continue
		}
		if err := cl.client.Create(ctx, o); err != nil {
			t.Fatalf("create %s %s: %v", o.GetKind(), o.GetName(), err)
		}
	}
	if authEnv == nil || ctlEnv == nil {
		t.Fatal("the render has no preview-auth or preview-controller ConfigMap")
	}
	var svc corev1.Service
	if err := cl.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "maul-preview-gateway"},
		&svc); err != nil {
		t.Fatalf("the gateway's Service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("the gateway-tls recipe's Service is %s, not a LoadBalancer", svc.Spec.Type)
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "127.0.0.1"}}
	if err := cl.client.Status().Update(ctx, &svc); err != nil {
		t.Fatal(err)
	}
	return authEnv, ctlEnv
}

// configMapEnv is a ConfigMap's data as envFrom puts it in a container's
// environment.
func configMapEnv(t *testing.T, o *unstructured.Unstructured) []string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &cm); err != nil {
		t.Fatal(err)
	}
	env := make([]string, 0, len(cm.Data))
	for k, v := range cm.Data {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)
	return env
}

// nodeIsolation is the generated values' preview node selector and taint
// key.
func nodeIsolation(t *testing.T, valuesFile string) (map[string]string, string) {
	t.Helper()
	var v struct {
		Preview struct {
			NodeIsolation struct {
				Selector map[string]string `json:"selector"`
				TaintKey string            `json:"taintKey"`
			} `json:"nodeIsolation"`
		} `json:"preview"`
	}
	if err := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(readFile(t, valuesFile)), 4096).Decode(&v); err != nil {
		t.Fatal(err)
	}
	ni := v.Preview.NodeIsolation
	if len(ni.Selector) == 0 || ni.TaintKey == "" {
		t.Fatalf("the generated values isolate no preview node: %+v", ni)
	}
	return ni.Selector, ni.TaintKey
}

// certificateNames are the generated Certificate's dnsNames.
func certificateNames(t *testing.T, path string) []string {
	t.Helper()
	var c struct {
		Spec struct {
			DNSNames []string `json:"dnsNames"`
		} `json:"spec"`
	}
	if err := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(readFile(t, path)), 4096).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if len(c.Spec.DNSNames) == 0 {
		t.Fatal("the generated Certificate names no host")
	}
	return c.Spec.DNSNames
}

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "maul e2e CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key
}

// writeGatewayPair issues a certificate for names from ca and writes it
// where the chart's gateway-tls projected volume puts the Secret's tls.crt
// and tls.key, under dir.
func writeGatewayPair(t *testing.T, objs []*unstructured.Unstructured, dir string, ca *x509.Certificate,
	caKey *ecdsa.PrivateKey, names []string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	content := map[string][]byte{
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
	var dep *unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() == "Deployment" && o.GetName() == "maul-preview-auth" {
			dep = o
		}
	}
	if dep == nil {
		t.Fatal("the render has no preview-auth Deployment")
	}
	volumes, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "volumes")
	written := 0
	for _, v := range volumes {
		vol, _ := v.(map[string]any)
		if vol["name"] != "gateway-tls" {
			continue
		}
		sources, _, _ := unstructured.NestedSlice(vol, "projected", "sources")
		for _, s := range sources {
			items, _, _ := unstructured.NestedSlice(s.(map[string]any), "secret", "items")
			for _, i := range items {
				item, _ := i.(map[string]any)
				key, _ := item["key"].(string)
				path, _ := item["path"].(string)
				data, ok := content[key]
				if !ok || path == "" {
					t.Fatalf("the gateway-tls volume projects key %q to %q", key, path)
				}
				file := filepath.Join(dir, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, data, 0o600); err != nil {
					t.Fatal(err)
				}
				written++
			}
		}
	}
	if written != len(content) {
		t.Fatalf("the gateway-tls volume projected %d files, want %d", written, len(content))
	}
}
