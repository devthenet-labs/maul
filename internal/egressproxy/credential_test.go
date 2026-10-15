// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// sentinel is the fixed credential every leak test looks for.
const sentinel = "s3cr3t-TOKEN-1234"

// leakForms are the forms of a secret that must never appear: the value,
// and base64 of the forms basic auth sends.
func leakForms(secrets ...string) []string {
	out := make([]string, 0, 2*len(secrets))
	for _, s := range secrets {
		out = append(out, s, base64.StdEncoding.EncodeToString([]byte(s)))
	}
	return out
}

func sampleCredentials(t *testing.T) map[string]Credential {
	t.Helper()
	bearer, err := BearerCredential(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	basic, err := BasicCredential("svc-patchy", sentinel)
	if err != nil {
		t.Fatal(err)
	}
	caNPM, err := CodeArtifactCredential("npm", sentinel, time.Unix(1_900_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	caPyPI, err := CodeArtifactCredential("pypi", sentinel, time.Unix(1_900_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Credential{"bearer": bearer, "basic": basic, "codeartifact-npm": caNPM,
		"codeartifact-pypi": caPyPI}
}

// TestCredentialNeverPrinted: every formatter, slog handler and encoder
// prints [redacted], directly, as a struct field, through a pointer and
// wrapped in an error.
func TestCredentialNeverPrinted(t *testing.T) {
	forbidden := leakForms(sentinel, "svc-patchy:"+sentinel, "aws:"+sentinel)
	for name, c := range sampleCredentials(t) {
		t.Run(name, func(t *testing.T) {
			holder := struct {
				Registry string
				Cred     Credential
				Ptr      *Credential
			}{"r", c, &c}
			var outs []string
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T %v", "%10.3s"} {
				outs = append(outs, fmt.Sprintf(verb, c), fmt.Sprintf(verb, holder), fmt.Sprintf(verb, &c))
			}
			outs = append(outs, fmt.Sprint(c), fmt.Sprintln(c, holder), c.String(), c.GoString(),
				fmt.Errorf("upstream refused %v: %w", c, errors.New("x")).Error())
			for _, h := range []func(*bytes.Buffer) slog.Handler{
				func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
				func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
			} {
				var buf bytes.Buffer
				log := slog.New(h(&buf))
				log.Info("credential_loaded", "credential", c, "holder", holder, slog.Any("ptr", &c))
				outs = append(outs, buf.String())
			}
			for _, v := range []any{c, holder, &c, []Credential{c}, map[string]Credential{"k": c}} {
				j, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				outs = append(outs, string(j))
			}
			if x, err := xml.Marshal(holder); err == nil {
				outs = append(outs, string(x))
			}
			for _, out := range outs {
				for _, f := range forbidden {
					if strings.Contains(out, f) {
						t.Errorf("printed form %q holds the secret (%q)", out, f)
					}
				}
			}
			if s := fmt.Sprintf("%+v", c); s != redacted {
				t.Errorf("%%+v = %q, want %q", s, redacted)
			}
		})
	}
}

func TestCredentialHeaders(t *testing.T) {
	creds := sampleCredentials(t)
	pair := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for name, want := range map[string]struct {
		header  string
		kind    CredentialKind
		needles []string
	}{
		"bearer": {"Bearer " + sentinel, CredentialStatic, []string{sentinel}},
		"basic": {"Basic " + pair("svc-patchy:"+sentinel), CredentialStatic,
			[]string{pair("svc-patchy:" + sentinel), sentinel}},
		"codeartifact-npm": {"Bearer " + sentinel, CredentialCodeArtifact, []string{sentinel}},
		"codeartifact-pypi": {"Basic " + pair("aws:"+sentinel), CredentialCodeArtifact,
			[]string{pair("aws:" + sentinel), sentinel}},
	} {
		c := creds[name]
		if c.Authorization() != want.header || c.Kind() != want.kind {
			t.Errorf("%s: kind %s", name, c.Kind())
		}
		got := c.EchoNeedles()
		if len(got) != len(want.needles) {
			t.Fatalf("%s: %d needles", name, len(got))
		}
		for i := range got {
			if string(got[i]) != want.needles[i] {
				t.Errorf("%s: needle %d differs", name, i)
			}
		}
		got[0][0] = 'X' // a copy: the credential keeps its own
		if string(c.EchoNeedles()[0]) != want.needles[0] {
			t.Errorf("%s: EchoNeedles shares its bytes", name)
		}
	}
	var zero Credential
	if zero.Kind() != CredentialNone || zero.Authorization() != "" || len(zero.EchoNeedles()) != 0 ||
		!zero.Usable(time.Now()) {
		t.Error("the zero Credential is not 'no credential'")
	}
	if n := NoCredential(); n.Kind() != CredentialNone || n.Authorization() != "" {
		t.Error("NoCredential sends a header")
	}
	exp := creds["codeartifact-npm"]
	if !exp.Usable(time.Unix(1_899_999_999, 0)) || exp.Usable(time.Unix(1_900_000_000, 0)) {
		t.Error("a minted credential is usable exactly until its expiry")
	}
	if !creds["bearer"].Usable(time.Unix(4_000_000_000, 0)) {
		t.Error("a static credential expired")
	}
	again, _ := BearerCredential(sentinel)
	other, _ := BearerCredential(sentinel + "x")
	if !creds["bearer"].SameValue(again) || creds["bearer"].SameValue(other) ||
		creds["bearer"].SameValue(creds["codeartifact-npm"]) {
		t.Error("SameValue compares kind and header")
	}
}

func TestCredentialRefusesBadValues(t *testing.T) {
	for _, v := range []string{
		"", "a\r\nX-Evil: 1", "a\nb", "a\x00", "a\tb", "a\x7f", strings.Repeat("a", MaxSecretBytes+1),
	} {
		if _, err := BearerCredential(v); err == nil {
			t.Errorf("BearerCredential(%q) admitted it", v)
		}
		if _, err := BasicCredential("u", v); err == nil {
			t.Errorf("BasicCredential password %q admitted", v)
		}
		if _, err := BasicCredential(v, "p"); err == nil {
			t.Errorf("BasicCredential username %q admitted", v)
		}
		if _, err := CodeArtifactCredential("npm", v, time.Now()); err == nil {
			t.Errorf("CodeArtifactCredential(%q) admitted it", v)
		}
	}
	if _, err := BasicCredential("a:b", "p"); err == nil {
		t.Error("a username with a colon was admitted")
	}
	if _, err := CodeArtifactCredential("go", "t", time.Now()); err == nil {
		t.Error("a go CodeArtifact credential was admitted")
	}
	if _, err := BearerCredential(strings.Repeat("a", MaxSecretBytes)); err != nil {
		t.Errorf("a %d-byte token was refused: %v", MaxSecretBytes, err)
	}
	// The refusal never quotes the value.
	_, err := BearerCredential(sentinel + "\n")
	if err == nil || strings.Contains(err.Error(), sentinel) {
		t.Errorf("refusal = %v", err)
	}
}
