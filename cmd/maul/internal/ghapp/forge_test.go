// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestForgeFileGolden pins the forge file byte for byte: the Forge alone,
// and with the Secret reader Role and RoleBinding. An organization login
// that reads as a number stays a string.
func TestForgeFileGolden(t *testing.T) {
	for _, tt := range []struct {
		name, forge, secret, org, sa string
	}{
		{"forge", "acme", "maul-github-acme", "Acme", ""},
		{"forge-reader", "acme", "maul-github-acme", "Acme", DefaultIntentControllerSA},
		{"forge-numeric-org", "123", "maul-github-123", "123", "custom-intent-controller"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ForgeFile(tt.forge, "maul", tt.secret, tt.org, tt.sa)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", tt.name+".yaml")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s drifted:\n%s\nwant\n%s", path, got, want)
			}
		})
	}
}

// TestForgeFileParses: every document of the forge file reads strictly as
// the type it names, the Forge holds exactly the org and Secret, and the
// Role grants get on exactly the one Secret to exactly the one subject.
func TestForgeFileParses(t *testing.T) {
	raw, err := ForgeFile("acme", "maul", "maul-github-acme", "Acme", "maul-intent-controller")
	if err != nil {
		t.Fatal(err)
	}
	docs := strings.Split(string(raw), "---\n")
	if len(docs) != 3 {
		t.Fatalf("%d documents, want 3:\n%s", len(docs), raw)
	}
	var forge v1alpha1.Forge
	if err := yaml.UnmarshalStrict([]byte(docs[0]), &forge); err != nil {
		t.Fatal(err)
	}
	if forge.Name != "acme" || forge.Namespace != "maul" || forge.Spec.Provider != v1alpha1.ForgeProviderGitHub ||
		forge.Spec.SecretRef.Name != "maul-github-acme" || !slices.Equal(forge.Spec.Orgs, []string{"Acme"}) ||
		len(forge.Spec.Repositories) != 0 {
		t.Errorf("the Forge is %+v", forge)
	}
	var role rbacv1.Role
	if err := yaml.UnmarshalStrict([]byte(docs[1]), &role); err != nil {
		t.Fatal(err)
	}
	want := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"},
		ResourceNames: []string{"maul-github-acme"}, Verbs: []string{"get"}}}
	if role.Kind != "Role" || role.Name != "maul-forge-secret-maul-github-acme" || role.Namespace != "maul" ||
		!reflect.DeepEqual(role.Rules, want) {
		t.Errorf("the Role is %+v", role)
	}
	var binding rbacv1.RoleBinding
	if err := yaml.UnmarshalStrict([]byte(docs[2]), &binding); err != nil {
		t.Fatal(err)
	}
	wantRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}
	wantSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "maul-intent-controller", Namespace: "maul"}}
	if binding.Kind != "RoleBinding" || binding.Name != role.Name || binding.RoleRef != wantRef ||
		!reflect.DeepEqual(binding.Subjects, wantSubjects) {
		t.Errorf("the RoleBinding is %+v", binding)
	}
}

// TestForgeRenderersRefuseEmpty: an empty value is an error, never a
// manifest; above all an empty Secret name, which as empty resourceNames
// would grant every Secret in the namespace.
func TestForgeRenderersRefuseEmpty(t *testing.T) {
	for _, tt := range []struct {
		name   string
		render func() ([]byte, error)
	}{
		{"forge name", func() ([]byte, error) { return ForgeManifest("", "maul", "s", "acme") }},
		{"forge namespace", func() ([]byte, error) { return ForgeManifest("acme", "", "s", "acme") }},
		{"forge secret", func() ([]byte, error) { return ForgeManifest("acme", "maul", "", "acme") }},
		{"forge org", func() ([]byte, error) { return ForgeManifest("acme", "maul", "s", "") }},
		{"reader secret", func() ([]byte, error) { return SecretReaderManifests("", "maul", "sa") }},
		{"reader namespace", func() ([]byte, error) { return SecretReaderManifests("s", "", "sa") }},
		{"reader service account", func() ([]byte, error) { return SecretReaderManifests("s", "maul", "") }},
		{"file secret", func() ([]byte, error) { return ForgeFile("acme", "maul", "", "acme", "sa") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if out, err := tt.render(); err == nil || out != nil {
				t.Errorf("rendered %q, err %v; want an error and nothing", out, err)
			}
		})
	}
}
