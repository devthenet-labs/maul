// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/devthenet-labs/maul/internal/provider"
	"github.com/devthenet-labs/maul/internal/usagewire"
)

// One long-context codex turn, as the fake OpenAI upstream reports it: a
// Responses usage object whose input_tokens include 400,000 cached tokens.
// The broker's OpenAI dialect maps it to 3,000 fresh input, 400,000 cache
// read and 2,000 output, so 405,000 tokens a turn. Priced as gpt-5.3-codex
// ($1.75 in, $0.175 cached input, $14 out per million) it is 5,250 + 70,000 +
// 28,000 = 103,250 micro-USD.
const (
	codexTurnInput    = 403_000 // input_tokens, cached included
	codexTurnCached   = 400_000
	codexTurnOutput   = 2_000
	codexTurnTokens   = codexTurnInput + codexTurnOutput
	codexTurnMicroUSD = 103_250
	// codexTurnMaxOutput is each request's max_output_tokens, the output
	// bound the broker reserves on admission: 8,000 tokens, or 112,000
	// micro-USD at gpt-5.3-codex's output price.
	codexTurnMaxOutput = 8_000
	// usageReaderSA is the run controller identity the usage endpoint
	// answers to in this test.
	usageReaderSA = "usage-reader"
)

// TestBrokerCostLimitsOpenAI is TestBrokerCostLimits for brokered codex: the
// shipped egress-broker's openai route against a fake OpenAI Responses
// upstream whose every response is a heavy cached-input turn, with the real
// API server's TokenReview and pod-bound tokens. The route is measured (the
// usage endpoint the run controllers settle from reports the turns' tokens
// for the pod's Job, by model, route and credential kind), shares the limits
// claude runs are held to (the token unit cuts a long-context codex run a
// few turns in; the cost unit prices the same turns from the registry's
// OpenAI rates and admits the run) and, under the cost unit, the run's
// budget annotation still bounds it. A refusal never reaches the upstream,
// and is OpenAI's insufficient_quota so codex does not retry it.
func TestBrokerCostLimitsOpenAI(t *testing.T) {
	cl := startCluster(t)
	cs := cl.clientset(t)

	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get(provider.BrokerTokenHeader) != "" {
			t.Errorf("the caller token reached the upstream")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+openAIOrgKey {
			t.Errorf("upstream Authorization = %q, want the org OpenAI key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"resp_e2e","object":"response","status":"completed","model":"gpt-5.3-codex",`+
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],`+
			`"usage":{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d},"output_tokens":%d,`+
			`"output_tokens_details":{"reasoning_tokens":500},"total_tokens":%d}}`,
			codexTurnInput, codexTurnCached, codexTurnOutput, codexTurnInput+codexTurnOutput)
	}))
	t.Cleanup(upstream.Close)
	ensureAgentSA(t, cs)
	if _, err := cs.CoreV1().ServiceAccounts(namespace).Create(context.Background(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: usageReaderSA},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// The token unit at 2M tokens a pod: under five of these turns.
	tokensURL, tokensUsage := startOpenAICostBroker(t, cl, upstream.URL, "--tokens-per-pod", "2000000")
	tokenRun := agentPodWith(t, cs, "codex-token-unit-a1b2c", nil)
	admitted, refusal := runCodexTurns(t, tokensURL, tokenRun)
	if admitted != 5 {
		t.Errorf("token unit admitted %d turns, want 5 (2M tokens at %d a turn)", admitted, codexTurnTokens)
	}
	if want := provider.LimitMessagePrefix + ": tokens per pod (2000000) reached"; refusal != want {
		t.Errorf("token unit refusal = %q, want %q", refusal, want)
	}
	// Measured: the five admitted turns are in the ledger, mapped by the
	// OpenAI dialect, for the Job the pod belongs to.
	usage := readJobUsage(t, cs, tokensUsage, "codex-token-unit")
	if len(usage.Pods) != 1 || len(usage.Pods[0].Models) != 1 {
		t.Fatalf("usage = %+v, want one pod with one model", usage)
	}
	m := usage.Pods[0].Models[0]
	want := usagewire.Model{
		Model: "gpt-5.3-codex", Route: "openai", Credential: usagewire.CredentialAPIKey,
		Input:     5 * (codexTurnInput - codexTurnCached),
		CacheRead: 5 * codexTurnCached,
		Output:    5 * codexTurnOutput,
	}
	if m != want {
		t.Errorf("measured usage = %+v, want %+v", m, want)
	}

	// The cost unit at $5 a pod: ten turns cost $1.0325, so the whole run is
	// admitted, though it moved 4.05M tokens.
	costURL, _ := startOpenAICostBroker(t, cl, upstream.URL, "--limit-unit", "cost", "--cost-per-pod", "5000000")
	costRun := agentPodWith(t, cs, "codex-cost-unit-a1b2c", nil)
	if admitted, refusal := runCodexTurns(t, costURL, costRun); admitted != turns {
		t.Errorf("cost unit admitted %d turns, want all %d (%d micro-USD of 5000000); refusal %q",
			admitted, turns, turns*codexTurnMicroUSD, refusal)
	}

	// A $0.30 budget annotation lowers that pod's cap: two turns fit
	// (206,500 micro-USD), and the third's 112,000 reservation would pass it.
	budgetRun := agentPodWith(t, cs, "codex-cost-budget-a1b2c",
		map[string]string{provider.BudgetAnnotation: "300000"})
	admitted, refusal = runCodexTurns(t, costURL, budgetRun)
	if admitted != 2 {
		t.Errorf("budgeted pod admitted %d turns, want 2", admitted)
	}
	if want := provider.LimitMessagePrefix + ": cost per pod (300000 micro-USD) reached"; refusal != want {
		t.Errorf("budgeted pod refusal = %q, want %q", refusal, want)
	}

	if want := int64(5 + turns + 2); hits.Load() != want {
		t.Errorf("upstream answered %d requests, want %d: a refused request reached it", hits.Load(), want)
	}
}

// startOpenAICostBroker runs the shipped egress-broker with an openai route
// to upstream holding openAIOrgKey, its usage endpoint answering the
// usage-reader ServiceAccount, and the given limit flags. It returns the
// proxy's and the usage endpoint's base URLs.
func startOpenAICostBroker(t *testing.T, cl *cluster, upstream string, limits ...string) (proxy, usage string) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "openai-key")
	if err := os.WriteFile(keyFile, []byte(openAIOrgKey), 0o600); err != nil {
		t.Fatal(err)
	}
	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	usageListen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	args := append([]string{
		"--listen-addr", listen,
		"--agent-namespace", agentsNS,
		"--agent-service-account", agentSA,
		"--token-audience", brokerAudience,
		"--openai-api-key-file", keyFile,
		"--openai-base-url", upstream,
		"--usage-addr", usageListen,
		"--usage-callers", namespace + ":" + usageReaderSA,
	}, limits...)
	cl.serveBinary(t, "egress-broker", build(t, "egress-broker"), args...)
	return "http://" + listen, "http://" + usageListen
}

// readJobUsage reads job's ledger entries from the broker's usage endpoint
// as a run controller does: a projected token for the usage audience, bound
// to the controller's pod.
func readJobUsage(t *testing.T, cs kubernetes.Interface, base, job string) usagewire.Response {
	t.Helper()
	ctx := context.Background()
	pod, err := cs.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "remediation-controller-", Namespace: namespace},
		Spec: corev1.PodSpec{
			ServiceAccountName: usageReaderSA,
			Containers:         []corev1.Container{{Name: "controller", Image: "maul/controller:e2e"}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ttl := int64(3600)
	tr, err := cs.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, usageReaderSA,
		&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{
			Audiences: []string{usagewire.Audience}, ExpirationSeconds: &ttl,
			BoundObjectRef: &authnv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
		}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, base+usagewire.PathPrefix+job, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tr.Status.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usage of %s: %d %s", job, resp.StatusCode, raw)
	}
	var out usagewire.Response
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("usage of %s: %q: %v", job, raw, err)
	}
	return out
}

// runCodexTurns sends up to turns Responses requests for gpt-5.3-codex under
// token, one at a time as codex does, and returns how many the broker
// admitted and the message of the first refusal. Any answer but 200 or the
// per-pod limit's 429, in OpenAI's envelope as insufficient_quota, fails the
// test.
func runCodexTurns(t *testing.T, base, token string) (admitted int, refusal string) {
	t.Helper()
	body := `{"model":"gpt-5.3-codex","max_output_tokens":` + strconv.Itoa(codexTurnMaxOutput) +
		`,"input":[{"role":"user","content":[{"type":"input_text","text":"continue"}]}]}`
	for range turns {
		req, err := http.NewRequest(http.MethodPost, base+"/openai/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+provider.PlaceholderAuthToken)
		req.Header.Set(provider.BrokerTokenHeader, token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("turn %d: %v", admitted+1, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			admitted++
			continue
		case http.StatusTooManyRequests:
			var e struct {
				Error struct {
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatalf("turn %d: refusal body %q: %v", admitted+1, raw, err)
			}
			if e.Error.Code != "insufficient_quota" {
				t.Errorf("turn %d: refusal code %q, want insufficient_quota", admitted+1, e.Error.Code)
			}
			return admitted, e.Error.Message
		default:
			t.Fatalf("turn %d: status %d: %s", admitted+1, resp.StatusCode, raw)
		}
	}
	return admitted, ""
}
