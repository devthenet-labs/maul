// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The slot's chart-owned preview-isolation policy admits no traffic between
// a slot's Pods. A verification-capable Preview's declared edges
// (spec.environment, re-derived from the operator's Project before anything
// renders) are admitted by policies of the controller's own, one per part
// that has an edge, named preview-<p>-np-<part>, each additive to
// preview-isolation. A policy selects one part of one Preview by labels
// only (its UID and the part), always covers both directions, and admits
// exactly the part's edges: ingress from each calling part on the edge's
// numeric TCP port, egress to each called part likewise. A part with no
// incoming edge has no ingress rule (nothing may call the tester); one with
// no outgoing edge has no egress rule, so preview-isolation's DNS stays its
// only egress. Peers are matched by pod selector within the slot, after the
// Service's address translation, so the port is the called container's.
// The maul-preview-networkpolicies admission policy holds every write to
// this shape.

// policyInfix separates a policy's name from its part.
const policyInfix = "-np-"

// policyName names part's policy.
func policyName(p *v1alpha1.Preview, part string) string {
	return "preview-" + p.Name + policyInfix + part
}

// partSelector selects part's Pods of Preview p: its UID and the part,
// exactly the two labels the admission policy admits.
func partSelector(p *v1alpha1.Preview, part string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{
		labelPreviewUID: string(p.UID), labelPart: part,
	}}
}

// networkPolicies renders one policy per part of Preview p with an edge, in
// name order, or none when the Preview has no environment or verification is
// off.
func (s Settings) networkPolicies(p *v1alpha1.Preview, slot int32) []*networkingv1.NetworkPolicy {
	if !s.verifying() || p.Spec.Environment == nil {
		return nil
	}
	edges := slices.Clone(p.Spec.Environment.Edges)
	slices.SortFunc(edges, func(a, b v1alpha1.PreviewEdge) int {
		return cmp.Or(strings.Compare(a.From, b.From), strings.Compare(a.To, b.To), cmp.Compare(a.Port, b.Port))
	})
	parts := map[string]bool{}
	for _, e := range edges {
		parts[e.From], parts[e.To] = true, true
	}
	tcp := corev1.ProtocolTCP
	rulePorts := func(port int32) []networkingv1.NetworkPolicyPort {
		return []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: ptr(intstr.FromInt32(port))}}
	}
	out := make([]*networkingv1.NetworkPolicy, 0, len(parts))
	for _, part := range slices.Sorted(maps.Keys(parts)) {
		var ingress []networkingv1.NetworkPolicyIngressRule
		var egress []networkingv1.NetworkPolicyEgressRule
		for _, e := range edges {
			if e.To == part {
				ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: partSelector(p, e.From)}},
					Ports: rulePorts(e.Port),
				})
			}
			if e.From == part {
				egress = append(egress, networkingv1.NetworkPolicyEgressRule{
					To:    []networkingv1.NetworkPolicyPeer{{PodSelector: partSelector(p, e.To)}},
					Ports: rulePorts(e.Port),
				})
			}
		}
		out = append(out, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: policyName(p, part), Namespace: s.slotName(slot), Labels: labelsFor(p)},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: *partSelector(p, part),
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				Ingress:     ingress,
				Egress:      egress,
			},
		})
	}
	return out
}

// ensureNetworkPolicies renders Preview p's policies into its slot and
// deletes the ones it owns that no part renders any more. A policy is
// written with update, never patch (the chart grants no patch), and never
// over a foreign object. With verification off it reads and writes nothing.
func (r *Reconciler) ensureNetworkPolicies(ctx context.Context, p *v1alpha1.Preview, slot int32) error {
	if !r.Settings.verifying() {
		return nil
	}
	desired := r.Settings.networkPolicies(p, slot)
	names := make(map[string]bool, len(desired))
	for _, np := range desired {
		names[np.Name] = true
		if err := r.ensureNetworkPolicy(ctx, p, np); err != nil {
			return err
		}
	}
	var live networkingv1.NetworkPolicyList
	if err := r.List(ctx, &live, client.InNamespace(r.Settings.slotName(slot)), ownedSelector(p)); err != nil {
		return err
	}
	for i := range live.Items {
		np := &live.Items[i]
		// A verification's ingress policy is its own (toolpolicy.go).
		if names[np.Name] || isEvaluatorPolicy(p, np.Name) || !ownedBy(np, p) || !np.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, np); err != nil && !kerrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// ensureEdges renders Preview p's policies (ensureNetworkPolicies) without
// ever holding up its deployment: the edges are additive to
// preview-isolation, so a policy missing leaves the slot closed rather than
// open, and the Preview serves without one. A failure (a write refused at
// admission while a rollout's policy catches up, a foreign object on a
// policy's name, an API error) is a log line and a Warning Event, retried at
// the next poll; a verification waits for its policies (policiesInPlace).
func (r *Reconciler) ensureEdges(ctx context.Context, p *v1alpha1.Preview, slot int32) {
	err := r.ensureNetworkPolicies(ctx, p, slot)
	if err == nil {
		return
	}
	if r.Log != nil {
		r.Log.LogAttrs(ctx, slog.LevelWarn, "preview NetworkPolicy not rendered; edges stay closed",
			slog.String("preview", p.Name), slog.Int("slot", int(slot)), slog.Any("error", err))
	}
	if r.Events != nil {
		r.Events.Eventf(p, nil, corev1.EventTypeWarning, "NetworkPolicyNotRendered", "WriteNetworkPolicy",
			"a preview edge policy could not be written; the slot's edges stay closed and the controller "+
				"retries at the next poll: %v", err)
	}
}

// policiesInPlace reports whether every policy Preview p renders is in its
// slot exactly as rendered: what a verification waits for before its tester
// starts, since an edge still closed would read as the application failing.
// With no policy to render it reports false: nothing would reach a part.
func (r *Reconciler) policiesInPlace(ctx context.Context, p *v1alpha1.Preview, slot int32) (bool, error) {
	desired := r.Settings.networkPolicies(p, slot)
	if len(desired) == 0 {
		return false, nil
	}
	for _, np := range desired {
		var live networkingv1.NetworkPolicy
		err := r.Get(ctx, client.ObjectKeyFromObject(np), &live)
		if kerrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !ownedBy(&live, p) || !live.DeletionTimestamp.IsZero() || !reflect.DeepEqual(live.Spec, np.Spec) {
			return false, nil
		}
	}
	return true, nil
}

func (r *Reconciler) ensureNetworkPolicy(ctx context.Context, p *v1alpha1.Preview,
	desired *networkingv1.NetworkPolicy) error {
	var live networkingv1.NetworkPolicy
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &live)
	if kerrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !ownedBy(&live, p) {
		return fmt.Errorf("network policy %s is foreign", desired.Name)
	}
	if !live.DeletionTimestamp.IsZero() {
		return errDeleting
	}
	if reflect.DeepEqual(live.Spec, desired.Spec) && maps.Equal(live.Labels, desired.Labels) {
		return nil
	}
	live.Spec = desired.Spec
	live.Labels = desired.Labels
	return r.Update(ctx, &live)
}
