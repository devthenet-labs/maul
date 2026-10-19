// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/ghapp"
)

// The Dex client secret's two Secrets (design §4.5): one Dex reads in its
// own namespace (as the environment variable its static client's
// secretEnv names), one the relay reads in the release namespace
// (previewAuth.dex.existingSecret).
const (
	// RelayDexSecret is previewAuth.dex.existingSecret, in the release
	// namespace, holding the secret under RelayDexSecretKey
	// (previewAuth.dex.clientSecretKey's default).
	RelayDexSecret    = "patchy-preview-auth-dex"
	RelayDexSecretKey = "clientSecret"
	// DexClientSecret is the Secret in Dex's namespace, holding the same
	// secret under DexClientSecretKey, which Dex reads into DexSecretEnv.
	DexClientSecret    = "patchy-preview-auth-client"
	DexClientSecretKey = "clientSecret"
	DexSecretEnv       = "PATCHY_PREVIEW_AUTH_CLIENT_SECRET"

	// The two files, under secrets/ in the output directory.
	RelayDexSecretFile  = "secrets/" + RelayDexSecret + ".secret.yaml"
	DexClientSecretFile = "secrets/dex-" + DexClientSecret + ".secret.yaml"

	// clientSecretBytes is the client secret's entropy, from crypto/rand.
	clientSecretBytes = 32
)

// ClientSecret is the relay's Dex client secret. Its formatting is
// redacted, so it can be logged or wrapped into an error by mistake
// without leaking: it leaves the process only through a Secret manifest.
type ClientSecret struct{ value string }

const redacted = "[REDACTED]"

func (ClientSecret) String() string               { return redacted }
func (ClientSecret) GoString() string             { return redacted }
func (ClientSecret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (ClientSecret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (ClientSecret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// IsZero reports a secret never drawn or read.
func (c ClientSecret) IsZero() bool { return c.value == "" }

func (c ClientSecret) equal(o ClientSecret) bool { return c.value == o.value }
func (c ClientSecret) bytes() []byte             { return []byte(c.value) }

// NewClientSecret draws a client secret: 32 bytes from r (crypto/rand's
// Reader outside tests), base64url without padding.
func NewClientSecret(r io.Reader) (ClientSecret, error) {
	b := make([]byte, clientSecretBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return ClientSecret{}, fmt.Errorf("draw the Dex client secret: %w", err)
	}
	return ClientSecret{value: base64.RawURLEncoding.EncodeToString(b)}, nil
}

// Secrets are the secrets a plan renders into Secret manifests.
type Secrets struct {
	DexClient ClientSecret
	// Fresh is set when DexClient was drawn by this run, not read back.
	Fresh bool
	// DNS is the DNS-01 solver's credential, zero when the operator gave
	// none (the Secret is then theirs to create, as NEXT-STEPS.md says).
	DNS Credential
	// Tunnel is the cloudflare-tunnel recipe's tunnel credentials, which
	// that recipe cannot render without.
	Tunnel TunnelCredentials
	// TailscaleOAuth is the Tailscale operator's OAuth client, zero when
	// the operator gave none (its Secret is then theirs to create).
	TailscaleOAuth TailscaleOAuth
}

// RotateDexClientSecret names the secret a run draws anew although one is on disk.
const RotateDexClientSecret = "dex-client-secret"

// ErrSecretsDisagree reports two Secret files holding different client
// secrets: one was edited or rotated by hand, and which is live is not
// the wizard's guess.
var ErrSecretsDisagree = errors.New("the two Dex client secret files hold different secrets")

// ResolveSecrets keeps the Dex client secret stable across re-runs (design
// §4.1): a secret file already under dir is read back, never regenerated;
// with one of the two files missing, the other's secret is written again.
// Only rotate draws a new one while a file exists. A file that cannot be
// read back is an error, never silently replaced.
func ResolveSecrets(dir string, rotate bool, rand io.Reader) (Secrets, error) {
	if rotate {
		s, err := NewClientSecret(rand)
		return Secrets{DexClient: s, Fresh: true}, err
	}
	var found []ClientSecret
	var from []string
	for _, f := range []struct{ path, key string }{
		{RelayDexSecretFile, RelayDexSecretKey},
		{DexClientSecretFile, DexClientSecretKey},
	} {
		v, ok, err := readSecretKey(filepath.Join(dir, filepath.FromSlash(f.path)), f.key,
			"--rotate "+RotateDexClientSecret)
		if err != nil {
			return Secrets{}, err
		}
		if ok {
			found = append(found, ClientSecret{value: string(v)})
			from = append(from, f.path)
		}
	}
	switch {
	case len(found) == 2 && !found[0].equal(found[1]):
		return Secrets{}, fmt.Errorf("%w (%s, %s): delete the stale one, or pass --rotate %s to draw a new "+
			"secret and register it at Dex again", ErrSecretsDisagree, from[0], from[1], RotateDexClientSecret)
	case len(found) > 0:
		return Secrets{DexClient: found[0]}, nil
	}
	s, err := NewClientSecret(rand)
	return Secrets{DexClient: s, Fresh: true}, err
}

// readSecretKey reads one key of a Secret manifest; ok is false when the
// file does not exist. fix is the flag that replaces it, for the refusal.
func readSecretKey(path, key, fix string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file: never read through", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	var m struct {
		Kind string            `json:"kind"`
		Data map[string][]byte `json:"data"`
	}
	if err := yaml.Unmarshal(data, &m); err != nil || m.Kind != "Secret" || len(m.Data[key]) == 0 {
		return nil, false, fmt.Errorf("%s is not a Secret holding %s: restore it, delete it, or pass %s",
			path, key, fix)
	}
	return m.Data[key], true, nil
}

// secretFiles renders the two Dex client Secret manifests.
func secretFiles(a Answers, s Secrets) ([]File, error) {
	if s.DexClient.IsZero() {
		return nil, errors.New("no Dex client secret to render")
	}
	relay, err := ghapp.RenderSecret(RelayDexSecret, a.Namespace,
		map[string][]byte{RelayDexSecretKey: s.DexClient.bytes()})
	if err != nil {
		return nil, err
	}
	dex, err := ghapp.RenderSecret(DexClientSecret, a.Dex.Namespace,
		map[string][]byte{DexClientSecretKey: s.DexClient.bytes()})
	if err != nil {
		return nil, err
	}
	files := []File{
		{Path: RelayDexSecretFile, Data: relay, Secret: true},
		{Path: DexClientSecretFile, Data: dex, Secret: true},
	}
	if key := a.DNSCredentialKey(); key != "" && !s.DNS.IsZero() {
		dns, err := ghapp.RenderSecret(DNSSecret, a.Namespace, map[string][]byte{key: s.DNS.bytes()})
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: DNSSecretFile, Data: dns, Secret: true})
	}
	tunnel, err := tunnelSecretFile(a, s)
	if err != nil {
		return nil, err
	}
	oauth, err := tailscaleOAuthSecretFiles(a, s)
	if err != nil {
		return nil, err
	}
	return slices.Concat(files, tunnel, oauth), nil
}

// Credential is a provider credential the operator hands the wizard (the
// DNS-01 solver's token or key), held only to be written into a Secret
// manifest. Its formatting is redacted, as ClientSecret's is.
type Credential struct{ value []byte }

func (Credential) String() string               { return redacted }
func (Credential) GoString() string             { return redacted }
func (Credential) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (Credential) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Credential) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// IsZero reports no credential.
func (c Credential) IsZero() bool { return len(c.value) == 0 }

func (c Credential) bytes() []byte { return c.value }

// maxCredential bounds a credential file: a token is tens of bytes, a
// service account key a few kilobytes.
const maxCredential = 64 << 10

// ErrCredential reports a credential the wizard refuses to write. It
// never quotes the credential.
var ErrCredential = errors.New("DNS credential")

// ParseCredential reads a DNS credential as given (a file's or stdin's
// bytes) for a's solver: a JSON document is kept as it is, every other
// credential is one token, trimmed of surrounding white space and refused
// if anything else is left in it (two lines are a mistake, never a token).
func ParseCredential(a Answers, raw []byte) (Credential, error) {
	if len(raw) > maxCredential {
		return Credential{}, fmt.Errorf("%w: more than %d bytes", ErrCredential, maxCredential)
	}
	if a.DNSCredentialIsJSON() {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return Credential{}, fmt.Errorf("%w: --dns %s takes a JSON key file, and this is not one",
				ErrCredential, a.Certificate.DNS)
		}
		return Credential{value: slices.Clone(raw)}, nil
	}
	tok := bytes.TrimSpace(raw)
	if len(tok) == 0 {
		return Credential{}, fmt.Errorf("%w: empty", ErrCredential)
	}
	for _, c := range tok {
		if c <= ' ' || c == 0x7f {
			return Credential{}, fmt.Errorf("%w: white space or a control character inside it: give a file "+
				"holding the one token", ErrCredential)
		}
	}
	return Credential{value: slices.Clone(tok)}, nil
}

// ResolveDNSCredential is the DNS credential a plan renders (design §4.1:
// a secret file is never regenerated while it exists): the one given this
// run, else the one an earlier run wrote under dir, else none. A credential
// given to a solver that takes none is refused, and so is one on disk
// under another provider's key (pass a new one).
func ResolveDNSCredential(dir string, a Answers, given Credential) (Credential, error) {
	key := a.DNSCredentialKey()
	if !given.IsZero() {
		if key == "" {
			return Credential{}, fmt.Errorf("%w: --dns %s with these options takes none (an ambient identity, or "+
				"your own issuer)", ErrCredential, a.Certificate.DNS)
		}
		return given, nil
	}
	if key == "" {
		return Credential{}, nil
	}
	v, ok, err := readSecretKey(filepath.Join(dir, filepath.FromSlash(DNSSecretFile)), key,
		"--dns-credentials-file")
	if err != nil || !ok {
		return Credential{}, err
	}
	return Credential{value: v}, nil
}
