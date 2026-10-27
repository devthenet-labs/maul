// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// probeEdges is the live isolation probe's edges fixture: the policies its
// edges run creates in a slot (hack/preview-isolation-probe/run.sh edges).
const probeEdges = "../../../hack/preview-isolation-probe/edges.yaml"

// probeManagedBy is the managed-by label the fixture carries in place of
// preview-controller's, so the orphan sweep (which deletes a policy of a
// Preview that holds no slot) leaves the probe's policies alone.
const probeManagedBy = "maul-isolation-probe"

// TestIsolationProbeEdgesAreRendered: the edges run proves the policies
// preview-controller writes, not a copy that could drift from them. Its
// fixture is exactly what the controller renders for the fixture
// environment (the tester to web, api and render on 8080, spec §9) of a
// Preview named edges, name for name and spec for spec, save the
// managed-by label; each is of the shape the admission policy admits; and
// it names no namespace, which the run gives on its command line.
func TestIsolationProbeEdgesAreRendered(t *testing.T) {
	b, err := os.ReadFile(probeEdges)
	if err != nil {
		t.Fatal(err)
	}
	var got []networkingv1.NetworkPolicy
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	for {
		var np networkingv1.NetworkPolicy
		if err := dec.Decode(&np); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode %s: %v", probeEdges, err)
		}
		got = append(got, np)
	}
	edges := make([]v1alpha1.PreviewEdge, 0, 3)
	for _, to := range []string{"web", "api", "render"} {
		edges = append(edges, v1alpha1.PreviewEdge{From: string(v1alpha1.PreviewRoleTester), To: to, Port: 8080})
	}
	p := &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: "edges", UID: types.UID(probeUIDFromFixture(t, got))},
		Spec:       v1alpha1.PreviewSpec{Environment: &v1alpha1.PreviewEnvironment{Verification: true, Edges: edges}},
	}
	want := verifySettings().networkPolicies(p, 0)
	if len(got) != len(want) {
		t.Fatalf("%s holds %d policies, preview-controller renders %d", probeEdges, len(got), len(want))
	}
	for i, w := range want {
		g := &got[i]
		if err := vapShape(w); err != nil {
			t.Errorf("rendered %s is not of the admitted shape: %v", w.Name, err)
		}
		labels := maps.Clone(w.Labels)
		labels[labelManagedBy] = probeManagedBy
		if g.Kind != "NetworkPolicy" || g.APIVersion != "networking.k8s.io/v1" {
			t.Errorf("policy %d is %s %s, want networking.k8s.io/v1 NetworkPolicy", i, g.APIVersion, g.Kind)
		}
		if g.Name != w.Name || g.Namespace != "" || !maps.Equal(g.Labels, labels) || len(g.Annotations) != 0 {
			t.Errorf("policy %d metadata: name %q namespace %q labels %v annotations %v; want name %q, "+
				"no namespace, labels %v", i, g.Name, g.Namespace, g.Labels, g.Annotations, w.Name, labels)
		}
		if !equality.Semantic.DeepEqual(g.Spec, w.Spec) {
			t.Errorf("%s spec differs from preview-controller's render:\nfixture: %+v\nrender:  %+v",
				w.Name, g.Spec, w.Spec)
		}
	}
}

// probeUIDFromFixture is the Preview UID the fixture's first policy
// selects; the comparison then holds every other place it appears to it.
func probeUIDFromFixture(t *testing.T, nps []networkingv1.NetworkPolicy) string {
	t.Helper()
	if len(nps) == 0 {
		t.Fatalf("%s holds no policy", probeEdges)
	}
	uid := nps[0].Spec.PodSelector.MatchLabels[labelPreviewUID]
	if uid == "" {
		t.Fatalf("%s's first policy selects no Preview UID", probeEdges)
	}
	return uid
}
