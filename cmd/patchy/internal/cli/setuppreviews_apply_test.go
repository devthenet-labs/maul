// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/kubecfg"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewapply"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
)

// applyCluster is a Cluster that keeps what was applied; existing are the
// objects already there, by kind namespace/name.
type applyCluster struct {
	applied  []string
	existing map[string]bool
}

func applyKey(o *unstructured.Unstructured) string {
	return o.GetKind() + " " + o.GetNamespace() + "/" + o.GetName()
}

func (c *applyCluster) Apply(_ context.Context, o *unstructured.Unstructured) error {
	c.applied = append(c.applied, applyKey(o))
	return nil
}
func (c *applyCluster) Exists(_ context.Context, o *unstructured.Unstructured) (bool, error) {
	return c.existing[applyKey(o)], nil
}
func (*applyCluster) NamespaceExists(context.Context, string) (bool, error)         { return true, nil }
func (*applyCluster) EnsureNamespace(context.Context, string) (bool, error)         { return false, nil }
func (*applyCluster) SecretExists(context.Context, string, string) (bool, error)    { return true, nil }
func (*applyCluster) WaitCertificateReady(context.Context, string, string) error    { return nil }
func (*applyCluster) WaitDeploymentAvailable(context.Context, string, string) error { return nil }

// applyHelm is a Helm that keeps the commands run, failing one release.
type applyHelm struct {
	runs [][]string
	fail string
}

func (*applyHelm) Version(context.Context) (string, error)               { return "v3.19.0", nil }
func (*applyHelm) Revision(context.Context, string, string) (int, error) { return 1, nil }
func (h *applyHelm) Run(_ context.Context, args []string) error {
	h.runs = append(h.runs, args)
	if slices.Contains(args, h.fail) {
		return errors.New("UPGRADE FAILED")
	}
	return nil
}

// applyRun is one `setup previews --apply` over fakes.
type applyRun struct {
	cluster  applyCluster
	helm     applyHelm
	checks   int
	contacts int
}

func (r *applyRun) exec(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	opts := &Options{Out: out, ErrOut: errOut, Output: "table", NoColor: true,
		promptTerminal: func(io.Reader) bool { return false },
		previewApply: func(*kubecfg.Env, string) previewApplyDeps {
			r.contacts++
			return previewApplyDeps{cluster: &r.cluster, helm: &r.helm,
				check: func(context.Context) error { r.checks++; return nil }}
		}}
	opts.WithEnv(&kubecfg.Env{})
	root := NewRoot(opts)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetArgs(args)
	err := root.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// valuesFile writes the operator's own values file.
func valuesFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "patchy.yaml")
	if err := os.WriteFile(p, []byte("intentController:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSetupPreviewsApplyFlags: the --apply flags are refused without it,
// and --apply without the values and chart version it upgrades with,
// before anything is written or contacted.
func TestSetupPreviewsApplyFlags(t *testing.T) {
	values := valuesFile(t)
	for name, tc := range map[string]struct {
		extra []string
		want  string
	}{
		"values alone":     {[]string{"--values", values}, "--values needs --apply"},
		"version alone":    {[]string{"--chart-version", "0.12.42"}, "--chart-version needs --apply"},
		"no values":        {[]string{"--apply", "--chart-version", "0.12.42"}, "--apply needs --values"},
		"no version":       {[]string{"--apply", "--values", values}, "--apply needs --chart-version"},
		"bad version":      {[]string{"--apply", "--values", values, "--chart-version", "latest"}, "not one"},
		"missing values":   {[]string{"--apply", "--values", values + ".absent", "--chart-version", "1.0.0"}, "--values"},
		"directory values": {[]string{"--apply", "--values", t.TempDir(), "--chart-version", "1.0.0"}, "regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "patchy-previews")
			r := &applyRun{}
			_, _, err := r.exec(t, previewsArgs(dir, tc.extra...)...)
			if err == nil || exitCode(err) != 2 || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (exit %d), want usage %q", err, exitCode(err), tc.want)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Error("files were written")
			}
			if r.contacts != 0 {
				t.Error("the cluster was contacted")
			}
		})
	}
}

// TestSetupPreviewsApplyDryRun prints the steps as commands and contacts
// nothing.
func TestSetupPreviewsApplyDryRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	values := valuesFile(t)
	r := &applyRun{}
	out, errOut, err := r.exec(t, previewsArgs(dir, "--apply", "--values", values, "--chart-version", "v0.12.42",
		"--dry-run")...)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, errOut)
	}
	for _, want := range []string{"--apply would run these 7 steps", "helm status patchy -n patchy",
		"kubectl apply --server-side", "helm upgrade patchy " + previewsetup.ChartRef + " --version 0.12.42 -n patchy -f " +
			values + " -f " + filepath.Join(dir, previewsetup.ValuesFile),
		"kubectl -n patchy wait certificate/preview-wildcard", "patchy check previews -n patchy",
		"the two DNS records"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run does not print %q:\n%s", want, out)
		}
	}
	if r.contacts != 0 {
		t.Error("a dry run contacted the cluster")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dry run wrote files")
	}
}

// TestSetupPreviewsApply writes the files and runs the steps over the
// fakes, the secret never printed; a failed upgrade stops before the
// checks with the files still written.
func TestSetupPreviewsApply(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	values := valuesFile(t)
	args := previewsArgs(dir, "--apply", "--values", values, "--chart-version", "0.12.42")
	r := &applyRun{}
	out, errOut, err := r.exec(t, args...)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, errOut)
	}
	want := []string{"Secret patchy/patchy-preview-auth-dex", "Secret dex/patchy-preview-auth-client",
		"Certificate patchy/preview-wildcard"}
	if !slices.Equal(r.cluster.applied, want) {
		t.Errorf("applied %v, want %v", r.cluster.applied, want)
	}
	if len(r.helm.runs) != 1 || r.helm.runs[0][1] != "patchy" || r.checks != 1 {
		t.Errorf("helm %v, checks %d", r.helm.runs, r.checks)
	}
	for _, line := range []string{"patchy: apply 7/7: done", "left to you: the two DNS records", "no check failed"} {
		if !strings.Contains(errOut, line) {
			t.Errorf("stderr lacks %q:\n%s", line, errOut)
		}
	}
	if secret := clientSecret(t, dir); strings.Contains(out+errOut, secret) {
		t.Error("the client secret was printed")
	}

	failing := &applyRun{helm: applyHelm{fail: "patchy"}}
	_, errOut, err = failing.exec(t, args...)
	if err == nil || !strings.Contains(err.Error(), "apply stopped at step 6/7") {
		t.Fatalf("failing apply: %v", err)
	}
	if failing.checks != 0 || !strings.Contains(errOut, "helm rollback patchy 1 -n patchy") {
		t.Errorf("checks %d, stderr:\n%s", failing.checks, errOut)
	}
}

// TestSetupPreviewsApplyConnection: --context and --kubeconfig ride into
// every command the dry-run plan prints, so a pasted command reaches the
// cluster --apply would.
func TestSetupPreviewsApplyConnection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	values := valuesFile(t)
	r := &applyRun{}
	out, errOut, err := r.exec(t, previewsArgs(dir, "--apply", "--values", values, "--chart-version", "0.12.42",
		"--dry-run", "--context", "staging", "--kubeconfig", "/tmp/kube.yaml")...)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, errOut)
	}
	for _, want := range []string{"against context staging of kubeconfig /tmp/kube.yaml",
		"helm --kubeconfig /tmp/kube.yaml --kube-context staging status patchy -n patchy",
		"kubectl --kubeconfig /tmp/kube.yaml --context staging apply --server-side",
		"helm --kubeconfig /tmp/kube.yaml --kube-context staging upgrade patchy",
		"patchy check previews -n patchy --kubeconfig /tmp/kube.yaml --context staging"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run does not print %q:\n%s", want, out)
		}
	}
}

// TestSetupPreviewsApplyFreshSecret: a run whose output directory holds no
// secret file draws a new Dex client secret, and --apply then refuses to
// replace the relay's Secret already in the cluster, changing nothing;
// --rotate dex-client-secret replaces it on purpose.
func TestSetupPreviewsApplyFreshSecret(t *testing.T) {
	values := valuesFile(t)
	live := map[string]bool{"Secret patchy/patchy-preview-auth-dex": true}
	dir := filepath.Join(t.TempDir(), "patchy-previews")
	r := &applyRun{cluster: applyCluster{existing: live}}
	_, errOut, err := r.exec(t, previewsArgs(dir, "--apply", "--values", values, "--chart-version", "0.12.42")...)
	if !errors.Is(err, previewapply.ErrWouldReplace) || !strings.Contains(err.Error(), "--rotate dex-client-secret") {
		t.Fatalf("apply over a live secret: %v\n%s", err, errOut)
	}
	if len(r.cluster.applied) > 0 || len(r.helm.runs) > 0 || r.checks > 0 {
		t.Errorf("changed the cluster: %v %v", r.cluster.applied, r.helm.runs)
	}
	if strings.Contains(errOut, clientSecret(t, dir)) {
		t.Error("the client secret was printed")
	}

	rotated := &applyRun{cluster: applyCluster{existing: live}}
	_, errOut, err = rotated.exec(t, previewsArgs(filepath.Join(t.TempDir(), "patchy-previews"), "--apply",
		"--values", values, "--chart-version", "0.12.42", "--rotate", "dex-client-secret")...)
	if err != nil || !slices.Contains(rotated.cluster.applied, "Secret patchy/patchy-preview-auth-dex") {
		t.Fatalf("--rotate: %v, applied %v\n%s", err, rotated.cluster.applied, errOut)
	}
	// Read back from the directory that wrote it, the secret is the live
	// one and is applied as before.
	again := &applyRun{cluster: applyCluster{existing: live}}
	if _, errOut, err = again.exec(t, previewsArgs(dir, "--apply", "--values", values, "--chart-version",
		"0.12.42")...); err != nil {
		t.Fatalf("re-run: %v\n%s", err, errOut)
	}
}

// TestSetupPreviewsApplyCheckBounded drives the real check step (no
// previewApply fake): it is bounded by check previews' own timeout, as
// every other step is by --wait-timeout.
func TestSetupPreviewsApplyCheckBounded(t *testing.T) {
	var reads int
	h := checkProjectHarness(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			reads++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > checkPreviewsTimeout {
				t.Errorf("the checks read the cluster with no bound (deadline %v, %v)", deadline, ok)
			}
			return c.List(ctx, list, opts...)
		},
	}, previewControllerSettings())
	h.opts.tailnetStatus = noTailnet
	h.opts.previewChecksNet = func() checkPreviewsDeps {
		deps := checkPreviewsDepsFake
		deps.resolver = boundedDNS{t: t, reads: &reads}
		return deps
	}
	env, err := h.opts.Connect()
	if err != nil {
		t.Fatal(err)
	}
	err = h.opts.applyDeps(env, testNamespace).check(context.Background())
	if err == nil || !strings.HasSuffix(err.Error(), "check failed") {
		t.Fatalf("check = %v\n%s", err, h.out)
	}
	if reads < 2 {
		t.Errorf("the checks read the cluster and DNS %d times", reads)
	}
}

// boundedDNS resolves nothing, failing t when asked with no bound.
type boundedDNS struct {
	t     *testing.T
	reads *int
}

func (d boundedDNS) LookupHost(ctx context.Context, _ string) ([]string, error) {
	*d.reads++
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > checkPreviewsTimeout {
		d.t.Errorf("DNS asked with no bound (deadline %v, %v)", deadline, ok)
	}
	return nil, errors.New("no such host")
}

var _ previewapply.Cluster = (*applyCluster)(nil)
