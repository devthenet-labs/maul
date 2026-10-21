// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// TestPreviewPathMatchesIngressExamples holds PreviewPathMatches to the
// pathType Prefix examples of the Kubernetes Ingress documentation.
func TestPreviewPathMatchesIngressExamples(t *testing.T) {
	for _, tc := range []struct {
		prefix, path string
		want         bool
	}{
		{"/", "/", true},
		{"/", "/anything/at/all", true},
		{"/foo", "/foo", true},
		{"/foo", "/foo/", true},
		{"/foo/", "/foo", true},
		{"/foo/", "/foo/", true},
		{"/aaa/bb", "/aaa/bbb", false},
		{"/aaa/bbb", "/aaa/bbb", true},
		{"/aaa/bbb/", "/aaa/bbb", true},
		{"/aaa/bbb", "/aaa/bbb/", true},
		{"/aaa/bbb", "/aaa/bbb/ccc", true},
		{"/aaa/bbb", "/aaa/bbbxyz", false},
		{"/aaa", "/ccc", false},
		{"/api", "", false},
		{"/", "", true},
		{"/api", "api", false},
		{"/", "api", false},
		{"", "/", false},
	} {
		if got := PreviewPathMatches(tc.prefix, tc.path); got != tc.want {
			t.Errorf("PreviewPathMatches(%q, %q) = %v, want %v", tc.prefix, tc.path, got, tc.want)
		}
	}
}

// TestPreviewRouteIngressExamples: the longest matching prefix wins,
// whatever the order the prefixes are listed in, as an Ingress chooses.
func TestPreviewRouteIngressExamples(t *testing.T) {
	for _, tc := range []struct {
		prefixes []string
		path     string
		want     int
	}{
		{[]string{"/", "/aaa"}, "/aaa/ccc", 1},
		{[]string{"/aaa", "/"}, "/aaa/ccc", 0},
		{[]string{"/aaa", "/"}, "/ccc", 1},
		{[]string{"/aaa"}, "/ccc", -1},
		{[]string{"/api", "/api/v2", "/"}, "/api/v2/x", 1},
		{[]string{"/api", "/api/v2", "/"}, "/api/v20", 0},
		{nil, "/", -1},
	} {
		if got := PreviewRoute(tc.prefixes, tc.path); got != tc.want {
			t.Errorf("PreviewRoute(%q, %q) = %d, want %d", tc.prefixes, tc.path, got, tc.want)
		}
	}
}

// elementMatch is an independent statement of Ingress Prefix matching: the
// prefix's "/"-separated elements are the first elements of the path's, a
// trailing slash ignored on both.
func elementMatch(prefix, path string) bool {
	if !strings.HasPrefix(prefix, "/") {
		return false
	}
	pe := strings.Split(strings.TrimRight(prefix, "/"), "/")
	re := strings.Split(strings.TrimRight(path, "/"), "/")
	return len(re) >= len(pe) && slices.Equal(re[:len(pe)], pe)
}

// routeCase is a random list of component prefixes and a request path drawn
// from overlapping segments, so near misses (/a against /ab) are common.
type routeCase struct {
	Prefixes []string
	Path     string
}

var routeSegments = []string{"a", "ab", "b", "api", "v1", "x-y"}

func randomRoutePath(r *rand.Rand, maxDepth int) string {
	var b strings.Builder
	for range r.Intn(maxDepth + 1) {
		b.WriteString("/")
		if r.Intn(12) == 0 {
			b.WriteString("/") // an empty element
		}
		b.WriteString(routeSegments[r.Intn(len(routeSegments))])
	}
	switch r.Intn(10) {
	case 0:
		return strings.TrimPrefix(b.String(), "/") // no leading slash
	case 1, 2:
		return b.String() + "/"
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

func (routeCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := routeCase{}
	seen := map[string]bool{}
	for range 1 + r.Intn(MaxPreviewComponents) {
		p := "/"
		for range r.Intn(3) {
			p = strings.TrimSuffix(p, "/") + "/" + routeSegments[r.Intn(len(routeSegments))]
		}
		if !seen[p] {
			seen[p] = true
			c.Prefixes = append(c.Prefixes, p)
		}
	}
	c.Path = randomRoutePath(r, 4)
	return reflect.ValueOf(c)
}

// TestPreviewRoutingProperty: PreviewPathMatches agrees with the
// element-wise statement for every prefix and path, and PreviewRoute picks a
// matching prefix that no other matching prefix is longer than, or -1 only
// when none matches. Seeded, so the gate is deterministic.
func TestPreviewRoutingProperty(t *testing.T) {
	cfg := &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(20261016))}
	check := func(c routeCase) bool {
		for _, p := range c.Prefixes {
			if PreviewPathMatches(p, c.Path) != elementMatch(p, c.Path) {
				t.Logf("PreviewPathMatches(%q, %q) = %v disagrees with the element-wise match", p, c.Path,
					PreviewPathMatches(p, c.Path))
				return false
			}
		}
		got := PreviewRoute(c.Prefixes, c.Path)
		longest := -1
		for i, p := range c.Prefixes {
			if elementMatch(p, c.Path) && (longest < 0 || len(p) > len(c.Prefixes[longest])) {
				longest = i
			}
		}
		if got != longest {
			t.Logf("PreviewRoute(%q, %q) = %d, want %d", c.Prefixes, c.Path, got, longest)
			return false
		}
		return true
	}
	if err := quick.Check(check, cfg); err != nil {
		t.Error(err)
	}
}

// TestPreviewComponentServiceName: the first component keeps preview-<p>,
// each further one is preview-<p>-<component>.
func TestPreviewComponentServiceName(t *testing.T) {
	p := &Preview{}
	p.Name = "demo-7"
	p.Spec.Components = []PreviewComponent{{Name: "web"}, {Name: "api"}}
	if got := PreviewComponentServiceName(p, 0); got != "preview-demo-7" {
		t.Errorf("first component's Service = %q", got)
	}
	if got := PreviewComponentServiceName(p, 1); got != "preview-demo-7-api" {
		t.Errorf("second component's Service = %q", got)
	}
}

func TestPreviewSlotNamespace(t *testing.T) {
	for n, want := range map[int32]string{0: "maul-preview-0", 3: "maul-preview-3"} {
		if got := PreviewSlotNamespace(n); got != want {
			t.Errorf("PreviewSlotNamespace(%d) = %s, want %s", n, got, want)
		}
	}
}
