// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/devthenet-labs/maul/internal/runnercfg"
)

// TestRetiredCheckReviewFlags ("evaluator observed"): the check reviewer
// is retired, so --intent-checkreview-model and
// --intent-checkreview-max-turns stay registered, hidden, for one release:
// set, from the command line or the environment (the chart renders both
// until it drops them), they are accepted whatever they hold, ignored, and
// each is warned about once at startup, naming the migration. Unset, there
// is no warning.
func TestRetiredCheckReviewFlags(t *testing.T) {
	on := []string{"--intent-previews-enabled", "--intent-evaluator"}
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		want []string
	}{
		{"unset", nil, on, nil},
		{"on the command line", nil, append(on, "--intent-checkreview-model", "not-a-model",
			"--intent-checkreview-max-turns", "0"),
			[]string{"intent-checkreview-model", "intent-checkreview-max-turns"}},
		{"from the environment", map[string]string{"MAUL_INTENT_CHECKREVIEW_MODEL": "openai/gpt-5.3-codex",
			"MAUL_INTENT_CHECKREVIEW_MAX_TURNS": "5000"}, on,
			[]string{"intent-checkreview-model", "intent-checkreview-max-turns"}},
		{"the model alone, without the evaluator", nil, []string{"--intent-checkreview-model", "x"},
			[]string{"intent-checkreview-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			opts := resolved(t, tc.args...)
			if _, err := settings(opts, "maul", "maul-agents"); err != nil {
				t.Fatalf("settings(%v) = %v, want the retired flags ignored", tc.args, err)
			}
			var buf bytes.Buffer
			logRetiredFlags(t.Context(), slog.New(slog.NewTextHandler(&buf, nil)), opts)
			out := buf.String()
			if len(tc.want) == 0 {
				if out != "" {
					t.Errorf("startup logged %q, want nothing", out)
				}
				return
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) != len(tc.want) {
				t.Fatalf("startup logged %d lines, want %d: %q", len(lines), len(tc.want), out)
			}
			for i, flag := range tc.want {
				for _, part := range []string{"level=WARN", "--" + flag, "retired",
					"docs/intents/evaluator.md#migrating-from-saved-checks"} {
					if !strings.Contains(lines[i], part) {
						t.Errorf("warning %q does not say %q", lines[i], part)
					}
				}
			}
		})
	}
	_, _, serve := command(t)
	for _, name := range []string{"intent-checkreview-model", "intent-checkreview-max-turns"} {
		if f := serve.Flags().Lookup(name); f == nil || !f.Hidden {
			t.Errorf("--%s = %+v, want it registered and hidden", name, f)
		}
	}
}

// TestRetiredCheckReviewModelNeverResolved: the retired check reviewer's
// model is not resolved at startup, so one no intent harness runs stops
// nothing.
func TestRetiredCheckReviewModelNeverResolved(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer broker.Close()
	opts := resolved(t, "--claude-agent-image=claude:1", "--broker-url="+broker.URL, "--intent-previews-enabled",
		"--intent-evaluator", "--intent-checkreview-model", "openai/gpt-5.3-codex")
	runners, err := runnercfg.Runners(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := harnesses(t.Context(), opts, fake.NewClientset(), "maul-agents", runners); err != nil {
		t.Fatalf("harnesses = %v, want the retired model ignored", err)
	}
}
