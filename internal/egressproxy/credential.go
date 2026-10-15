// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// ErrCredentialUnavailable is a registry credential that is not there to
// use: never loaded or minted, or past its expiry. The registry route
// answers it 503 credential_unavailable; the other routes are unaffected.
var ErrCredentialUnavailable = errors.New("egress-proxy: registry credential unavailable")

// MaxSecretBytes bounds one credential value (a token, a username or a
// password).
const MaxSecretBytes = 8 << 10

// redacted is what every formatter prints for a Credential.
const redacted = "[redacted]"

// CredentialKind is what a credential is, for the audit line's credential
// attribute: never its value.
type CredentialKind string

// The credential kinds.
const (
	CredentialNone         CredentialKind = "none"
	CredentialStatic       CredentialKind = "static"
	CredentialCodeArtifact CredentialKind = "codeartifact"
)

// Credential is the login a registry route adds outbound. Its value is
// held in unexported fields and every way of printing or encoding it
// (fmt's verbs, slog, JSON, text) prints "[redacted]", so a credential
// that reaches a log line, an audit line or an error string shows nothing.
// The zero value is no credential (CredentialNone).
type Credential struct {
	kind CredentialKind
	// header is the Authorization value; "" sends none.
	header string
	// needles are the byte strings the echo scan refuses in a response.
	needles []string
	// expires is when the credential stops working; zero never.
	expires time.Time
}

// NoCredential is a registry without a login: no Authorization header.
func NoCredential() Credential { return Credential{kind: CredentialNone} }

// ValidateSecret judges one credential value read from a file or minted:
// non-empty, at most MaxSecretBytes, and free of control characters (CR,
// LF, NUL and the rest), which would split or end a header.
func ValidateSecret(field, v string) error {
	switch {
	case v == "":
		return fmt.Errorf("%s is empty", field)
	case len(v) > MaxSecretBytes:
		return fmt.Errorf("%s is over %d bytes", field, MaxSecretBytes)
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("%s holds a control character", field)
		}
	}
	return nil
}

// BearerCredential is "Authorization: Bearer <token>".
func BearerCredential(token string) (Credential, error) {
	if err := ValidateSecret("token", token); err != nil {
		return Credential{}, err
	}
	return Credential{kind: CredentialStatic, header: "Bearer " + token, needles: []string{token}}, nil
}

// BasicCredential is "Authorization: Basic base64(username:password)". The
// echo scan looks for the encoded pair and the password alone.
func BasicCredential(username, password string) (Credential, error) {
	if err := ValidateSecret("username", username); err != nil {
		return Credential{}, err
	}
	if strings.Contains(username, ":") {
		return Credential{}, errors.New("username holds a colon")
	}
	if err := ValidateSecret("password", password); err != nil {
		return Credential{}, err
	}
	pair := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return Credential{kind: CredentialStatic, header: "Basic " + pair, needles: []string{pair, password}}, nil
}

// CodeArtifactCredential is a minted CodeArtifact authorization token, in
// the form the ecosystem's client sends it: npm "Bearer <token>", pypi
// basic auth as user "aws". It stops working at expires.
func CodeArtifactCredential(ecosystem, token string, expires time.Time) (Credential, error) {
	if err := ValidateSecret("token", token); err != nil {
		return Credential{}, err
	}
	var c Credential
	switch ecosystem {
	case netprofile.EcosystemNPM:
		c = Credential{header: "Bearer " + token, needles: []string{token}}
	case netprofile.EcosystemPyPI:
		pair := base64.StdEncoding.EncodeToString([]byte("aws:" + token))
		c = Credential{header: "Basic " + pair, needles: []string{pair, token}}
	default:
		return Credential{}, fmt.Errorf("codeartifact serves npm and pypi, not %q", ecosystem)
	}
	c.kind, c.expires = CredentialCodeArtifact, expires
	return c, nil
}

// Kind is the credential's kind (CredentialNone for the zero value).
func (c Credential) Kind() CredentialKind {
	if c.kind == "" {
		return CredentialNone
	}
	return c.kind
}

// Authorization is the outbound Authorization header's value; "" sends
// none. It is the one accessor that returns the secret, for the request
// the proxy builds and nothing else.
func (c Credential) Authorization() string { return c.header }

// EchoNeedles are the byte strings a response must never contain: the
// secret, and for basic auth its encoded pair and the password alone. The
// slice is a copy.
func (c Credential) EchoNeedles() [][]byte {
	out := make([][]byte, len(c.needles))
	for i, n := range c.needles {
		out[i] = []byte(n)
	}
	return out
}

// ExpiresAt is when the credential stops working; the zero time for a
// credential that does not expire.
func (c Credential) ExpiresAt() time.Time { return c.expires }

// Usable reports whether the credential may be sent at now: a static or
// absent credential always, a minted one before its expiry.
func (c Credential) Usable(now time.Time) bool {
	return c.expires.IsZero() || now.Before(c.expires)
}

// SameValue reports whether c and o send the same header.
func (c Credential) SameValue(o Credential) bool { return c.header == o.header && c.kind == o.kind }

// String prints "[redacted]", as GoString, Format, LogValue, MarshalJSON
// and MarshalText do: the value never leaves through a formatter.
func (Credential) String() string { return redacted }

// GoString is %#v.
func (Credential) GoString() string { return redacted }

// Format covers every fmt verb and flag.
func (Credential) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue is slog's view.
func (Credential) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON is encoding/json's view.
func (Credential) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText is every text encoder's view.
func (Credential) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// CredentialSource hands a registry route its credential. Get returns
// ErrCredentialUnavailable (wrapped) when there is none to use; Invalidate
// asks the source to re-read or re-mint after the registry answered 401,
// rate-limited by the source.
type CredentialSource interface {
	Get(registry string) (Credential, error)
	Invalidate(registry string)
}
