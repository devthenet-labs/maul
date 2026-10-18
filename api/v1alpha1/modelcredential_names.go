// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// The credential store's naming contract (per-user credentials design, L3,
// revision 1). Everything is derived from the owner ("github-id:<id>") and
// the provider, so no name is ever chosen by a caller and no record can point
// at a Secret other than its own. The chart's admission policies repeat the
// patterns and the label (TestUserCredentialPatternsAreTheAPIs pins them).
const (
	// DefaultCredentialsNamespace is the namespace the person's credential
	// Secrets live in (chart egressBroker.userCredentials.namespace).
	DefaultCredentialsNamespace = "patchy-credentials"

	// LabelCredentialOwner is on every credential Secret: its owner's hash
	// (ModelCredentialOwnerHash), which must equal the hash in the Secret's
	// own name. The broker checks it before using the Secret, so a Secret
	// renamed or copied into place under another person's name is refused.
	LabelCredentialOwner = "patchy.bitwisemedia.uk/credential-owner"

	// ModelCredentialSecretKey is the one data key of a credential Secret.
	ModelCredentialSecretKey = "credential"

	// ModelCredentialSecretMaxBytes bounds a stored credential. An Anthropic
	// API key or setup-token is a little over a hundred bytes. A multiple of
	// three, so the admission policy can bound the value exactly whether its
	// CEL sees bytes or their base64 text (4096 characters).
	ModelCredentialSecretMaxBytes = 3072

	// ModelCredentialNamePattern is the grammar of a ModelCredential's name.
	ModelCredentialNamePattern = `^[0-9a-f]{16}-(anthropic)$`

	// ModelCredentialSecretNamePattern is the grammar of a credential
	// Secret's name.
	ModelCredentialSecretNamePattern = `^cred-[0-9a-f]{16}-(anthropic)$`

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
