// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The slot NetworkPolicy's shape, proven statically before the live probe.
// The live probe's agents-namespace client is held back by the chart's own
// default-deny in the agents' namespace too, so its BLOCKED proves that
// agents cannot reach a slot by any path, not that the slot policy itself
// refuses them: a slot policy that also admitted the agents' namespace
// would pass the live run. So every NetworkPolicy in the slot namespace is
// read, and only the chart's peers are allowed: the gateway (the release
// namespace AND the gateway's label, exactly) and, while the legacy load
// balancer path serves, ipBlocks.

// metadataNameLabel is the label the API server puts on every namespace.
const metadataNameLabel = "kubernetes.io/metadata.name"

// slotPolicy lists the slot namespace's NetworkPolicies and judges their
// shape (judgeSlotPolicies); it returns the ipBlock ranges they admit.
func slotPolicy(ctx context.Context, r client.Reader, slotNS, releaseNS string, legacy bool) ([]string, error) {
	var list networkingv1.NetworkPolicyList
	if err := r.List(ctx, &list, client.InNamespace(slotNS)); err != nil {
		return nil, fmt.Errorf("cannot list the NetworkPolicies in the slot namespace %s, so the slot policy's "+
			"shape cannot be proven: %w", slotNS, err)
	}
	return judgeSlotPolicies(list.Items, slotNS, releaseNS, legacy)
}

// judgeSlotPolicies is the static check of a slot namespace's
// NetworkPolicies, pure. The chart's preview-isolation must exist, govern
// ingress for every Pod of the slot and admit the gateway's peer; no
// policy there may admit any other peer: no rule without a from (every
// source), no namespaceSelector or podSelector but the gateway's exact
// one, no match expression, and an ipBlock only while the legacy load
// balancer path serves (legacy). Any other peer is an error naming the
// policy and the peer.
func judgeSlotPolicies(policies []networkingv1.NetworkPolicy, slotNS, releaseNS string,
	legacy bool) ([]string, error) {
	slices.SortFunc(policies, func(a, b networkingv1.NetworkPolicy) int { return strings.Compare(a.Name, b.Name) })
	var problems, blocks []string
	found, admits := false, false
	for i := range policies {
		np := &policies[i]
		where := slotNS + "/" + np.Name
		if np.Name == slotPolicyName {
			found = true
			if len(np.Spec.PodSelector.MatchLabels) > 0 || len(np.Spec.PodSelector.MatchExpressions) > 0 {
				problems = append(problems, where+" selects only some of the slot's Pods (its podSelector is not {})")
			}
			if len(np.Spec.PolicyTypes) > 0 && !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) {
				problems = append(problems, where+" does not govern ingress (its policyTypes lack Ingress)")
			}
		}
		for _, rule := range np.Spec.Ingress {
			if len(rule.From) == 0 {
				problems = append(problems, where+" has an ingress rule with no from, which admits every source")
				continue
			}
			for _, peer := range rule.From {
				switch {
				case isGatewayPeer(peer, releaseNS):
					admits = admits || np.Name == slotPolicyName
				case isBlockPeer(peer) && legacy:
					blocks = append(blocks, peer.IPBlock.CIDR)
				case isBlockPeer(peer):
					problems = append(problems, fmt.Sprintf("%s admits ipBlock %s, but preview.legacyIngress is "+
						"false, so no load balancer needs it", where, peer.IPBlock.CIDR))
				default:
					problems = append(problems, fmt.Sprintf("%s admits an ingress peer other than the preview "+
						"gateway's (%s AND %s=true): %s", where, metadataNameLabel+"="+releaseNS, gatewayLabel,
						peerJSON(peer)))
				}
			}
		}
	}
	switch {
	case !found:
		return nil, fmt.Errorf("there is no slot NetworkPolicy %s/%s: is the slot created (preview.enabled)?",
			slotNS, slotPolicyName)
	case len(problems) > 0:
		return nil, fmt.Errorf("the slot's NetworkPolicies admit more than the preview gateway: %s",
			strings.Join(problems, "; "))
	case !admits:
		return nil, fmt.Errorf("the slot NetworkPolicy %s/%s admits no preview gateway Pod: is "+
			"previewAuth.gateway.enabled set?", slotNS, slotPolicyName)
	}
	slices.Sort(blocks)
	return slices.Compact(blocks), nil
}

// isGatewayPeer reports whether peer is exactly the chart's gateway peer:
// the release namespace by its name label AND the gateway's label, by
// labels alone.
func isGatewayPeer(peer networkingv1.NetworkPolicyPeer, releaseNS string) bool {
	return peer.IPBlock == nil && peer.NamespaceSelector != nil && peer.PodSelector != nil &&
		len(peer.NamespaceSelector.MatchExpressions) == 0 && len(peer.PodSelector.MatchExpressions) == 0 &&
		maps.Equal(peer.NamespaceSelector.MatchLabels, map[string]string{metadataNameLabel: releaseNS}) &&
		maps.Equal(peer.PodSelector.MatchLabels, map[string]string{gatewayLabel: "true"})
}

// isBlockPeer reports whether peer is an ipBlock alone.
func isBlockPeer(peer networkingv1.NetworkPolicyPeer) bool {
	return peer.IPBlock != nil && peer.IPBlock.CIDR != "" && peer.NamespaceSelector == nil && peer.PodSelector == nil
}

// peerJSON renders a peer for a reason.
func peerJSON(peer networkingv1.NetworkPolicyPeer) string {
	b, err := json.Marshal(peer)
	if err != nil {
		return fmt.Sprintf("%+v", peer)
	}
	return string(b)
}
