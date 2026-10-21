// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package awsca

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/codeartifact"

	"github.com/devthenet-labs/maul/internal/egressproxy"
)

// isolateAWSEnv points the SDK's ambient configuration at nothing, so a
// test reads no credential from the developer's machine.
func isolateAWSEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("HOME", dir)
	for _, k := range []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS", "AWS_REGION",
		"AWS_DEFAULT_REGION"} {
		t.Setenv(k, "")
	}
}

func TestPodIdentityPolicyAdmitsOnlyTheAgent(t *testing.T) {
	p := podIdentityPolicy{}
	if err := p.Check(PodIdentityAddr); err != nil {
		t.Fatalf("the Pod Identity agent was refused: %v", err)
	}
	for _, a := range []string{"169.254.170.23:443", "169.254.170.23:8080", "169.254.169.254:80",
		"169.254.170.2:80", "1.1.1.1:443", "1.1.1.1:80", "127.0.0.1:80", "10.0.0.1:80"} {
		if err := p.Check(netip.MustParseAddrPort(a)); !errors.Is(err, egressproxy.ErrDeniedAddress) {
			t.Errorf("Pod Identity policy admitted %s", a)
		}
	}
	// The API client's policy (the forwarding routes' AddrPolicy) never
	// admits the agent.
	if err := (egressproxy.AddrPolicy{}).Check(PodIdentityAddr); !errors.Is(err, egressproxy.ErrDeniedAddress) {
		t.Fatal("the public policy admits the Pod Identity agent")
	}
}

// TestClientsDialPolicies: each client refuses the other's address before
// any connection (the refusal is the dialer's, so no network is touched),
// and the API client records the class of what it judged.
func TestClientsDialPolicies(t *testing.T) {
	c := NewClients(egressproxy.AddrPolicy{})
	if c.LastAddressClass() != ClassNone {
		t.Fatalf("class before any dial = %s", c.LastAddressClass())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	get := func(cl *http.Client, url string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := cl.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	for url, class := range map[string]string{
		"http://169.254.170.23/v1/credentials": ClassLinkLocal,
		"http://169.254.169.254/latest":        ClassLinkLocal,
		"https://10.1.2.3/":                    ClassPrivate,
		"https://127.0.0.1/":                   ClassLoopback,
	} {
		if err := get(c.API, url); !errors.Is(err, egressproxy.ErrDeniedAddress) {
			t.Errorf("API client GET %s = %v, want a dial refusal", url, err)
		}
		if got := c.LastAddressClass(); got != class {
			t.Errorf("after %s the class is %s, want %s", url, got, class)
		}
	}
	for _, url := range []string{"https://1.1.1.1/", "http://169.254.169.254/", "https://169.254.170.23/"} {
		if err := get(c.PodIdentity, url); !errors.Is(err, egressproxy.ErrDeniedAddress) {
			t.Errorf("Pod Identity client GET %s = %v, want a dial refusal", url, err)
		}
	}
}

func TestAddressClass(t *testing.T) {
	for a, want := range map[string]string{
		"52.94.1.1": ClassPublic, "10.0.0.1": ClassPrivate, "172.16.4.4": ClassPrivate, "192.168.1.1": ClassPrivate,
		"100.64.0.1": ClassPrivate, "169.254.170.23": ClassLinkLocal, "127.0.0.1": ClassLoopback,
		"224.0.0.1": ClassOther, "0.0.0.0": ClassOther,
	} {
		if got := AddressClass(netip.MustParseAddr(a)); got != want {
			t.Errorf("AddressClass(%s) = %s, want %s", a, got, want)
		}
	}
}

// TestLoadConfigUsesTheGuardedClients: the SDK's credential chain reaches
// STS (IRSA) through the API client and the container-credentials
// endpoint (Pod Identity) through the PodIdentity client. Each fake
// endpoint is on loopback, which both policies refuse, so a refused dial
// with no request at the endpoint proves which client carried it.
func TestLoadConfigUsesTheGuardedClients(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Run("pod identity", func(t *testing.T) {
		isolateAWSEnv(t)
		t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", srv.URL+"/v1/credentials")
		t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN", "pod-token")
		c := NewClients(egressproxy.AddrPolicy{})
		cfg, err := c.LoadConfig(context.Background(), "us-east-1")
		if err != nil {
			t.Fatal(err)
		}
		_, err = cfg.Credentials.Retrieve(context.Background())
		if !errors.Is(err, egressproxy.ErrDeniedAddress) {
			t.Fatalf("Retrieve = %v, want the Pod Identity client's dial refusal", err)
		}
		if c.LastAddressClass() != ClassNone {
			t.Fatalf("the container-credentials call went through the API client (class %s)", c.LastAddressClass())
		}
	})
	t.Run("irsa", func(t *testing.T) {
		isolateAWSEnv(t)
		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte("jwt"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::377946145366:role/maul-egress-proxy")
		t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
		t.Setenv("AWS_ENDPOINT_URL_STS", srv.URL)
		c := NewClients(egressproxy.AddrPolicy{})
		cfg, err := c.LoadConfig(context.Background(), "us-east-1")
		if err != nil {
			t.Fatal(err)
		}
		_, err = cfg.Credentials.Retrieve(context.Background())
		if !errors.Is(err, egressproxy.ErrDeniedAddress) {
			t.Fatalf("Retrieve = %v, want the API client's dial refusal", err)
		}
		if c.LastAddressClass() != ClassLoopback {
			t.Fatalf("STS did not go through the API client (class %s)", c.LastAddressClass())
		}
	})
	if hits.Load() != 0 {
		t.Fatalf("a refused endpoint received %d requests", hits.Load())
	}
}

// TestSDKWireShape drives the real CodeArtifact client against a fake
// endpoint answering GetAuthorizationToken in the service's REST-JSON
// shape, and mints through it.
func TestSDKWireShape(t *testing.T) {
	isolateAWSEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/authorization-token" || q.Get("domain") != "maul-gate" ||
			q.Get("domain-owner") != "377946145366" || q.Get("duration") != "3600" ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Errorf("unexpected request %s %s %v", r.Method, r.URL, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"authorizationToken": sentinel, "expiration": 1900000000})
	}))
	defer srv.Close()
	cfg := aws.Config{Region: "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "")}
	api := NewAPI(cfg, func(o *codeartifact.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.HTTPClient = srv.Client()
	})
	m, err := New([]Registry{{Name: "ca", Ecosystem: "npm", Domain: "maul-gate", DomainOwner: "377946145366",
		Region: "us-east-1", Duration: time.Hour}}, func(string) (API, error) { return api, nil },
		Options{Now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := m.mint(context.Background(), m.entries["ca"])
	if err != nil {
		t.Fatal(err)
	}
	if cred.Authorization() != "Bearer "+sentinel || !cred.ExpiresAt().Equal(time.Unix(1_900_000_000, 0)) {
		t.Fatalf("minted %s expiring %s", cred.Kind(), cred.ExpiresAt())
	}
}
