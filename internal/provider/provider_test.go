// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package provider

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/harness"
	"github.com/devthenet-labs/maul/internal/model"
)

const broker = "http://maul-egress-broker.maul.svc.cluster.local:8080"

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"anthropic ok", Config{Name: Anthropic, BrokerURL: broker}, ""},
		{"unknown provider", Config{Name: "acme", BrokerURL: broker}, "not one of"},
		// OpenAI is a broker route, never a claude runner's provider.
		{"openai is not a claude provider", Config{Name: OpenAI, BrokerURL: broker}, "not one of"},
		{"no broker url", Config{Name: Anthropic}, "broker URL is required"},
		{"bedrock needs region", Config{Name: Bedrock, BrokerURL: broker}, "region is required"},
		{"bedrock ok", Config{Name: Bedrock, BrokerURL: broker, Region: "us-east-1"}, ""},
		{"vertex needs project", Config{Name: Vertex, BrokerURL: broker, Region: "europe-west1"},
			"region and project id"},
		{"vertex ok", Config{Name: Vertex, BrokerURL: broker, Region: "europe-west1", ProjectID: "p"}, ""},
		{"foundry ok", Config{Name: Foundry, BrokerURL: broker}, ""},
		{"credential extra env", Config{Name: Anthropic, BrokerURL: broker,
			ExtraEnv: map[string]string{"ANTHROPIC_API_KEY": "sk"}}, "cannot be set here"},
		{"gateway extra env", Config{Name: Anthropic, BrokerURL: broker,
			ExtraEnv: map[string]string{"ANTHROPIC_BASE_URL": "https://elsewhere"}}, "cannot be set here"},
		{"placeholder extra env", Config{Name: Anthropic, BrokerURL: broker,
			ExtraEnv: map[string]string{PlaceholderAuthEnv: "operator-token"}}, "cannot be set here"},
		{"benign extra env", Config{Name: Anthropic, BrokerURL: broker,
			ExtraEnv: map[string]string{"HTTPS_PROXY": "http://proxy"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestEnv(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want map[string]string
	}{
		{
			"anthropic",
			Config{Name: Anthropic, BrokerURL: broker + "/"},
			map[string]string{
				"ANTHROPIC_BASE_URL":                       broker + "/anthropic",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			},
		},
		{
			"bedrock",
			Config{Name: Bedrock, BrokerURL: broker, Region: "us-east-1"},
			map[string]string{
				"CLAUDE_CODE_USE_BEDROCK":                  "1",
				"CLAUDE_CODE_SKIP_BEDROCK_AUTH":            "1",
				"ANTHROPIC_BEDROCK_BASE_URL":               broker + "/bedrock",
				"AWS_REGION":                               "us-east-1",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			},
		},
		{
			"vertex",
			Config{Name: Vertex, BrokerURL: broker, Region: "europe-west1", ProjectID: "proj"},
			map[string]string{
				"CLAUDE_CODE_USE_VERTEX":                   "1",
				"CLAUDE_CODE_SKIP_VERTEX_AUTH":             "1",
				"ANTHROPIC_VERTEX_BASE_URL":                broker + "/vertex",
				"ANTHROPIC_VERTEX_PROJECT_ID":              "proj",
				"CLOUD_ML_REGION":                          "europe-west1",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			},
		},
		{
			"foundry",
			Config{Name: Foundry, BrokerURL: broker},
			map[string]string{
				"CLAUDE_CODE_USE_FOUNDRY":                  "1",
				"CLAUDE_CODE_SKIP_FOUNDRY_AUTH":            "1",
				"ANTHROPIC_FOUNDRY_BASE_URL":               broker + "/foundry",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Env(tt.cfg, nil)
			if !maps.Equal(got, tt.want) {
				t.Fatalf("Env() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBaseURLEnvNames: every name Env points at the broker, for every
// provider, is one internal/jobs reads a host from, and every one of those is
// a gateway name. A provider whose route rode a name missing here would leave
// a pod without a resolver unable to reach the broker.
func TestBaseURLEnvNames(t *testing.T) {
	cfgs := []Config{
		{Name: Anthropic, BrokerURL: broker},
		{Name: Bedrock, BrokerURL: broker, Region: "us-east-1"},
		{Name: Vertex, BrokerURL: broker, Region: "europe-west1", ProjectID: "proj"},
		{Name: Foundry, BrokerURL: broker},
	}
	if len(cfgs) != len(ClaudeNames) {
		t.Fatalf("the test covers %d providers, and there are %d", len(cfgs), len(ClaudeNames))
	}
	seen := map[string]bool{}
	envs := map[string]map[string]string{"codex": CodexEnv(broker)}
	for _, cfg := range cfgs {
		envs[cfg.Name] = Env(cfg, nil)
	}
	for name, env := range envs {
		for k, v := range env {
			if !strings.HasPrefix(v, broker) {
				continue
			}
			if !slices.Contains(BaseURLEnvNames, k) {
				t.Errorf("provider %s points %s at the broker, and BaseURLEnvNames does not list it", name, k)
			}
			seen[k] = true
		}
	}
	for _, k := range BaseURLEnvNames {
		if !slices.Contains(GatewayEnvNames, k) {
			t.Errorf("BaseURLEnvNames lists %s, which is not a gateway name", k)
		}
		if !seen[k] {
			t.Errorf("BaseURLEnvNames lists %s, which no provider sets", k)
		}
	}
}

func TestEnvModelMapAndExtra(t *testing.T) {
	cfg := Config{Name: Anthropic, BrokerURL: broker, ExtraEnv: map[string]string{"HTTPS_PROXY": "http://p"}}
	got := Env(cfg, map[string]string{"anthropic/claude-sonnet-5": "claude-sonnet-5"})
	if got["MAUL_MODEL_MAP"] != "anthropic/claude-sonnet-5=claude-sonnet-5" {
		t.Errorf("MAUL_MODEL_MAP = %q", got["MAUL_MODEL_MAP"])
	}
	if got["HTTPS_PROXY"] != "http://p" {
		t.Errorf("ExtraEnv not passed through: %v", got)
	}
}

func TestEffectiveModelMap(t *testing.T) {
	models := model.Builtins()

	t.Run("bedrock derives prefixed ids", func(t *testing.T) {
		got, err := EffectiveModelMap(Config{Name: Bedrock, BrokerURL: broker, Region: "us-east-1"}, models)
		if err != nil {
			t.Fatal(err)
		}
		if id := got["anthropic/claude-sonnet-5"]; id != "us.anthropic.claude-sonnet-5" {
			t.Errorf("sonnet id = %q", id)
		}
		if _, ok := got["openai/gpt-5.3-codex"]; ok {
			t.Error("non-claude model mapped")
		}
	})
	t.Run("bedrock eu region", func(t *testing.T) {
		got, err := EffectiveModelMap(Config{Name: Bedrock, BrokerURL: broker, Region: "eu-west-2"}, models)
		if err != nil {
			t.Fatal(err)
		}
		if id := got["anthropic/claude-haiku-4-5"]; id != "eu.anthropic.claude-haiku-4-5" {
			t.Errorf("haiku id = %q", id)
		}
	})
	t.Run("bedrock ap region derives apac", func(t *testing.T) {
		got, err := EffectiveModelMap(Config{Name: Bedrock, BrokerURL: broker, Region: "ap-southeast-2"}, models)
		if err != nil {
			t.Fatal(err)
		}
		if id := got["anthropic/claude-sonnet-5"]; id != "apac.anthropic.claude-sonnet-5" {
			t.Errorf("sonnet id = %q", id)
		}
	})
	t.Run("bedrock underivable region errors", func(t *testing.T) {
		_, err := EffectiveModelMap(Config{Name: Bedrock, BrokerURL: broker, Region: "ca-central-1"}, models)
		if err == nil || !strings.Contains(err.Error(), "region prefix") {
			t.Fatalf("err = %v, want region-prefix error", err)
		}
	})
	t.Run("bedrock explicit prefix wins", func(t *testing.T) {
		got, err := EffectiveModelMap(Config{
			Name: Bedrock, BrokerURL: broker, Region: "ca-central-1", RegionPrefix: "us",
		}, models)
		if err != nil {
			t.Fatal(err)
		}
		if id := got["anthropic/claude-sonnet-5"]; id != "us.anthropic.claude-sonnet-5" {
			t.Errorf("sonnet id = %q", id)
		}
	})
	t.Run("vertex derives bare ids", func(t *testing.T) {
		got, err := EffectiveModelMap(Config{
			Name: Vertex, BrokerURL: broker, Region: "europe-west1", ProjectID: "p",
		}, models)
		if err != nil {
			t.Fatal(err)
		}
		if id := got["anthropic/claude-opus-5"]; id != "claude-opus-5" {
			t.Errorf("opus id = %q", id)
		}
	})
	t.Run("operator override wins", func(t *testing.T) {
		got, err := EffectiveModelMap(Config{
			Name: Bedrock, BrokerURL: broker, Region: "us-east-1",
			ModelMap: map[string]string{"anthropic/claude-sonnet-5": "us.anthropic.claude-sonnet-5-v2:0"},
		}, models)
		if err != nil {
			t.Fatal(err)
		}
		if id := got["anthropic/claude-sonnet-5"]; id != "us.anthropic.claude-sonnet-5-v2:0" {
			t.Errorf("sonnet id = %q", id)
		}
	})
	t.Run("foundry is the operator map exactly", func(t *testing.T) {
		mm := map[string]string{"anthropic/claude-sonnet-5": "my-sonnet-deployment"}
		got, err := EffectiveModelMap(Config{Name: Foundry, BrokerURL: broker, ModelMap: mm}, models)
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(got, mm) {
			t.Errorf("map = %v, want %v", got, mm)
		}
	})
}

// TestPlaceholderAuthChannel: the placeholder rides one of claude's own
// credential channels, which is what keeps it maul-owned — internal/jobs
// reserves and filters every such channel, so no operator env reaches it —
// and the gateway env never emits it (jobs sets it on brokered pods only).
func TestPlaceholderAuthChannel(t *testing.T) {
	if !slices.Contains(harness.NewClaude().EnvKeys(), PlaceholderAuthEnv) {
		t.Errorf("%s is not a claude credential channel", PlaceholderAuthEnv)
	}
	if PlaceholderAuthToken == "" || !strings.Contains(PlaceholderAuthToken, "placeholder") {
		t.Errorf("PlaceholderAuthToken = %q, want a self-describing non-secret", PlaceholderAuthToken)
	}
	for _, name := range ClaudeNames {
		if _, ok := Env(Config{Name: name, BrokerURL: broker}, nil)[PlaceholderAuthEnv]; ok {
			t.Errorf("provider %s gateway env emits %s; only internal/jobs sets it", name, PlaceholderAuthEnv)
		}
	}
}

func TestValidateCoverage(t *testing.T) {
	models := model.Builtins()
	cfg := Config{Name: Foundry, BrokerURL: broker}
	mm := map[string]string{"anthropic/claude-sonnet-5": "sonnet-deploy"}

	if err := ValidateCoverage(cfg, models, mm, []string{"anthropic/claude-sonnet-5"}); err != nil {
		t.Errorf("covered model rejected: %v", err)
	}
	err := ValidateCoverage(cfg, models, mm, []string{"anthropic/claude-sonnet-5", "anthropic/claude-opus-5"})
	if err == nil || !strings.Contains(err.Error(), "anthropic/claude-opus-5") {
		t.Errorf("uncovered model accepted: %v", err)
	}
	// A model another harness runs is not this runner's problem.
	if err := ValidateCoverage(cfg, models, mm, []string{"openai/gpt-5.3-codex"}); err != nil {
		t.Errorf("codex model demanded a claude map entry: %v", err)
	}
	// Anthropic never needs a map.
	if err := ValidateCoverage(Config{Name: Anthropic, BrokerURL: broker}, models, nil,
		[]string{"anthropic/claude-sonnet-5"}); err != nil {
		t.Errorf("anthropic coverage failed: %v", err)
	}
}

func TestModelMapCodec(t *testing.T) {
	in := map[string]string{
		"anthropic/claude-sonnet-5": "us.anthropic.claude-sonnet-5",
		"anthropic/claude-opus-5":   "us.anthropic.claude-opus-5",
	}
	wire := FormatModelMap(in)
	want := "anthropic/claude-opus-5=us.anthropic.claude-opus-5," +
		"anthropic/claude-sonnet-5=us.anthropic.claude-sonnet-5"
	if wire != want {
		t.Errorf("FormatModelMap = %q, want %q (sorted)", wire, want)
	}
	out, err := ParseModelMap(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(in, out) {
		t.Errorf("round trip = %v, want %v", out, in)
	}

	if m, err := ParseModelMap("  "); err != nil || m != nil {
		t.Errorf("blank map = %v, %v; want nil, nil", m, err)
	}
	if _, err := ParseModelMap("no-equals"); err == nil {
		t.Error("malformed entry accepted")
	}
	if _, err := ParseModelMap("=x"); err == nil {
		t.Error("empty canonical id accepted")
	}
}

func TestBareModelID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"claude-sonnet-5", "claude-sonnet-5"},
		{" Claude-Sonnet-5 ", "claude-sonnet-5"},
		{"anthropic/claude-sonnet-5", "claude-sonnet-5"},
		{"anthropic.claude-sonnet-5-v1:0", "claude-sonnet-5-v1:0"},
		{"us.anthropic.claude-sonnet-5-v1:0", "claude-sonnet-5-v1:0"},
		{"global.anthropic.claude-sonnet-5-v1:0", "claude-sonnet-5-v1:0"},
		{"us-gov.anthropic.claude-sonnet-5-v1:0", "claude-sonnet-5-v1:0"},
		{"apac.anthropic.claude-opus-5-20260401-v1:0", "claude-opus-5-20260401-v1:0"},
		{"xx.anthropic.claude-sonnet-5", "xx.anthropic.claude-sonnet-5"},
		{"claude-sonnet-5@20260514", "claude-sonnet-5@20260514"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := BareModelID(tt.in); got != tt.want {
			t.Errorf("BareModelID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestBedrockPrefixesNormalize: every id EffectiveModelMap derives for
// bedrock normalizes back to the vendor id, so the broker's allowlist
// admits what the controllers configure.
func TestBedrockPrefixesNormalize(t *testing.T) {
	for _, region := range []string{"us-east-1", "eu-west-1", "ap-northeast-1"} {
		prefix, err := bedrockPrefix(Config{Region: region})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(BedrockGeoPrefixes, prefix) {
			t.Errorf("derived prefix %q for %s is not in BedrockGeoPrefixes", prefix, region)
		}
		if got := BareModelID(prefix + ".anthropic.claude-sonnet-5"); got != "claude-sonnet-5" {
			t.Errorf("%s: BareModelID = %q", region, got)
		}
	}
}

// TestCodexGatewayNames: the codex gateway's names are gateway names (so
// internal/jobs reserves them and a repository image's ENV may not name
// them), its route is a base-URL name (so a pod with no resolver gets the
// broker pinned), and CodexEnv and CodexGateway agree on them.
func TestCodexGatewayNames(t *testing.T) {
	for _, n := range []string{CodexBaseURLEnv, CodexTokenEnv, CodexPlaceholderKeyEnv} {
		if !slices.Contains(GatewayEnvNames, n) {
			t.Errorf("%s is not a gateway name", n)
		}
	}
	if !slices.Contains(BaseURLEnvNames, CodexBaseURLEnv) {
		t.Errorf("%s is not a base-URL name", CodexBaseURLEnv)
	}
	env := CodexEnv("http://broker.maul.svc:8080/")
	if got := env[CodexBaseURLEnv]; got != "http://broker.maul.svc:8080/openai/v1" || len(env) != 1 {
		t.Errorf("CodexEnv = %v", env)
	}
	gw := CodexGateway(env[CodexBaseURLEnv])
	if gw.BaseURL != env[CodexBaseURLEnv] || gw.KeyEnv != CodexPlaceholderKeyEnv ||
		gw.HeaderEnv[BrokerTokenHeader] != CodexTokenEnv || len(gw.HeaderEnv) != 1 {
		t.Errorf("CodexGateway = %+v", gw)
	}
}
