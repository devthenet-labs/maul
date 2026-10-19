// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const (
	testClientID     = "kSENTINEL0CNTRL"
	testClientSecret = "tskey-client-kSENTINEL0CNTRL-SENTINEL-not-a-secret"
)

// tailscaleAnswers is the desktop's tailscale recipe, which installs the
// operator.
func tailscaleAnswers() Answers {
	a := goldenCases()["tailscale-route53-k3d"]
	a.Normalize()
	return a
}

func TestParseTailscaleOAuth(t *testing.T) {
	c, err := ParseTailscaleOAuth([]byte(" "+testClientID+"\n"), []byte(testClientSecret+"\n"))
	if err != nil || string(c.id) != testClientID || string(c.secret) != testClientSecret {
		t.Fatalf("a client as the console shows it: %v", err)
	}
	for name, tc := range map[string]struct {
		id, secret, want string
	}{
		"empty id":         {"", testClientSecret, "client_id is empty"},
		"empty secret":     {testClientID, "  \n", "client_secret is empty"},
		"swapped":          {testClientSecret, testClientID, "swapped"},
		"id with a space":  {"kSENTINEL 0CNTRL", testClientSecret, "not a client ID"},
		"auth key":         {testClientID, "tskey-auth-kSENTINEL0CNTRL-SENTINEL", "another kind of key"},
		"api token":        {testClientID, "tskey-api-kSENTINEL0CNTRL-SENTINEL", "another kind of key"},
		"two lines":        {testClientID, testClientSecret + "\n" + testClientSecret, "not an OAuth client secret"},
		"not a secret":     {testClientID, "hunter2-SENTINEL", "not an OAuth client secret"},
		"too big":          {testClientID, strings.Repeat("a", maxTailscaleOAuth+1), "more than"},
		"control in a key": {testClientID, "tskey-client-kSENTINEL\x00CNTRL-x", "not an OAuth client secret"},
	} {
		_, err := ParseTailscaleOAuth([]byte(tc.id), []byte(tc.secret))
		if !errors.Is(err, ErrTailscaleOAuth) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "SENTINEL") {
			t.Errorf("%s: the refusal quotes a value: %v", name, err)
		}
	}
}

func TestTailscaleOAuthRedacted(t *testing.T) {
	c := tailscaleOAuthSentinel
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s %q %x", c, c, c, c, c, c),
		fmt.Sprint(Secrets{TailscaleOAuth: c}), fmt.Sprintf("%+v", Secrets{TailscaleOAuth: c}),
		fmt.Errorf("wrapped: %v", c).Error()} {
		if strings.Contains(s, "SENTINEL") || !strings.Contains(s, redacted) {
			t.Errorf("formatting leaks the OAuth client: %s", s)
		}
	}
	if b, _ := c.MarshalJSON(); strings.Contains(string(b), "SENTINEL") {
		t.Error("MarshalJSON leaks the OAuth client")
	}
}

// TestTailscaleOAuthSecretManifest: the Secret is the one the operator's
// chart mounts, in its namespace, with its two keys; and the client
// reaches no other file of the plan.
func TestTailscaleOAuthSecretManifest(t *testing.T) {
	a := tailscaleAnswers()
	files := planWith(t, a, Secrets{DexClient: sentinel, DNS: dnsSentinel, TailscaleOAuth: tailscaleOAuthSentinel})
	var sec corev1.Secret
	for _, f := range files {
		if f.Path == TailscaleOAuthFile {
			if !f.Secret {
				t.Error("the OAuth Secret is not marked secret")
			}
			if err := yaml.UnmarshalStrict(f.Data, &sec); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if strings.Contains(string(f.Data), "SENTINEL0CNTRL") {
			t.Errorf("%s holds the OAuth client", f.Path)
		}
	}
	if sec.Name != "operator-oauth" || sec.Namespace != TailscaleOperator.Namespace ||
		string(sec.Data["client_id"]) != testClientID || string(sec.Data["client_secret"]) != testClientSecret ||
		len(sec.Data) != 2 {
		t.Errorf("Secret = %s/%s %v", sec.Namespace, sec.Name, sec.Data)
	}
	// The operator's values name that very Secret.
	values := byPath(files)[TailscaleOperator.ValuesFile]
	if !strings.Contains(values, "Secret "+sec.Name+" in namespace "+sec.Namespace) {
		t.Errorf("the operator's values do not name the Secret:\n%s", values)
	}
}

// writeOAuthSecret writes a's OAuth Secret manifest under dir, as an
// earlier run would have.
func writeOAuthSecret(t *testing.T, dir string, a Answers, c TailscaleOAuth) {
	t.Helper()
	files, err := tailscaleOAuthSecretFiles(a, Secrets{TailscaleOAuth: c})
	if err != nil || len(files) != 1 {
		t.Fatalf("render: %v, %d files", err, len(files))
	}
	p := filepath.Join(dir, filepath.FromSlash(TailscaleOAuthFile))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, files[0].Data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveTailscaleOAuth(t *testing.T) {
	a := tailscaleAnswers()
	given := tailscaleOAuthSentinel
	empty := t.TempDir()
	if got, err := ResolveTailscaleOAuth(empty, a, given); err != nil || string(got.secret) != testClientSecret {
		t.Errorf("given: %v", err)
	}
	if got, err := ResolveTailscaleOAuth(empty, a, TailscaleOAuth{}); err != nil || !got.IsZero() {
		t.Errorf("none: %v", err)
	}
	installed := a
	installed.Tailscale.OperatorInstalled = true
	other := a
	other.Edge = EdgeGatewayTLS
	for name, b := range map[string]Answers{"operator installed": installed, "another edge": other} {
		if _, err := ResolveTailscaleOAuth(empty, b, given); !errors.Is(err, ErrTailscaleOAuth) {
			t.Errorf("given with %s: %v", name, err)
		}
	}

	dir := t.TempDir()
	writeOAuthSecret(t, dir, a, given)
	got, err := ResolveTailscaleOAuth(dir, a, TailscaleOAuth{})
	if err != nil || string(got.id) != testClientID || string(got.secret) != testClientSecret {
		t.Errorf("read back: %v", err)
	}
	// Once the operator runs, the file on disk is no longer rendered: Compare
	// reports it stale instead of the plan failing.
	if got, err := ResolveTailscaleOAuth(dir, installed, TailscaleOAuth{}); err != nil || !got.IsZero() {
		t.Errorf("operator installed, file on disk: %v", err)
	}
	newer, err := ParseTailscaleOAuth([]byte("kNEWER0CNTRL"), []byte("tskey-client-kNEWER0CNTRL-newer"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveTailscaleOAuth(dir, a, newer); err != nil || string(got.id) != "kNEWER0CNTRL" {
		t.Errorf("a new client given wins: %v", err)
	}

	broken := t.TempDir()
	p := filepath.Join(broken, filepath.FromSlash(TailscaleOAuthFile))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("kind: Secret\ndata:\n  client_id: a1NFTlRJTkVM\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTailscaleOAuth(broken, a, TailscaleOAuth{}); err == nil ||
		!strings.Contains(err.Error(), "--tailscale-oauth-dir") {
		t.Errorf("a Secret missing client_secret: %v", err)
	}
}

// TestNextStepsOAuthClient: NEXT-STEPS.md gives the OAuth client's exact
// recipe whenever it installs the operator, and applies the Secret file
// only when one was written.
func TestNextStepsOAuthClient(t *testing.T) {
	a := tailscaleAnswers()
	flat := func(s Secrets) string {
		return strings.Join(strings.Fields(byPath(planWith(t, a, s))[NextStepsFile]), " ")
	}
	without := flat(Secrets{DexClient: sentinel, DNS: dnsSentinel})
	with := flat(Secrets{DexClient: sentinel, DNS: dnsSentinel, TailscaleOAuth: tailscaleOAuthSentinel})
	for _, want := range []string{"**Devices > Core**", "**Keys > Auth Keys**", "**General > Services**",
		"each tagged `tag:k8s-operator`", "letters, digits and spaces, at most 50 characters"} {
		for name, s := range map[string]string{"without": without, "with": with} {
			if !strings.Contains(s, want) {
				t.Errorf("%s the client: NEXT-STEPS.md lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(with, "kubectl apply -f "+TailscaleOAuthFile) || strings.Contains(with, "create secret generic") {
		t.Error("with the client given, NEXT-STEPS.md does not apply its file alone")
	}
	if !strings.Contains(without, "--tailscale-oauth-dir") || !strings.Contains(without, "create secret generic") {
		t.Error("without the client, NEXT-STEPS.md does not say how to give it")
	}
	installed := a
	installed.Tailscale.OperatorInstalled = true
	got := strings.Join(strings.Fields(byPath(planWith(t, installed, Secrets{DexClient: sentinel,
		DNS: dnsSentinel}))[NextStepsFile]), " ")
	if strings.Contains(got, "Trust credentials") {
		t.Error("with the operator installed, NEXT-STEPS.md still asks for an OAuth client")
	}
}
