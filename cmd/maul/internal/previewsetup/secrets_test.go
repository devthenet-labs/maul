// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestNewClientSecret(t *testing.T) {
	s, err := NewClientSecret(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s.value)
	if err != nil || len(raw) != clientSecretBytes {
		t.Errorf("secret %q is not %d bytes of base64url: %v", s.value, clientSecretBytes, err)
	}
	other, _ := NewClientSecret(rand.Reader)
	if s.equal(other) {
		t.Error("two draws gave the same secret")
	}
	if _, err := NewClientSecret(iotest.ErrReader(errors.New("no entropy"))); err == nil {
		t.Error("a failed draw gave a secret")
	}
}

// TestClientSecretRedacted: no formatting reveals the secret, mirroring
// ghapp.Credentials.
func TestClientSecretRedacted(t *testing.T) {
	s := sentinel
	j, _ := json.Marshal(struct{ S ClientSecret }{s})
	y, _ := yaml.Marshal(struct{ S ClientSecret }{s})
	for _, out := range []string{
		fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%s", s),
		fmt.Sprintf("%q", s), fmt.Sprintf("%x", s), fmt.Sprint(Secrets{DexClient: s}),
		fmt.Sprintf("%#v", Secrets{DexClient: s}), fmt.Errorf("wrapped: %v", s).Error(), string(j), string(y),
	} {
		if strings.Contains(out, s.value) || !strings.Contains(out, redacted) {
			t.Errorf("formatting leaked or lost the redaction: %q", out)
		}
	}
}

// writeSecrets writes both Secret files for a, holding s, under dir.
func writeSecrets(t *testing.T, dir string, a Answers, s ClientSecret) {
	t.Helper()
	files, err := secretFiles(a, Secrets{DexClient: s})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compare(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(c, Accept{All: true}); err != nil {
		t.Fatal(err)
	}
}

// TestResolveSecretsFresh: an empty directory draws a secret.
func TestResolveSecretsFresh(t *testing.T) {
	s, err := ResolveSecrets(t.TempDir(), false, rand.Reader)
	if err != nil || s.DexClient.IsZero() || !s.Fresh {
		t.Errorf("ResolveSecrets on an empty directory = %v, %v", s, err)
	}
}

// TestResolveSecretsReadsBack: a written secret is read back, never drawn again.
func TestResolveSecretsReadsBack(t *testing.T) {
	a := testAnswers()
	dir := t.TempDir()
	writeSecrets(t, dir, a, sentinel)
	s, err := ResolveSecrets(dir, false, iotest.ErrReader(errors.New("must not draw")))
	if err != nil || !s.DexClient.equal(sentinel) || s.Fresh {
		t.Errorf("ResolveSecrets = %+v, %v; want the sentinel read back", s.Fresh, err)
	}
}

// TestResolveSecretsOneFileLost: with one file gone, the other's secret is kept.
func TestResolveSecretsOneFileLost(t *testing.T) {
	a := testAnswers()
	for _, lost := range []string{RelayDexSecretFile, DexClientSecretFile} {
		dir := t.TempDir()
		writeSecrets(t, dir, a, sentinel)
		if err := os.Remove(filepath.Join(dir, lost)); err != nil {
			t.Fatal(err)
		}
		s, err := ResolveSecrets(dir, false, iotest.ErrReader(errors.New("must not draw")))
		if err != nil || !s.DexClient.equal(sentinel) {
			t.Errorf("without %s: %v", lost, err)
		}
	}
}

// TestResolveSecretsDisagree: two files holding different secrets are refused, not guessed between.
func TestResolveSecretsDisagree(t *testing.T) {
	a := testAnswers()
	dir := t.TempDir()
	writeSecrets(t, dir, a, sentinel)
	other := a
	other.Namespace = "elsewhere"
	files, _ := secretFiles(other, Secrets{DexClient: ClientSecret{value: "another"}})
	if err := os.WriteFile(filepath.Join(dir, files[1].Path), files[1].Data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSecrets(dir, false, rand.Reader); !errors.Is(err, ErrSecretsDisagree) {
		t.Errorf("ResolveSecrets = %v, want ErrSecretsDisagree", err)
	}
	if s, err := ResolveSecrets(dir, true, rand.Reader); err != nil || !s.Fresh {
		t.Errorf("--rotate settles it: %v", err)
	}
}

// TestResolveSecretsRotate: --rotate draws anew although a secret is written.
func TestResolveSecretsRotate(t *testing.T) {
	a := testAnswers()
	dir := t.TempDir()
	writeSecrets(t, dir, a, sentinel)
	s, err := ResolveSecrets(dir, true, rand.Reader)
	if err != nil || s.DexClient.equal(sentinel) || !s.Fresh {
		t.Errorf("rotate kept the old secret: %v", err)
	}
}

// TestResolveSecretsUnreadable: a file that is no Secret holding the key is an error, never replaced.
func TestResolveSecretsUnreadable(t *testing.T) {
	for name, data := range map[string]string{
		"not yaml":   "{{{",
		"no key":     "apiVersion: v1\nkind: Secret\ndata: {other: eA==}\n",
		"not secret": "apiVersion: v1\nkind: ConfigMap\ndata: {clientSecret: eA==}\n",
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, RelayDexSecretFile)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveSecrets(dir, false, rand.Reader); err == nil {
			t.Errorf("%s: a broken secret file was replaced silently", name)
		}
	}
}

// TestResolveSecretsSymlink: a secret is never read through a symbolic link.
func TestResolveSecretsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.yaml")
	_ = os.WriteFile(target, []byte("x"), 0o600)
	_ = os.MkdirAll(filepath.Join(dir, "secrets"), 0o700)
	if err := os.Symlink(target, filepath.Join(dir, RelayDexSecretFile)); err != nil {
		t.Skip(err)
	}
	if _, err := ResolveSecrets(dir, false, rand.Reader); err == nil {
		t.Error("a secret was read through a symbolic link")
	}
}

// TestSecretFilesAreSecrets: the two manifests decode as the v1 Secrets
// the relay and Dex read, the same secret under the documented keys.
func TestSecretFilesAreSecrets(t *testing.T) {
	a := testAnswers()
	files, err := secretFiles(a, Secrets{DexClient: sentinel})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{
		RelayDexSecretFile:  {RelayDexSecret, a.Namespace, RelayDexSecretKey},
		DexClientSecretFile: {DexClientSecret, a.Dex.Namespace, DexClientSecretKey},
	}
	for _, f := range files {
		var s corev1.Secret
		if err := yaml.UnmarshalStrict(f.Data, &s); err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		w := want[f.Path]
		if s.Kind != "Secret" || s.Name != w[0] || s.Namespace != w[1] || !bytes.Equal(s.Data[w[2]], sentinel.bytes()) ||
			len(s.Data) != 1 || !f.Secret {
			t.Errorf("%s = %+v", f.Path, s)
		}
	}
	if _, err := secretFiles(a, Secrets{}); err == nil {
		t.Error("rendered Secrets without a secret")
	}
}
