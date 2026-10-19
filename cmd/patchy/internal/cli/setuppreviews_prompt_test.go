// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/kubecfg"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/tailnet"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// execPreviews runs `patchy` with args, standard input answering the
// prompts, as if on a terminal when terminal says so, against env when one
// is given, and returns what it printed on stderr.
func execPreviews(t *testing.T, terminal bool, env *kubecfg.Env, stdin string, args ...string) (string, error) {
	t.Helper()
	return execPreviewsTailnet(t, terminal, env, noTailnet, stdin, args...)
}

// noTailnet is a machine with no Tailscale client: the tests never ask
// the real one.
func noTailnet(context.Context) ([]byte, error) { return nil, tailnet.ErrNoClient }

// execPreviewsTailnet is execPreviews with status standing in for this
// machine's Tailscale client.
func execPreviewsTailnet(t *testing.T, terminal bool, env *kubecfg.Env, status tailnet.StatusFunc, stdin string,
	args ...string) (string, error) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	opts := &Options{Out: out, ErrOut: errOut, Output: "table", NoColor: true,
		promptTerminal: func(io.Reader) bool { return terminal }, tailnetStatus: status}
	if env != nil {
		opts.WithEnv(env)
	}
	root := NewRoot(opts)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return errOut.String(), err
}

// answers joins prompt answers, one per line.
func answers(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// TestSetupPreviewsAsks: on a terminal with no answer flag, every answer is
// asked for, each prompt naming its flag; a refused answer is asked again;
// the files are written; and a re-run from answers.yaml asks nothing.
func TestSetupPreviewsAsks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	in := answers(
		"k3s",                            // --platform
		"",                               // --edge: k3s's default, tailscale
		"acme-previews.dev",              // --host-suffix
		"preview-auth.acme-previews.dev", // --sign-in-host, refused: under the suffix
		"preview-auth.patchy.acme.dev",   // --sign-in-host
		"https://dex.patchy.acme.dev",    // --dex-issuer
		"acme:qa, octocat",               // viewers
		"GHCR.io",                        // --registry
		"route53",                        // --dns
		"us-east-1",                      // region
		"",                               // accessKeyID: none, cert-manager's own identity
		"",                               // --tailscale-oauth-dir: later
	)
	errOut, err := execPreviews(t, true, nil, in, "setup", "previews", "-o", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	for _, want := range []string{"(--platform) [k3d, k3s, eks, gke, aks, other]", "(--edge)", "(tailscale)",
		"(--host-suffix)", "sits under --host-suffix", "(--viewer-team, --viewer-user or --no-viewers)",
		"(--dns) [route53,", "(--dns-option region=)", "(--dns-option accessKeyID=)", "(blank: none)",
		"(--tailscale-oauth-dir)"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the prompts lack %q:\n%s", want, errOut)
		}
	}
	a, err := previewsetup.ParseAnswers([]byte(readFile(t, dir, previewsetup.AnswersFile)))
	if err != nil {
		t.Fatal(err)
	}
	if a.Platform != previewsetup.PlatformK3s || a.Edge != previewsetup.EdgeTailscale ||
		a.SignInHost != "preview-auth.patchy.acme.dev" || a.Registry != "ghcr.io" ||
		strings.Join(a.Viewers.Teams, ",") != "acme:qa" || strings.Join(a.Viewers.Users, ",") != "octocat" ||
		a.Certificate.DNS != "route53" || a.Certificate.DNSOptions["region"] != "us-east-1" ||
		a.Certificate.DNSOptions["accessKeyID"] != "" {
		t.Errorf("answers.yaml = %+v", a)
	}
	errOut, err = execPreviews(t, true, nil, "", "setup", "previews", "-o", dir)
	if err != nil || strings.Contains(errOut, "? ") || !strings.Contains(errOut, "up to date") {
		t.Errorf("a re-run asked or changed something: %v\n%s", err, errOut)
	}
}

// TestSetupPreviewsWithoutATerminal: off a terminal, or with
// --non-interactive, nothing is asked and a missing answer is a usage
// error naming its flag.
func TestSetupPreviewsWithoutATerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		args     []string
		hint     string
	}{
		{"no terminal", false, nil, "run it in a terminal to be asked"},
		{"non-interactive", true, []string{"--non-interactive"}, "run it in a terminal to be asked"},
		// The credential is standard input, so it asks nothing there.
		{"credential on stdin", true, []string{"--dns-credentials-from-stdin"}, "run it in a terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"setup", "previews", "-o", t.TempDir(), "--platform", "k3s"}, tc.args...)
			errOut, err := execPreviews(t, tc.terminal, nil, "k3s\n", args...)
			var u *usageError
			if !errors.As(err, &u) || !strings.Contains(err.Error(), "--edge, --host-suffix") ||
				!strings.Contains(err.Error(), tc.hint) {
				t.Errorf("err = %v, want a usage error naming --edge and --host-suffix", err)
			}
			if strings.Contains(errOut, "? ") {
				t.Errorf("asked a question:\n%s", errOut)
			}
		})
	}
}

// TestSetupPreviewsInputClosed: standard input ending before every answer
// is given is a usage error, and nothing is written.
func TestSetupPreviewsInputClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	_, err := execPreviews(t, true, nil, "k3s\n", "setup", "previews", "-o", dir)
	if !errors.Is(err, errPromptClosed) {
		t.Fatalf("err = %v, want errPromptClosed", err)
	}
	if exists(dir) {
		t.Error("wrote files after the input closed")
	}
}

// TestSetupPreviewsRefusesThreeTimes: an answer refused three times ends
// the run.
func TestSetupPreviewsRefusesThreeTimes(t *testing.T) {
	_, err := execPreviews(t, true, nil, answers("mars", "venus", "pluto"), "setup", "previews", "-o",
		t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no answer --platform takes after 3 tries") {
		t.Errorf("err = %v", err)
	}
}

// TestSetupPreviewsConfirmsChanges: a re-run that changes a file asks
// before writing on a terminal (no by default), and -y writes without
// asking.
func TestSetupPreviewsConfirmsChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	if errOut, err := execPreviews(t, false, nil, "", previewsArgs(dir)...); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	change := []string{"setup", "previews", "-o", dir, "--registry", "registry.acme.dev"}
	var conflict *previewsetup.ConflictError
	if errOut, err := execPreviews(t, true, nil, "\n", change...); !errors.As(err, &conflict) ||
		!strings.Contains(errOut, "? write these changes? [y/N]") {
		t.Errorf("an empty answer wrote the change: %v\n%s", err, errOut)
	}
	if strings.Contains(readFile(t, dir, previewsetup.ValuesFile), "registry.acme.dev") {
		t.Fatal("the change was written without a yes")
	}
	if errOut, err := execPreviews(t, true, nil, "y\n", change...); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if !strings.Contains(readFile(t, dir, previewsetup.ValuesFile), "registry.acme.dev") {
		t.Error("yes did not write the change")
	}
	back := []string{"setup", "previews", "-o", dir, "--registry", "ghcr.io", "-y"}
	if errOut, err := execPreviews(t, false, nil, "", back...); err != nil || strings.Contains(errOut, "? ") {
		t.Fatalf("-y: %v\n%s", err, errOut)
	}
	if strings.Contains(readFile(t, dir, previewsetup.ValuesFile), "registry.acme.dev") {
		t.Error("-y did not write the change")
	}
}

// TestSetupPreviewsAsksForTheTunnelCredentials: the cloudflare-tunnel
// recipe asks for its credentials file's path (never its content), and
// reads it as --tunnel-credentials would.
func TestSetupPreviewsAsksForTheTunnelCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	creds := writeTunnelFile(t, firstTunnel, 0o600)
	args := cloudflareArgs(dir)
	errOut, err := execPreviews(t, true, nil, answers(filepath.Join(t.TempDir(), "missing.json"), creds),
		args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, "(--tunnel-credentials)") || !strings.Contains(errOut, "no such file") {
		t.Errorf("the prompt or its refusal is missing:\n%s", errOut)
	}
	if got := tunnelInSecret(t, dir); !strings.Contains(got, firstTunnel) {
		t.Errorf("the tunnel Secret holds %q", got)
	}
	if strings.Contains(errOut, tunnelSecret(firstTunnel)) {
		t.Error("the tunnel secret was printed")
	}
}

// detectWorld is a k3d cluster with cert-manager and an earlier release's
// preview settings, plus Secrets that must never be read.
func detectWorld() []client.Object {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1",
		Kind: "CustomResourceDefinition"})
	crd.SetName("certificates.cert-manager.io")
	labels := func(c string) map[string]string {
		return map[string]string{"app.kubernetes.io/name": c, "app.kubernetes.io/part-of": "patchy"}
	}
	return []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k3d-dev-server-0"},
			Spec: corev1.NodeSpec{ProviderID: "k3s://k3d-dev-server-0"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "k3d-dev-agent-0"},
			Spec: corev1.NodeSpec{ProviderID: "k3s://k3d-dev-agent-0"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kubernetes"},
			Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.1"}},
		crd,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "patchy-preview-controller",
			Labels: labels("preview-controller")}, Data: map[string]string{
			"PATCHY_PREVIEW_HOST_SUFFIX": "acme-previews.dev", "PATCHY_PREVIEW_LEGACY_INGRESS": "false"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "patchy-preview-auth-config",
			Labels: labels("preview-auth")}, Data: map[string]string{
			"PATCHY_PREVIEW_AUTH_ISSUER":         "https://preview-auth.patchy.acme.dev",
			"PATCHY_PREVIEW_AUTH_DEX_ISSUER_URL": "https://dex.patchy.acme.dev",
			"PATCHY_PREVIEW_AUTH_GATEWAY":        "true"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: "patchy-preview-auth-dex"},
			Data: map[string][]byte{"clientSecret": []byte("NEVER-READ")}},
	}
}

// guardedEnv is an environment over objs that fails the test on any Secret
// read or any write, and counts the calls made.
func guardedEnv(t *testing.T, objs []client.Object, calls *int) *kubecfg.Env {
	t.Helper()
	fail := func(what string) error {
		t.Errorf("setup previews %s", what)
		return errors.New(what)
	}
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).WithInterceptorFuncs(
		interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				*calls++
				if _, ok := obj.(*corev1.Secret); ok || obj.GetObjectKind().GroupVersionKind().Kind == "Secret" {
					return fail("read Secret " + key.String())
				}
				return c.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
				*calls++
				if _, ok := l.(*corev1.SecretList); ok || l.GetObjectKind().GroupVersionKind().Kind == "SecretList" {
					return fail("listed Secrets")
				}
				return c.List(ctx, l, opts...)
			},
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return fail("created an object")
			},
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return fail("updated an object")
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return fail("patched an object")
			},
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return fail("deleted an object")
			},
		}).Build()
	return &kubecfg.Env{Client: c, Namespace: "patchy"}
}

// TestSetupPreviewsDetect: --detect fills in what the cluster says (the
// platform from the nodes, an existing release's names, cert-manager for a
// recipe that needs it), says what it found, never overrides a flag, and
// reads no Secret.
func TestSetupPreviewsDetect(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	calls := 0
	env := guardedEnv(t, detectWorld(), &calls)
	errOut, err := execPreviews(t, false, env, "", "setup", "previews", "-o", dir, "--detect",
		"--edge", "tailscale", "--dns", "route53", "--dns-option", "region=us-east-1", "--viewer-team", "acme:qa",
		"--registry", "ghcr.io", "--dex-issuer", "https://dex.other.dev")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if calls == 0 {
		t.Fatal("--detect read nothing")
	}
	for _, want := range []string{
		"patchy: detected: platform: k3d (2 nodes, provider ID k3s://)",
		"patchy: detected: cert-manager: installed",
		"patchy: detected answer: --platform k3d",
		"patchy: detected answer: --host-suffix acme-previews.dev",
		"patchy: detected answer: --sign-in-host preview-auth.patchy.acme.dev",
		"patchy: detected answer: --cert-manager-installed true",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "--dex-issuer https://dex.patchy.acme.dev") {
		t.Errorf("detection overrode --dex-issuer:\n%s", errOut)
	}
	a, err := previewsetup.ParseAnswers([]byte(readFile(t, dir, previewsetup.AnswersFile)))
	if err != nil {
		t.Fatal(err)
	}
	if a.Platform != previewsetup.PlatformK3d || a.Dex.Issuer != "https://dex.other.dev" ||
		!a.Certificate.CertManagerInstalled || a.APIServerCIDR != previewsetup.DefaultAPIServerCIDR {
		t.Errorf("answers.yaml = %+v", a)
	}
	if exists(filepath.Join(dir, "upstream", "cert-manager.values.yaml")) {
		t.Error("values for cert-manager written though it is installed")
	}
}

// TestSetupPreviewsContactsNoClusterWithoutDetect: without --detect the
// command makes no cluster call at all (design §4.1: the kubeconfig flags
// are inert).
func TestSetupPreviewsContactsNoClusterWithoutDetect(t *testing.T) {
	calls := 0
	env := guardedEnv(t, detectWorld(), &calls)
	if errOut, err := execPreviews(t, false, env, "", previewsArgs(t.TempDir())...); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if calls != 0 {
		t.Errorf("made %d cluster calls without --detect", calls)
	}
}

// TestSetupPreviewsDetectNeedsACluster: --detect without a reachable
// cluster fails rather than silently detecting nothing.
func TestSetupPreviewsDetectNeedsACluster(t *testing.T) {
	_, err := execPreviews(t, false, nil, "", append(previewsArgs(t.TempDir()), "--detect",
		"--kubeconfig", filepath.Join(t.TempDir(), "absent"))...)
	if err == nil {
		t.Fatal("--detect with no cluster succeeded")
	}
}

// TestSetupPreviewsDetectUnreachableCluster: a kubeconfig that loads, but
// whose API server refuses every connection, is an error too, not a run
// that detects nothing (the client connects lazily, so loading it proves
// nothing), and no file is written.
func TestSetupPreviewsDetectUnreachableCluster(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := "https://" + l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: refused
  cluster:
    server: `+server+`
    insecure-skip-tls-verify: true
users:
- name: refused
  user:
    token: not-a-token
contexts:
- name: refused
  context: {cluster: refused, user: refused, namespace: patchy}
current-context: refused
`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	errOut, err := execPreviews(t, false, nil, "", append(previewsArgs(dir), "--detect", "--non-interactive",
		"--kubeconfig", kubeconfig)...)
	if err == nil || !strings.Contains(err.Error(), "--detect could not reach the cluster") {
		t.Fatalf("err = %v, want the cluster unreachable\n%s", err, errOut)
	}
	if strings.Contains(errOut, "previews not configured yet") {
		t.Errorf("judged an unreachable cluster:\n%s", errOut)
	}
	if exists(dir) {
		t.Error("wrote files though detection failed")
	}
}

// TestSetupPreviewsDetectOnATerminal: on a terminal, detection runs again
// over the answers given at the prompts, so an answer that turns on a
// detected fill (the edge, here: cert-manager matters only to an edge that
// terminates TLS) gets it, and answers.yaml records it.
func TestSetupPreviewsDetectOnATerminal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	calls := 0
	env := guardedEnv(t, detectWorld(), &calls)
	errOut, err := execPreviews(t, true, env, answers("tailscale", "", "", ""), "setup", "previews", "-o", dir, "--detect",
		"--dns", "route53", "--dns-option", "region=us-east-1",
		"--viewer-team", "acme:qa", "--registry", "ghcr.io")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	prompt := strings.Index(errOut, "(--edge)")
	filled := strings.Index(errOut, "patchy: detected answer: --cert-manager-installed true")
	if prompt < 0 || filled < prompt {
		t.Fatalf("--cert-manager-installed not filled after the --edge prompt (prompt at %d, fill at %d):\n%s",
			prompt, filled, errOut)
	}
	a, err := previewsetup.ParseAnswers([]byte(readFile(t, dir, previewsetup.AnswersFile)))
	if err != nil {
		t.Fatal(err)
	}
	if a.Edge != previewsetup.EdgeTailscale || !a.Certificate.CertManagerInstalled {
		t.Errorf("answers.yaml = %+v", a)
	}
	if exists(filepath.Join(dir, "upstream", "cert-manager.values.yaml")) {
		t.Error("values for cert-manager written though it is installed")
	}
}

// TestSetupPreviewsDetectOnARerun: a re-run's answers.yaml is the starting
// point, and detection fills what is still open there, as documented: a
// recorded false --cert-manager-installed becomes true once cert-manager's
// CRD is found, while a recorded answer of any other kind (here the Dex
// issuer) stays.
func TestSetupPreviewsDetectOnARerun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	args := []string{"setup", "previews", "-o", dir, "--platform", "k3d", "--edge", "tailscale",
		"--host-suffix", "acme-previews.dev", "--sign-in-host", "preview-auth.patchy.acme.dev",
		"--dex-issuer", "https://dex.other.dev", "--dns", "route53", "--dns-option", "region=us-east-1",
		"--viewer-team", "acme:qa", "--registry", "ghcr.io"}
	if errOut, err := execPreviews(t, false, nil, "", args...); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	calls := 0
	errOut, err := execPreviews(t, false, guardedEnv(t, detectWorld(), &calls), "", "setup", "previews", "-o",
		dir, "--detect", "-y")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, "patchy: detected answer: --cert-manager-installed true") ||
		strings.Contains(errOut, "detected answer: --dex-issuer") {
		t.Errorf("the re-run's fills:\n%s", errOut)
	}
	a, err := previewsetup.ParseAnswers([]byte(readFile(t, dir, previewsetup.AnswersFile)))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Certificate.CertManagerInstalled || a.Dex.Issuer != "https://dex.other.dev" {
		t.Errorf("answers.yaml = %+v", a)
	}
}

// TestSetupPreviewsAsksSharedDomain: on a terminal, a sign-in host on the
// suffix's registrable domain is explained and the acknowledgement asked
// for, once, taking only "acknowledged"; a blank answer stops with the
// flags to change.
func TestSetupPreviewsAsksSharedDomain(t *testing.T) {
	base := []string{"setup", "previews", "--platform", "k3s", "--edge", "existing", "--ingress-class", "traefik",
		"--viewer-team", "acme:qa", "--registry", "ghcr.io"}
	dir := filepath.Join(t.TempDir(), "out")
	in := answers(
		"previews.acme.dev",     // --host-suffix
		"preview-auth.acme.dev", // --sign-in-host, same-site with the previews
		"yes",                   // --shared-domain, refused
		"acknowledged",          // --shared-domain
		"https://dex.acme.dev",  // --dex-issuer: already acknowledged, not asked again
	)
	errOut, err := execPreviews(t, true, nil, in, append(base, "-o", dir)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	for _, want := range []string{"preview-auth.acme.dev is on acme.dev", "never same-origin", "__Host-",
		"no app there may rely on a parent-domain cookie or on SameSite for CSRF", "(--shared-domain)",
		"answer acknowledged, or leave it blank"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the prompts lack %q:\n%s", want, errOut)
		}
	}
	if n := strings.Count(errOut, "(--shared-domain)"); n != 2 {
		t.Errorf("asked --shared-domain %d times, want 2 (one refused answer):\n%s", n, errOut)
	}
	a, err := previewsetup.ParseAnswers([]byte(readFile(t, dir, previewsetup.AnswersFile)))
	if err != nil || a.SharedDomainAck != previewsetup.SharedDomainAcknowledged {
		t.Errorf("answers.yaml: %+v, %v", a, err)
	}

	// Every name given by flag: the terminal still explains and asks.
	dir = filepath.Join(t.TempDir(), "out")
	errOut, err = execPreviews(t, true, nil, answers(""), append(base, "-o", dir, "--host-suffix",
		"previews.acme.dev", "--sign-in-host", "preview-auth.acme.dev", "--dex-issuer", "https://dex.acme.dev")...)
	var u *usageError
	if !errors.As(err, &u) || !strings.Contains(err.Error(), "give another --host-suffix") {
		t.Errorf("a blank acknowledgement: %v\n%s", err, errOut)
	}
	if exists(dir) {
		t.Error("wrote files without the acknowledgement")
	}
}
