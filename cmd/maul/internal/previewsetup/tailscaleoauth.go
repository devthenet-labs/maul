// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/devthenet-labs/maul/cmd/maul/internal/ghapp"
)

// The Tailscale operator's OAuth client (setup-previews-easy (a)): the
// two files --tailscale-oauth-dir holds, and the Secret manifest the
// wizard writes them into, the one the operator's chart mounts
// (tailscaleOAuthSecret in TailscaleOperator's namespace).
const (
	// TailscaleOAuthIDFile and TailscaleOAuthSecretFile name the files in
	// --tailscale-oauth-dir, which are also the Secret's keys.
	TailscaleOAuthIDFile     = "client_id"
	TailscaleOAuthSecretFile = "client_secret"
	// TailscaleOAuthFile is the Secret manifest, under secrets/.
	TailscaleOAuthFile = "secrets/tailscale-operator-oauth.secret.yaml"

	// maxTailscaleOAuth bounds each of the two values: a client ID is a
	// dozen characters, a client secret under a hundred.
	maxTailscaleOAuth = 512
)

// The two values' grammars. A client ID is letters and digits; a client
// secret is tskey-client-<id>-<secret>, so an auth key (tskey-auth-) or an
// API access token (tskey-api-) given by mistake is named, never written.
var (
	tailscaleClientIDPattern     = regexp.MustCompile(`^[A-Za-z0-9]{4,128}$`)
	tailscaleClientSecretPattern = regexp.MustCompile(`^tskey-client-[A-Za-z0-9-]{8,400}$`)
	tailscaleOtherKeyPattern     = regexp.MustCompile(`^tskey-(auth|api|webhook|scim)-`)
)

// TailscaleOAuth is the operator's OAuth client, held only to be written
// into its Secret manifest. Its formatting is redacted, as ClientSecret's
// is: the client ID is no secret, but it travels with one.
type TailscaleOAuth struct{ id, secret []byte }

func (TailscaleOAuth) String() string               { return redacted }
func (TailscaleOAuth) GoString() string             { return redacted }
func (TailscaleOAuth) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (TailscaleOAuth) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (TailscaleOAuth) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// IsZero reports no OAuth client.
func (c TailscaleOAuth) IsZero() bool { return len(c.id) == 0 && len(c.secret) == 0 }

// ErrTailscaleOAuth reports an OAuth client the wizard refuses to write.
// It never quotes either value.
var ErrTailscaleOAuth = errors.New("Tailscale OAuth client")

// ParseTailscaleOAuth checks the two files' contents (each trimmed of
// surrounding white space, which an editor or `pbpaste >` leaves) against
// their grammars: one token each, the secret an OAuth client's.
func ParseTailscaleOAuth(clientID, clientSecret []byte) (TailscaleOAuth, error) {
	id, secret := bytes.TrimSpace(clientID), bytes.TrimSpace(clientSecret)
	switch {
	case len(clientID) > maxTailscaleOAuth || len(clientSecret) > maxTailscaleOAuth:
		return TailscaleOAuth{}, fmt.Errorf("%w: a file holds more than %d bytes", ErrTailscaleOAuth,
			maxTailscaleOAuth)
	case len(id) == 0:
		return TailscaleOAuth{}, fmt.Errorf("%w: %s is empty", ErrTailscaleOAuth, TailscaleOAuthIDFile)
	case len(secret) == 0:
		return TailscaleOAuth{}, fmt.Errorf("%w: %s is empty", ErrTailscaleOAuth, TailscaleOAuthSecretFile)
	case tailscaleClientSecretPattern.Match(id):
		return TailscaleOAuth{}, fmt.Errorf("%w: %s holds a client secret: the two files are swapped",
			ErrTailscaleOAuth, TailscaleOAuthIDFile)
	case !tailscaleClientIDPattern.Match(id):
		return TailscaleOAuth{}, fmt.Errorf("%w: %s is not a client ID (letters and digits, as the admin "+
			"console shows it)", ErrTailscaleOAuth, TailscaleOAuthIDFile)
	case tailscaleOtherKeyPattern.Match(secret):
		return TailscaleOAuth{}, fmt.Errorf("%w: %s holds another kind of key (an auth key or an API token): "+
			"generate an OAuth client, whose secret starts tskey-client-", ErrTailscaleOAuth, TailscaleOAuthSecretFile)
	case !tailscaleClientSecretPattern.Match(secret):
		return TailscaleOAuth{}, fmt.Errorf("%w: %s is not an OAuth client secret (tskey-client-..., one line)",
			ErrTailscaleOAuth, TailscaleOAuthSecretFile)
	}
	return TailscaleOAuth{id: slices.Clone(id), secret: slices.Clone(secret)}, nil
}

// WritesTailscaleOAuth reports whether a's recipe installs the Tailscale
// operator, so its OAuth client's Secret is one the wizard can write.
func (a Answers) WritesTailscaleOAuth() bool {
	return a.Edge == EdgeTailscale && !a.Tailscale.OperatorInstalled
}

// ResolveTailscaleOAuth is the OAuth client a plan renders (design §4.1: a
// secret file is never regenerated while it exists): the one given this
// run, else the one an earlier run wrote under dir, else none (the Secret
// is then the operator's to create, as NEXT-STEPS.md says). One given to a
// recipe that installs no operator is refused; one on disk then is left
// for Compare to report as no longer generated.
func ResolveTailscaleOAuth(dir string, a Answers, given TailscaleOAuth) (TailscaleOAuth, error) {
	if !given.IsZero() {
		if !a.WritesTailscaleOAuth() {
			return TailscaleOAuth{}, fmt.Errorf("%w: --tailscale-oauth-dir is for --edge tailscale while the "+
				"recipe installs the operator: with --tailscale-operator-installed its Secret already exists",
				ErrTailscaleOAuth)
		}
		return given, nil
	}
	if !a.WritesTailscaleOAuth() {
		return TailscaleOAuth{}, nil
	}
	path := filepath.Join(dir, filepath.FromSlash(TailscaleOAuthFile))
	id, ok, err := readSecretKey(path, TailscaleOAuthIDFile, "--tailscale-oauth-dir")
	if err != nil || !ok {
		return TailscaleOAuth{}, err
	}
	secret, _, err := readSecretKey(path, TailscaleOAuthSecretFile, "--tailscale-oauth-dir")
	if err != nil {
		return TailscaleOAuth{}, err
	}
	c, err := ParseTailscaleOAuth(id, secret)
	if err != nil {
		return TailscaleOAuth{}, fmt.Errorf("%s: %w; give --tailscale-oauth-dir again", TailscaleOAuthFile, err)
	}
	return c, nil
}

// tailscaleOAuthSecretFiles renders the operator's OAuth Secret manifest,
// when the recipe installs the operator and one was given.
func tailscaleOAuthSecretFiles(a Answers, s Secrets) ([]File, error) {
	if !a.WritesTailscaleOAuth() || s.TailscaleOAuth.IsZero() {
		return nil, nil
	}
	data, err := ghapp.RenderSecret(tailscaleOAuthSecret, TailscaleOperator.Namespace, map[string][]byte{
		TailscaleOAuthIDFile:     s.TailscaleOAuth.id,
		TailscaleOAuthSecretFile: s.TailscaleOAuth.secret,
	})
	if err != nil {
		return nil, err
	}
	return []File{{Path: TailscaleOAuthFile, Data: data, Secret: true}}, nil
}
