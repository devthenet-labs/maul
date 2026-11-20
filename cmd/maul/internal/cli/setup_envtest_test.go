// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// TestSetupGitHubAppForgeFileGrantsOneSecret applies the forge file `setup
// github-app --intents` writes to a real API server: the CRD admits the
// Forge, and RBAC lets intent-controller's ServiceAccount get exactly the
// App's Secret, neither another Secret nor a list of them.
func TestSetupGitHubAppForgeFileGrantsOneSecret(t *testing.T) {
	opts, _, _, c := startGetEnv(t)
	g := newFakeAppGitHub(t)
	dir := t.TempDir()
	var started string
	if _, stderr, err := execSetup(t, g.deps(g.browser(false, &started), nil), "setup", "github-app", "--org",
		"acme", "--intents", "-n", testNamespace, "-o", filepath.Join(dir, "s.yaml"), "--timeout", "20s"); err != nil {
		t.Fatalf("setup: %v\n%s", err, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "maul-github-acme.forge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(raw), "---\n") {
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(t.Context(), u); err != nil {
			t.Fatalf("apply %s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}
	var forge v1alpha1.Forge
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "acme"}, &forge); err != nil {
		t.Fatalf("the Forge was not admitted: %v", err)
	}
	if forge.Spec.SecretRef.Name != "maul-github-acme" || len(forge.Spec.Orgs) != 1 || forge.Spec.Orgs[0] != "acme" {
		t.Errorf("the Forge is %+v", forge.Spec)
	}

	sa := "system:serviceaccount:" + testNamespace + ":maul-intent-controller"
	groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + testNamespace}
	secret := authorizationv1.ResourceAttributes{Namespace: testNamespace, Verb: "get", Resource: "secrets",
		Name: "maul-github-acme"}
	// The grant first: a denial checked before the authorizer sees it
	// would prove nothing.
	rbactest.WaitAllowed(t, c, rbactest.Review(sa, groups, secret))

	cs, err := kubernetes.NewForConfig(opts.env.Config)
	if err != nil {
		t.Fatal(err)
	}
	other, list := secret, secret
	other.Name = "maul-github"
	list.Verb, list.Name = "list", ""
	for name, attrs := range map[string]authorizationv1.ResourceAttributes{"another Secret": other,
		"list Secrets": list} {
		sar, err := cs.AuthorizationV1().SubjectAccessReviews().Create(t.Context(),
			&authorizationv1.SubjectAccessReview{Spec: rbactest.Review(sa, groups, attrs)}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if sar.Status.Allowed {
			t.Errorf("%s: the ServiceAccount is allowed: %s", name, sar.Status.Reason)
		}
	}
}

// TestSetupProjectManifestsAdmitted applies the Project manifests `setup
// project` writes (its goldens, and one written by the command) to a real
// API server, whose CRD admits each. A name the CRD refuses is refused by
// the command first, before any file is written.
func TestSetupProjectManifestsAdmitted(t *testing.T) {
	_, _, _, c := startGetEnv(t)
	dir := t.TempDir()
	written := filepath.Join(dir, "web.project.yaml")
	if _, stderr, err := execSetup(t, nil, "setup", "project", "web", "--intent-repo", "acme/intents", "--repo",
		"api=https://ghe.example.com/acme/backend", "--approver", "alice", "--approve-label", "ok", "-n",
		testNamespace, "-o", written); err != nil {
		t.Fatalf("setup project: %v\n%s", err, stderr)
	}
	golden := filepath.Join("..", "projectsetup", "testdata")
	for _, path := range []string{filepath.Join(golden, "project-one.yaml"),
		filepath.Join(golden, "project-multi.yaml"), written} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(raw, &u.Object); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(t.Context(), u); err != nil {
			t.Errorf("%s was not admitted: %v", path, err)
		}
	}

	long := strings.Repeat("a", 26)
	_, _, err := execSetup(t, nil, "setup", "project", long, "--intent-repo", "acme/intents", "--repo", "acme/shop",
		"--approver", "alice", "-o", filepath.Join(dir, "long.yaml"))
	if err == nil || !isUsage(err) {
		t.Fatalf("a 26-character name = %v, want a usage error", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "long.yaml")); !os.IsNotExist(err) {
		t.Errorf("a refused name wrote a file: %v", err)
	}
	// The CRD refuses it too: the command holds the schema's own rule.
	p := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: long, Namespace: testNamespace},
		Spec: v1alpha1.ProjectSpec{IntentRepository: "https://github.com/acme/intents",
			Approvers:    v1alpha1.ProjectApprovers{Logins: []string{"alice"}},
			Repositories: []v1alpha1.ProjectRepository{{Name: "shop", URL: "https://github.com/acme/shop"}}}}
	if err := c.Create(t.Context(), p); err == nil {
		t.Error("the API server admitted a 26-character Project name")
	}
}
