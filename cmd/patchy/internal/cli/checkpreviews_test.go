// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/kubecfg"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewcheck"
)

// noWeb answers nothing.
type noWeb struct{}

func (noWeb) Do(*http.Request) (*http.Response, error) { return nil, errors.New("not asked in tests") }

// checkPreviewsDepsFake reaches nothing beyond the fake cluster.
var checkPreviewsDepsFake = checkPreviewsDeps{
	resolver: noDNS{},
	dialTLS: func(context.Context, string) (tls.ConnectionState, error) {
		return tls.ConnectionState{}, errors.New("not dialed in tests")
	},
	http: noWeb{},
}

// previewControllerSettings is a preview-controller ConfigMap with
// previews on, gateway-only, and node isolation off.
func previewControllerSettings() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "patchy-preview-controller", Namespace: testNamespace,
			Labels: map[string]string{previewcheck.LabelName: previewcheck.PreviewController,
				"app.kubernetes.io/part-of": "patchy"}},
		Data: map[string]string{"PATCHY_PREVIEW_HOST_SUFFIX": "acme-previews.test",
			"PATCHY_PREVIEW_LEGACY_INGRESS": "false", "PATCHY_PREVIEW_NODE_ISOLATION": "none"},
	}
}

// TestCheckPreviewsTable: with previews on but sign-in off, the table
// reports preview-config's failure, the node WARN, and exits one.
func TestCheckPreviewsTable(t *testing.T) {
	h := checkProjectHarness(t, interceptor.Funcs{}, previewControllerSettings())
	err := runCheckPreviews(context.Background(), h.opts, checkPreviewsDepsFake)
	if err == nil || err.Error() != "1 check failed" {
		t.Fatalf("check previews error = %v, want 1 check failed\n%s", err, h.out)
	}
	if code := exitCode(err); code != ExitError {
		t.Errorf("exit code = %d, want %d", code, ExitError)
	}
	out := h.out.String()
	for _, want := range []string{
		"FAIL  preview-config   preview sign-in is off",
		"SKIP  preview-viewers  preview sign-in is off",
		"WARN  preview-nodes    node isolation is off",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestCheckPreviewsStructured: -o json and -o yaml print the report as
// data, the WARN included.
func TestCheckPreviewsStructured(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			h := checkProjectHarness(t, interceptor.Funcs{}, previewControllerSettings())
			h.opts.Output = format
			if err := runCheckPreviews(context.Background(), h.opts, checkPreviewsDepsFake); err == nil {
				t.Fatal("check previews passed with sign-in off")
			}
			raw := h.out.Bytes()
			if format == "yaml" {
				var err error
				if raw, err = yaml.YAMLToJSON(raw); err != nil {
					t.Fatal(err)
				}
			}
			var report previewcheck.Report
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatalf("not a report: %v\n%s", err, h.out)
			}
			if report.Namespace != testNamespace || len(report.Checks) == 0 ||
				report.Checks[0].Name != previewcheck.CheckPreviewConfig {
				t.Errorf("report = %+v", report)
			}
			warned := false
			for _, c := range report.Checks {
				warned = warned || (c.Name == previewcheck.CheckPreviewNodes && c.Status == checkreport.Warn)
			}
			if !warned {
				t.Errorf("no preview-nodes WARN in %+v", report)
			}
		})
	}
}

// TestCheckPreviewsUsage: -A, a bad format and no namespace are usage
// errors, not reads of every namespace.
func TestCheckPreviewsUsage(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*harness)
	}{
		{"all namespaces", func(h *harness) { h.opts.AllNamespaces = true }},
		{"bad format", func(h *harness) { h.opts.Output = "csv" }},
		{"no namespace", func(h *harness) { h.opts.WithEnv(&kubecfg.Env{Client: h.client}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := checkProjectHarness(t, interceptor.Funcs{})
			tc.setup(h)
			err := runCheckPreviews(context.Background(), h.opts, checkPreviewsDepsFake)
			if code := exitCode(err); code != ExitUsage {
				t.Errorf("exit code = %d (%v), want %d", code, err, ExitUsage)
			}
		})
	}
}

// TestCheckPreviewsNeverReadsSecrets: the harness fails the test on any
// Secret read, whatever the cluster holds.
func TestCheckPreviewsNeverReadsSecrets(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "patchy-preview-auth-dex", Namespace: testNamespace}}
	h := checkProjectHarness(t, interceptor.Funcs{}, []client.Object{previewControllerSettings(), secret}...)
	_ = runCheckPreviews(context.Background(), h.opts, checkPreviewsDepsFake)
}

// TestCheckPreviewsHelp: check names its new noun, and the noun says what
// it reads.
func TestCheckPreviewsHelp(t *testing.T) {
	out, err := execDev(t, "check", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "previews reads the cluster too") {
		t.Errorf("check --help lacks previews:\n%s", out)
	}
	out, err = execDev(t, "check", "previews", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"never reads a Secret", "patchy-probe-0.<host suffix>", "preview-nodes",
		"preview-upstreams", "--isolation", "preview-netpol", "without --isolation it never writes"} {
		if !strings.Contains(out, want) {
			t.Errorf("check previews --help lacks %q:\n%s", want, out)
		}
	}
}

// TestCheckPreviewsIsolationFlags: the --isolation-* flags need
// --isolation, and its timeout must be positive; each is a usage error
// before any cluster is reached.
func TestCheckPreviewsIsolationFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--isolation-slot", "1"},
		{"--isolation-image", "registry.example/p/netprobe:sha-0123456789abcdef0123456789abcdef01234567"},
		{"--isolation-timeout", "1m"},
		{"--isolation", "--isolation-timeout", "0s"},
	} {
		_, err := execDev(t, append([]string{"check", "previews", "-n", "patchy"}, args...)...)
		if code := exitCode(err); code != ExitUsage {
			t.Errorf("%v: exit code = %d (%v), want %d", args, code, err, ExitUsage)
		}
	}
}

// TestCheckPreviewsIsolation: with --isolation the report ends with
// preview-netpol, which fails when the probe cannot run (no slot policy
// here) and then creates no Pod; no Secret is read either way.
func TestCheckPreviewsIsolation(t *testing.T) {
	created := 0
	h := checkProjectHarness(t, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			created++
			return c.Create(ctx, obj, opts...)
		},
	}, previewControllerSettings(),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "patchy-preview-auth-dex", Namespace: testNamespace}})
	deps := checkPreviewsDepsFake
	deps.isolation = &previewcheck.Isolation{Image: "registry.example/p/netprobe:sha-" +
		"0123456789abcdef0123456789abcdef01234567", Timeout: time.Second,
		Logs: func(context.Context, string, string) ([]byte, error) { return nil, errors.New("not read") }}
	h.opts.Output = "json"
	if err := runCheckPreviews(context.Background(), h.opts, deps); err == nil {
		t.Fatal("check previews passed")
	}
	var report previewcheck.Report
	if err := json.Unmarshal(h.out.Bytes(), &report); err != nil {
		t.Fatalf("not a report: %v\n%s", err, h.out)
	}
	last := report.Checks[len(report.Checks)-1]
	if last.Name != previewcheck.CheckPreviewNetpol || last.Status != checkreport.Fail ||
		!strings.Contains(last.Reason, "no slot NetworkPolicy") {
		t.Errorf("last line = %+v", last)
	}
	if created != 0 {
		t.Errorf("%d objects created when the probe could not run", created)
	}
}
