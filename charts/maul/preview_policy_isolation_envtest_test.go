// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// genericSelector is the generic preview node label the isolation tests use.
var genericSelector = map[string]string{"maul.example.com/previews": "true"}

// TestPreviewGenericIsolationAgainstAPIServer: with a generic node selector
// in place of the EKS Auto Mode names, the rendered slot policies admit a
// Pod selecting exactly those nodes and tolerating the taint, refuse one
// selecting the EKS pool instead or nothing, and still keep the taint off
// every other namespace.
func TestPreviewGenericIsolationAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	env, admin := startPreviewPolicyEnv(t, "--set", "preview.nodeIsolation.nodePool=",
		"--set", "preview.nodeIsolation.nodeClass=",
		"--set-json", `preview.nodeIsolation.selector={"maul.example.com/previews":"true"}`)
	testIngressProbePods(t, env, "--node-selector", "maul.example.com/previews=true", "--taint-key",
		previewTaintKey)
	generic := func(name string) *corev1.Pod {
		return mutatePod(pod("maul-preview-0", name, previewImage), func(p *corev1.Pod) {
			p.Spec.NodeSelector = genericSelector
		})
	}
	admitted(t, admin, generic("generic"))
	admitted(t, admin, mutatePod(generic("generic-and-more"), func(p *corev1.Pod) {
		p.Spec.NodeSelector = map[string]string{"maul.example.com/previews": "true", "zone": "a"}
	}))
	refused(t, admin, pod("maul-preview-0", "eks-pool", previewImage), "dedicated preview nodes")
	refused(t, admin, mutatePod(generic("wrong-value"), func(p *corev1.Pod) {
		p.Spec.NodeSelector = map[string]string{"maul.example.com/previews": "false"}
	}), "dedicated preview nodes")
	refused(t, admin, mutatePod(generic("no-selector"), func(p *corev1.Pod) { p.Spec.NodeSelector = nil }),
		"dedicated preview nodes")
	refused(t, admin, mutatePod(generic("no-toleration"), func(p *corev1.Pod) { p.Spec.Tolerations = nil }),
		"preview-only taint")
	refused(t, admin, mutatePod(pod("ordinary", "taint", "nginx:latest"), func(p *corev1.Pod) {
		p.Spec.Tolerations = previewTolerations()
	}), "preview-only toleration")
}

// TestPreviewNoIsolationAgainstAPIServer: node isolation none is an explicit
// opt-out, so a slot Pod without a node selector or toleration is admitted,
// every other slot rule still holds, and tolerate-all stays refused outside
// the slots.
func TestPreviewNoIsolationAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	_, admin := startPreviewPolicyEnv(t, "--set", "preview.nodeIsolation.nodePool=",
		"--set", "preview.nodeIsolation.nodeClass=", "--set", "preview.nodeIsolation.taintKey=",
		"--set", "preview.nodeIsolation.mode=none")
	bare := func(name, image string) *corev1.Pod {
		return mutatePod(pod("maul-preview-0", name, image), func(p *corev1.Pod) {
			p.Spec.NodeSelector, p.Spec.Tolerations = nil, nil
		})
	}
	admitted(t, admin, bare("bare", previewImage))
	refused(t, admin, bare("wrong-image", "nginx:latest"), "only immutable")
	refused(t, admin, mutatePod(bare("direct-node", previewImage), func(p *corev1.Pod) {
		p.Spec.NodeName = "i-0123456789abcdef0"
	}), "default scheduler")
	refused(t, admin, withSecretEnv(bare("secret", previewImage)), "may not read a Secret")
	refused(t, admin, mutatePod(pod("ordinary", "tolerate-all", "nginx:latest"), func(p *corev1.Pod) {
		p.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}), "tolerate-all")
}

// admitted: a dry-run create of obj succeeds.
func admitted(t *testing.T, admin client.Client, obj client.Object) {
	t.Helper()
	if err := admin.Create(t.Context(), obj, client.DryRunAll); err != nil {
		t.Errorf("%s/%s refused: %v", obj.GetNamespace(), obj.GetName(), err)
	}
}

// refused: a dry-run create of obj fails with want in its message.
func refused(t *testing.T, admin client.Client, obj client.Object, want string) {
	t.Helper()
	err := admin.Create(t.Context(), obj, client.DryRunAll)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s/%s: %v, want a refusal containing %q", obj.GetNamespace(), obj.GetName(), err, want)
	}
}
