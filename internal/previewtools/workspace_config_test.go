// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolexec"
)

// wsConfig is an agents workspace's config: a cli surface.
func wsConfig() Config {
	return Config{
		Mode: ModeWorkspace,
		Workspace: &Workspace{
			Build:               []string{"go", "build", "-o", "/work/bin/bb", "./cmd/bb"},
			Binary:              "/work/bin/bb",
			BuildTimeoutSeconds: 600,
			SearchPath:          "/usr/local/go/bin:/usr/bin:/bin",
		},
		MaxToolCalls:   50,
		TimeoutSeconds: 1800,
	}
}

func TestWorkspaceConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Config)
		ok   bool
	}{
		{"cli", func(*Config) {}, true},
		{"library", func(c *Config) {
			c.Workspace.Build, c.Workspace.Binary, c.Workspace.Language = nil, "", "go"
		}, true},
		{"explicit slot mode with a workspace", func(c *Config) { c.Mode = ModeSlot }, false},
		{"unknown mode", func(c *Config) { c.Mode = "agents" }, false},
		{"no workspace", func(c *Config) { c.Workspace = nil }, false},
		{"components", func(c *Config) {
			c.Components = []Component{{Key: "api", Path: "/", Addr: "10.0.0.1:80"}}
		}, false},
		{"a preview host", func(c *Config) { c.Host = "x.example" }, false},
		{"a browser", func(c *Config) { c.Browser = true }, false},
		{"the slot sandbox flag", func(c *Config) { c.Sandbox = true }, false},
		{"a viewer is allowed", func(c *Config) { c.Viewer = "maul-evaluator" }, true},
		{"a bad viewer", func(c *Config) { c.Viewer = "has space" }, false},
		{"the 75m ceiling", func(c *Config) { c.TimeoutSeconds = 75 * 60 }, true},
		{"past the 75m ceiling", func(c *Config) { c.TimeoutSeconds = 75*60 + 1 }, false},
		{"cli and library", func(c *Config) { c.Workspace.Language = "go" }, false},
		{"neither cli nor library", func(c *Config) { c.Workspace.Build, c.Workspace.Binary = nil, "" }, false},
		{"a build that writes nowhere", func(c *Config) { c.Workspace.Build = []string{"make"} }, false},
		{"a binary outside /work/bin", func(c *Config) { c.Workspace.Binary = "/usr/bin/bb" }, false},
		{"a library in python", func(c *Config) {
			c.Workspace.Build, c.Workspace.Binary, c.Workspace.Language = nil, "", "python"
		}, false},
		{"a 59s build", func(c *Config) { c.Workspace.BuildTimeoutSeconds = 59 }, false},
		{"a 20m build", func(c *Config) { c.Workspace.BuildTimeoutSeconds = 1200 }, true},
		{"a 21m build", func(c *Config) { c.Workspace.BuildTimeoutSeconds = 1260 }, false},
		{"no search path", func(c *Config) { c.Workspace.SearchPath = "" }, false},
		{"a relative search path", func(c *Config) { c.Workspace.SearchPath = "/bin:bin" }, false},
		{"a control character in the path", func(c *Config) { c.Workspace.SearchPath = "/bin\n" }, false},
		{"profile env", func(c *Config) {
			c.Workspace.Env = []toolexec.EnvVar{{Name: "GOPROXY", Value: "http://proxy:3128/go"},
				{Name: "GOTOOLCHAIN", Value: "local"}, {Name: "MAUL_NETWORK_PROFILE", Value: "go"}}
		}, true},
		{"a repeated variable", func(c *Config) {
			c.Workspace.Env = []toolexec.EnvVar{{Name: "GOPROXY", Value: "a"}, {Name: "GOPROXY", Value: "b"}}
		}, false},
		{"a bad variable name", func(c *Config) { c.Workspace.Env = []toolexec.EnvVar{{Name: "A=B", Value: "x"}} }, false},
		{"a NUL in a value", func(c *Config) { c.Workspace.Env = []toolexec.EnvVar{{Name: "A", Value: "x\x00"}} }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := wsConfig()
			w := *c.Workspace
			c.Workspace = &w
			tt.mut(&c)
			err := c.Validate()
			if (err == nil) != tt.ok {
				t.Fatalf("Validate = %v, want ok %v", err, tt.ok)
			}
		})
	}
}

// TestSlotConfigRefusesWorkspace: a slot config naming a workspace is
// refused, and a slot config's JSON is unchanged by the workspace fields.
func TestSlotConfigRefusesWorkspace(t *testing.T) {
	c := baseConfig()
	c.Workspace = wsConfig().Workspace
	if err := c.Validate(); err == nil {
		t.Fatal("a slot config with a workspace was admitted")
	}
	b, err := json.Marshal(baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "mode") || strings.Contains(string(b), "workspace") {
		t.Fatalf("a slot config's JSON names the workspace fields: %s", b)
	}
	slot := baseConfig()
	slot.Mode = ModeSlot
	if err := slot.Validate(); err != nil {
		t.Fatalf("an explicit slot mode was refused: %v", err)
	}
}

// TestWorkspaceConfigRoundTrip: what ic writes, the relay parses.
func TestWorkspaceConfigRoundTrip(t *testing.T) {
	c := wsConfig()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseConfig(b)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if !got.IsWorkspace() || got.Workspace.Binary != c.Workspace.Binary {
		t.Fatalf("round trip lost the workspace: %+v", got)
	}
}

// TestWorkspaceBoundsMatchCRD holds the relay's bounds and the API's
// constants to the PreviewVerification CRD's markers (SPEC S2-08, the
// lockstep test S2-01b left): base runs, ledger builds, the agents
// timeout, the cli build timeout and the binary's depth.
func TestWorkspaceBoundsMatchCRD(t *testing.T) {
	b, err := os.ReadFile("../../deploy/kustomize/base/crds/maul.run_previewverifications.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatal(err)
	}
	v := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)
	schema := sub(v, "schema", "openAPIV3Schema", "properties")
	status := sub(schema, "status", "properties")
	spec := sub(schema, "spec", "properties")
	cli := sub(spec, "workspace", "properties", "surface", "properties", "cli", "properties")
	check := func(what string, got, want any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: CRD %v, Go %v", what, got, want)
		}
	}
	check("status.base.runs maxItems", sub(status, "base", "properties", "runs")["maxItems"], maulv1.MaxBaseRuns)
	check("status.ledger.builds maxItems", sub(status, "ledger", "properties", "builds")["maxItems"],
		maulv1.MaxLedgerBuilds)
	check("relay base runs", MaxBaseRuns, maulv1.MaxBaseRuns)
	if maxBuilds > maulv1.MaxLedgerBuilds {
		t.Errorf("the relay records %d builds, the CRD keeps %d", maxBuilds, maulv1.MaxLedgerBuilds)
	}
	rule := fmt.Sprint(schema["spec"].(map[string]any)["x-kubernetes-validations"])
	agents := fmt.Sprintf("duration(''%dm'')", int(maulv1.MaxAgentsVerificationTimeout/time.Minute))
	if !strings.Contains(rule, strings.ReplaceAll(agents, "''", "'")) && !strings.Contains(rule, agents) {
		t.Errorf("the CRD's timeout rule does not name %s: %s", agents, rule)
	}
	check("workspace timeout ceiling", MaxWorkspaceTimeoutSeconds, int(maulv1.MaxAgentsVerificationTimeout/time.Second))
	buildRule := fmt.Sprint(cli["buildTimeout"].(map[string]any)["x-kubernetes-validations"])
	for _, d := range []time.Duration{time.Duration(MinBuildTimeoutSeconds) * time.Second,
		time.Duration(MaxBuildTimeoutSeconds) * time.Second} {
		if !strings.Contains(buildRule, fmt.Sprintf("duration('%dm')", int(d/time.Minute))) {
			t.Errorf("the CRD's build timeout rule does not name %s: %s", d, buildRule)
		}
	}
	check("build timeout ceiling", time.Duration(MaxBuildTimeoutSeconds)*time.Second, maulv1.MaxCLIBuildTimeout)
	check("shim build ceiling", toolexec.MaxBuildTimeout, maulv1.MaxCLIBuildTimeout)
	if p := fmt.Sprint(cli["binary"].(map[string]any)["pattern"]); !strings.HasSuffix(p, "{1,2}$") {
		t.Errorf("the CRD's binary pattern %q does not hold it to two names below /work/bin", p)
	}
	check("cli build args maxItems", cli["build"].(map[string]any)["maxItems"], maulv1.MaxCLIBuildArgs)
}

func sub(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}
