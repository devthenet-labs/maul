// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

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
		s, ok, err := readSecretKey(filepath.Join(dir, filepath.FromSlash(f.path)), f.key)
		if err != nil {
			return Secrets{}, err
		}
		if ok {
			found = append(found, s)
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
// file does not exist.
func readSecretKey(path, key string) (ClientSecret, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ClientSecret{}, false, nil
	}
	if err != nil {
		return ClientSecret{}, false, err
	}
	if !info.Mode().IsRegular() {
		return ClientSecret{}, false, fmt.Errorf("%s is not a regular file: never read through", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ClientSecret{}, false, err
	}
	var m struct {
		Kind string            `json:"kind"`
		Data map[string][]byte `json:"data"`
	}
	if err := yaml.Unmarshal(data, &m); err != nil || m.Kind != "Secret" || len(m.Data[key]) == 0 {
		return ClientSecret{}, false, fmt.Errorf("%s is not a Secret holding %s: restore it, delete it with its "+
			"twin, or pass --rotate %s", path, key, RotateDexClientSecret)
	}
	return ClientSecret{value: string(m.Data[key])}, true, nil
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
	return []File{
		{Path: RelayDexSecretFile, Data: relay, Secret: true},
		{Path: DexClientSecretFile, Data: dex, Secret: true},
	}, nil
}
