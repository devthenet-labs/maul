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
	"k8s.io/client-go/tools/clientcmd"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// One long-context agent turn, as the fake model upstream reports it: the
// shape of the feature-sized runs in the large-features limits design (§0),
// where cache reads are ~97% of the volume but ~10% of the input price.
// 413,000 tokens a turn; priced as claude-opus-5 ($5 in, $6.25 5-minute cache
// write, $0.50 cache read, $25 out per million) it is 15,000 + 50,000 +
// 200,000 + 50,000 = 315,000 micro-USD.
const (
	turnInput      = 3_000
	turnCacheWrite = 8_000
	turnCacheRead  = 400_000
	turnOutput     = 2_000
	turnTokens     = turnInput + turnCacheWrite + turnCacheRead + turnOutput
	turnMicroUSD   = 315_000
	// turnMaxTokens is each request's output bound, which the broker
	// reserves on admission: 8,000 tokens, or 200,000 micro-USD at opus's
	// output price.
	turnMaxTokens = 8_000
	turns         = 10
	// brokerAudience is the audience the test's caller tokens are bound to.
	brokerAudience = "patchy-egress-broker-e2e"
	agentSA        = "patchy-agent"
)

// TestBrokerCostLimits drives the shipped egress-broker against a fake model
// upstream whose every response is a heavy cache-read turn, with the real
// API server's TokenReview and pod-bound projected tokens. It is the
// large-features limits design's evidence as a test (LF3): under the token
// unit a per-pod limit sized like the live one cuts a long-context run a few
// turns in, because a cache read weighs as much as an output token; under the
// cost unit the same turns are priced and the same run is admitted in full.
// The cost unit is still bounded: a pod whose budget annotation (the run's
// stage maxCostMicroUSD, stamped by its controller) is lower than
// --cost-per-pod is held to the annotation, read with get pods and checked
// against the token's pod UID.
func TestBrokerCostLimits(t *testing.T) {
	cl := startCluster(t)
	cs := cl.clientset(t)

	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get(provider.BrokerTokenHeader) != "" {
			t.Errorf("the caller token reached the upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-opus-5",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,`+
			`"output_tokens":%d}}`, turnInput, turnCacheWrite, turnCacheRead, turnOutput)
	}))
	t.Cleanup(upstream.Close)
	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte("e2e-model-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().ServiceAccounts(agentsNS).Create(context.Background(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agentSA},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// The token unit, sized like the live default in proportion: 2M tokens a
	// pod is under five of these turns.
	tokensURL := startBroker(t, cl, upstream.URL, keyFile, "--tokens-per-pod", "2000000")
	tokenRun := agentPod(t, cs, "token-unit", "")
	admitted, refusal := runTurns(t, tokensURL, tokenRun)
	if admitted != 5 {
		t.Errorf("token unit admitted %d turns, want 5 (2M tokens at %d a turn)", admitted, turnTokens)
	}
	if want := provider.LimitMessagePrefix + ": tokens per pod (2000000) reached"; refusal != want {
		t.Errorf("token unit refusal = %q, want %q", refusal, want)
	}

	// The cost unit at $5 a pod: ten turns cost $3.15, so the whole run is
	// admitted, though it moved 4.13M tokens.
	costURL := startBroker(t, cl, upstream.URL, keyFile, "--limit-unit", "cost", "--cost-per-pod", "5000000")
	costRun := agentPod(t, cs, "cost-unit", "")
	if admitted, refusal := runTurns(t, costURL, costRun); admitted != turns {
		t.Errorf("cost unit admitted %d turns, want all %d (%d micro-USD of 5000000); refusal %q",
			admitted, turns, turns*turnMicroUSD, refusal)
	}

	// A $1 budget annotation lowers that pod's cap: three turns (945,000)
	// fit, and the fourth's 200,000 reservation would pass it.
	budgetRun := agentPod(t, cs, "cost-budget", "1000000")
	admitted, refusal = runTurns(t, costURL, budgetRun)
	if admitted != 3 {
		t.Errorf("budgeted pod admitted %d turns, want 3", admitted)
	}
	if want := provider.LimitMessagePrefix + ": cost per pod (1000000 micro-USD) reached"; refusal != want {
		t.Errorf("budgeted pod refusal = %q, want %q", refusal, want)
	}

	if want := int64(5 + turns + 3); hits.Load() != want {
		t.Errorf("upstream answered %d requests, want %d: a refused request reached it", hits.Load(), want)
	}
}

// clientset is a typed client over the cluster's admin credentials, for the
// subresources the controller-runtime client does not reach (TokenRequest).
func (cl *cluster) clientset(t *testing.T) kubernetes.Interface {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", cl.kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// startBroker runs the shipped egress-broker with an anthropic route to
// upstream and the given limit flags, returning its base URL.
func startBroker(t *testing.T, cl *cluster, upstream, keyFile string, limits ...string) string {
	t.Helper()
	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	args := append([]string{
		"--listen-addr", listen,
		"--agent-namespace", agentsNS,
		"--agent-service-account", agentSA,
		"--token-audience", brokerAudience,
		"--anthropic-api-key-file", keyFile,
		"--anthropic-base-url", upstream,
	}, limits...)
	cl.serveBinary(t, "egress-broker", build(t, "egress-broker"), args...)
	return "http://" + listen
}

// agentPod creates an agent pod, with the run's budget annotation when
// budget is set, and returns a projected token bound to it, as the kubelet
// mounts one into a brokered claude pod. envtest has no kubelet, so the pod
// never runs: the broker only needs it to exist for TokenReview and the
// budget read.
func agentPod(t *testing.T, cs kubernetes.Interface, name, budget string) string {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentsNS},
		Spec: corev1.PodSpec{
			ServiceAccountName: agentSA,
			Containers:         []corev1.Container{{Name: "agent", Image: "patchy/claude-agent-runner:e2e"}},
		},
	}
	if budget != "" {
		pod.Annotations = map[string]string{provider.BudgetAnnotation: budget}
	}
	pod, err := cs.CoreV1().Pods(agentsNS).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod %s: %v", name, err)
	}
	ttl := int64(3600)
	tr, err := cs.CoreV1().ServiceAccounts(agentsNS).CreateToken(ctx, agentSA, &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{
			Audiences:         []string{brokerAudience},
			ExpirationSeconds: &ttl,
			BoundObjectRef:    &authnv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("token for pod %s: %v", name, err)
	}
	return tr.Status.Token
}

// runTurns sends up to turns Messages requests for claude-opus-5 under token,
// one at a time as an agent does, and returns how many the broker admitted
// and the message of the first refusal. Any answer but 200 or the per-pod
// limit's 429 fails the test.
func runTurns(t *testing.T, base, token string) (admitted int, refusal string) {
	t.Helper()
	body := `{"model":"claude-opus-5","max_tokens":` + strconv.Itoa(turnMaxTokens) +
		`,"messages":[{"role":"user","content":"continue"}]}`
	for range turns {
		req, err := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
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
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatalf("turn %d: refusal body %q: %v", admitted+1, raw, err)
			}
			return admitted, e.Error.Message
		default:
			t.Fatalf("turn %d: status %d: %s", admitted+1, resp.StatusCode, raw)
		}
	}
	return admitted, ""
}
