// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestUserCredentialPatternsAreTheAPIs: the credential Secret policy holds
// every Secret to v1alpha1's naming contract (the name grammar, the owner
// label, the data key and its bound), which the broker and the status
// server derive their names from, so neither side can change alone: a wider
// chart grammar would admit Secrets no one looks up, a wider Go one would
// have the status server's writes refused.
func TestUserCredentialPatternsAreTheAPIs(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	var policy *unstructured.Unstructured
	for _, u := range renderObjects(t, userCredentialArgs) {
		if u.GetKind() == "ValidatingAdmissionPolicy" && u.GetName() == "patchy-credential-secrets" {
			policy = u
		}
	}
	if policy == nil {
		t.Fatal("the user-credentials render has no patchy-credential-secrets policy")
	}
	validations, _, _ := unstructured.NestedSlice(policy.Object, "spec", "validations")
	var exprs strings.Builder
	for _, v := range validations {
		expr, _ := v.(map[string]any)["expression"].(string)
		exprs.WriteString(expr + "\n")
	}
	all := exprs.String()
	matches := matchesArg.FindAllStringSubmatch(all, -1)
	grammars := make([]string, 0, len(matches))
	for _, m := range matches {
		grammars = append(grammars, m[1])
	}
	if len(grammars) != 1 || grammars[0] != v1alpha1.ModelCredentialSecretNamePattern {
		t.Errorf("the Secret policy's name grammars are %q, want only v1alpha1.ModelCredentialSecretNamePattern %q",
			grammars, v1alpha1.ModelCredentialSecretNamePattern)
	}
	// The owner hash the label must equal is the 16 characters after "cred-".
	hash := v1alpha1.ModelCredentialOwnerHash(v1alpha1.ModelCredentialOwner(1))
	name := v1alpha1.ModelCredentialSecretName(v1alpha1.ModelCredentialOwner(1), v1alpha1.ModelCredentialProviderAnthropic)
	if name[5:21] != hash || !strings.Contains(all, "object.metadata.name.substring(5, 21)") {
		t.Errorf("the Secret policy does not compare the owner label with name[5:21] (%q in %q)", hash, name)
	}
	maxBase64 := v1alpha1.ModelCredentialSecretMaxBytes / 3 * 4
	for _, want := range []string{
		"[?'" + v1alpha1.LabelCredentialOwner + "']",
		"'" + v1alpha1.ModelCredentialSecretKey + "' in variables.data",
		"== bytes ? " + strconv.Itoa(v1alpha1.ModelCredentialSecretMaxBytes) + " : " + strconv.Itoa(maxBase64) + ")",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the Secret policy lacks %q:\n%s", want, all)
		}
	}
	if v1alpha1.ModelCredentialSecretMaxBytes%3 != 0 {
		t.Errorf("ModelCredentialSecretMaxBytes %d is not a multiple of three: its base64 bound is not exact",
			v1alpha1.ModelCredentialSecretMaxBytes)
	}
}
