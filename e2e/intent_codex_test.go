// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/provider"
)

const (
	// codexRunnerImage is the codex runner image intent-controller is
	// configured with; a plan runs on it, a build on the repository's own.
	codexRunnerImage = "patchy/codex-agent-runner:e2e"
	// openAIOrgKey is the org OpenAI key the broker holds.
	openAIOrgKey = "sk-proj-e2e-org-openai-key"
)

// openAIUpstream is a fake OpenAI Responses API: it records every request
// the broker forwards and answers with a completed response and its usage.
type openAIUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	seen []openAIRequest
}

// openAIRequest is what the upstream saw of one request.
type openAIRequest struct {
	Path, Authorization, CallerToken, Model string
}

func newOpenAIUpstream(t *testing.T) *openAIUpstream {
	t.Helper()
	up := &openAIUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		up.mu.Lock()
		up.seen = append(up.seen, openAIRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"),
			CallerToken: r.Header.Get(provider.BrokerTokenHeader), Model: body.Model})
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"resp_e2e","object":"response","status":"completed","model":%q,`+
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],`+
			`"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":200},"output_tokens":300,`+
			`"total_tokens":1500}}`, body.Model)
	}))
	t.Cleanup(up.Close)
	return up
}

func (u *openAIUpstream) requests() []openAIRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]openAIRequest(nil), u.seen...)
}

// startOpenAIBroker runs the shipped egress-broker with an openai route to
// upstream, holding openAIOrgKey, and returns its base URL.
func startOpenAIBroker(t *testing.T, cl *cluster, upstream string) string {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "openai-key")
	if err := os.WriteFile(keyFile, []byte(openAIOrgKey), 0o600); err != nil {
		t.Fatal(err)
	}
	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.serveBinary(t, "egress-broker", build(t, "egress-broker"),
		"--listen-addr", listen,
		"--agent-namespace", agentsNS,
		"--agent-service-account", agentSA,
		"--token-audience", brokerAudience,
		"--openai-api-key-file", keyFile,
		"--openai-base-url", upstream)
	return "http://" + listen
}

// TestIntentOnBrokeredCodex runs an intent of a Project that chooses codex
// (spec.harness codex) end to end under --intent-codex: the plan and the
// build each run the REAL agent-runner on the codex harness, with
// hack/fake-agent/codex as the CLI, whose model requests go through the
// shipped egress-broker, admitted by TokenReview of a caller token bound to
// the Job's pod, to a fake OpenAI upstream. The upstream sees the org key
// the broker attached and never the caller token; no codex pod carries a
// model credential; every run records codex; and the build's changeset is
// pushed and opened as a pull request, as on claude.
func TestIntentOnBrokeredCodex(t *testing.T) {
	up := newOpenAIUpstream(t)
	cl, gh, registry := startSourceStack(t, appRepo)
	cs := cl.clientset(t)
	ensureAgentSA(t, cs)
	brokerURL := startOpenAIBroker(t, cl, up.URL)

	e := withIntentsFor(t, cl, gh, v1alpha1.ProjectSpec{
		IntentRepository: intentRepoURL,
		Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login, reader.Login}},
		Repositories:     []v1alpha1.ProjectRepository{{Name: appRepo, URL: appRepoURL}},
		Harness:          v1alpha1.ProjectHarnessCodex,
	}, "--intent-codex", "--codex-agent-image", codexRunnerImage, "--broker-url", brokerURL,
		"--broker-token-audience", brokerAudience)
	e.registry = registry
	e.kubelet.useAgentRunner(t, "plan", "build")
	e.kubelet.useBrokerTokens(cs)

	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	if len(in.Status.PullRequests) != 1 {
		t.Fatalf("pull requests = %+v, want the build's one", in.Status.PullRequests)
	}

	// Both stages ran agent-runner on codex, the plan on the codex runner
	// image and the build on the repository's own, and no pod held a model
	// credential.
	for _, phase := range []string{"plan", "build"} {
		r := e.kubelet.waitRun(t, "the "+phase+" Job", func(r agentRun) bool { return r.Env["PATCHY_PHASE"] == phase })
		if got := r.Job.Spec.Template.Labels[v1alpha1.LabelHarness]; got != "codex" {
			t.Errorf("%s Job harness label = %q, want codex", phase, got)
		}
		if phase == "plan" && r.Image != codexRunnerImage {
			t.Errorf("plan image = %q, want the codex runner %s", r.Image, codexRunnerImage)
		}
		if phase == "build" && r.Job.Annotations[runnerImageSourceAnnotation] == "" {
			t.Errorf("build Job %s did not run the repository's image", r.Job.Name)
		}
		if r.ExitCode != 0 {
			t.Errorf("%s agent-runner exited %d:\n%s", phase, r.ExitCode, r.Stdout)
		}
		for _, c := range r.Job.Spec.Template.Spec.Containers {
			for _, env := range c.Env {
				if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
					t.Errorf("%s container %s reads Secret %s as %s", phase, c.Name,
						env.ValueFrom.SecretKeyRef.Name, env.Name)
				}
				switch env.Name {
				case "OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN":
					t.Errorf("%s container %s sets %s", phase, c.Name, env.Name)
				}
			}
		}
		calls, err := os.ReadFile(filepath.Join(r.Workspace, ".fake-codex", "calls"))
		if want := phase + " gpt-5.3-codex 200\n"; err != nil || string(calls) != want {
			t.Errorf("%s: the scripted codex recorded %q (%v), want %q", phase, calls, err, want)
		}
	}

	// The broker attached the org key upstream and stripped the caller
	// token; every request was the codex model's.
	seen := up.requests()
	if len(seen) != 2 {
		t.Fatalf("the upstream saw %d requests, want the plan's and the build's: %+v", len(seen), seen)
	}
	for _, r := range seen {
		if r.Path != "/v1/responses" || r.Authorization != "Bearer "+openAIOrgKey || r.CallerToken != "" ||
			r.Model != "gpt-5.3-codex" {
			t.Errorf("upstream request = %+v, want POST /v1/responses for gpt-5.3-codex on the org key, "+
				"with no caller token", r)
		}
	}

	for runName, run := range e.runs(t, name) {
		if run.Spec.Harness != v1alpha1.ProjectHarnessCodex {
			t.Errorf("run %s records harness %q, want codex", runName, run.Spec.Harness)
		}
		if run.Status.Outcome != "ok" {
			t.Errorf("run %s = %s %q, want ok", runName, run.Status.Outcome, run.Status.Detail)
		}
	}
	if strings.Contains(e.describeRuns(t, name), openAIOrgKey) {
		t.Error("the org key reached a run's record")
	}
}
