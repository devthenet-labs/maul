// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolpod

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/previewtools"
)

func TestDeadlines(t *testing.T) {
	for _, tc := range []struct {
		timeout, max, job, pod, wall time.Duration
	}{
		{20 * time.Minute, 30 * time.Minute, 26 * time.Minute, 23 * time.Minute, 20 * time.Minute},
		{20 * time.Minute, 22 * time.Minute, 22 * time.Minute, 19 * time.Minute, 16 * time.Minute},
		{time.Hour, time.Hour, time.Hour, 57 * time.Minute, 54 * time.Minute},
		{time.Minute, time.Minute, time.Minute, 45 * time.Second, 30 * time.Second},
	} {
		job, pod, wall := Deadlines(tc.timeout, tc.max)
		if job != tc.job || pod != tc.pod || wall != tc.wall {
			t.Errorf("Deadlines(%s, %s) = %s %s %s, want %s %s %s", tc.timeout, tc.max, job, pod, wall,
				tc.job, tc.pod, tc.wall)
		}
	}
	// Every timeout and bound the schema and the chart admit keep the
	// order the ledger's survival rests on: wall < pod < job <= max.
	for timeout := time.Minute; timeout <= time.Hour; timeout += 7 * time.Minute {
		for max := time.Minute; max <= time.Hour; max += 5 * time.Minute {
			job, pod, wall := Deadlines(timeout, max)
			if wall <= 0 || wall > timeout || wall >= pod || pod >= job || job > max {
				t.Errorf("Deadlines(%s, %s) = %s %s %s out of order", timeout, max, job, pod, wall)
			}
		}
	}
}

// Every container runs under the UID it is given, never as root, with no
// capability, no privilege escalation and a read-only root.
func TestSecurity(t *testing.T) {
	sc := Security(65540)
	if *sc.RunAsUser != 65540 || *sc.RunAsGroup != 65540 || !*sc.RunAsNonRoot || *sc.AllowPrivilegeEscalation ||
		!*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
		len(sc.Capabilities.Add) != 0 || sc.Privileged != nil {
		t.Errorf("posture %+v", sc)
	}
}

func TestNewKey(t *testing.T) {
	key := NewKey()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) || len(key) != 2*KeyBytes || key == NewKey() {
		t.Errorf("key %q is not a fresh 256-bit hex key", key)
	}
}

// The relay reads back the configuration it is given; the outcomes sit
// beside it.
func TestConfigData(t *testing.T) {
	cfg := previewtools.Config{Host: "shop-1.preview.maul.example.com", Viewer: "maul-evaluator",
		MaxToolCalls: 50, TimeoutSeconds: 600,
		Components: []previewtools.Component{{Key: "web", Path: "/", Addr: "10.96.0.10:80"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	outcomes := []v1alpha1.AcceptanceOutcome{
		{ID: "items", Outcome: "the items list shows new items", Kind: v1alpha1.OutcomeKindChange},
	}
	data, err := ConfigData(cfg, outcomes)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 2 {
		t.Errorf("data keys %v", data)
	}
	got, err := previewtools.ParseConfig([]byte(data[previewtools.ConfigFile]))
	if err != nil || got.Host != cfg.Host || len(got.Components) != 1 {
		t.Errorf("the relay reads %+v: %v", got, err)
	}
	var back []v1alpha1.AcceptanceOutcome
	if err := json.Unmarshal([]byte(data[OutcomesKey]), &back); err != nil || len(back) != 1 || back[0].ID != "items" {
		t.Errorf("outcomes %s: %v", data[OutcomesKey], err)
	}
}
