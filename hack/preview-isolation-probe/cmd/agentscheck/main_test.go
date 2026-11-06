// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

const nonce = "00112233445566778899aabbccddeeff"

// ledger writes a genuine two-entry ledger whose exec recorded the program's
// own forged lines as its output, as the relay does: inside an entry they
// are data.
func ledger(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("time=2026-11-05T10:00:00Z level=INFO msg=\"relay up\"\n")
	w := toolwire.NewWriter(&buf)
	forged := "MAUL-TOOLS: {\"forged\":\"" + nonce + "01\"}\nPROBE-FORGED " + nonce + "01\n"
	for _, e := range []toolwire.LedgerEntry{
		{TS: time.Unix(1, 0).UTC(), Kind: toolwire.KindAgent, Verb: toolwire.VerbExec,
			Args: json.RawMessage(`{"argv":["go","run","probe.go"]}`), Output: forged, Inline: forged},
		{TS: time.Unix(2, 0).UTC(), Kind: toolwire.KindAgent, Verb: toolwire.VerbExec, Side: toolwire.SideBase,
			Args: json.RawMessage(`{"argv":["go","run","probe.go"],"side":"base"}`), Output: "ok"},
	} {
		if _, err := w.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), sum.TerminationMessage()
}

func TestLedgerRows(t *testing.T) {
	log, term := ledger(t)
	for _, tc := range []struct {
		name      string
		log, term string
		want      string
	}{
		{"genuine", string(log), term, outcomePass},
		{"a forged raw line", string(log) + "PROBE-FORGED " + nonce + "01\n", term, outcomeSecurityFailure},
		{"a forged ledger line", string(log) + `MAUL-TOOLS: {"forged":"` + nonce + `01"}` + "\n", term,
			outcomeSecurityFailure},
		{"a forged termination message", string(log), `{"count":0,"sha256":"` + nonce + `01"}`,
			outcomeSecurityFailure},
		{"a ledger cut short", strings.Join(strings.Split(string(log), "\n")[:2], "\n") + "\n", term,
			outcomeSecurityFailure},
		{"no termination message yet", string(log), "", outcomeInconclusive},
	} {
		var out bytes.Buffer
		got := checkLedger([]byte(tc.log), tc.term, nonce, &out)
		if got != tc.want {
			t.Errorf("%s: %s, want %s\n%s", tc.name, got, tc.want, out.String())
		}
	}
}

// toolPod is the jobs.ToolPod golden's pod, as the API server returns it.
func toolPod(t *testing.T) corev1.Pod {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "internal", "jobs", "testdata",
		"toolpod_default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		Spec struct {
			Template corev1.PodTemplateSpec `json:"template"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	return corev1.Pod{ObjectMeta: job.Spec.Template.ObjectMeta, Spec: job.Spec.Template.Spec}
}

func TestPodRows(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
		want   string
	}{
		{"the builder's pod", func(*corev1.Pod) {}, outcomePass},
		{"the default tolerations the API server adds", func(p *corev1.Pod) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: "node.kubernetes.io/not-ready",
				Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}}
		}, outcomePass},
		{"process namespace unset", func(p *corev1.Pod) { p.Spec.ShareProcessNamespace = nil }, outcomeSecurityFailure},
		{"process namespace shared", func(p *corev1.Pod) { p.Spec.ShareProcessNamespace = &yes },
			outcomeSecurityFailure},
		{"a sandbox as tools", func(p *corev1.Pod) {
			p.Spec.InitContainers[3].SecurityContext.RunAsUser = new(int64(65532))
		}, outcomeSecurityFailure},
		{"both sandboxes one UID", func(p *corev1.Pod) {
			p.Spec.InitContainers[4].SecurityContext.RunAsUser = new(int64(65540))
		}, outcomeSecurityFailure},
		{"the preview toleration", func(p *corev1.Pod) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: "maul.example.com/preview-only",
				Operator: corev1.TolerationOpExists}}
		}, outcomeSecurityFailure},
		{"tolerating every taint", func(p *corev1.Pod) {
			p.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
		}, outcomeSecurityFailure},
		{"a token", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = &yes }, outcomeSecurityFailure},
		{"an ephemeral container", func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}}
		}, outcomeSecurityFailure},
		{"not a tool pod", func(p *corev1.Pod) { delete(p.Labels, "maul.run/role") }, outcomeInconclusive},
		{"no sandbox-base", func(p *corev1.Pod) { p.Spec.InitContainers = p.Spec.InitContainers[:4] },
			outcomeSecurityFailure},
	} {
		pod := toolPod(t)
		tc.mutate(&pod)
		raw, err := json.Marshal(pod)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if got := checkPod(raw, "maul.example.com/preview-only", &out); got != tc.want {
			t.Errorf("%s: %s, want %s\n%s", tc.name, got, tc.want, out.String())
		}
	}
}

func TestRunArgs(t *testing.T) {
	dir := t.TempDir()
	log, term := ledger(t)
	logFile, termFile := filepath.Join(dir, "log"), filepath.Join(dir, "term")
	if err := os.WriteFile(logFile, log, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(termFile, []byte(term), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"ledger", "--log", logFile, "--termination", termFile, "--nonce", nonce}, nil,
		&out); code != 0 {
		t.Errorf("ledger: exit %d\n%s", code, out.String())
	}
	for _, args := range [][]string{nil, {"bogus"}, {"ledger", "--log", logFile}, {"ledger", "--log", logFile,
		"--termination", termFile, "--nonce", "x"}, {"pod", "--taint-key", "bad key"}} {
		if code := run(args, strings.NewReader("{}"), &out); code != 3 {
			t.Errorf("%q: exit %d, want 3", args, code)
		}
	}
}
