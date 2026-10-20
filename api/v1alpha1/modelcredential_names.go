// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
)

// The credential store's naming contract (per-user credentials design, L3,
// revision 1). Everything is derived from the owner ("github-id:<id>") and
// the provider, so no name is ever chosen by a caller and no record can point
// at a Secret other than its own. The chart's admission policies repeat the
// patterns and the label (TestUserCredentialPatternsAreTheAPIs pins them).
const (
	// DefaultCredentialsNamespace is the namespace the person's credential
	// Secrets live in (chart egressBroker.userCredentials.namespace).
	DefaultCredentialsNamespace = "maul-credentials"

	// LabelCredentialOwner is on every credential Secret: its owner's hash
	// (ModelCredentialOwnerHash), which must equal the hash in the Secret's
	// own name. The broker checks it before using the Secret, so a Secret
	// renamed or copied into place under another person's name is refused.
	LabelCredentialOwner = "maul.run/credential-owner"

	// ModelCredentialSecretKey is the one data key of a credential Secret.
	ModelCredentialSecretKey = "credential"

	// ModelCredentialSecretMaxBytes bounds a stored credential. An Anthropic
	// API key or setup-token, or an OpenAI API key, is a few hundred bytes at
	// most. A multiple of
	// three, so the admission policy can bound the value exactly whether its
	// CEL sees bytes or their base64 text (4096 characters).
	ModelCredentialSecretMaxBytes = 3072

	// ModelCredentialNamePattern is the grammar of a ModelCredential's name.
	ModelCredentialNamePattern = `^[0-9a-f]{16}-(anthropic|openai)$`

	// ModelCredentialSecretNamePattern is the grammar of a credential
	// Secret's name.
	ModelCredentialSecretNamePattern = `^cred-[0-9a-f]{16}-(anthropic|openai)$`

	// ModelCredentialOwnerPrefix starts every owner value.
	ModelCredentialOwnerPrefix = "github-id:"

	// modelCredentialSecretPrefix starts every credential Secret's name.
	modelCredentialSecretPrefix = "cred-"

	// fingerprintMinBytes is the shortest credential whose last four
	// characters a fingerprint shows; a shorter one shows none.
	fingerprintMinBytes = 16
)

// ModelCredentialOwner is the owner value for a GitHub numeric id, the
// attributed-to annotation's person form.
func ModelCredentialOwner(githubID int64) string {
	return ModelCredentialOwnerPrefix + strconv.FormatInt(githubID, 10)
}

// ModelCredentialOwnerHash is the owner's 16-hex-digit hash: the first
// eight bytes of SHA-256 over the owner value, hex encoded. It names the
// owner's records and labels their Secrets without putting the GitHub id in
// a name or a selector.
func ModelCredentialOwnerHash(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:8])
}

// ModelCredentialName is the name of owner's ModelCredential for provider.
func ModelCredentialName(owner string, provider ModelCredentialProvider) string {
	return ModelCredentialOwnerHash(owner) + "-" + string(provider)
}

// ModelCredentialSecretName is the name of owner's credential Secret for
// provider, in the credentials namespace.
func ModelCredentialSecretName(owner string, provider ModelCredentialProvider) string {
	return modelCredentialSecretPrefix + ModelCredentialName(owner, provider)
}

// ModelCredentialFingerprint identifies a credential without revealing it:
// its last four characters, each outside [A-Za-z0-9_-] shown as '*' (all
// four as '*' when the credential is shorter than 16 bytes), then ':' and
// the first six bytes of its SHA-256, hex encoded.
func ModelCredentialFingerprint(credential []byte) string {
	tail := []byte("****")
	if len(credential) >= fingerprintMinBytes {
		for i, c := range credential[len(credential)-4:] {
			if fingerprintChar(c) {
				tail[i] = c
			}
		}
	}
	sum := sha256.Sum256(credential)
	return string(tail) + ":" + hex.EncodeToString(sum[:6])
}

func fingerprintChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// HealthFor is the credential's health for the Secret at resourceVersion
// current: the recorded Health when it was judged against that very version,
// Unknown otherwise (never probed, judged against an earlier credential, or
// no Secret at all). A health is never carried over to a credential nobody
// probed.
func (s ModelCredentialStatus) HealthFor(current string) ModelCredentialHealth {
	if current == "" || s.SecretVersion != current {
		return ModelCredentialUnknown
	}
	switch s.Health {
	case ModelCredentialValid, ModelCredentialRejected:
		return s.Health
	}
	return ModelCredentialUnknown
}

// ModelCredentialProviders are the providers a person may store a
// credential for, in display order.
var ModelCredentialProviders = []ModelCredentialProvider{
	ModelCredentialProviderAnthropic, ModelCredentialProviderOpenAI,
}

// ModelCredentialKinds are the kinds a credential for provider may be:
// Anthropic takes an API key or a setup-token, OpenAI an API key only
// (owner decision D1: no ChatGPT-plan sign-in, no Codex access tokens). nil
// for a provider that takes none.
func ModelCredentialKinds(provider ModelCredentialProvider) []ModelCredentialKind {
	switch provider {
	case ModelCredentialProviderAnthropic:
		return []ModelCredentialKind{ModelCredentialAPIKey, ModelCredentialOAuthToken}
	case ModelCredentialProviderOpenAI:
		return []ModelCredentialKind{ModelCredentialAPIKey}
	}
	return nil
}

// ModelCredentialKindAllowed reports whether provider takes a credential of
// kind.
func ModelCredentialKindAllowed(provider ModelCredentialProvider, kind ModelCredentialKind) bool {
	return slices.Contains(ModelCredentialKinds(provider), kind)
}

// Credential value prefixes the providers issue, used only to catch a
// credential stored under the wrong kind or provider before the provider
// rejects it.
const (
	anthropicPrefix           = "sk-ant-"
	anthropicAPIKeyPrefix     = "sk-ant-api"
	anthropicOAuthTokenPrefix = "sk-ant-oat"
	// openaiPrefix starts every OpenAI secret key (sk-, sk-proj-,
	// sk-svcacct-, sk-admin-); an Anthropic one starts with it too, so it
	// marks OpenAI only when anthropicPrefix does not follow.
	openaiPrefix = "sk-"
)

// ModelCredentialValue is the credential a person stores for provider, as
// the egress broker will use it: raw trimmed of surrounding whitespace, then
// 1 to ModelCredentialSecretMaxBytes of printable ASCII with no space (a
// single token: nothing in it can split or inject a header). ok is false for
// anything else; for a kind provider does not take (ModelCredentialKinds);
// for a value that carries the other kind's Anthropic prefix (a setup-token
// stored as an API key, or the reverse); and for a value that carries the
// other provider's prefix (an sk-ant- value stored as an OpenAI key, an
// OpenAI sk- value stored as an Anthropic credential), each of which the
// provider would only reject.
func ModelCredentialValue(raw string, provider ModelCredentialProvider, kind ModelCredentialKind) (
	value string, ok bool) {
	if len(raw) > ModelCredentialSecretMaxBytes || !ModelCredentialKindAllowed(provider, kind) {
		return "", false
	}
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x21 || c > 0x7e {
			return "", false
		}
	}
	anthropic := strings.HasPrefix(v, anthropicPrefix)
	switch {
	case provider == ModelCredentialProviderOpenAI:
		if anthropic {
			return "", false
		}
	case strings.HasPrefix(v, openaiPrefix) && !anthropic:
		return "", false
	case kind == ModelCredentialAPIKey && strings.HasPrefix(v, anthropicOAuthTokenPrefix),
		kind == ModelCredentialOAuthToken && strings.HasPrefix(v, anthropicAPIKeyPrefix):
		return "", false
	}
	return v, true
}
