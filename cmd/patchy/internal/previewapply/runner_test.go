// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewapply

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
)

// fakeCluster is an in-memory API server: objects by kind, namespace and
// name, with failures injected per object.
type fakeCluster struct {
	objects    map[string]*unstructured.Unstructured
	namespaces map[string]bool
	secrets    map[string]bool // namespace/name of Secrets created out of band
	fail       map[string]error
	certReady  bool
	applied    []string
}

func newFakeCluster(namespaces ...string) *fakeCluster {
	c := &fakeCluster{objects: map[string]*unstructured.Unstructured{}, namespaces: map[string]bool{},
		secrets: map[string]bool{}, fail: map[string]error{}, certReady: true}
	for _, ns := range namespaces {
		c.namespaces[ns] = true
	}
	return c
}

func key(o *unstructured.Unstructured) string {
	return o.GetKind() + " " + o.GetNamespace() + "/" + o.GetName()
}

func (c *fakeCluster) Apply(_ context.Context, o *unstructured.Unstructured) error {
	if err := c.fail[key(o)]; err != nil {
		return err
	}
	if ns := o.GetNamespace(); ns != "" && !c.namespaces[ns] {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, ns)
	}
	if o.GetKind() == "Namespace" {
		c.namespaces[o.GetName()] = true
	}
	c.objects[key(o)] = o.DeepCopy()
	c.applied = append(c.applied, key(o))
	return nil
}

func (c *fakeCluster) Exists(_ context.Context, o *unstructured.Unstructured) (bool, error) {
	_, ok := c.objects[key(o)]
	return ok, nil
}

func (c *fakeCluster) NamespaceExists(_ context.Context, name string) (bool, error) {
	return c.namespaces[name], nil
}

func (c *fakeCluster) EnsureNamespace(_ context.Context, name string) (bool, error) {
	if c.namespaces[name] {
		return false, nil
	}
	c.namespaces[name] = true
	return true, nil
}

func (c *fakeCluster) SecretExists(_ context.Context, namespace, name string) (bool, error) {
	_, applied := c.objects["Secret "+namespace+"/"+name]
	return applied || c.secrets[namespace+"/"+name], nil
}

func (c *fakeCluster) WaitCertificateReady(ctx context.Context, namespace, name string) error {
	if _, ok := c.objects["Certificate "+namespace+"/"+name]; !ok {
		return errors.New("no such Certificate")
	}
	if !c.certReady {
		<-ctx.Done()
		return fmt.Errorf("certificate %s/%s is not Ready", namespace, name)
	}
	return nil
}

func (c *fakeCluster) WaitDeploymentAvailable(_ context.Context, namespace, name string) error {
	if _, ok := c.objects["Deployment "+namespace+"/"+name]; !ok {
		return errors.New("no such Deployment")
	}
	return nil
}

// fakeHelm keeps each release's revision and records every command run.
type fakeHelm struct {
	revisions map[string]int // namespace/release
	runs      [][]string
	fail      map[string]error // by release
	missing   bool
}

func newFakeHelm() *fakeHelm {
	return &fakeHelm{revisions: map[string]int{"patchy/patchy": 3}, fail: map[string]error{}}
}

func (h *fakeHelm) Version(context.Context) (string, error) {
	if h.missing {
		return "", errors.New("no helm command")
	}
	return "v3.19.0", nil
}

func (h *fakeHelm) Revision(_ context.Context, release, namespace string) (int, error) {
	return h.revisions[namespace+"/"+release], nil
}

func (h *fakeHelm) Run(_ context.Context, args []string) error {
	h.runs = append(h.runs, slices.Clone(args))
	release := firstRelease(args)
	if err := h.fail[release]; err != nil {
		return err
	}
	ns := args[slices.Index(args, "-n")+1]
	if h.revisions[ns+"/"+release] == 0 && !slices.Contains(args, "--install") {
		return errors.New("has no deployed releases")
	}
	h.revisions[ns+"/"+release]++
	return nil
}

// released are the releases helm ran, in order.
func (h *fakeHelm) released() []string {
	out := make([]string, 0, len(h.runs))
	for _, r := range h.runs {
		out = append(out, firstRelease(r))
	}
	return out
}

// oauthSecret is the OAuth client secret the tailscale tree carries.
const oauthSecret = "tskey-client-kTEST0CNTRL-not-a-real-secret-0123456789"

// writeTree writes a's tree under a temporary directory and plans --apply
// over it.
func writeTree(t *testing.T, a previewsetup.Answers, oauth bool) []previewsetup.Step {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	a.Normalize()
	s, err := previewsetup.ResolveSecrets(dir, false, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if oauth {
		if s.TailscaleOAuth, err = previewsetup.ParseTailscaleOAuth([]byte("kTEST0CNTRL"),
			[]byte(oauthSecret)); err != nil {
			t.Fatal(err)
		}
	}
	files, err := previewsetup.Plan(a, s, "v0.12.41")
	if err != nil {
		t.Fatal(err)
	}
	c, err := previewsetup.Compare(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := previewsetup.Write(c, previewsetup.Accept{}); err != nil {
		t.Fatal(err)
	}
	steps, err := previewsetup.ApplySteps(a, files, previewsetup.ApplyInput{Dir: dir,
		ValuesFiles: []string{"my-values.yaml"}, ChartVersion: "0.12.41", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

// tailscaleAnswers are the desktop's: k3d, the Tailscale operator and
// cert-manager installed by the recipe, a Route53 key.
func tailscaleAnswers() previewsetup.Answers {
	a := previewsetup.Defaults()
	a.Platform, a.Edge = previewsetup.PlatformK3d, previewsetup.EdgeTailscale
	a.HostSuffix, a.SignInHost, a.Dex.Issuer = "acme-previews.dev", "preview-auth.acme.dev", "https://dex.acme.dev"
	a.Viewers.Teams = []string{"acme:qa"}
	a.Registry = "ghcr.io"
	a.Certificate = previewsetup.Certificate{DNS: "route53", DNSOptions: map[string]string{"region": "us-east-1"}}
	return a
}

// runner runs over the fakes, its log and the checks' calls kept.
type runner struct {
	cluster *fakeCluster
	helm    *fakeHelm
	log     bytes.Buffer
	checks  int
	check   error
}

func (r *runner) run(t *testing.T, steps []previewsetup.Step) error {
	t.Helper()
	return Runner{Cluster: r.cluster, Helm: r.helm, Log: &r.log,
		Check: func(context.Context) error { r.checks++; return r.check }}.Run(t.Context(), steps)
}

// TestRunTailscale runs the desktop's plan end to end over the fakes: every
// manifest applied in order, the upstream charts before patchy, the checks
// last, and no secret in the log; a second run changes nothing it created.
func TestRunTailscale(t *testing.T) {
	steps := writeTree(t, tailscaleAnswers(), true)
	r := &runner{cluster: newFakeCluster("patchy", "dex"), helm: newFakeHelm()}
	if err := r.run(t, steps); err != nil {
		t.Fatalf("Run: %v\n%s", err, r.log.String())
	}
	if got, want := r.helm.released(), []string{"cert-manager", "tailscale-operator", "patchy"}; !slices.Equal(got,
		want) {
		t.Errorf("helm ran %v, want %v", got, want)
	}
	want := []string{"Secret patchy/patchy-preview-auth-dex", "Secret dex/patchy-preview-auth-client",
		"Secret tailscale/operator-oauth", "ClusterIssuer /preview-dns01-patchy", "Certificate patchy/preview-wildcard"}
	if !slices.Equal(r.cluster.applied, want) {
		t.Errorf("applied %v, want %v", r.cluster.applied, want)
	}
	if r.checks != 1 {
		t.Errorf("checks ran %d times", r.checks)
	}
	patchy := r.helm.runs[2]
	if !slices.Contains(patchy, "my-values.yaml") || slices.Contains(patchy, "--install") {
		t.Errorf("patchy's upgrade %q", patchy)
	}
	if strings.Contains(r.log.String(), oauthSecret) {
		t.Error("the log carries the OAuth client secret")
	}
	// Again: every object updated in place, every release upgraded.
	r.log.Reset()
	if err := r.run(t, steps); err != nil {
		t.Fatalf("second Run: %v\n%s", err, r.log.String())
	}
	if strings.Contains(r.log.String(), "(created)") {
		t.Errorf("a second run created something:\n%s", r.log.String())
	}
	if r.helm.revisions["tailscale/tailscale-operator"] != 2 || r.checks != 2 {
		t.Errorf("revisions %v, checks %d", r.helm.revisions, r.checks)
	}
}

// TestRunStopsAtFirstFailure: a failed operator install stops the run
// before patchy's upgrade and the checks, and prints how to undo what the
// run created, newest first, the failed release's uninstall first.
func TestRunStopsAtFirstFailure(t *testing.T) {
	steps := writeTree(t, tailscaleAnswers(), true)
	r := &runner{cluster: newFakeCluster("patchy", "dex"), helm: newFakeHelm()}
	r.helm.fail["tailscale-operator"] = errors.New("timed out waiting for the condition")
	err := r.run(t, steps)
	if err == nil || !strings.Contains(err.Error(), "tailscale-operator") {
		t.Fatalf("Run = %v", err)
	}
	if slices.Contains(r.helm.released(), "patchy") || r.checks != 0 {
		t.Errorf("ran past the failure: helm %v, checks %d", r.helm.released(), r.checks)
	}
	log := r.log.String()
	order := []string{
		"helm uninstall tailscale-operator -n tailscale",
		"kubectl -n patchy delete certificate.cert-manager.io/preview-wildcard",
		"kubectl delete clusterissuer.cert-manager.io/preview-dns01-patchy",
		"helm uninstall cert-manager -n cert-manager",
		"kubectl -n tailscale delete secret/operator-oauth",
		"kubectl delete namespace tailscale",
		"kubectl -n patchy delete secret/patchy-preview-auth-dex",
		"Rolling back",
	}
	at := -1
	for _, line := range order {
		i := strings.Index(log, line)
		if i <= at {
			t.Errorf("%q not after the line before it in:\n%s", line, log)
		}
		at = i
	}
}

// TestRunUndoUpgrade: an upgrade that fails is undone by rolling back to
// the revision it replaced, and an object updated in place is not
// offered for deletion.
func TestRunUndoUpgrade(t *testing.T) {
	steps := writeTree(t, tailscaleAnswers(), true)
	r := &runner{cluster: newFakeCluster("patchy", "dex"), helm: newFakeHelm()}
	if err := r.run(t, steps); err != nil {
		t.Fatal(err)
	}
	r.log.Reset()
	r.helm.fail["patchy"] = errors.New("UPGRADE FAILED")
	if err := r.run(t, steps); err == nil {
		t.Fatal("Run succeeded")
	}
	log := r.log.String()
	if !strings.Contains(log, "helm rollback patchy 4 -n patchy") || !strings.Contains(log,
		"helm rollback tailscale-operator 1 -n tailscale") {
		t.Errorf("no rollback to the replaced revisions:\n%s", log)
	}
	if strings.Contains(log, "delete secret") || strings.Contains(log, "delete namespace") {
		t.Errorf("offers to delete what was there before:\n%s", log)
	}
}

// TestRunPreflight: nothing changes when helm cannot run, the patchy
// release is not installed, or the operator's Secret is neither written
// nor there.
func TestRunPreflight(t *testing.T) {
	steps := writeTree(t, tailscaleAnswers(), true)
	noOAuth := tailscaleAnswers()
	noOAuth.Tailscale.Hostname = "previews"
	for name, tc := range map[string]struct {
		mutate func(*runner)
		steps  []previewsetup.Step
		want   string
	}{
		"no helm":     {func(r *runner) { r.helm.missing = true }, steps, "runs helm"},
		"no release":  {func(r *runner) { delete(r.helm.revisions, "patchy/patchy") }, steps, "install patchy first"},
		"no Secret":   {func(*runner) {}, oauthLess(t), "--tailscale-oauth-dir"},
		"has Secret":  {func(r *runner) { r.cluster.secrets["tailscale/operator-oauth"] = true }, oauthLess(t), ""},
		"Dex outside": {func(r *runner) { delete(r.cluster.namespaces, "dex") }, steps, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := &runner{cluster: newFakeCluster("patchy", "dex"), helm: newFakeHelm()}
			tc.mutate(r)
			err := r.run(t, tc.steps)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Run: %v\n%s", err, r.log.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, want %q", err, tc.want)
			}
			if len(r.cluster.applied) > 0 || len(r.helm.runs) > 0 {
				t.Errorf("changed the cluster first: %v %v", r.cluster.applied, r.helm.runs)
			}
			if !strings.Contains(r.log.String(), "created nothing to delete") {
				t.Errorf("log:\n%s", r.log.String())
			}
		})
	}
	r := &runner{cluster: newFakeCluster("patchy"), helm: newFakeHelm()}
	if err := r.run(t, steps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.log.String(), "skipped: no namespace dex here") {
		t.Errorf("Dex's copy not skipped:\n%s", r.log.String())
	}
}

// oauthLess is the tailscale plan with no OAuth client given: the
// operator's Secret is the operator's to create.
func oauthLess(t *testing.T) []previewsetup.Step {
	t.Helper()
	return writeTree(t, tailscaleAnswers(), false)
}

// TestRunSecretErrorRedacted: the API server's message about a Secret is
// never shown, since it could quote the value sent.
func TestRunSecretErrorRedacted(t *testing.T) {
	steps := writeTree(t, tailscaleAnswers(), true)
	r := &runner{cluster: newFakeCluster("patchy", "dex"), helm: newFakeHelm()}
	r.cluster.fail["Secret tailscale/operator-oauth"] = apierrors.NewBadRequest("bad value " + oauthSecret)
	err := r.run(t, steps)
	if err == nil {
		t.Fatal("Run succeeded")
	}
	if strings.Contains(err.Error(), oauthSecret) || strings.Contains(r.log.String(), oauthSecret) {
		t.Errorf("the secret was shown: %v\n%s", err, r.log.String())
	}
	if !strings.Contains(err.Error(), "BadRequest") {
		t.Errorf("no reason in %v", err)
	}
}

// TestRunCheckFailure: a failed check leaves everything applied and
// offers no undo.
func TestRunCheckFailure(t *testing.T) {
	steps := writeTree(t, tailscaleAnswers(), true)
	r := &runner{cluster: newFakeCluster("patchy", "dex"), helm: newFakeHelm(), check: errors.New("2 checks failed")}
	err := r.run(t, steps)
	if !errors.Is(err, ErrCheckFailed) {
		t.Fatalf("Run = %v", err)
	}
	if strings.Contains(r.log.String(), "helm uninstall") {
		t.Errorf("offers an undo for a failed check:\n%s", r.log.String())
	}
}

// TestRunCertificateTimeout: a Certificate that never becomes Ready stops
// the run at its wait.
func TestRunCertificateTimeout(t *testing.T) {
	steps := writeTree(t, tailscaleAnswers(), true)
	r := &runner{cluster: newFakeCluster("patchy", "dex"), helm: newFakeHelm()}
	r.cluster.certReady = false
	err := r.run(t, steps)
	if err == nil || !strings.Contains(err.Error(), "not Ready") {
		t.Fatalf("Run = %v", err)
	}
	if slices.Contains(r.helm.released(), "tailscale-operator") {
		t.Error("ran past the wait")
	}
}
