// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/e2e/fakegithub"
	"github.com/devthenet-labs/maul/e2e/fakeoidc"
	"github.com/devthenet-labs/maul/internal/controller/remediation"
	"github.com/devthenet-labs/maul/internal/provider"
)

// personalOpenAIKey is the approver's own OpenAI API key, as they paste it
// into the Account page.
const personalOpenAIKey = "sk-proj-octocat-e2e-personal-openai-key"

// replacementOpenAIKey is the key the approver stores in place of
// personalOpenAIKey, so the broker's cached copy of the Secret must go.
const replacementOpenAIKey = "sk-proj-octocat-e2e-replacement-openai"

// TestUserCredentialsCodexRemediation runs per-user OpenAI API keys end to
// end for a codex remediation under --user-credentials enforce: the shipped
// egress-broker with only its openai route (enforce, records and Secrets
// read from the cluster, health probed against a fake OpenAI upstream that
// records which key every request carried), remediation-controller choosing
// codex for an OpenAI model and holding the attempt until the approver's
// OpenAI key is Valid, and status-server's Account page offering OpenAI,
// where the approver stores theirs.
//
// The approver's Finding is held with no Remediation, on the openai
// provider (a codex run is never held on, or paid by, an Anthropic record);
// once they store an OpenAI key the broker's probe finds it Valid and the
// remediation is created, attributed to them, its codex Job credential-less;
// a request from that Job's pod reaches the upstream with the approver's
// key, never the org's; once they replace it, the old key stops paying
// within the revocation cap though the broker holds a warm copy of its
// Secret; and once they delete it, their pod is refused within the cap.
func TestUserCredentialsCodexRemediation(t *testing.T) {
	ctx := context.Background()
	cl := startCluster(t)
	gh := fakegithub.New()
	t.Cleanup(gh.Close)
	cl.githubCredentials(t, gh.URL)
	cs := cl.clientset(t)
	if err := cl.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: credentialsNS}}); err != nil {
		t.Fatal(err)
	}
	ensureAgentSA(t, cs)

	up := newCredentialUpstream(openAIOrgKey, personalOpenAIKey, replacementOpenAIKey)
	t.Cleanup(up.Close)
	keyFile := filepath.Join(t.TempDir(), "openai-key")
	if err := os.WriteFile(keyFile, []byte(openAIOrgKey), 0o600); err != nil {
		t.Fatal(err)
	}
	brokerListen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cl.serveBinary(t, "egress-broker", build(t, "egress-broker"),
		"--listen-addr", brokerListen,
		"--agent-namespace", agentsNS,
		"--agent-service-account", agentSA,
		"--token-audience", brokerAudience,
		"--openai-api-key-file", keyFile,
		"--openai-base-url", up.URL,
		"--user-credentials", "enforce",
		"--credential-records-namespace", namespace,
		"--credentials-namespace", credentialsNS)
	brokerURL := "http://" + brokerListen

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	base := "http://" + listen
	accountURL := base + "/#/account"
	oidc, err := fakeoidc.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oidc.Close)
	oidc.AddUser(fakeoidc.User{Login: approver.Login, Name: "Octo Cat", GitHubID: approver.ID})
	grantCredentialsSelf(t, cl, "github:"+approver.Login)
	cl.controller(t, "status-server", "--listen-addr", listen,
		"--auth-config", writeAccountAuthConfig(t, oidc.URL, base+"/oauth2/callback"),
		"--agent-namespace", agentsNS, "--account-enabled", "--credentials-namespace", credentialsNS,
		"--account-providers", "anthropic,openai")

	// A Queued Finding the approver approved by a GitHub comment (so the
	// approval carries their id), whose remediation runs an OpenAI model.
	fnd := queuedApprovedFinding(t, cl, "finding-dddddddddd-1")
	cl.controller(t, "source-controller",
		"--artifact-addr", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"--artifact-base-url", "http://127.0.0.1:1",
		"--artifact-dir", t.TempDir())
	eventually(t, "the repository artifact", func() bool {
		var repo v1alpha1.Repository
		key := types.NamespacedName{Namespace: namespace, Name: fnd.Name + "-src"}
		return cl.client.Get(ctx, key, &repo) == nil && repo.Status.Artifact != nil
	})
	cl.controller(t, "remediation-controller", append([]string{
		"--attribution-mode", "attribute", "--user-credentials", "enforce", "--account-url", accountURL,
		"--model-allowlist", "openai/gpt-5.3-codex", "--remediate-model", "openai/gpt-5.3-codex",
	}, runnerArgs...)...)

	// 1. No OpenAI credential: the codex attempt is held on the openai
	// provider, naming the approver and the Account page, and no
	// Remediation exists.
	var cur v1alpha1.Finding
	eventually(t, "the codex remediation to be held on the approver's OpenAI credential", func() bool {
		if err := cl.client.Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
			return false
		}
		c := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionCredentialRequired)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "CredentialMissing"
	})
	if cur.Status.Phase != v1alpha1.PhaseQueued {
		t.Errorf("held finding phase = %s, want Queued", cur.Status.Phase)
	}
	if h := cur.Status.CredentialHold; h == nil || h.ID != approver.ID || h.Login != approver.Login {
		t.Errorf("credentialHold = %+v, want the approver %s (%d)", h, approver.Login, approver.ID)
	}
	if got := cur.Status.CredentialHoldProvider; got != v1alpha1.ModelCredentialProviderOpenAI {
		t.Errorf("credentialHoldProvider = %q, want openai", got)
	}
	c := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionCredentialRequired)
	if !strings.Contains(c.Message, "@"+approver.Login+" has no OpenAI credential") ||
		!strings.Contains(c.Message, accountURL) {
		t.Errorf("hold message = %q, want the approver's missing OpenAI credential and the Account page", c.Message)
	}
	var rems v1alpha1.RemediationList
	if err := cl.client.List(ctx, &rems, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(rems.Items) != 0 {
		t.Errorf("%d remediations created while held", len(rems.Items))
	}
	if n := up.count(""); n != 0 {
		t.Errorf("the upstream saw %d requests before anyone stored a credential", n)
	}

	// 2. The approver stores an OpenAI API key on the Account page, which
	// offers OpenAI with its one kind.
	browser := signIn(t, base, oidc, approver.Login)
	var acct struct {
		Credentials []struct {
			Provider string   `json:"provider"`
			Kinds    []string `json:"kinds"`
			Stored   bool     `json:"stored"`
		} `json:"credentials"`
	}
	getJSON(t, browser, base+"/api/account", &acct)
	var offered bool
	for _, cr := range acct.Credentials {
		if cr.Provider == "openai" {
			offered = !cr.Stored && len(cr.Kinds) == 1 && cr.Kinds[0] == "api-key"
		}
	}
	if !offered {
		t.Fatalf("account = %+v, want OpenAI offered, API keys only, nothing stored", acct)
	}
	if code, body := accountPost(t, browser, base+"/api/account/credentials/openai",
		`{"kind":"api-key","credential":"`+personalOpenAIKey+`"}`); code != http.StatusOK ||
		strings.Contains(body, personalOpenAIKey) {
		t.Fatalf("store the OpenAI key = %d %s, want 200 without the key echoed", code, body)
	}

	// 3. The broker's openai probe finds it Valid; the hold lifts without a
	// re-approval and the remediation runs as the approver's, on codex.
	owner := v1alpha1.ModelCredentialOwner(approver.ID)
	eventually(t, "the broker's probe to find the OpenAI key Valid", func() bool {
		var rec v1alpha1.ModelCredential
		key := types.NamespacedName{Namespace: namespace,
			Name: v1alpha1.ModelCredentialName(owner, v1alpha1.ModelCredentialProviderOpenAI)}
		return cl.client.Get(ctx, key, &rec) == nil && rec.Status.Health == v1alpha1.ModelCredentialValid
	})
	if up.count("/v1/models", personalOpenAIKey) == 0 {
		t.Errorf("the key went Valid without the broker probing it upstream")
	}
	// The spawner looks at a held Finding again every CredentialRecheck,
	// so the hold lifts within about that long of the key going Valid.
	var job batchv1.Job
	for deadline := time.Now().Add(remediation.CredentialRecheck + 30*time.Second); ; {
		var jobs batchv1.JobList
		if err := cl.client.List(ctx, &jobs, client.InNamespace(agentsNS)); err == nil && len(jobs.Items) == 1 {
			job = jobs.Items[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the held codex remediation's Job was not created within %s of its key going Valid",
				remediation.CredentialRecheck+30*time.Second)
		}
		time.Sleep(250 * time.Millisecond)
	}
	want := "github-id:" + strconv.FormatInt(approver.ID, 10)
	if got := job.Spec.Template.Annotations[v1alpha1.AnnotationAttributedTo]; got != want {
		t.Errorf("Job pod template attributed-to = %q, want %q", got, want)
	}
	if got := job.Labels[v1alpha1.LabelHarness]; got != "codex" {
		t.Errorf("Job harness label = %q, want codex", got)
	}
	assertBrokeredCodexPod(t, job.Spec.Template.Spec)
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionCredentialRequired); c == nil ||
		c.Status != metav1.ConditionFalse || cur.Status.CredentialHold != nil {
		t.Errorf("after the hold lifted: CredentialRequired %+v, credentialHold %+v; want False and none",
			c, cur.Status.CredentialHold)
	}

	// 4. Delivery: the Job's pod (as the Job controller makes it, carrying
	// the template's attribution) pays with the approver's OpenAI key; an
	// org-attributed pod keeps the org key.
	person := agentPodWith(t, cs, job.Name+"-x7k2p", job.Spec.Template.Annotations)
	if code, body := codexTurn(t, brokerURL, person); code != http.StatusOK {
		t.Fatalf("the approver's codex pod: %d %s, want 200", code, body)
	}
	if up.count("/v1/responses", personalOpenAIKey) != 1 || up.count("/v1/responses", openAIOrgKey) != 0 {
		t.Errorf("responses by credential = %v, want one on the approver's key and none on the org key",
			up.byPath("/v1/responses"))
	}
	org := agentPodWith(t, cs, "org-codex-run", map[string]string{v1alpha1.AnnotationAttributedTo: "org"})
	if code, body := codexTurn(t, brokerURL, org); code != http.StatusOK {
		t.Fatalf("an org-attributed pod: %d %s, want 200", code, body)
	}
	if up.count("/v1/responses", openAIOrgKey) != 1 {
		t.Errorf("responses by credential = %v, want the org pod on the org key", up.byPath("/v1/responses"))
	}

	// 5. Replacement: the record stays and only the Secret changes, so the
	// broker's warm copy of the Secret must go within its maximum age
	// (DefaultMaxSecretAge, the revocation cap): the old key stops paying,
	// and once the new one has paid, the old one never pays again.
	if code, body := accountPost(t, browser, base+"/api/account/credentials/openai",
		`{"kind":"api-key","credential":"`+replacementOpenAIKey+`"}`); code != http.StatusOK ||
		strings.Contains(body, replacementOpenAIKey) {
		t.Fatalf("replace the OpenAI key = %d %s, want 200 without the key echoed", code, body)
	}
	replaced := time.Now()
	for up.count("/v1/responses", replacementOpenAIKey) == 0 {
		if code, body := codexTurn(t, brokerURL, person); code != http.StatusOK {
			t.Fatalf("after replacement: %d %s, want 200", code, body)
		}
		if up.count("/v1/responses", replacementOpenAIKey) > 0 {
			break
		}
		if time.Since(replaced) > revocationCap {
			t.Fatalf("a replaced OpenAI key still paid %s after its replacement, past the %s cap: %v",
				time.Since(replaced).Round(time.Second), revocationCap, up.byPath("/v1/responses"))
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("replaced OpenAI key stopped paying %s after its replacement", time.Since(replaced).Round(time.Millisecond))
	oldPaid := up.count("/v1/responses", personalOpenAIKey)
	for range 3 {
		if code, body := codexTurn(t, brokerURL, person); code != http.StatusOK {
			t.Fatalf("on the replacement key: %d %s, want 200", code, body)
		}
	}
	if n := up.count("/v1/responses", personalOpenAIKey); n != oldPaid {
		t.Errorf("the replaced OpenAI key paid again after its replacement had: %v", up.byPath("/v1/responses"))
	}

	// 6. Revocation: once the approver deletes their OpenAI key, their pod
	// is refused within the cap, never falling back to the org key.
	if code, body := accountPost(t, browser, base+"/api/account/credentials/openai/delete", ""); code !=
		http.StatusNoContent {
		t.Fatalf("delete the OpenAI key = %d %s, want 204", code, body)
	}
	revoked := time.Now()
	for {
		code, body := codexTurn(t, brokerURL, person)
		if code == http.StatusForbidden &&
			(strings.Contains(body, "credential_missing") || strings.Contains(body, "credential_secret_missing")) {
			break
		}
		if code != http.StatusOK {
			t.Fatalf("after revocation: %d %s, want 200 until refused as missing", code, body)
		}
		if time.Since(revoked) > revocationCap {
			t.Fatalf("a deleted OpenAI key still paid %s after its deletion, past the %s cap",
				time.Since(revoked).Round(time.Second), revocationCap)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("revoked OpenAI key refused %s after its deletion", time.Since(revoked).Round(time.Millisecond))
	if n := up.count("/v1/responses", openAIOrgKey); n != 1 {
		t.Errorf("the org key paid for %d responses, want only the org pod's one: %v", n, up.byPath("/v1/responses"))
	}
}

// queuedApprovedFinding fabricates a Queued Finding approved by the
// approver's GitHub comment (By and ByID), with its pinned Repository and
// the completed Investigation its remediation executes.
func queuedApprovedFinding(t *testing.T, cl *cluster, name string) *v1alpha1.Finding {
	t.Helper()
	ctx := context.Background()
	repoURL := "https://127.0.0.1/acme/shop"
	now := metav1.Now()
	fnd := &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "github"},
			Source:         "ghas",
			Advisories:     []string{"CWE-79"},
			Title:          "Reflected cross-site scripting",
			Description:    "User-controlled input is written into the response unescaped.",
			Severity:       v1alpha1.LevelHigh,
			Repository: &v1alpha1.FindingRepository{
				Type: v1alpha1.RepositoryTypeGitHub, URL: repoURL, Name: "acme/shop", DefaultBranch: "main",
			},
			Approval: &v1alpha1.Approval{By: approver.Login, ByID: approver.ID, At: now},
		},
	}
	if err := cl.client.Create(ctx, fnd); err != nil {
		t.Fatal(err)
	}
	fnd.Status.Phase = v1alpha1.PhaseQueued
	fnd.Status.PhaseTimes = []v1alpha1.PhaseTime{{Phase: v1alpha1.PhaseQueued, At: now}}
	fnd.Status.Attempts = v1alpha1.AttemptCounts{Investigation: 1}
	fnd.Status.Investigation = &v1alpha1.InvestigationSummary{
		Name: name + "-inv-1", Attempt: 1, Outcome: "ok",
		Recommendation: v1alpha1.RecommendationRemediate, Confidence: "0.9",
		Exploitability: v1alpha1.RatingHigh, Likelihood: v1alpha1.RatingHigh, Impact: v1alpha1.RatingHigh,
		CompletedAt: &now,
	}
	if err := cl.client.Status().Update(ctx, fnd); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Create(ctx, &v1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-src", Namespace: namespace},
		Spec:       v1alpha1.RepositorySpec{URL: repoURL},
	}); err != nil {
		t.Fatal(err)
	}
	inv := &v1alpha1.Investigation{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-inv-1", Namespace: namespace},
		Spec: v1alpha1.InvestigationSpec{
			FindingRef:    v1alpha1.ObjectReference{Name: name, UID: fnd.UID},
			Attempt:       1,
			RepositoryRef: &v1alpha1.LocalObjectReference{Name: name + "-src"},
		},
	}
	if err := cl.client.Create(ctx, inv); err != nil {
		t.Fatal(err)
	}
	inv.Status.Phase = v1alpha1.RunComplete
	inv.Status.Report = "---\nanalysis---\nfix the sink\n"
	if err := cl.client.Status().Update(ctx, inv); err != nil {
		t.Fatal(err)
	}
	return fnd
}

// codexTurn sends one small Responses request under token, as brokered
// codex does, and returns the answer's status and body.
func codexTurn(t *testing.T, base, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/openai/v1/responses",
		strings.NewReader(`{"model":"gpt-5.3-codex","max_output_tokens":16,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+provider.PlaceholderAuthToken)
	req.Header.Set(provider.BrokerTokenHeader, token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}
