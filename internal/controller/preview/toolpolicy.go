// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/previewtools"
)

// The tool pod's network (DESIGN 4.2; SPEC Q2, Q6). Its egress is the
// tester part's: the Preview's own edge policy for part tester admits only
// the components on their declared ports, and preview-isolation (which
// alone grants DNS) does not select role tools. Its ingress is this one
// policy per verification, beside the tester part's (which admits nothing):
// on previewtools.Port, from the agents namespace by name, only the pod
// labelled exactly maul.run/role evaluator and maul.run/preview-verification
// the verification's UID. A NetworkPolicy cannot say "the tool pod of the
// same verification", so the chart's evaluator egress reaches any tool pod;
// this policy is the binding control. It is named preview-<p>-np-ev-<hash>
// (a part name, so the sweep and the slot cleanup cover it) and is left
// alone by the Preview's own policy reconcile.

// evaluatorPolicyPart prefixes the ingress policy's part in its name.
const evaluatorPolicyPart = "ev-"

// evaluatorPolicyName names verification pv's ingress policy.
func evaluatorPolicyName(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification) string {
	return toolName(p, pv, policyInfix+evaluatorPolicyPart)
}

// isEvaluatorPolicy reports whether name is a per-verification ingress
// policy of Preview p, which its own edge reconcile never deletes.
func isEvaluatorPolicy(p *v1alpha1.Preview, name string) bool {
	hash, ok := strings.CutPrefix(name, policyName(p, evaluatorPolicyPart))
	return ok && verificationHash.MatchString(hash)
}

// verificationHash is the hash a verification's slot objects are named by.
var verificationHash = regexp.MustCompile(`^[0-9a-f]{10}$`)

// evaluatorPolicy renders verification pv's ingress policy in Preview p's
// slot.
func (s Settings) evaluatorPolicy(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification,
	slot int32) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	l := labelsFor(p)
	l[labelVerificationUID] = string(pv.UID)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: evaluatorPolicyName(p, pv), Namespace: s.slotName(slot), Labels: l},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: *partSelector(p, v1alpha1.PreviewPartTester),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						corev1.LabelMetadataName: s.AgentsNamespace}},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						v1alpha1.LabelRole: v1alpha1.RoleEvaluator, v1alpha1.LabelPreviewVerification: string(pv.UID)}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp,
					Port: ptr(intstr.FromInt32(previewtools.Port))}},
			}},
		},
	}
}
