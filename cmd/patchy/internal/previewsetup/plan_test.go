// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"encoding/base64"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// randAnswers are valid, complete answers for the existing edge: hosts on
// two registrable domains, viewers, a registry, and one of every way R4
// reaches the gateway, on any platform.
func randAnswers(r *rand.Rand) Answers {
	a := Defaults()
	a.Edge = EdgeExisting
	ps := Platforms()
	a.Platform = Platform(ps[r.Intn(len(ps))])
	a.HostSuffix = randUnder(r, randDomain(r))
	site, _ := Site(a.HostSuffix)
	for {
		a.SignInHost = randUnder(r, randDomain(r))
		if s, _ := Site(a.SignInHost); s != site {
			break
		}
	}
	a.Dex.Issuer = "https://dex." + a.SignInHost
	for range 1 + r.Intn(3) {
		a.Viewers.Teams = append(a.Viewers.Teams, randLabel(r)+":"+randLabel(r))
	}
	if r.Intn(2) == 0 {
		a.Viewers.Users = append(a.Viewers.Users, "u"+strings.ReplaceAll(randLabel(r), "-", ""))
	}
	a.Registry = randDomain(r)
	if r.Intn(4) == 0 {
		a.Registry += ":5000"
	}
	if a.Platform == PlatformOther {
		a.NodeIsolation.Mode = []string{IsolationRequired, IsolationNone}[r.Intn(2)]
	}
	switch r.Intn(3) {
	case 0:
		a.Existing.GatewayParent = randLabel(r) + "/" + randLabel(r)
	case 1:
		a.Existing.IngressClass = []string{"nginx", "traefik", "alb", "haproxy"}[r.Intn(4)]
	default:
		if a.Platform != PlatformEKS {
			a.Existing.IngressClass = "nginx"
		}
	}
	a.Normalize()
	return a
}

// randSecret is a client secret of random bytes, the size a draw gives.
func randSecret(r *rand.Rand) ClientSecret {
	b := make([]byte, clientSecretBytes)
	r.Read(b)
	return ClientSecret{value: base64.RawURLEncoding.EncodeToString(b)}
}

// TestSecretNeverLeaves (seeded): whatever the answers, the Dex client
// secret, raw or base64-encoded, is in no file but the two Secret
// manifests, and in both of those.
func TestSecretNeverLeaves(t *testing.T) {
	cfg := quickConfig(500)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(randAnswers(r))
		args[1] = reflect.ValueOf(randSecret(r).value)
	}
	prop := func(a Answers, secret string) bool {
		s := ClientSecret{value: secret}
		files, err := Plan(a, Secrets{DexClient: s}, testVersion)
		if err != nil {
			t.Logf("Plan(%+v): %v", a, err)
			return false
		}
		enc := []byte(base64.StdEncoding.EncodeToString([]byte(secret)))
		for _, f := range files {
			has := bytes.Contains(f.Data, []byte(secret)) || bytes.Contains(f.Data, enc)
			if has != f.Secret {
				t.Logf("%s: holds the secret %v, is a secret file %v", f.Path, has, f.Secret)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestPlanIsByteStable (seeded): the same answers, secret and version
// render the same bytes, and answers.yaml reads back as the answers it
// was rendered from, so a re-run with no new flag changes nothing.
func TestPlanIsByteStable(t *testing.T) {
	cfg := quickConfig(300)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) { args[0] = reflect.ValueOf(randAnswers(r)) }
	prop := func(a Answers) bool {
		first, err := Plan(a, Secrets{DexClient: sentinel}, testVersion)
		if err != nil {
			t.Logf("Plan: %v", err)
			return false
		}
		read, err := ParseAnswers([]byte(byPath(first)[AnswersFile]))
		if err != nil {
			t.Logf("ParseAnswers: %v", err)
			return false
		}
		read.Normalize()
		second, err := Plan(read, Secrets{DexClient: sentinel}, testVersion)
		if err != nil || len(first) != len(second) {
			t.Logf("re-plan: %v", err)
			return false
		}
		for i := range first {
			if first[i].Path != second[i].Path || !bytes.Equal(first[i].Data, second[i].Data) {
				t.Logf("%s differs on a re-run from answers.yaml:\n%s", first[i].Path,
					unifiedDiff(first[i].Path, first[i].Data, second[i].Data))
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func TestPlanRefuses(t *testing.T) {
	a := testAnswers()
	a.Platform = PlatformK3s
	a.Existing.IngressClass = "nginx"
	for _, edge := range []Edge{EdgeGatewayTLS, EdgeTailscale, EdgeCloudflareTunnel} {
		b := a
		b.Edge = edge
		if _, err := Plan(b, Secrets{DexClient: sentinel}, testVersion); !errors.Is(err, ErrRecipeNotReady) {
			t.Errorf("--edge %s: %v, want ErrRecipeNotReady", edge, err)
		}
	}
	b := a
	b.HostSuffix = ""
	if _, err := Plan(b, Secrets{DexClient: sentinel}, testVersion); err == nil ||
		!strings.Contains(err.Error(), "--host-suffix") {
		t.Errorf("an unanswered suffix: %v", err)
	}
	b = a
	b.SignInHost = "auth." + a.HostSuffix
	if _, err := Plan(b, Secrets{DexClient: sentinel}, testVersion); err == nil {
		t.Error("an invalid answer planned")
	}
	if _, err := Plan(a, Secrets{}, testVersion); err == nil {
		t.Error("planned without a client secret")
	}
}

func TestParseAnswersRefusesUnknownFields(t *testing.T) {
	if _, err := ParseAnswers([]byte("edge: existing\nhostSufix: typo.dev\n")); !errors.Is(err, ErrAnswers) {
		t.Errorf("ParseAnswers = %v, want ErrAnswers", err)
	}
}
