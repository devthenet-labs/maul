// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewdetect

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/devthenet-labs/maul/cmd/maul/internal/previewcheck"
	"github.com/devthenet-labs/maul/cmd/maul/internal/previewsetup"
	"github.com/devthenet-labs/maul/internal/kube"
)

// world is a k3d cluster with Traefik, cert-manager, a tainted preview
// node, and a maul release whose previews already run behind the gateway.
// It holds Secrets everywhere detection might look, so a read of one would
// be seen.
func world() []client.Object {
	node := func(name string, labels map[string]string, taints ...corev1.Taint) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Spec: corev1.NodeSpec{ProviderID: "k3s://" + name, Taints: taints}}
	}
	cm := func(name, controller string, data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "maul",
			Labels: map[string]string{previewcheck.LabelName: controller, "app.kubernetes.io/part-of": "maul"}},
			Data: data}
	}
	secret := func(ns, name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Data: map[string][]byte{"k": []byte("never-read")}}
	}
	gc := &unstructured.Unstructured{}
	gc.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1",
		Kind: "GatewayClass"})
	gc.SetName("traefik-gw")
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	crd.SetName(CertManagerCRD)
	ready := true
	return []client.Object{
		node("k3d-dev-server-0", nil),
		node("k3d-dev-agent-0", map[string]string{previewsetup.DefaultNodeLabel: "true"},
			corev1.Taint{Key: previewsetup.DefaultTaintKey, Value: "true", Effect: corev1.TaintEffectNoExecute}),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kubernetes"},
			Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.1", ClusterIPs: []string{"10.43.0.1"}}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kubernetes",
			Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"172.18.0.2"},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready}}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-dns"},
			Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.10"}},
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "traefik",
			Annotations: map[string]string{IngressClassDefault: "true"}},
			Spec: networkingv1.IngressClassSpec{Controller: "traefik.io/ingress-controller"}},
		gc, crd,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "coredns-1"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "coredns",
				Image: "rancher/mirrored-coredns-coredns:1.12.0"}}}},
		cm("maul-preview-controller", previewcheck.PreviewController, map[string]string{
			KeyPreviewHostSuffix: "acme-previews.dev", KeyPreviewLegacy: "false"}),
		cm("maul-preview-auth-config", previewcheck.PreviewAuth, map[string]string{
			KeyAuthIssuer: "https://preview-auth.maul.acme.dev", KeyAuthDexIssuer: "https://dex.maul.acme.dev",
			KeyAuthGateway: "true"}),
		secret("maul", "maul-preview-auth-dex"), secret("kube-system", "k3s-serving"),
		secret("default", "kubernetes-token"), secret("maul", "maul-status-auth"),
	}
}

// isSecret reports a typed or unstructured Secret (or list of them).
func isSecret(o any) bool {
	switch v := o.(type) {
	case *corev1.Secret, *corev1.SecretList:
		return true
	case *unstructured.Unstructured:
		return v.GetKind() == "Secret"
	case *unstructured.UnstructuredList:
		return v.GetKind() == "SecretList" || v.GetKind() == "Secret"
	case *metav1.PartialObjectMetadata:
		return v.Kind == "Secret"
	case *metav1.PartialObjectMetadataList:
		return v.Kind == "SecretList"
	}
	return false
}

// observe runs Observe over objs, failing the test on any Secret read and
// any write (detection is read-only); fn intercepts reads first when given.
func observe(t *testing.T, objs []client.Object, fn interceptor.Funcs) Observation {
	t.Helper()
	get, list := fn.Get, fn.List
	fn.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if isSecret(obj) {
			t.Errorf("detection read Secret %s", key)
		}
		if get != nil {
			return get(ctx, c, key, obj, opts...)
		}
		return c.Get(ctx, key, obj, opts...)
	}
	fn.List = func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		if isSecret(l) {
			t.Error("detection listed Secrets")
		}
		if list != nil {
			return list(ctx, c, l, opts...)
		}
		return c.List(ctx, l, opts...)
	}
	write := func(verb string) {
		t.Errorf("detection wrote to the cluster (%s)", verb)
	}
	fn.Create = func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		write("create")
		return nil
	}
	fn.Update = func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		write("update")
		return nil
	}
	fn.Patch = func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		write("patch")
		return nil
	}
	fn.Delete = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		write("delete")
		return nil
	}
	fn.DeleteAllOf = func(context.Context, client.WithWatch, client.Object, ...client.DeleteAllOfOption) error {
		write("deleteallof")
		return nil
	}
	reader := fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).WithInterceptorFuncs(fn).Build()
	return Observe(context.Background(), reader, "maul")
}

// TestObserveReadsNoSecretAndWritesNothing pins the --detect boundary
// (design §4.2, §5.3): with Secrets present in every namespace detection
// reads, none is read, nothing is written, and what it does read is
// observed.
func TestObserveReadsNoSecretAndWritesNothing(t *testing.T) {
	o := observe(t, world(), interceptor.Funcs{})
	if len(o.Notes) != 0 {
		t.Errorf("Notes = %q, want none", o.Notes)
	}
	if got := len(o.Nodes); got != 2 {
		t.Errorf("Nodes = %d, want 2", got)
	}
	for name, pair := range map[string][2]any{
		"APIServiceIPs":  {o.APIServiceIPs, []string{"10.43.0.1"}},
		"APIEndpoints":   {o.APIEndpoints, []string{"172.18.0.2"}},
		"ClusterDNS":     {o.ClusterDNS, "10.43.0.10"},
		"GatewayClasses": {o.GatewayClasses, []string{"traefik-gw"}},
		"IngressClasses": {o.IngressClasses, []IngressClass{{Name: "traefik",
			Controller: "traefik.io/ingress-controller", Default: true}}},
		"CertManager": {o.CertManager, Present},
		"Tailscale":   {o.Tailscale, Absent},
		"Images":      {o.Images, []string{"rancher/mirrored-coredns-coredns:1.12.0"}},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s = %#v, want %#v", name, pair[0], pair[1])
		}
	}
	if !o.Release.Preview.Found() || !o.Release.Auth.Found() {
		t.Errorf("release settings not read: %+v", o.Release)
	}
}

// TestObserveRefusedReadsAreNotes: a read the caller may not make (an RBAC
// refusal), or of a kind the cluster does not serve, is a note or an
// absence, never an error, and the other reads still happen.
func TestObserveRefusedReadsAreNotes(t *testing.T) {
	o := observe(t, world(), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			switch l.(type) {
			case *corev1.NodeList, *corev1.PodList:
				return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
			}
			return c.List(ctx, l, opts...)
		},
	})
	if len(o.Nodes) != 0 || len(o.Images) != 0 {
		t.Errorf("refused reads returned data: %d nodes, %d images", len(o.Nodes), len(o.Images))
	}
	if len(o.Notes) != 2 || !strings.Contains(o.Notes[0], "the nodes") ||
		!strings.Contains(o.Notes[1], "kube-system Pods") {
		t.Errorf("Notes = %q, want the nodes and the kube-system Pods", o.Notes)
	}
	if len(o.IngressClasses) != 1 {
		t.Errorf("the other reads stopped: IngressClasses = %v", o.IngressClasses)
	}
}

// TestObserveEmptyCluster: a cluster with none of the optional kinds
// installed observes them absent, with no note.
func TestObserveEmptyCluster(t *testing.T) {
	o := observe(t, nil, interceptor.Funcs{})
	// Only the default/kubernetes Service read fails (it does not exist here).
	if len(o.Notes) != 1 || !strings.Contains(o.Notes[0], "default/kubernetes Service") {
		t.Errorf("Notes = %q", o.Notes)
	}
	if o.CertManager != Absent || o.Tailscale != Absent || len(o.GatewayClasses) != 0 || o.ClusterDNS != "" {
		t.Errorf("Observation = %+v, want nothing installed", o)
	}
	if o.Release.Preview.Found() || slices.Contains(o.Notes, "") {
		t.Errorf("release found in an empty cluster")
	}
}

// TestObserveUnreachable: when no read reaches the API server (the client
// connects lazily, so each read fails on its own), Observe says the
// cluster is unreachable; a cluster that refuses every read was reached.
func TestObserveUnreachable(t *testing.T) {
	refused := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	every := func(err error) interceptor.Funcs {
		return interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return err
			},
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return err
			},
		}
	}
	o := observe(t, world(), every(refused))
	if !errors.Is(o.Unreachable, refused) {
		t.Errorf("Unreachable = %v, want %v", o.Unreachable, refused)
	}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
	if o := observe(t, world(), every(forbidden)); o.Unreachable != nil || len(o.Notes) == 0 {
		t.Errorf("every read refused: Unreachable = %v, Notes = %q; want reached, with notes", o.Unreachable,
			o.Notes)
	}
	if o := observe(t, world(), interceptor.Funcs{}); o.Unreachable != nil {
		t.Errorf("a reachable cluster: Unreachable = %v", o.Unreachable)
	}
}

// TestObserveVPCCNI: the VPC CNI network policy agent's command line and
// the network policy controller switch are observed, nothing else of them.
func TestObserveVPCCNI(t *testing.T) {
	objs := append(world(),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "aws-node-abcde"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "aws-node", Image: "602401143452.dkr.ecr.us-east-1.amazonaws.com/amazon-k8s-cni:v1.19.0"},
				{Name: "aws-eks-nodeagent",
					Image:   "602401143452.dkr.ecr.us-east-1.amazonaws.com/amazon/aws-network-policy-agent:v1.1.5",
					Command: []string{"/controller"}, Args: []string{"--enable-network-policy=false"}},
			}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "amazon-vpc-cni"},
			Data: map[string]string{KeyVPCCNIPolicyController: " true ", "warm-ip-target": "5"}},
	)
	o := observe(t, objs, interceptor.Funcs{})
	if want := [][]string{{"/controller", "--enable-network-policy=false"}}; !reflect.DeepEqual(o.PolicyAgentArgs,
		want) {
		t.Errorf("PolicyAgentArgs = %q, want %q", o.PolicyAgentArgs, want)
	}
	if o.VPCCNIPolicyController != "true" {
		t.Errorf("VPCCNIPolicyController = %q", o.VPCCNIPolicyController)
	}
	if len(o.Notes) != 0 {
		t.Errorf("Notes = %q", o.Notes)
	}
}
