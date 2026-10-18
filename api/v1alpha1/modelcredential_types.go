// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelCredentialProvider is the model provider a person's credential is
// for. Version 1 carries Anthropic only: Bedrock, Vertex and Foundry Entra
// authenticate with the broker's one cloud identity, so they get attribution
// and usage but no per-person credential.
// +kubebuilder:validation:Enum=anthropic
type ModelCredentialProvider string

// Model credential providers.
const (
	// ModelCredentialProviderAnthropic is the Anthropic API.
	ModelCredentialProviderAnthropic ModelCredentialProvider = "anthropic"
)

// ModelCredentialKind is how a stored credential authenticates.
// +kubebuilder:validation:Enum=api-key;oauth-token
type ModelCredentialKind string

// Model credential kinds.
const (
	// ModelCredentialAPIKey is an Anthropic API key, sent as x-api-key.
	ModelCredentialAPIKey ModelCredentialKind = "api-key"
	// ModelCredentialOAuthToken is a long-lived OAuth token minted with
	// `claude setup-token`, sent as an Authorization bearer. It is a Claude
	// subscription's, so it must be its owner's own.
	ModelCredentialOAuthToken ModelCredentialKind = "oauth-token"
)

// ModelCredentialHealth is what the egress broker last learned of a
// credential from a probe it issued itself (never from agent traffic).
// +kubebuilder:validation:Enum=Valid;Rejected;Unknown
type ModelCredentialHealth string

// Model credential health values. An empty health is Unknown.
const (
	// ModelCredentialValid: the provider accepted the credential.
	ModelCredentialValid ModelCredentialHealth = "Valid"
	// ModelCredentialRejected: the provider refused the credential (a 401
	// to the broker's probe of the Secret version still current).
	ModelCredentialRejected ModelCredentialHealth = "Rejected"
	// ModelCredentialUnknown: not yet probed since the Secret was last
	// written.
	ModelCredentialUnknown ModelCredentialHealth = "Unknown"
)

// ModelCredentialSpec is whose credential this is and for what. There is no
// Secret pointer: the Secret's name is derived from the owner and provider
// (ModelCredentialSecretName), so nobody can aim a credential at a Secret
// of their choosing.
// +kubebuilder:validation:XValidation:rule="self.owner == oldSelf.owner",message="spec.owner is immutable"
// +kubebuilder:validation:XValidation:rule="self.provider == oldSelf.provider",message="spec.provider is immutable"
type ModelCredentialSpec struct {
	// Owner is the person the credential belongs to and pays for, in the
	// attributed-to grammar: "github-id:<numeric id>". A run attributed to
	// that value is the only one that may use it.
	// +kubebuilder:validation:Pattern=`^github-id:[1-9][0-9]{0,18}$`
	// +kubebuilder:validation:MaxLength=29
	Owner string `json:"owner"`
	// OwnerLogin is the owner's GitHub login when the credential was last
	// written, for display only (the id is the identity).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	OwnerLogin string `json:"ownerLogin,omitempty"`
	// Provider the credential is for.
	Provider ModelCredentialProvider `json:"provider"`
	// Kind is how it authenticates; replacing an API key with a setup-token
	// (or back) changes it.
	Kind ModelCredentialKind `json:"kind"`
}

// ModelCredentialStatus holds the non-secret facts about the stored
// credential. The status server writes the fingerprint (and resets health
// to Unknown) when it writes the Secret; the egress broker writes health and
// the timestamps. The chart's admission policy holds each to its own fields.
type ModelCredentialStatus struct {
	// Fingerprint identifies the stored credential without revealing it:
	// its last four characters and a SHA-256 prefix
	// (ModelCredentialFingerprint).
	// +optional
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_*-]{4}:[0-9a-f]{12}$`
	Fingerprint string `json:"fingerprint,omitempty"`
	// Health is what the broker's last probe found; empty is Unknown.
	// +optional
	Health ModelCredentialHealth `json:"health,omitempty"`
	// LastUsedAt is when the broker last attached the credential to a
	// request.
	// +optional
	LastUsedAt *metav1.Time `json:"lastUsedAt,omitempty"`
	// LastRejectedAt is when the provider last refused it.
	// +optional
	LastRejectedAt *metav1.Time `json:"lastRejectedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcred
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Login",type=string,JSONPath=`.spec.ownerLogin`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
// +kubebuilder:printcolumn:name="Fingerprint",type=string,JSONPath=`.status.fingerprint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[0-9a-f]{16}-[a-z]+$') && self.metadata.name.substring(17) == self.spec.provider",message="a ModelCredential is named <sha256(spec.owner)[:16]>-<spec.provider>"

// ModelCredential is the non-secret record of one person's own model
// credential for one provider (per-user credentials design, L3). The
// credential itself is a Secret in the credentials namespace, named
// ModelCredentialSecretName and labelled with the owner hash; nobody reads
// it back but the egress broker. Both names derive from the owner and the
// provider, so there is at most one per (person, provider). It is outside the
// patchy category on purpose: whose credential exists is visible only to
// its owner and to admins.
type ModelCredential struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ModelCredentialSpec `json:"spec"`
	// +optional
	Status ModelCredentialStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelCredentialList contains a list of ModelCredential.
type ModelCredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelCredential `json:"items"`
}
