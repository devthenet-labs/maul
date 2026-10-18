// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
)

// deployment runs image in namespace/name, with labels on the Deployment.
func deployment(namespace, name, image string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "sidecar", Image: "busybox:1.36"}, {Name: "main", Image: image}},
		}}},
	}
}

// TestInstallUpstreams: each pinned upstream the cluster runs is compared
// with its pin by its image's tag, else its version label; an older one
// warns, and none running reports nothing.
func TestInstallUpstreams(t *testing.T) {
	tests := map[string]struct {
		objs   func(t *testing.T) []client.Object
		status checkreport.Status
		want   []string
	}{
		"the recipe's own cloudflared": {
			objs: func(t *testing.T) []client.Object {
				d := goldenObjects[appsv1.Deployment](t, "manifests/cloudflared.yaml", "Deployment")[0]
				return []client.Object{&d}
			},
			status: checkreport.Pass,
			want: []string{"cloudflared " + previewsetup.Cloudflared.Tag + " (patchy-cloudflared/cloudflared) at or " +
				"past the version"},
		},
		"an older cert-manager": {
			objs: func(*testing.T) []client.Object {
				return []client.Object{
					deployment("cert-manager", "cert-manager", "quay.io/jetstack/cert-manager-controller:v1.0.0", nil),
					deployment("cloudflared", "tunnel", "cloudflare/cloudflared:9999.1.0", nil),
				}
			},
			status: checkreport.Warn,
			want: []string{"cert-manager v1.0.0 (cert-manager/cert-manager) is older than " +
				previewsetup.CertManager.Version, "upgrade it", "cloudflared 9999.1.0 (cloudflared/tunnel) at or past"},
		},
		"a mirrored Tailscale operator by digest, versioned by its label": {
			objs: func(*testing.T) []client.Object {
				return []client.Object{deployment("tailscale", "operator",
					"registry.acme.test:5000/mirror/tailscale/k8s-operator@sha256:0123",
					map[string]string{labelVersion: "v" + previewsetup.TailscaleOperator.Version})}
			},
			status: checkreport.Pass,
			want:   []string{"the Tailscale operator v" + previewsetup.TailscaleOperator.Version + " (tailscale/operator)"},
		},
		"an older Tailscale operator by tag": {
			objs: func(*testing.T) []client.Object {
				return []client.Object{deployment("tailscale", "operator", "tailscale/k8s-operator:v1.2.3", nil)}
			},
			status: checkreport.Warn,
			want:   []string{"the Tailscale operator v1.2.3 (tailscale/operator) is older than"},
		},
		"no readable version": {
			objs: func(*testing.T) []client.Object {
				return []client.Object{deployment("cf", "cloudflared", "docker.io/cloudflare/cloudflared:latest", nil)}
			},
			status: checkreport.Skip,
			want:   []string{"cloudflared (cf/cloudflared) runs an image whose version", "cannot be compared"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := newInstallWorld()
			w.objs = append(w.objs, tc.objs(t)...)
			r := w.run(t, interceptor.Funcs{})
			expect(t, r.Checks, CheckPreviewUpstreams, tc.status, tc.want...)
		})
	}
}

// TestInstallUpstreamsNoneOrUnreadable: no pinned upstream running is no
// line at all; Deployments that may not be listed are a SKIP.
func TestInstallUpstreamsNoneOrUnreadable(t *testing.T) {
	w := newInstallWorld()
	w.objs = append(w.objs, deployment("web", "shop", "ghcr.io/acme/shop:1.0.0", nil))
	for _, l := range w.run(t, interceptor.Funcs{}).Checks {
		if l.Name == CheckPreviewUpstreams {
			t.Errorf("a line with no upstream running: %s %s", l.Status, l.Reason)
		}
	}
	forbidden := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := l.(*appsv1.DeploymentList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "",
				errors.New("RBAC"))
		}
		return c.List(ctx, l, opts...)
	}}
	expect(t, w.run(t, forbidden).Checks, CheckPreviewUpstreams, checkreport.Skip, "cannot list the Deployments")
}

// TestParseImage: registries, ports, mirrors' prefixes, digests and Docker
// Hub's library/ prefix.
func TestParseImage(t *testing.T) {
	for ref, want := range map[string][2]string{
		"cloudflare/cloudflared:2026.10.1":                               {"cloudflare/cloudflared", "2026.10.1"},
		"docker.io/cloudflare/cloudflared:2026.10.1@sha256:abc":          {"cloudflare/cloudflared", "2026.10.1"},
		"quay.io/jetstack/cert-manager-controller@sha256:abc":            {"jetstack/cert-manager-controller", ""},
		"localhost/tailscale/k8s-operator:v1":                            {"tailscale/k8s-operator", "v1"},
		"registry.acme.test:5000/mirror/tailscale/k8s-operator:v1.104.1": {"mirror/tailscale/k8s-operator", "v1.104.1"},
		"busybox":                        {"busybox", ""},
		"docker.io/library/busybox:1.36": {"busybox", "1.36"},
	} {
		if path, tag := parseImage(ref); path != want[0] || tag != want[1] {
			t.Errorf("parseImage(%q) = %q, %q, want %q, %q", ref, path, tag, want[0], want[1])
		}
	}
}

// TestParseImageRecovers (seeded): whatever registry, path, tag and digest
// a reference is built from, parseImage gives back the path and the tag.
func TestParseImageRecovers(t *testing.T) {
	registries := []string{"", "docker.io/", "quay.io/", "registry.acme.test:5000/", "localhost/", "127.0.0.1:5000/"}
	segs := []string{"jetstack", "cert-manager-controller", "mirror", "a.b", "x_y", "tailscale", "k8s-operator"}
	tags := []string{"", "v1.21.2", "2026.10.1", "latest", "v1.2.3-rc.1"}
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		var path []string
		for range 1 + r.Intn(3) {
			path = append(path, segs[r.Intn(len(segs))])
		}
		args[0] = reflect.ValueOf(registries[r.Intn(len(registries))])
		args[1] = reflect.ValueOf(strings.Join(path, "/"))
		args[2] = reflect.ValueOf(tags[r.Intn(len(tags))])
		args[3] = reflect.ValueOf(r.Intn(2) == 0)
	}
	prop := func(registry, path, tag string, digest bool) bool {
		ref := registry + path
		if tag != "" {
			ref += ":" + tag
		}
		if digest {
			ref += "@sha256:0123456789abcdef"
		}
		gotPath, gotTag := parseImage(ref)
		// A first segment with a dot names a registry when no registry
		// precedes it, as the container runtimes read it too.
		wantPath := path
		if first, rest, ok := strings.Cut(path, "/"); registry == "" && ok && strings.Contains(first, ".") {
			wantPath = rest
		}
		if gotPath != wantPath || gotTag != tag {
			t.Logf("parseImage(%q) = %q, %q, want %q, %q", ref, gotPath, gotTag, wantPath, tag)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestJudgeUpstreamsWarnsOnlyWhenOlder (seeded): the line is a WARN
// exactly when some upstream's version is older than its pin, a PASS when
// none is and one is readable, and never a FAIL.
func TestJudgeUpstreamsWarnsOnlyWhenOlder(t *testing.T) {
	versions := []string{"", "v0.1.0", "v1.0.0", "1.104.1", "v1.104.1", "v1.21.2", "2026.10.1", "2020.1.0",
		"v9999.0.0", "v1.21.2-rc.1"}
	cfg := quickConfig(3000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		var found []installed
		for range 1 + r.Intn(4) {
			up := upstreams()[r.Intn(len(upstreams()))]
			found = append(found, installed{up: up, where: "ns/d", version: versions[r.Intn(len(versions))]})
		}
		args[0] = reflect.ValueOf(found)
	}
	prop := func(found []installed) bool {
		older, readable := false, false
		for _, f := range found {
			if f.version != "" {
				readable = true
				older = older || versionLess(f.version, f.up.pin)
			}
		}
		v := judgeUpstreams(found)
		want := checkreport.Skip
		switch {
		case older:
			want = checkreport.Warn
		case readable:
			want = checkreport.Pass
		}
		if v.status != want || v.reason == "" {
			t.Logf("%+v: %s %q, want %s", found, v.status, v.reason, want)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// versionLess compares two versions numerically, independently of semver:
// major, minor and patch, a pre-release below its release.
func versionLess(a, b string) bool {
	split := func(v string) ([3]int, bool) {
		v = strings.TrimPrefix(v, "v")
		core, pre, _ := strings.Cut(v, "-")
		var out [3]int
		for i, p := range strings.SplitN(core, ".", 3) {
			n := 0
			for _, ch := range p {
				n = n*10 + int(ch-'0')
			}
			out[i] = n
		}
		return out, pre != ""
	}
	x, xpre := split(a)
	y, ypre := split(b)
	if x != y {
		for i := range 3 {
			if x[i] != y[i] {
				return x[i] < y[i]
			}
		}
	}
	return xpre && !ypre
}
