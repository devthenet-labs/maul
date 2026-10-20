// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"os"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// The fake client cannot show that a real API server returns the
// cluster-scoped Nodes and ClusterRoleBindings preview-nodes and
// preview-viewers list, or that an operator without the cluster-wide reads
// gets a SKIP rather than a FAIL from RBAC's refusal. These run both
// against envtest's kube-apiserver, with its RBAC authorizer.

// startInstallEnv boots envtest with the CRDs, seeds the install world's
// settings, nodes and viewer ClusterRole into it, and returns the admin
// client and config.
func startInstallEnv(t *testing.T) (client.Client, *rest.Config) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping check previews envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../../deploy/kustomize/base/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	ctx := t.Context()
	mustCreate(t, admin, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "maul"}})
	for _, o := range newInstallWorld().objs {
		switch o.(type) {
		case *corev1.ConfigMap, *corev1.Node:
			mustCreate(t, admin, o)
		}
	}
	mustCreate(t, admin, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "maul-preview-viewer"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{"maul.run"}, Resources: []string{"projects/previews"},
			Verbs: []string{"get"}}}})
	var nodes corev1.NodeList
	if err := admin.List(ctx, &nodes); err != nil || len(nodes.Items) != 2 {
		t.Fatalf("seeded nodes = %d, %v", len(nodes.Items), err)
	}
	return admin, cfg
}

func mustCreate(t *testing.T, c client.Client, o client.Object) {
	t.Helper()
	if err := c.Create(t.Context(), o); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %T %s: %v", o, o.GetName(), err)
	}
}

// installOn runs the install-level checks with reader as the operator's
// client, the network seams answering as the install world does.
func installOn(reader client.Reader) Report {
	w := newInstallWorld()
	dial := func(context.Context, string) (tls.ConnectionState, error) {
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{{DNSNames: []string{"*." + iSuffix},
			Issuer: pkix.Name{CommonName: "Test CA"}, NotAfter: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}}}, nil
	}
	return Install(context.Background(), Config{Reader: reader, Namespace: "maul", Resolver: w.resolver,
		DialTLS: dial, HTTP: w.web})
}

// TestInstallEnvtestClusterScoped: an operator who may read the whole
// cluster gets preview-nodes from the real Nodes (the isolated one passes,
// the plain worker is no preview node) and preview-viewers from a real
// ClusterRoleBinding, which the API server lists cluster-wide.
func TestInstallEnvtestClusterScoped(t *testing.T) {
	admin, _ := startInstallEnv(t)
	mustCreate(t, admin, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "maul-preview-viewers"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "maul-preview-viewer"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "github:acme:qa"},
			{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: "maul"}},
	})
	r := installOn(admin)
	expect(t, r.Checks, CheckPreviewNodes, checkreport.Pass, "node preview-1 carries", iTaint+"=true:NoExecute",
		"no other node carries either")
	expect(t, r.Checks, CheckPreviewViewers, checkreport.Pass, "ClusterRoleBinding maul-preview-viewers binds",
		"1 team and 0 users", "ServiceAccount default never signs in")

	// A node with the selector but not the taint is found on the real list.
	mustCreate(t, admin, node("preview-2", map[string]string{iNodeLabel: "true"}))
	r = installOn(admin)
	expect(t, r.Checks, CheckPreviewNodes, checkreport.Fail, "node preview-2 matches the node selector without "+
		"the taint")
}

// TestInstallEnvtestForbidden: an operator who may read the release
// namespace but neither Nodes nor ClusterRoleBindings gets SKIPs from RBAC's
// refusal, never a FAIL; a RoleBinding in the namespace still passes
// preview-viewers.
func TestInstallEnvtestForbidden(t *testing.T) {
	admin, cfg := startInstallEnv(t)
	ctx := t.Context()
	const user = "namespace-operator"
	mustCreate(t, admin, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "maul-preview-viewers"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "maul-preview-viewer"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "github:acme:qa"}},
	})
	reads := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps", "services"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"rolebindings"},
			Verbs: []string{"get", "list"}},
		{APIGroups: []string{"maul.run"}, Resources: []string{"previews"}, Verbs: []string{"get", "list"}},
	}
	mustCreate(t, admin, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "maul"},
		Rules: reads})
	mustCreate(t, admin, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "maul"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "operator"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}})
	rbactest.WaitAllowed(t, admin,
		rbactest.Review(user, nil, authorizationv1.ResourceAttributes{Namespace: "maul", Verb: "list",
			Resource: "configmaps"}),
		rbactest.Review(user, nil, authorizationv1.ResourceAttributes{Namespace: "maul", Verb: "list",
			Group: "rbac.authorization.k8s.io", Resource: "rolebindings"}))
	userCfg := rest.CopyConfig(cfg)
	userCfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	operator, err := client.New(userCfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("build operator client: %v", err)
	}
	var crbs rbacv1.ClusterRoleBindingList
	if err := operator.List(ctx, &crbs); !apierrors.IsForbidden(err) {
		t.Fatalf("operator listing ClusterRoleBindings = %v, want Forbidden", err)
	}

	r := installOn(operator)
	expect(t, r.Checks, CheckPreviewConfig, checkreport.Pass)
	expect(t, r.Checks, CheckPreviewNodes, checkreport.Skip, "cannot list the nodes", "forbidden")
	expect(t, r.Checks, CheckPreviewViewers, checkreport.Skip, "not every binding could be listed", "forbidden")

	// The namespace's own RoleBinding is readable, so it passes alone.
	mustCreate(t, admin, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "maul", Name: "maul-preview-viewers"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "maul-preview-viewer"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "github:octocat"}},
	})
	r = installOn(operator)
	expect(t, r.Checks, CheckPreviewViewers, checkreport.Pass, "RoleBinding maul-preview-viewers binds",
		"0 teams and 1 user")
	expect(t, r.Checks, CheckPreviewNodes, checkreport.Skip, "cannot list the nodes")
}
