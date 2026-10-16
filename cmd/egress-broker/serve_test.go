// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/internal/cli"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("egress-broker", "test", opts)
	serve := newServeCmd(opts)
	serve.RunE = func(*cobra.Command, []string) error { return nil }
	root.AddCommand(serve)
	root.SetArgs(append([]string{"serve"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return opts
}

// isolateCloudEnv points every cloud SDK's ambient credential lookup at
// files that do not exist, so route construction reads nothing from the
// developer's machine.
func isolateCloudEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "aws-credentials"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("HOME", dir)
}

// writeGoogleCreds writes an authorized_user ADC file: constructing a token
// source from it reaches no network (tokens are fetched lazily).
func writeGoogleCreds(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adc.json")
	const adc = `{"type":"authorized_user","client_id":"id","client_secret":"secret","refresh_token":"rt"}`
	if err := os.WriteFile(path, []byte(adc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}

func TestUpstreams(t *testing.T) {
	isolateCloudEnv(t)
	writeGoogleCreds(t)
	tests := []struct {
		name    string
		args    []string
		want    []string
		hosts   map[string]string
		wantErr string
	}{
		{name: "none configured", wantErr: "no upstream configured"},
		{
			name:  "anthropic key",
			args:  []string{"--anthropic-api-key-file", "/k"},
			want:  []string{"anthropic"},
			hosts: map[string]string{"anthropic": "api.anthropic.com"},
		},
		{
			name: "anthropic token with custom base",
			args: []string{"--anthropic-api-key-file", "/k", "--anthropic-auth", "token",
				"--anthropic-base-url", "http://gateway.internal:8080"},
			want:  []string{"anthropic"},
			hosts: map[string]string{"anthropic": "gateway.internal:8080"},
		},
		{
			name:    "anthropic bad auth mode",
			args:    []string{"--anthropic-api-key-file", "/k", "--anthropic-auth", "oauth"},
			wantErr: `--anthropic-auth "oauth" is not key or token`,
		},
		{
			name:    "anthropic bad base url",
			args:    []string{"--anthropic-api-key-file", "/k", "--anthropic-base-url", "ftp://x"},
			wantErr: "not an absolute http(s) URL",
		},
		{
			name:  "bedrock default url",
			args:  []string{"--bedrock-region", "eu-west-2"},
			want:  []string{"bedrock"},
			hosts: map[string]string{"bedrock": "bedrock-runtime.eu-west-2.amazonaws.com"},
		},
		{
			name:    "bedrock bad base url",
			args:    []string{"--bedrock-region", "eu-west-2", "--bedrock-base-url", "/relative"},
			wantErr: "not an absolute http(s) URL",
		},
		{
			name:    "vertex without project",
			args:    []string{"--vertex-region", "us-east5"},
			wantErr: "--vertex-region requires --vertex-project",
		},
		{
			name:  "vertex default url",
			args:  []string{"--vertex-region", "us-east5", "--vertex-project", "p"},
			want:  []string{"vertex"},
			hosts: map[string]string{"vertex": "us-east5-aiplatform.googleapis.com"},
		},
		{
			name:    "vertex bad base url",
			args:    []string{"--vertex-region", "us-east5", "--vertex-project", "p", "--vertex-base-url", "nope"},
			wantErr: "not an absolute http(s) URL",
		},
		{
			name:    "foundry key without file",
			args:    []string{"--foundry-resource", "res"},
			wantErr: "--foundry-auth key requires --foundry-api-key-file",
		},
		{
			name:  "foundry key",
			args:  []string{"--foundry-resource", "res", "--foundry-api-key-file", "/f"},
			want:  []string{"foundry"},
			hosts: map[string]string{"foundry": "res.services.ai.azure.com"},
		},
		{
			name:  "foundry base url alone enables the route",
			args:  []string{"--foundry-base-url", "https://foundry.example", "--foundry-api-key-file", "/f"},
			want:  []string{"foundry"},
			hosts: map[string]string{"foundry": "foundry.example"},
		},
		{
			name:    "foundry bad base url",
			args:    []string{"--foundry-base-url", "::", "--foundry-api-key-file", "/f"},
			wantErr: "upstream URL",
		},
		{
			name:    "foundry bad auth mode",
			args:    []string{"--foundry-resource", "res", "--foundry-auth", "password"},
			wantErr: `--foundry-auth "password" is not key or entra`,
		},
		{
			name: "every route",
			args: []string{"--anthropic-api-key-file", "/k", "--bedrock-region", "us-east-1",
				"--vertex-region", "us-east5", "--vertex-project", "p",
				"--foundry-resource", "res", "--foundry-api-key-file", "/f"},
			want: []string{"anthropic", "bedrock", "foundry", "vertex"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes, err := upstreams(t.Context(), serveOpts(t, tt.args...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("upstreams error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("upstreams: %v", err)
			}
			if got := slices.Sorted(maps.Keys(routes)); !slices.Equal(got, tt.want) {
				t.Errorf("routes = %v, want %v", got, tt.want)
			}
			for name, host := range tt.hosts {
				if got := routes[name].Target.Host; got != host {
					t.Errorf("%s target host = %q, want %q", name, got, host)
				}
			}
			if r, ok := routes["vertex"]; ok && (r.Project != "p" || r.Location != "us-east5") {
				t.Errorf("vertex route admits %q/%q, want p/us-east5", r.Project, r.Location)
			}
		})
	}
}

// TestFoundryEntraRoute: entra mode needs no key file; the credential is the
// broker's ambient Azure identity, resolved lazily.
func TestFoundryEntraRoute(t *testing.T) {
	isolateCloudEnv(t)
	routes, err := upstreams(t.Context(), serveOpts(t, "--foundry-resource", "res", "--foundry-auth", "entra"))
	if err != nil {
		t.Fatalf("upstreams: %v", err)
	}
	r, ok := routes["foundry"]
	if !ok || r.Target.Host != "res.services.ai.azure.com" || r.Credential == nil {
		t.Errorf("foundry route = %+v, want an entra route on res.services.ai.azure.com", r)
	}
}

// TestAnthropicCredentialMode: the --anthropic-auth mode decides which
// header carries the mounted key.
func TestAnthropicCredentialMode(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("sk-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		mode, header, want string
	}{
		{"key", "X-Api-Key", "sk-test"},
		{"token", "Authorization", "Bearer sk-test"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			routes, err := upstreams(t.Context(),
				serveOpts(t, "--anthropic-api-key-file", key, "--anthropic-auth", tt.mode))
			if err != nil {
				t.Fatalf("upstreams: %v", err)
			}
			req := httptest.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
			if err := routes["anthropic"].Credential(context.Background(), req); err != nil {
				t.Fatalf("credential: %v", err)
			}
			if got := req.Header.Get(tt.header); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	const cfg = `apiVersion: v1
kind: Config
current-context: test
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: test
    context:
      cluster: test
      user: test
users:
  - name: test
    user:
      token: t
`
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestServeStartupErrors: every misconfiguration fails startup with its
// cause, before a listener is bound.
func TestServeStartupErrors(t *testing.T) {
	isolateCloudEnv(t)
	writeGoogleCreds(t)
	kc := writeKubeconfig(t)
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing kubeconfig",
			args:    []string{"--kubeconfig", filepath.Join(t.TempDir(), "absent")},
			wantErr: "kubernetes config",
		},
		{name: "no upstream", args: []string{"--kubeconfig", kc}, wantErr: "no upstream configured"},
		{
			name: "invalid broker config",
			args: []string{"--kubeconfig", kc, "--anthropic-api-key-file", "/k",
				"--agent-namespace", ""},
			wantErr: "agent namespace and service account are required",
		},
		{
			name:    "vertex project the engine refuses",
			args:    []string{"--kubeconfig", kc, "--vertex-region", "us-east5", "--vertex-project", "a/b"},
			wantErr: "upstream vertex needs a project",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := serve(t.Context(), serveOpts(t, tt.args...))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tt.wantErr) {
				t.Fatalf("serve error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestServeStopsOnCancel: a valid configuration binds both listeners (on
// ephemeral ports) and returns cleanly once its context is cancelled.
func TestServeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	opts := serveOpts(t, "--kubeconfig", writeKubeconfig(t), "--listen-addr", "127.0.0.1:0",
		"--health-addr", "127.0.0.1:0", "--anthropic-api-key-file", "/k", "--beta-denylist", "none")
	if err := serve(ctx, opts); err != nil {
		t.Fatalf("serve after cancel = %v, want nil", err)
	}
}

// TestServeListenError: a listener that cannot bind fails the broker.
func TestServeListenError(t *testing.T) {
	opts := serveOpts(t, "--kubeconfig", writeKubeconfig(t), "--listen-addr", "127.0.0.1:not-a-port",
		"--health-addr", "127.0.0.1:0", "--anthropic-api-key-file", "/k")
	if err := serve(t.Context(), opts); err == nil {
		t.Fatal("serve with an unbindable listen address succeeded")
	}
}

// TestUpstreamFlagSurface pins the base-URL flags' defaults and help text,
// byte for byte: the defaults moved into internal/broker's exported
// constants (so the egress proxy's deny table can be pinned to them), and
// what an operator reads in --help and sets in values must not move with
// them.
func TestUpstreamFlagSurface(t *testing.T) {
	cmd := newServeCmd(cli.NewOptions())
	for _, c := range []struct{ flag, def, usage string }{
		{"anthropic-base-url", "https://api.anthropic.com", "Anthropic API base URL"},
		{"bedrock-base-url", "",
			"Bedrock runtime base URL (default: https://bedrock-runtime.<bedrock-region>.amazonaws.com)"},
		{"vertex-base-url", "", "Vertex AI base URL (default: https://<vertex-region>-aiplatform.googleapis.com)"},
		{"foundry-base-url", "", "Foundry base URL (default: https://<foundry-resource>.services.ai.azure.com)"},
	} {
		f := cmd.Flags().Lookup(c.flag)
		if f == nil {
			t.Errorf("--%s is not defined", c.flag)
			continue
		}
		if f.DefValue != c.def || f.Usage != c.usage {
			t.Errorf("--%s = default %q, usage %q; want %q, %q", c.flag, f.DefValue, f.Usage, c.def, c.usage)
		}
	}
}

// TestUsageConfig: the usage endpoint is off by default, needs an address
// and callers together, and expands each <namespace>:<serviceaccount> into
// its ServiceAccount username.
func TestUsageConfig(t *testing.T) {
	addr, cfg, err := usageConfig(serveOpts(t))
	if err != nil || addr != "" || cfg.Enabled() {
		t.Fatalf("default: addr %q cfg %+v err %v, want off", addr, cfg, err)
	}
	for name, args := range map[string][]string{
		"addr only":    {"--usage-addr", ":8082"},
		"callers only": {"--usage-callers", "patchy:a"},
		"bad caller":   {"--usage-addr", ":8082", "--usage-callers", "patchy"},
		"empty name":   {"--usage-addr", ":8082", "--usage-callers", "patchy:"},
		"extra colon":  {"--usage-addr", ":8082", "--usage-callers", "patchy:a:b"},
	} {
		if _, _, err := usageConfig(serveOpts(t, args...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	addr, cfg, err = usageConfig(serveOpts(t, "--usage-addr", ":8082",
		"--usage-callers", "patchy:patchy-intent-controller,patchy:patchy-evaluation-controller"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"system:serviceaccount:patchy:patchy-intent-controller",
		"system:serviceaccount:patchy:patchy-evaluation-controller"}
	if addr != ":8082" || !slices.Equal(cfg.Callers, want) || cfg.Audience != "patchy-egress-broker-usage" {
		t.Errorf("addr %q cfg %+v", addr, cfg)
	}
}
