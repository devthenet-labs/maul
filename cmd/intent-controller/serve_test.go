// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/controller/intent"
	"github.com/bitwise-media-group/patchy/internal/runnercfg"
)

// componentConfigMap is the kustomize component's own ConfigMap; the chart's
// intent-controller ConfigMap renders the same PATCHY_INTENT_* values
// (hack/chart-render-test.sh holds the two together).
const componentConfigMap = "../../deploy/kustomize/components/intent-controller/configmap.yaml"

// command builds the binary's command tree the way main does, with serve's
// RunE replaced so executing it only resolves the flags.
func command(t *testing.T, args ...string) (*cli.Options, *cobra.Command, *cobra.Command) {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("intent-controller", "test", opts)
	serve := newServeCmd(opts)
	serve.RunE = func(*cobra.Command, []string) error { return nil }
	root.AddCommand(serve)
	root.SetArgs(append([]string{"serve"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return opts, root, serve
}

// componentData reads the component ConfigMap's data.
func componentData(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(componentConfigMap)
	if err != nil {
		t.Fatalf("read %s: %v", componentConfigMap, err)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parse %s: %v", componentConfigMap, err)
	}
	if len(cm.Data) == 0 {
		t.Fatalf("%s has no data", componentConfigMap)
	}
	return cm.Data
}

// flagName maps an environment key onto the flag it sets, the way
// internal/cli binds them: PATCHY_ prefix off, underscores to dashes.
func flagName(key string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(key, "PATCHY_"), "_", "-"))
}

// sameValue compares a ConfigMap value with a flag default in the flag's own
// type, so 60s and 1m0s are the same duration.
func sameValue(f *pflag.Flag, value string) bool {
	if f.Value.Type() == "duration" {
		want, err1 := time.ParseDuration(value)
		got, err2 := time.ParseDuration(f.DefValue)
		return err1 == nil && err2 == nil && want == got
	}
	return f.DefValue == value
}

// TestComponentConfigBindsFlags holds the deployed configuration to the
// binary: every key the component sets is a flag intent-controller binds (a
// key that is not is silently ignored, which is how a renamed flag goes
// unnoticed), and every PATCHY_INTENT_* value is that flag's default, so the
// documented defaults and the running ones cannot drift apart.
func TestComponentConfigBindsFlags(t *testing.T) {
	_, root, serve := command(t)
	for key, value := range componentData(t) {
		name := flagName(key)
		f := serve.Flags().Lookup(name)
		if f == nil {
			f = root.PersistentFlags().Lookup(name)
		}
		if f == nil {
			t.Errorf("%s sets --%s, which intent-controller does not register", key, name)
			continue
		}
		if strings.HasPrefix(key, "PATCHY_INTENT_") && !sameValue(f, value) {
			t.Errorf("%s = %q, but --%s defaults to %q", key, value, name, f.DefValue)
		}
	}
}

// TestComponentConfigReachesSettings runs the component's keys through the
// environment, the way the Deployment's envFrom delivers them, and checks the
// reconcilers' settings come out as configured.
func TestComponentConfigReachesSettings(t *testing.T) {
	data := componentData(t)
	for key, value := range data {
		t.Setenv(key, value)
	}
	// Distinct from every default, so a value that reached settings by any
	// route but the environment would show.
	t.Setenv("PATCHY_INTENT_PLAN_MAX_TURNS", "7")
	t.Setenv("PATCHY_INTENT_BUILD_TIMEOUT", "61m")
	t.Setenv("PATCHY_INTENT_MULTI_REPO", "true")

	opts, root, _ := command(t)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	s, err := settings(opts, "patchy", "patchy-agents")
	if err != nil {
		t.Fatalf("settings refuses the component's configuration: %v", err)
	}
	want := intent.Settings{
		Namespace:            "patchy",
		AgentNamespace:       "patchy-agents",
		PollInterval:         time.Minute,
		ApprovalPollInterval: 30 * time.Second,
		PRPollInterval:       time.Minute,
		RateLimitFloor:       1000,
		MaxAttempts:          intent.DefaultMaxAttempts,
		MultiRepo:            true,
		Plan: intent.StageCeiling{MaxTurns: 7, TokenBudget: 200000, Timeout: 20 * time.Minute,
			IdleTimeout: 20 * time.Minute},
		Build: intent.StageCeiling{MaxTurns: 150, TokenBudget: 800000, Timeout: 61 * time.Minute,
			IdleTimeout: 20 * time.Minute},
		Revise: intent.StageCeiling{MaxTurns: 80, TokenBudget: 400000, Timeout: 45 * time.Minute,
			IdleTimeout: 20 * time.Minute},
		// Off unless the operator turns it on.
		Attribution:    attribution.ModeOff,
		JobDeadline:    90 * time.Minute,
		JobDeadlineMax: 4*time.Hour + 30*time.Minute,
		// No cost budgets until the broker counts cost.
		CostBudgets: false,
	}
	if s != want {
		t.Errorf("settings = %+v, want %+v", s, want)
	}
	if got := opts.Int("intent-max-concurrent-runs"); got != 1 {
		t.Errorf("intent-max-concurrent-runs = %d, want 1", got)
	}
	if got := opts.String("harnesses"); got != "claude" {
		t.Errorf("harnesses = %q, want claude", got)
	}
}

// TestResourceClasses: the classes reach intent-controller as the chart
// renders them, toJson leaving an unquoted cpu: 4 a JSON number; none set
// defines none, and the agent Jobs' default stays empty; a class the API
// server would refuse fails startup, naming it.
func TestResourceClasses(t *testing.T) {
	opts, root, _ := command(t)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	classes, err := resourceClasses(opts)
	if err != nil || classes != nil {
		t.Fatalf("no classes set = %v, %v; want none", classes, err)
	}
	if r, err := runnercfg.AgentResources(opts); err != nil || !r.IsZero() {
		t.Errorf("agent resources with no flags = %s, %v; want none", r, err)
	}

	t.Setenv("PATCHY_INTENT_RESOURCE_CLASSES",
		`{"large":{"limits":{"memory":"10Gi"},"requests":{"cpu":4,"memory":"8Gi"}}}`)
	opts, root, _ = command(t)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	classes, err = resourceClasses(opts)
	if err != nil {
		t.Fatal(err)
	}
	if large, ok := classes.Lookup("large"); !ok || large.String() != "requests cpu 4, memory 8Gi; limits memory 10Gi" {
		t.Errorf("classes = %s, want large", classes.Encode())
	}

	t.Setenv("PATCHY_INTENT_RESOURCE_CLASSES",
		`{"large":{"limits":{"memory":"6Gi"},"requests":{"cpu":4,"memory":"8Gi"}}}`)
	opts, root, _ = command(t)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := resourceClasses(opts); err == nil || !strings.Contains(err.Error(), `class "large"`) {
		t.Errorf("a class with its memory limit under its request = %v, want a startup error naming it", err)
	}
}

// TestSettingsDecouplesTheDeadlineFloor: --intent-job-deadline is a floor
// independent of the stage timeouts, so a build timeout raised to 3h starts
// under the default 90m floor (the build's Jobs alone stretch to 3h15m), as
// does a floor shorter than every stage. What is refused is a stage whose
// timeout plus 15m does not fit under --intent-job-deadline-max, or a floor
// that is not positive.
func TestSettingsDecouplesTheDeadlineFloor(t *testing.T) {
	load := func(t *testing.T, args ...string) (intent.Settings, error) {
		t.Helper()
		opts, root, _ := command(t, args...)
		if err := root.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		return settings(opts, "patchy", "patchy-agents")
	}
	for _, args := range [][]string{
		{"--intent-build-timeout", "3h"},
		{"--intent-job-deadline", "59m"},
		{"--intent-build-timeout", "4h15m"},
	} {
		if _, err := load(t, args...); err != nil {
			t.Errorf("settings(%v) = %v, want it to start", args, err)
		}
	}
	s, err := load(t, "--intent-build-timeout", "3h")
	if err != nil || s.JobDeadline != 90*time.Minute {
		t.Errorf("a 3h build kept floor %s, %v; want the default 90m untouched", s.JobDeadline, err)
	}
	for _, args := range [][]string{
		{"--intent-build-timeout", "4h16m"},
		{"--intent-plan-timeout", "2h", "--intent-job-deadline-max", "2h"},
		{"--intent-revise-timeout", "2h", "--intent-job-deadline-max", "2h14m"},
		{"--intent-job-deadline", "0s"},
	} {
		if _, err := load(t, args...); err == nil {
			t.Errorf("settings(%v) succeeded, want a startup error", args)
		}
	}
}

// TestBrokerBudgetsReachSettings: runs are granted cost budgets only under
// --broker-limit-unit cost, the hourly limit reaching the Project check with
// it; an hourly limit under the token unit, a negative one, an unknown unit
// or a deadline ceiling that is not positive stops startup.
func TestBrokerBudgetsReachSettings(t *testing.T) {
	load := func(t *testing.T, args ...string) (intent.Settings, error) {
		t.Helper()
		opts, root, _ := command(t, args...)
		if err := root.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		return settings(opts, "patchy", "patchy-agents")
	}
	s, err := load(t)
	if err != nil || s.CostBudgets || s.BrokerCostPerHour != 0 {
		t.Fatalf("default settings = budgets %v, per hour %d, %v; want neither", s.CostBudgets, s.BrokerCostPerHour, err)
	}
	s, err = load(t, "--broker-limit-unit", "cost", "--broker-cost-per-hour", "120000000")
	if err != nil || !s.CostBudgets || s.BrokerCostPerHour != 120000000 {
		t.Fatalf("cost settings = budgets %v, per hour %d, %v; want both", s.CostBudgets, s.BrokerCostPerHour, err)
	}
	s, err = load(t, "--broker-limit-unit", "cost")
	if err != nil || !s.CostBudgets || s.BrokerCostPerHour != 0 {
		t.Fatalf("cost with no hourly limit = budgets %v, per hour %d, %v", s.CostBudgets, s.BrokerCostPerHour, err)
	}
	for _, args := range [][]string{
		{"--broker-cost-per-hour", "1"},
		{"--broker-limit-unit", "cost", "--broker-cost-per-hour", "-1"},
		{"--broker-limit-unit", "dollars"},
		{"--intent-job-deadline-max", "0s"},
	} {
		if _, err := load(t, args...); err == nil {
			t.Errorf("settings(%v) succeeded, want a startup error", args)
		}
	}
}

// The intent controller cannot reach the broker under its NetworkPolicy.
// Agent pods can, so its startup must validate configuration without making
// even an advisory controller-side broker request.
func TestHarnessSkipsBrokerProbe(t *testing.T) {
	var calls atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer broker.Close()
	opts, root, _ := command(t, "--claude-agent-image=claude:1", "--broker-url="+broker.URL)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	runners, err := runnercfg.Runners(opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := harness(context.Background(), opts, fake.NewClientset(), "patchy-agents", runners)
	if err != nil {
		t.Fatal(err)
	}
	if got != "claude" {
		t.Errorf("harness = %q, want claude", got)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("intent controller made %d broker readiness requests, want none", n)
	}
}

// TestAgentDNSFlag: intent-controller registers --agent-dns (unprefixed, like
// the other agent Job flags), defaulting to the cluster resolver.
func TestAgentDNSFlag(t *testing.T) {
	_, _, serve := command(t)
	f := serve.Flags().Lookup("agent-dns")
	if f == nil {
		t.Fatal("--agent-dns is not registered")
	}
	if f.DefValue != "cluster" {
		t.Errorf("--agent-dns defaults to %q, want cluster", f.DefValue)
	}
}

// TestAgentsPausedFlag: intent-controller registers --agents-paused
// (unprefixed, like the other agent Job flags), off by default.
func TestAgentsPausedFlag(t *testing.T) {
	_, _, serve := command(t)
	f := serve.Flags().Lookup("agents-paused")
	if f == nil {
		t.Fatal("--agents-paused is not registered")
	}
	if f.DefValue != "false" {
		t.Errorf("--agents-paused defaults to %q, want false", f.DefValue)
	}
}

// TestAttributionModeReachesSettings: PATCHY_ATTRIBUTION_MODE (the chart's
// attribution.mode) reaches the reconcilers' settings, and an unknown mode
// stops startup rather than running unattributed.
func TestAttributionModeReachesSettings(t *testing.T) {
	for key, value := range componentData(t) {
		t.Setenv(key, value)
	}
	t.Setenv("PATCHY_ATTRIBUTION_MODE", "attribute")
	opts, root, _ := command(t)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	s, err := settings(opts, "patchy", "patchy-agents")
	if err != nil || s.Attribution != attribution.ModeAttribute {
		t.Fatalf("settings = %q, %v; want attribution attribute", s.Attribution, err)
	}

	t.Setenv("PATCHY_ATTRIBUTION_MODE", "enforce")
	opts, root, _ = command(t)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := settings(opts, "patchy", "patchy-agents"); err == nil ||
		!strings.Contains(err.Error(), "--attribution-mode") {
		t.Errorf("settings with mode enforce = %v, want the --attribution-mode refusal", err)
	}
}

// TestUsageMeasurementReachesMeter: measurement is off by default, and
// PATCHY_USAGE_MEASUREMENT with PATCHY_USAGE_BROKER_URL (the chart's wiring
// under attribution) turns the meter on; on without a broker URL stops
// startup.
func TestUsageMeasurementReachesMeter(t *testing.T) {
	for key, value := range componentData(t) {
		t.Setenv(key, value)
	}
	opts, root, _ := command(t)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if m, err := runnercfg.UsageMeter(opts, nil); err != nil || m.On() {
		t.Fatalf("default meter = %v, %v; want off", m, err)
	}
	t.Setenv("PATCHY_USAGE_MEASUREMENT", "true")
	opts, root, _ = command(t)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := runnercfg.UsageMeter(opts, nil); err == nil {
		t.Error("measurement on without a broker URL was accepted")
	}
	t.Setenv("PATCHY_USAGE_BROKER_URL", "http://patchy-egress-broker.patchy.svc:8082")
	opts, root, _ = command(t)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if m, err := runnercfg.UsageMeter(opts, nil); err != nil || !m.On() {
		t.Fatalf("meter = %v, %v; want on", m, err)
	}
}
