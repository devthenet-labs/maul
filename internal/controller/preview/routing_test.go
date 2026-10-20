// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// ingressBackendFor is the Service a rendered Ingress sends path to, judged
// as an Ingress controller judges pathType Prefix independently of
// v1alpha1.PreviewPathMatches: element by element, a trailing slash ignored,
// the longest matching path winning. "" is no route.
func ingressBackendFor(ing *networkingv1.Ingress, path string) string {
	best, bestLen := "", -1
	for _, rule := range ing.Spec.Rules {
		for _, p := range rule.HTTP.Paths {
			pe := strings.Split(strings.TrimRight(p.Path, "/"), "/")
			re := strings.Split(strings.TrimRight(path, "/"), "/")
			if len(re) < len(pe) || !slices.Equal(re[:len(pe)], pe) || len(pe) <= bestLen {
				continue
			}
			best, bestLen = p.Backend.Service.Name, len(pe)
		}
	}
	return best
}

// routedPreview is a random multi-component Preview with distinct component
// paths drawn from overlapping segments, and a request path near them.
type routedPreview struct {
	Preview *v1alpha1.Preview
	Path    string
}

var routedSegments = []string{"a", "ab", "api", "v1"}

func (routedPreview) Generate(r *rand.Rand, _ int) reflect.Value {
	_, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	base := p.Spec.Components[0]
	p.Spec.Components = nil
	seen := map[string]bool{}
	for i := range 1 + r.Intn(v1alpha1.MaxPreviewComponents) {
		path := ""
		for range r.Intn(3) {
			path += "/" + routedSegments[r.Intn(len(routedSegments))]
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		c := base
		c.Name = []string{"web", "api", "docs", "admin"}[i]
		c.Path = path
		p.Spec.Components = append(p.Spec.Components, c)
	}
	var b strings.Builder
	for range r.Intn(4) {
		b.WriteString("/" + routedSegments[r.Intn(len(routedSegments))])
	}
	if b.Len() == 0 || r.Intn(4) == 0 {
		b.WriteString("/")
	}
	return reflect.ValueOf(routedPreview{Preview: p, Path: b.String()})
}

// TestRoutingAgreesWithRenderedIngress pins the shared routing helpers to
// the Ingress the controller renders: for every Preview and request path,
// v1alpha1.PreviewRoute over the components' paths picks the component whose
// Service (v1alpha1.PreviewComponentServiceName, the name the Service is
// rendered under) the rendered Ingress routes the path to, and no route
// exactly when the Ingress has none; and so does routing over the serving
// snapshot the gateway reads. Seeded, so the gate is deterministic.
func TestRoutingAgreesWithRenderedIngress(t *testing.T) {
	s := testSettings()
	cfg := &quick.Config{MaxCount: 3000, Rand: rand.New(rand.NewSource(20261016))}
	check := func(c routedPreview) bool {
		p := c.Preview
		prefixes := make([]string, len(p.Spec.Components))
		for i, comp := range p.Spec.Components {
			prefixes[i] = v1alpha1.PreviewComponentPath(comp)
			if got, want := s.service(p, i, 0).Name, v1alpha1.PreviewComponentServiceName(p, i); got != want {
				t.Logf("component %d's Service is %q, the shared name %q", i, got, want)
				return false
			}
		}
		want := ingressBackendFor(s.ingress(p, 0), c.Path)
		got := ""
		if i := v1alpha1.PreviewRoute(prefixes, c.Path); i >= 0 {
			got = v1alpha1.PreviewComponentServiceName(p, i)
		}
		// The gateway's input is the serving snapshot, not the spec.
		snapshot := servingSnapshot(p)
		served := make([]string, len(snapshot.Components))
		for i, comp := range snapshot.Components {
			served[i] = comp.Path
		}
		fromSnapshot := ""
		if i := v1alpha1.PreviewRoute(served, c.Path); i >= 0 {
			fromSnapshot = snapshot.Components[i].Service
		}
		if got != want || fromSnapshot != want {
			t.Logf("paths %q, request %q: PreviewRoute -> %q, over the snapshot -> %q, the rendered Ingress -> %q",
				prefixes, c.Path, got, fromSnapshot, want)
		}
		return got == want && fromSnapshot == want
	}
	if err := quick.Check(check, cfg); err != nil {
		t.Error(err)
	}
}
