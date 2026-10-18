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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/e2e/fakeoidc"
	"github.com/bitwise-media-group/patchy/internal/provider"
	"github.com/bitwise-media-group/patchy/internal/rbactest"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

const (
	// credentialsNS is the namespace the people's credential Secrets live
	// in, as the chart renders it by default.
	credentialsNS = v1alpha1.DefaultCredentialsNamespace
	// orgModelKey is the org credential: the broker's mounted API key.
	orgModelKey = "e2e-org-model-key"
	// personalKey is the approver's own Anthropic API key, as they paste it
	// into the Account page.
	personalKey = "sk-ant-api03-octocat-e2e-personal-key"
	// revocationCap is the most time a revoked credential may still be used
	// (design revision 1, L3 "Enforce fails closed").
	revocationCap = 60 * time.Second
)

// TestUserCredentials runs per-user model credentials end to end under
// egressBroker.userCredentials.mode enforce, as the chart wires it: the
// shipped egress-broker (enforce, the records and Secrets read from the
// cluster, health probed against a fake Anthropic upstream that records
// which credential every request carried), intent-controller holding a
// person's run until their credential is Valid, and status-server's
// Account page, where the person stores theirs, signed in through a
// Dex-shaped issuer that names their GitHub id.
//
// It walks the design's L3 story: an approver with no credential files an
// intent, which is Blocked CredentialRequired with no run created and an
// issue that names them and links the Account page; they store an API key
// there; the broker's own probe finds it Valid and the plan runs, without a
// replan; a request from that plan's pod reaches the upstream with the
// person's key, never the org's, while an org-attributed pod still pays
// with the org key; enforce fails closed for a pod with no attribution and
// for a person with no credential, and nothing it refuses reaches the
// upstream; and once the person deletes their credential, their pod is
// refused within the revocation cap.
func TestUserCredentials(t *testing.T) {
	ctx := context.Background()
	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	base := "http://" + listen
	accountURL := base + "/#/account"

	e := startIntents(t, "--attribution-mode", "attribute", "--user-credentials", "enforce",
		"--account-url", accountURL)
	cs := e.cl.clientset(t)
	if err := e.cl.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: credentialsNS}}); err != nil {
		t.Fatal(err)
	}
	ensureAgentSA(t, cs)

	up := newCredentialUpstream(orgModelKey, personalKey)
	t.Cleanup(up.Close)
	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte(orgModelKey), 0o600); err != nil {
		t.Fatal(err)
	}
	brokerURL := startBroker(t, e.cl, up.URL, keyFile, "--user-credentials", "enforce",
		"--credential-records-namespace", namespace, "--credentials-namespace", credentialsNS)

	oidc, err := fakeoidc.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oidc.Close)
	oidc.AddUser(fakeoidc.User{Login: approver.Login, Name: "Octo Cat", GitHubID: approver.ID})
	grantCredentialsSelf(t, e.cl, "github:"+approver.Login)
	e.cl.controller(t, "status-server", "--listen-addr", listen,
		"--auth-config", writeAccountAuthConfig(t, oidc.URL, base+"/oauth2/callback"),
		"--agent-namespace", agentsNS, "--account-enabled", "--credentials-namespace", credentialsNS)

	// 1. No credential: the plan the approver's trigger opens is theirs, so
	// it is held. No run exists, and the issue says who and where.
	number, name := e.fileIntent(t)
	in := &v1alpha1.Intent{}
	eventually(t, "the intent to be Blocked on the approver's credential", func() bool {
		if err := e.cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, in); err != nil {
			return false
		}
		c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionCredentialRequired)
		return in.Status.Phase == v1alpha1.IntentBlocked && c != nil && c.Status == metav1.ConditionTrue &&
			c.Reason == "CredentialMissing"
	})
	if h := in.Status.CredentialHold; h == nil || h.ID != approver.ID || h.Login != approver.Login {
		t.Errorf("credentialHold = %+v, want the approver %s (%d)", h, approver.Login, approver.ID)
	}
	if runs := e.runs(t, name); len(runs) != 0 {
		t.Errorf("runs created while held: %s", e.describeRuns(t, name))
	}
	eventually(t, "the status comment to name the approver and link the Account page", func() bool {
		st := e.own(number, templates.IntentStatusMarker(namespace, name))
		return len(st) == 1 && strings.Contains(st[0].Body, "@"+approver.Login+" has no Anthropic credential") &&
			strings.Contains(st[0].Body, accountURL)
	})
	if n := up.count(""); n != 0 {
		t.Errorf("the upstream saw %d requests before anyone stored a credential", n)
	}

	// 2. The approver stores their key on the Account page.
	browser := signIn(t, base, oidc, approver.Login)
	var acct struct {
		Self        bool `json:"self"`
		Allowed     bool `json:"allowed"`
		Credentials []struct {
			Provider    string `json:"provider"`
			Stored      bool   `json:"stored"`
			Fingerprint string `json:"fingerprint"`
			Health      string `json:"health"`
		} `json:"credentials"`
	}
	getJSON(t, browser, base+"/api/account", &acct)
	if !acct.Self || !acct.Allowed || len(acct.Credentials) != 1 || acct.Credentials[0].Stored {
		t.Fatalf("account before storing = %+v, want the approver's own page with nothing stored", acct)
	}
	if code, body := accountPost(t, browser, base+"/api/account/credentials/anthropic",
		`{"kind":"api-key","credential":"`+personalKey+`"}`); code != http.StatusOK ||
		strings.Contains(body, personalKey) {
		t.Fatalf("store the credential = %d %s, want 200 without the credential echoed", code, body)
	}

	// 3. The broker's own probe finds it Valid, and the hold lifts without a
	// replan: the first plan run is created, attributed to the approver.
	owner := v1alpha1.ModelCredentialOwner(approver.ID)
	eventually(t, "the broker's probe to find the stored key Valid", func() bool {
		var rec v1alpha1.ModelCredential
		key := types.NamespacedName{Namespace: namespace,
			Name: v1alpha1.ModelCredentialName(owner, v1alpha1.ModelCredentialProviderAnthropic)}
		return e.cl.client.Get(ctx, key, &rec) == nil && rec.Status.Health == v1alpha1.ModelCredentialValid
	})
	if up.count("/v1/models", personalKey) == 0 {
		t.Errorf("the key went Valid without the broker probing it upstream")
	}
	planRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStagePlan, 1, "", 1)
	want := "github-id:" + strconv.FormatInt(approver.ID, 10)
	plan := e.kubelet.waitRun(t, "the held plan to run", phaseIs("plan"))
	checkAttributed(t, plan, e.run(t, planRun), v1alpha1.AttributionRequester, want)
	in = e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionCredentialRequired); c == nil ||
		c.Status != metav1.ConditionFalse {
		t.Errorf("CredentialRequired = %+v after the hold lifted, want False", c)
	}
	getJSON(t, browser, base+"/api/account", &acct)
	if c := acct.Credentials[0]; !c.Stored || c.Health != "Valid" ||
		!strings.HasPrefix(c.Fingerprint, personalKey[len(personalKey)-4:]) {
		t.Errorf("account after storing = %+v, want a Valid credential fingerprinted by its last four", c)
	}

	// 4. Delivery: the plan pod (as the Job controller makes it, carrying
	// its Job's attribution) is the approver's, so their key pays; an
	// org-attributed pod keeps the org key.
	planPod, err := cs.CoreV1().Pods(agentsNS).Get(ctx, plan.Job.Name+"-e2e", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	person := podToken(t, cs, planPod)
	if code, body := brokerTurn(t, brokerURL, person); code != http.StatusOK {
		t.Fatalf("the approver's pod: %d %s, want 200", code, body)
	}
	if up.count("/v1/messages", personalKey) != 1 || up.count("/v1/messages", orgModelKey) != 0 {
		t.Errorf("messages by credential = %v, want one on the approver's key and none on the org key", up.messages())
	}
	org := agentPodWith(t, cs, "org-run", map[string]string{v1alpha1.AnnotationAttributedTo: "org"})
	if code, body := brokerTurn(t, brokerURL, org); code != http.StatusOK {
		t.Fatalf("an org-attributed pod: %d %s, want 200", code, body)
	}
	if up.count("/v1/messages", orgModelKey) != 1 {
		t.Errorf("messages by credential = %v, want the org pod on the org key", up.messages())
	}

	// 5. Enforce fails closed: no attribution, or a person with no
	// credential, is refused with its reason, and never reaches the
	// upstream on the org key or any other.
	sent := up.count("")
	unattributed := agentPodWith(t, cs, "unattributed-run", nil)
	refused(t, brokerURL, unattributed, http.StatusForbidden, "attribution_missing")
	keyless := agentPodWith(t, cs, "keyless-run", map[string]string{
		v1alpha1.AnnotationAttributedTo: "github-id:" + strconv.FormatInt(mallory.ID, 10)})
	refused(t, brokerURL, keyless, http.StatusForbidden, "credential_missing")
	if n := up.count(""); n != sent {
		t.Errorf("a refused request reached the upstream: %d requests, want %d", n, sent)
	}

	// 6. Revocation: once the approver deletes their credential, their pod
	// is refused within the cap, however warm the broker's caches are.
	if code, body := accountPost(t, browser, base+"/api/account/credentials/anthropic/delete", ""); code !=
		http.StatusNoContent {
		t.Fatalf("delete the credential = %d %s, want 204", code, body)
	}
	revoked := time.Now()
	for {
		code, reason := brokerRefusal(t, brokerURL, person)
		if code == http.StatusForbidden &&
			(reason == "credential_missing" || reason == "credential_secret_missing") {
			break
		}
		if code != http.StatusOK {
			t.Fatalf("after revocation: %d %q, want 200 until refused as missing", code, reason)
		}
		if time.Since(revoked) > revocationCap {
			t.Fatalf("a deleted credential still paid %s after its deletion, past the %s cap",
				time.Since(revoked).Round(time.Second), revocationCap)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("revoked credential refused %s after its deletion", time.Since(revoked).Round(time.Millisecond))
	if up.count("", orgModelKey) != up.count("/v1/messages", orgModelKey) || up.count("/v1/messages", orgModelKey) != 1 {
		t.Errorf("the org key paid for more than the org pod: %v", up.messages())
	}
}

// credentialUpstream is a fake Anthropic API: it accepts the credentials it
// was given (as x-api-key or a bearer), answers the broker's health probe
// (GET /v1/models) and Messages, and records which credential each request
// carried.
type credentialUpstream struct {
	*httptest.Server
	valid map[string]bool
	mu    sync.Mutex
	seen  []upstreamRequest
}

type upstreamRequest struct{ path, credential string }

func newCredentialUpstream(valid ...string) *credentialUpstream {
	u := &credentialUpstream{valid: map[string]bool{}}
	for _, v := range valid {
		u.valid[v] = true
	}
	u.Server = httptest.NewServer(http.HandlerFunc(u.serve))
	return u
}

func (u *credentialUpstream) serve(w http.ResponseWriter, r *http.Request) {
	cred := r.Header.Get("x-api-key")
	if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		cred = b
	}
	if r.Header.Get(provider.BrokerTokenHeader) != "" {
		cred = "LEAKED-CALLER-TOKEN"
	}
	u.mu.Lock()
	u.seen = append(u.seen, upstreamRequest{path: r.URL.Path, credential: cred})
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !u.valid[cred] {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		_, _ = io.WriteString(w, `{"data":[{"type":"model","id":"claude-opus-5"}],"has_more":false}`)
	default:
		_, _ = io.WriteString(w, `{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-opus-5",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":10,"output_tokens":2}}`)
	}
}

// count is how many requests reached path (any, when empty) carrying the
// credential given (any, when none is).
func (u *credentialUpstream) count(path string, credential ...string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, s := range u.seen {
		if (path == "" || s.path == path) && (len(credential) == 0 || slices.Contains(credential, s.credential)) {
			n++
		}
	}
	return n
}

// messages is each Messages request's credential, by its last four
// characters, for a failure message.
func (u *credentialUpstream) messages() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for _, s := range u.seen {
		if s.path == "/v1/messages" {
			out = append(out, "…"+s.credential[max(len(s.credential)-4, 0):])
		}
	}
	return out
}

// ensureAgentSA creates the agent ServiceAccount the projected tokens are
// minted for, unless it exists.
func ensureAgentSA(t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	_, err := cs.CoreV1().ServiceAccounts(agentsNS).Create(context.Background(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agentSA},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

// grantCredentialsSelf binds the chart's credentials-self grant (the
// virtual modelcredentials/self) to user, and waits for the authorizer to
// see it.
func grantCredentialsSelf(t *testing.T, cl *cluster, user string) {
	t.Helper()
	ctx := context.Background()
	if err := cl.client.Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "patchy-credentials-self"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{v1alpha1.GroupVersion.Group},
			Resources: []string{"modelcredentials/self"}, Verbs: []string{"get", "update", "delete"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "credentials-self", Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "patchy-credentials-self"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
	}); err != nil {
		t.Fatal(err)
	}
	var reviews []authorizationv1.SubjectAccessReviewSpec
	for _, verb := range []string{"get", "update", "delete"} {
		reviews = append(reviews, rbactest.Review(user, nil, authorizationv1.ResourceAttributes{Namespace: namespace,
			Verb: verb, Group: v1alpha1.GroupVersion.Group, Resource: "modelcredentials", Subresource: "self"}))
	}
	rbactest.WaitAllowed(t, cl.client, reviews...)
}

// writeAccountAuthConfig is writeIntentsAuthConfig plus what the Account
// page needs: the federated:id scope, so the ID token names the person's
// GitHub id, and the GitHub connector it must come from.
func writeAccountAuthConfig(t *testing.T, issuer, redirect string) string {
	t.Helper()
	cfg := fmt.Sprintf(`mode: oidc
insecure: true
oidc:
  issuerURL: %s
  clientID: patchy-status-e2e
  clientSecret: e2e-status-client-secret
  redirectURL: %s
  scopes: [openid, profile, email, groups, federated:id]
  claims:
    username: preferred_username
    groups: groups
    usernamePrefix: "github:"
    groupsPrefix: "github:"
    githubConnector: github
`, issuer, redirect)
	path := filepath.Join(t.TempDir(), "auth.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// accountPost posts body (JSON, or nothing) as the Account page does: same
// origin, as the hardened envelope requires.
func accountPost(t *testing.T, c *http.Client, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// agentPodWith creates an agent pod carrying annotations and returns a
// projected token bound to it.
func agentPodWith(t *testing.T, cs kubernetes.Interface, name string, annotations map[string]string) string {
	t.Helper()
	pod, err := cs.CoreV1().Pods(agentsNS).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentsNS, Annotations: annotations},
		Spec: corev1.PodSpec{
			ServiceAccountName: agentSA,
			Containers:         []corev1.Container{{Name: "agent", Image: claudeRunnerImage}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod %s: %v", name, err)
	}
	return podToken(t, cs, pod)
}

// brokerTurn sends one small Messages request under token and returns the
// answer's status and body.
func brokerTurn(t *testing.T, base, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set(provider.BrokerTokenHeader, token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// brokerRefusal is brokerTurn, returning the refusal's reason (the
// parenthesised suffix of the broker's error message) when refused.
func brokerRefusal(t *testing.T, base, token string) (int, string) {
	t.Helper()
	code, body := brokerTurn(t, base, token)
	if code == http.StatusOK {
		return code, ""
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("refusal body %q: %v", body, err)
	}
	msg := e.Error.Message
	open := strings.LastIndex(msg, "(")
	if open < 0 || !strings.HasSuffix(msg, ")") {
		return code, msg
	}
	return code, msg[open+1 : len(msg)-1]
}

// refused asserts the broker refuses token with status and reason.
func refused(t *testing.T, base, token string, status int, reason string) {
	t.Helper()
	if code, got := brokerRefusal(t, base, token); code != status || got != reason {
		t.Errorf("refusal = %d %q, want %d %q", code, got, status, reason)
	}
}
