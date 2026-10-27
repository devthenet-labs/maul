// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// A small NetworkPolicy evaluator: just enough of the Kubernetes semantics
// to judge the slot's policies. A connection from src to dst on a TCP port
// is admitted when src's egress and dst's ingress both admit it. A pod no
// policy of a direction selects is open in that direction; a selected pod
// admits exactly the union of the rules of the policies selecting it. A
// peer's podSelector without a namespaceSelector matches pods of the
// policy's own namespace only; an ipBlock is modelled as matching no pod (no
// slot pod address is in the ALB's subnets); a named port resolves against
// the destination pod's container ports.

type npPod struct {
	name      string
	namespace string
	labels    map[string]string
	nsLabels  map[string]string
	ports     map[string]int32 // container port name to number
}

type npWorld struct {
	policies []*networkingv1.NetworkPolicy
}

func (w npWorld) admits(src, dst npPod, port int32) bool {
	return w.direction(dst, networkingv1.PolicyTypeIngress, func(p *networkingv1.NetworkPolicy) bool {
		return slices.ContainsFunc(p.Spec.Ingress, func(r networkingv1.NetworkPolicyIngressRule) bool {
			return peersMatch(r.From, p.Namespace, src) && portsMatch(r.Ports, dst, port)
		})
	}) && w.direction(src, networkingv1.PolicyTypeEgress, func(p *networkingv1.NetworkPolicy) bool {
		return slices.ContainsFunc(p.Spec.Egress, func(r networkingv1.NetworkPolicyEgressRule) bool {
			return peersMatch(r.To, p.Namespace, dst) && portsMatch(r.Ports, dst, port)
		})
	})
}

// direction reports whether pod is open in direction t: selected by no
// policy of that type, or admitted by a rule of one that selects it.
func (w npWorld) direction(pod npPod, t networkingv1.PolicyType, rule func(*networkingv1.NetworkPolicy) bool) bool {
	selected := false
	for _, p := range w.policies {
		if p.Namespace != pod.namespace || !slices.Contains(p.Spec.PolicyTypes, t) ||
			!selectorMatches(&p.Spec.PodSelector, pod.labels) {
			continue
		}
		selected = true
		if rule(p) {
			return true
		}
	}
	return !selected
}

func selectorMatches(s *metav1.LabelSelector, l map[string]string) bool {
	sel, err := metav1.LabelSelectorAsSelector(s)
	if err != nil {
		panic(err)
	}
	return sel.Matches(labels.Set(l))
}

// peersMatch: an absent or empty peer list admits every peer.
func peersMatch(peers []networkingv1.NetworkPolicyPeer, policyNS string, pod npPod) bool {
	if len(peers) == 0 {
		return true
	}
	for _, peer := range peers {
		if peer.IPBlock != nil {
			continue
		}
		switch {
		case peer.NamespaceSelector != nil:
			if !selectorMatches(peer.NamespaceSelector, pod.nsLabels) {
				continue
			}
		case pod.namespace != policyNS:
			continue
		}
		if peer.PodSelector != nil && !selectorMatches(peer.PodSelector, pod.labels) {
			continue
		}
		return true
	}
	return false
}

// portsMatch: an absent or empty port list admits every port.
func portsMatch(ports []networkingv1.NetworkPolicyPort, dst npPod, port int32) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p.Protocol != nil && *p.Protocol != corev1.ProtocolTCP {
			continue
		}
		if p.Port == nil {
			return true
		}
		want := p.Port.IntVal
		if p.Port.Type == intstr.String {
			named, ok := dst.ports[p.Port.StrVal]
			if !ok {
				continue
			}
			want = named
		}
		if p.EndPort != nil && port >= want && port <= *p.EndPort {
			return true
		}
		if port == want {
			return true
		}
	}
	return false
}

// isolationPolicy mirrors the chart's preview-isolation policy
// (charts/maul/templates/preview-foundation.yaml): ingress on the named port
// http from the gateway's Pods (and, on the legacy path, the ALB's subnets),
// egress to DNS only.
func isolationPolicy(ns string, gateway bool) *networkingv1.NetworkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	from := []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/24"}}}
	if gateway {
		from = append(from, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": "maul"}},
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"maul.run/preview-gateway": "true"}},
		})
	}
	dns := intstr.FromInt32(53)
	http := intstr.FromString("http")
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "preview-isolation", Namespace: ns},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: from,
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &http}}}},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
			}},
		},
	}
}

// randomEnvironmentPreview is a verification-capable Preview of one to four
// components with random names and ports and a random edge set: some tester
// edges and some component-to-component edges (slice 2's shape), each on a
// random port, so the renderer is judged on edges in general.
func randomEnvironmentPreview(rng *rand.Rand) *v1alpha1.Preview {
	n := 1 + rng.IntN(v1alpha1.MaxPreviewComponents)
	components := make([]v1alpha1.PreviewComponent, 0, n)
	paths := []string{"/", "/api", "/render", "/admin"}
	for i := range n {
		components = append(components, v1alpha1.PreviewComponent{
			Name:            fmt.Sprintf("c%d%c", i, 'a'+rune(rng.IntN(26))),
			ImageRepository: testSettings().ImagePrefix + fmt.Sprintf("leaf%d", i),
			Revision:        testSHA, Port: int32(1024 + rng.IntN(3)*1000 + i), ReadinessPath: "/health",
			Path: paths[i],
		})
	}
	parts := make([]string, 0, 1+n)
	parts = append(parts, v1alpha1.PreviewPartTester)
	for _, c := range components {
		parts = append(parts, c.Name)
	}
	seen := map[v1alpha1.PreviewEdge]bool{}
	var edges []v1alpha1.PreviewEdge
	for range 1 + rng.IntN(8) {
		from := parts[rng.IntN(len(parts))]
		to := components[rng.IntN(n)].Name
		if from == to {
			continue
		}
		port := []int32{components[0].Port, 8080, 5432, int32(1 + rng.IntN(65535))}[rng.IntN(4)]
		e := v1alpha1.PreviewEdge{From: from, To: to, Port: port}
		if !seen[e] {
			seen[e] = true
			edges = append(edges, e)
		}
	}
	if len(edges) == 0 {
		edges = append(edges, v1alpha1.PreviewEdge{From: v1alpha1.PreviewPartTester, To: components[0].Name,
			Port: components[0].Port})
	}
	return &v1alpha1.Preview{
		ObjectMeta: metav1.ObjectMeta{Name: "npsem", Namespace: "maul", UID: types.UID("uid-npsem")},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: "npsem", UID: "intent-npsem"}, HostLabel: "npsem",
			Components: components, TTL: metav1.Duration{Duration: time.Hour},
			Environment: &v1alpha1.PreviewEnvironment{Verification: true, Edges: edges},
		},
	}
}

// slotPods are the pods of Preview p in slot ns: each component's (its
// Deployment's template) and the tester's (its Job's template).
func slotPods(t *testing.T, s Settings, p *v1alpha1.Preview, ns string) map[string]npPod {
	t.Helper()
	nsLabels := map[string]string{"kubernetes.io/metadata.name": ns}
	pods := map[string]npPod{}
	for i, c := range p.Spec.Components {
		d := s.deployment(p, i, 0)
		ports := map[string]int32{}
		for _, cp := range d.Spec.Template.Spec.Containers[0].Ports {
			ports[cp.Name] = cp.ContainerPort
		}
		pods[c.Name] = npPod{name: c.Name, namespace: ns, labels: maps.Clone(d.Spec.Template.Labels),
			nsLabels: nsLabels, ports: ports}
	}
	pv := testVerification(p)
	job, err := s.testerJob(p, pv, 0, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	pods[v1alpha1.PreviewPartTester] = npPod{name: v1alpha1.PreviewPartTester, namespace: ns,
		labels: maps.Clone(job.Spec.Template.Labels), nsLabels: nsLabels}
	return pods
}

// TestNetworkPolicySemantics is the property the slot's isolation rests on:
// for random edge sets, the controller's per-part policies beside the
// chart's preview-isolation admit exactly the declared edges between the
// Preview's parts (part, part, port), and nothing between them and the same
// labels in another slot, another Preview's parts in the same slot, the
// gateway (beyond the component's http port it already had) or anything
// else. Nothing ever reaches the tester.
func TestNetworkPolicySemantics(t *testing.T) {
	s := verifySettings()
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewPCG(uint64(seed), 7))
	t.Logf("seed %d", seed)
	for iter := range 300 {
		p := randomEnvironmentPreview(rng)
		if err := s.validatePreview(p); err != nil {
			t.Fatalf("iteration %d: random Preview invalid: %v", iter, err)
		}
		gateway := rng.IntN(2) == 0
		world := npWorld{policies: []*networkingv1.NetworkPolicy{
			isolationPolicy("maul-preview-0", gateway), isolationPolicy("maul-preview-1", gateway),
		}}
		for _, np := range s.networkPolicies(p, 0) {
			world.policies = append(world.policies, np)
			// The same Preview's policies left behind in another slot.
			other := np.DeepCopy()
			other.Namespace = "maul-preview-1"
			world.policies = append(world.policies, other)
		}
		own := slotPods(t, s, p, "maul-preview-0")
		otherSlot := slotPods(t, s, p, "maul-preview-1")
		// Another Preview's parts in the same slot, under its own policies
		// (a leftover the sweep has not reached).
		stranger := p.DeepCopy()
		stranger.UID = "uid-stranger"
		strangers := slotPods(t, s, stranger, "maul-preview-0")
		world.policies = append(world.policies, s.networkPolicies(stranger, 0)...)
		if err := world.judge(p, own, gateway, otherSlot, strangers); err != nil {
			t.Fatalf("seed %d iteration %d: %v (edges %+v)", seed, iter, err, p.Spec.Environment.Edges)
		}
	}
}

// judge checks every connection the property names: between the Preview's
// own parts exactly the declared edges, nothing with a foreign part either
// way, and from the gateway only a component's http port (when the gateway
// is on), never to it. Each is tried on every port an edge or a component
// names, plus DNS, the Service port and two common ones.
func (w npWorld) judge(p *v1alpha1.Preview, own map[string]npPod, gateway bool,
	foreign ...map[string]npPod) error {
	gw := npPod{name: "gateway", namespace: "maul", labels: map[string]string{"maul.run/preview-gateway": "true"},
		nsLabels: map[string]string{"kubernetes.io/metadata.name": "maul"}}
	declared := map[v1alpha1.PreviewEdge]bool{}
	ports := make([]int32, 0, 4+len(p.Spec.Environment.Edges)+len(p.Spec.Components))
	ports = append(ports, 53, 80, 8080, 5432)
	for _, e := range p.Spec.Environment.Edges {
		declared[e] = true
		ports = append(ports, e.Port)
	}
	for _, c := range p.Spec.Components {
		ports = append(ports, c.Port)
	}
	strangers := make([]npPod, 0, 2*len(own))
	for _, f := range foreign {
		strangers = append(strangers, slices.Collect(maps.Values(f))...)
	}
	for _, src := range own {
		for _, port := range ports {
			for _, dst := range own {
				want := declared[v1alpha1.PreviewEdge{From: src.name, To: dst.name, Port: port}]
				if got := w.admits(src, dst, port); src.name != dst.name && got != want {
					return fmt.Errorf("%s -> %s:%d admitted = %v, declared = %v", src.name, dst.name, port, got, want)
				}
			}
			for _, f := range strangers {
				if w.admits(src, f, port) || w.admits(f, src, port) {
					return fmt.Errorf("%s and foreign %s/%s connect on %d", src.name, f.namespace,
						f.labels[labelPreviewUID], port)
				}
			}
			http, isComponent := src.ports["http"]
			if want := gateway && isComponent && port == http; w.admits(gw, src, port) != want {
				return fmt.Errorf("gateway -> %s:%d admitted = %v", src.name, port, !want)
			}
			if w.admits(src, gw, port) {
				return fmt.Errorf("%s -> gateway:%d admitted", src.name, port)
			}
		}
	}
	return nil
}

// The evaluator itself: preview-isolation alone admits nothing between two
// slot pods, and a policy-free namespace admits everything.
func TestNetworkPolicyEvaluator(t *testing.T) {
	a := npPod{name: "a", namespace: "maul-preview-0", labels: map[string]string{"x": "a"},
		ports: map[string]int32{"http": 8080}}
	b := npPod{name: "b", namespace: "maul-preview-0", labels: map[string]string{"x": "b"},
		ports: map[string]int32{"http": 8080}}
	if (npWorld{}).admits(a, b, 8080) != true {
		t.Error("no policy must admit")
	}
	w := npWorld{policies: []*networkingv1.NetworkPolicy{isolationPolicy("maul-preview-0", true)}}
	if w.admits(a, b, 8080) || w.admits(a, b, 53) {
		t.Error("preview-isolation admitted slot-internal traffic")
	}
	dns := npPod{name: "dns", namespace: "kube-system", nsLabels: map[string]string{
		"kubernetes.io/metadata.name": "kube-system"}}
	if !w.admits(a, dns, 53) || w.admits(a, dns, 443) {
		t.Error("preview-isolation DNS egress wrong")
	}
}
