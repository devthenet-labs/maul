// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

const awsFile = `# shared credentials
[default]
aws_access_key_id = AKIADEFAULT000000000
aws_secret_access_key = default/secret+key

[dns01]
; the DNS-01 user
AWS_Access_Key_ID=AKIAIOSFODNN7EXAMPLE
aws_secret_access_key   =   wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
region = us-east-1

[sso]
aws_access_key_id = ASIAEXAMPLE000000000
aws_secret_access_key = sso-secret
aws_session_token = sso-session-token
`

func TestParseAWSProfile(t *testing.T) {
	key, err := ParseAWSProfile([]byte(awsFile), "dns01")
	if err != nil {
		t.Fatal(err)
	}
	if key.ID != "AKIAIOSFODNN7EXAMPLE" || string(key.Secret.bytes()) != "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" {
		t.Errorf("read %s and a %d-byte secret", key.ID, len(key.Secret.bytes()))
	}
	if s := fmt.Sprintf("%v %+v %#v", key, key, key); strings.Contains(s, "wJalr") {
		t.Errorf("the secret formats: %s", s)
	}

	for name, tc := range map[string]struct{ file, profile, want string }{
		"missing profile lists the others": {awsFile, "prod", "no profile prod in the file (it has: default, dns01, sso)"},
		"session token":                    {awsFile, "sso", "temporary credentials"},
		"profile twice":                    {awsFile + "[dns01]\n", "dns01", "in the file twice"},
		"setting twice": {"[p]\naws_access_key_id = AKIAIOSFODNN7EXAMPLE\naws_access_key_id = AKIAIOSFODNN7EXAMPLE\n",
			"p", "sets aws_access_key_id twice"},
		"no secret": {"[p]\naws_access_key_id = AKIAIOSFODNN7EXAMPLE\n", "p", "needs both"},
		"no key id": {"[p]\naws_secret_access_key = s\n", "p", "needs both"},
		"not an IAM user's key": {"[p]\naws_access_key_id = AKIAshort\naws_secret_access_key = s\n", "p",
			"not an IAM user's"},
		"nested setting": {"[p]\naws_access_key_id = AKIAIOSFODNN7EXAMPLE\n  s3 =\n", "p", "nested setting"},
		"not key=value":  {"[p]\naws_access_key_id\n", "p", "not key = value"},
		"bad header":     {"[p\n", "p", "not a [profile] header"},
		"secret with a space": {"[p]\naws_access_key_id = AKIAIOSFODNN7EXAMPLE\naws_secret_access_key = a b\n",
			"p", "white space"},
		"too big": {strings.Repeat("#", maxCredential+1), "p", "more than"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseAWSProfile([]byte(tc.file), tc.profile)
			if !errors.Is(err, ErrCredential) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			for _, secret := range []string{"default/secret+key", "wJalr", "sso-secret", "sso-session-token"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error quotes a secret: %v", err)
				}
			}
		})
	}
}

// awsProfile is one generated profile of a shared credentials file.
type awsProfile struct{ name, id, secret string }

// randAWSFile is a credentials file of 1-5 profiles with distinct names,
// keys and secrets, written with the spacing, case and comments the AWS
// CLI accepts, and the index of one of them.
func randAWSFile(r *rand.Rand) (string, []awsProfile, int) {
	const upper, secretChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/+="
	pick := func(chars string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = chars[r.Intn(len(chars))]
		}
		return string(b)
	}
	pad := func() string { return strings.Repeat(" ", r.Intn(3)) }
	n := 1 + r.Intn(5)
	profiles := make([]awsProfile, n)
	var b strings.Builder
	for i := range profiles {
		p := awsProfile{name: fmt.Sprintf("%s-%d", randLabel(r), i), id: "AKIA" + pick(upper, 16),
			secret: fmt.Sprintf("%d%s", i, pick(secretChars, 39))}
		profiles[i] = p
		if r.Intn(2) == 0 {
			b.WriteString("# a comment\n")
		}
		fmt.Fprintf(&b, "[%s%s%s]\n", pad(), p.name, pad())
		lines := []string{
			fmt.Sprintf("aws_access_key_id%s=%s%s", pad(), pad(), p.id),
			fmt.Sprintf("%s%s=%s%s", []string{"aws_secret_access_key", "AWS_SECRET_ACCESS_KEY"}[r.Intn(2)], pad(),
				pad(), p.secret),
		}
		if r.Intn(2) == 0 {
			lines = append(lines, "region = us-east-1", "; another comment")
		}
		r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
		b.WriteString(strings.Join(lines, "\n") + "\n" + strings.Repeat("\n", r.Intn(2)))
	}
	return b.String(), profiles, r.Intn(n)
}

// TestParseAWSProfileProperties (seeded): out of any file of several
// profiles, the one named is read exactly, and no other profile's key or
// secret is ever returned.
func TestParseAWSProfileProperties(t *testing.T) {
	cfg := quickConfig(2000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		file, profiles, i := randAWSFile(r)
		args[0], args[1], args[2] = reflect.ValueOf(file), reflect.ValueOf(profiles), reflect.ValueOf(i)
	}
	prop := func(file string, profiles []awsProfile, i int) bool {
		key, err := ParseAWSProfile([]byte(file), profiles[i].name)
		if err != nil {
			t.Logf("%s: %v\n%s", profiles[i].name, err, file)
			return false
		}
		if key.ID != profiles[i].id || string(key.Secret.bytes()) != profiles[i].secret {
			t.Logf("%s: read another key\n%s", profiles[i].name, file)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
