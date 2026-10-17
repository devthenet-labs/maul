// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// configMap is a patchy ConfigMap labelled for controller.
func configMap(name, controller string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy",
		Labels: map[string]string{LabelName: controller, labelPartOf: partOf}}, Data: data}
}

// TestNewDefaults: nil seams become the CLI's own.
func TestNewDefaults(t *testing.T) {
	c := New(Config{})
	if c.cfg.Resolver != net.DefaultResolver || c.cfg.DialTLS == nil || c.cfg.HTTP == nil {
		t.Errorf("defaults not applied: %+v", c.cfg)
	}
	web := doerFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })
	if c := New(Config{HTTP: web}); c.cfg.HTTP == nil {
		t.Error("a configured doer was dropped")
	}
}

// TestLoadSettings: each controller reads its own ConfigMap over the shared
// one, the relay never reads the shared one, a ConfigMap without PATCHY_
// settings is not a controller's, and every listed ConfigMap is kept,
// sorted by name. A failed list is every controller's error.
func TestLoadSettings(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(
		configMap("patchy", sharedName, map[string]string{"PATCHY_SHARED": "1", "PATCHY_PREVIEW_HOST_SUFFIX": "a"}),
		configMap("z-preview", PreviewController, map[string]string{"PATCHY_PREVIEW_HOST_SUFFIX": "b"}),
		configMap("b-auth", PreviewAuth, map[string]string{"PATCHY_PREVIEW_AUTH_ISSUER": "https://relay"}),
		configMap("a-source-key", SourceController, map[string]string{"cosign.pub": "pem"}),
	).Build()
	s := LoadSettings(context.Background(), reader, "patchy")
	if got := s.Preview.Data["PATCHY_PREVIEW_HOST_SUFFIX"]; got != "b" {
		t.Errorf("preview suffix = %q, want its own over the shared one", got)
	}
	if !s.Preview.Bool("PATCHY_SHARED") || s.Preview.From != "patchy/patchy and patchy/z-preview" ||
		s.Preview.Name != "z-preview" {
		t.Errorf("preview = %+v", s.Preview)
	}
	if s.Auth.Data["PATCHY_SHARED"] != "" || s.Auth.From != "patchy/b-auth" {
		t.Errorf("auth read the shared ConfigMap: %+v", s.Auth)
	}
	if s.Source.Name != "" || s.Source.From != "patchy/patchy" {
		t.Errorf("source picked a ConfigMap without settings: %+v", s.Source)
	}
	names := make([]string, 0, len(s.ConfigMaps))
	for _, cm := range s.ConfigMaps {
		names = append(names, cm.Name)
	}
	if got := strings.Join(names, ","); got != "a-source-key,b-auth,patchy,z-preview" {
		t.Errorf("ConfigMaps = %s", got)
	}

	failing := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	s = LoadSettings(context.Background(), failing, "patchy")
	for _, c := range []ControllerConfig{s.Source, s.Intent, s.Preview, s.Auth} {
		if c.Err == nil || c.Found() || !strings.Contains(c.Err.Error(), "forbidden") {
			t.Errorf("failed list = %+v", c)
		}
	}
	if s.ConfigMaps != nil {
		t.Errorf("ConfigMaps after a failed list = %v", s.ConfigMaps)
	}
}

func TestControllerConfigReads(t *testing.T) {
	c := ControllerConfig{From: "x", Data: map[string]string{"B": " true ", "L": " a, ,b ,", "N": "nope"}}
	if !c.Found() || !c.Bool("B") || c.Bool("N") || c.Bool("missing") {
		t.Errorf("reads of %+v", c)
	}
	if got := strings.Join(c.List("L"), "|"); got != "a|b" {
		t.Errorf("List = %q", got)
	}
	if (ControllerConfig{}).Found() {
		t.Error("an empty config is found")
	}
}

// TestCheckerOff: without the relay and with nothing required, Auth reports
// nothing and asks nothing; Host without a suffix skips both host checks.
func TestCheckerOff(t *testing.T) {
	asked := false
	web := doerFunc(func(*http.Request) (*http.Response, error) {
		asked = true
		return nil, errors.New("unexpected request")
	})
	c := New(Config{Namespace: "patchy", Project: "demo", HTTP: web})
	c.Auth(context.Background(), Settings{}, "preview.example.com")
	if len(c.Lines()) != 0 || asked {
		t.Fatalf("sign-in off reported %s (asked %v)", dump(c.Lines()), asked)
	}
	c.Host(context.Background(), Settings{}, "")
	expect(t, c.Lines(), CheckPreviewDNS, checkreport.Skip, "host suffix is unknown")
	expect(t, c.Lines(), CheckPreviewTLS, checkreport.Skip, "host suffix is unknown")
}

// TestReasonsArePrintable: text from outside the CLI is escaped before it
// becomes a reason.
func TestReasonsArePrintable(t *testing.T) {
	c := &Checker{}
	c.add(CheckPreviewAuth, checkreport.Fail, "%s: %v", "relay", errors.New("boom\x1b[31m"))
	if r := c.Lines()[0].Reason; strings.ContainsRune(r, '\x1b') || !strings.Contains(r, `\x1b`) {
		t.Errorf("reason not printable: %q", r)
	}
}
