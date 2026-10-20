// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewapply

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/devthenet-labs/maul/cmd/maul/internal/previewsetup"
)

// clusterScoped are the kinds the recipes write with no namespace.
var clusterScoped = map[string]bool{"Namespace": true, "ClusterIssuer": true}

// TestObjectsGoldenTrees: every manifest and Secret file any recipe writes
// decodes into objects --apply can apply, each namespaced unless its kind
// is cluster-scoped, since the apply names no namespace of its own.
func TestObjectsGoldenTrees(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "previewsetup", "testdata", "golden", "*", "*", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range paths {
		dir := filepath.Base(filepath.Dir(p))
		if dir != "manifests" && dir != "secrets" {
			continue
		}
		data, err := os.ReadFile(p) //nolint:gosec // the golden trees
		if err != nil {
			t.Fatal(err)
		}
		objs, err := Objects(data)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		for _, o := range objs {
			n++
			if (o.GetNamespace() == "") != clusterScoped[o.GetKind()] {
				t.Errorf("%s: %s has namespace %q", p, o.GetKind(), o.GetNamespace())
			}
		}
	}
	if n < 20 {
		t.Errorf("only %d objects in the golden trees", n)
	}
}

// TestObjectsRefuses: a file that is no manifest is refused.
func TestObjectsRefuses(t *testing.T) {
	for name, data := range map[string]string{
		"empty":    "",
		"comments": "# nothing\n---\n",
		"no name":  "apiVersion: v1\nkind: Secret\nmetadata: {}\n",
		"no kind":  "apiVersion: v1\nmetadata: {name: x}\n",
		"not yaml": "{:",
	} {
		if _, err := Objects([]byte(data)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	objs, err := Objects([]byte("---\napiVersion: v1\nkind: Namespace\nmetadata: {name: a}\n---\n---\n" +
		"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: b, namespace: a}\n"))
	if err != nil || len(objs) != 2 {
		t.Fatalf("Objects = %v, %v", objs, err)
	}
}

func obj(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion(apiVersion)
	o.SetKind(kind)
	o.SetNamespace(namespace)
	o.SetName(name)
	return o
}

func TestDeleteCommand(t *testing.T) {
	for want, o := range map[string]*unstructured.Unstructured{
		"kubectl -n maul delete secret/s":                obj("v1", "Secret", "maul", "s"),
		"kubectl delete namespace/n":                     obj("v1", "Namespace", "", "n"),
		"kubectl delete clusterissuer.cert-manager.io/i": obj("cert-manager.io/v1", "ClusterIssuer", "", "i"),
		"kubectl -n x delete httproute.gateway.networking.k8s.io/r": obj("gateway.networking.k8s.io/v1",
			"HTTPRoute", "x", "r"),
	} {
		if got := deleteCommand(previewsetup.Connection{}, o); got != want {
			t.Errorf("deleteCommand = %q, want %q", got, want)
		}
	}
	// The caller's --kubeconfig and --context ride along, so a pasted undo
	// reaches the cluster the run did, not kubectl's current context.
	conn := previewsetup.Connection{Kubeconfig: "/home/me/kube config", Context: "staging"}
	want := "kubectl --kubeconfig '/home/me/kube config' --context staging -n maul delete secret/s"
	if got := deleteCommand(conn, obj("v1", "Secret", "maul", "s")); got != want {
		t.Errorf("deleteCommand = %q, want %q", got, want)
	}
}

// TestApplyError: a secret file's object never has the API server's
// message shown; any other object's error is shown whole.
func TestApplyError(t *testing.T) {
	const value = "SENTINEL-value"
	o := obj("v1", "Secret", "maul", "s")
	bad := apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, "s", nil)
	bad.ErrStatus.Message = "data: " + value
	if got := applyError(o, true, bad).Error(); strings.Contains(got, value) || !strings.Contains(got, "Invalid") {
		t.Errorf("secret: %q", got)
	}
	if got := applyError(o, false, bad).Error(); !strings.Contains(got, value) {
		t.Errorf("not a secret: %q", got)
	}
	transport := errors.New("dial tcp: connection refused")
	if got := applyError(o, true, transport); !errors.Is(got, transport) {
		t.Errorf("transport: %v", got)
	}
}
