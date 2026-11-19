// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghapp

import (
	"errors"
	"fmt"

	"sigs.k8s.io/yaml"
)

// DefaultIntentControllerSA is intent-controller's ServiceAccount in a
// release named maul, and in the kustomize component.
const DefaultIntentControllerSA = "maul-intent-controller"

// secretReaderPrefix names the Role and RoleBinding that let
// intent-controller read one Forge Secret.
const secretReaderPrefix = "maul-forge-secret-"

// SecretReaderName is the name of the Role and RoleBinding
// SecretReaderManifests renders for secretName.
func SecretReaderName(secretName string) string {
	return secretReaderPrefix + secretName
}

// forgeManifest is a maul.run/v1alpha1 Forge with only the fields this
// command sets: no status, no creationTimestamp, defaults left to the CRD.
type forgeManifest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   secretMetadata  `json:"metadata"`
	Spec       forgeSpecFields `json:"spec"`
}

type forgeSpecFields struct {
	Provider  string        `json:"provider"`
	SecretRef secretRefName `json:"secretRef"`
	Orgs      []string      `json:"orgs"`
}

type secretRefName struct {
	Name string `json:"name"`
}

// ForgeManifest renders the Forge name in namespace that covers org's
// repositories with the GitHub App credentials in Secret secretName.
func ForgeManifest(name, namespace, secretName, org string) ([]byte, error) {
	if name == "" || namespace == "" || secretName == "" || org == "" {
		return nil, errors.New("render the Forge: its name, namespace, Secret and organization are all required")
	}
	out, err := yaml.Marshal(forgeManifest{
		APIVersion: "maul.run/v1alpha1",
		Kind:       "Forge",
		Metadata:   secretMetadata{Name: name, Namespace: namespace},
		Spec: forgeSpecFields{Provider: "github", SecretRef: secretRefName{Name: secretName},
			Orgs: []string{org}},
	})
	if err != nil {
		return nil, fmt.Errorf("render the Forge: %w", err)
	}
	return out, nil
}

// roleManifest is an rbac.authorization.k8s.io/v1 Role.
type roleManifest struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   secretMetadata `json:"metadata"`
	Rules      []policyRule   `json:"rules"`
}

type policyRule struct {
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	ResourceNames []string `json:"resourceNames"`
	Verbs         []string `json:"verbs"`
}

// roleBindingManifest is an rbac.authorization.k8s.io/v1 RoleBinding.
type roleBindingManifest struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   secretMetadata `json:"metadata"`
	RoleRef    roleRef        `json:"roleRef"`
	Subjects   []subject      `json:"subjects"`
}

type roleRef struct {
	APIGroup string `json:"apiGroup"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

type subject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// SecretReaderManifests renders a Role and RoleBinding, both named
// SecretReaderName(secretName), that let ServiceAccount serviceAccount in
// namespace get exactly the Secret secretName and nothing else. RBAC rules
// add up across Roles, so this grants intent-controller the read beside
// whatever the chart granted. An empty secretName is refused: a rule with
// no resourceNames would grant every Secret in the namespace.
func SecretReaderManifests(secretName, namespace, serviceAccount string) ([]byte, error) {
	if secretName == "" || namespace == "" || serviceAccount == "" {
		return nil, errors.New("render the Secret reader Role: its Secret, namespace and ServiceAccount are all " +
			"required")
	}
	name := SecretReaderName(secretName)
	meta := secretMetadata{Name: name, Namespace: namespace}
	role, err := yaml.Marshal(roleManifest{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "Role",
		Metadata:   meta,
		Rules: []policyRule{{APIGroups: []string{""}, Resources: []string{"secrets"},
			ResourceNames: []string{secretName}, Verbs: []string{"get"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("render the Secret reader Role: %w", err)
	}
	binding, err := yaml.Marshal(roleBindingManifest{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "RoleBinding",
		Metadata:   meta,
		RoleRef:    roleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
		Subjects:   []subject{{Kind: "ServiceAccount", Name: serviceAccount, Namespace: namespace}},
	})
	if err != nil {
		return nil, fmt.Errorf("render the Secret reader RoleBinding: %w", err)
	}
	return joinDocuments(role, binding), nil
}

// joinDocuments joins YAML documents into one multi-document stream.
func joinDocuments(docs ...[]byte) []byte {
	var out []byte
	for i, d := range docs {
		if i > 0 {
			out = append(out, "---\n"...)
		}
		out = append(out, d...)
	}
	return out
}

// ForgeFile is the forge file `setup github-app` writes: the Forge, then,
// with a ServiceAccount, the Role and RoleBinding letting it read the
// Forge's Secret. It holds no secret.
func ForgeFile(forgeName, namespace, secretName, org, serviceAccount string) ([]byte, error) {
	forge, err := ForgeManifest(forgeName, namespace, secretName, org)
	if err != nil || serviceAccount == "" {
		return forge, err
	}
	reader, err := SecretReaderManifests(secretName, namespace, serviceAccount)
	if err != nil {
		return nil, err
	}
	return joinDocuments(forge, reader), nil
}
