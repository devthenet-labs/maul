// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/web/auth"
)

var (
	accountOctocat = &auth.Identity{Username: "github:octocat", DisplayName: "octocat", Session: true, GitHubID: 42}
	accountNoRole  = &auth.Identity{Username: "github:hubot", DisplayName: "hubot", Session: true, GitHubID: 7}
	accountLDAP    = &auth.Identity{Username: "ldap:octocat", DisplayName: "Octo", Session: true}
)

const apiKey = "sk-ant-api03-the-whole-secret-value-0000-abcd"

// accountGranter grants modelcredentials/self to everyone but github:hubot.
type accountGranter struct{ asked []string }

func (g *accountGranter) Allowed(_ context.Context, id auth.Identity, verb string) (bool, error) {
	g.asked = append(g.asked, id.Username+" "+verb)
	return id.Username != "github:hubot", nil
}

// fakeStore keeps records in memory and remembers the credential each Put
// stored, so a test can check the handler stored it and never returned it.
type fakeStore struct {
	records map[string]*v1alpha1.ModelCredential
	stored  map[string]string
	writes  []CredentialWrite
	deletes []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{records: map[string]*v1alpha1.ModelCredential{}, stored: map[string]string{}}
}

func (f *fakeStore) Record(_ context.Context, owner string, p v1alpha1.ModelCredentialProvider) (
	*v1alpha1.ModelCredential, error) {
	return f.records[v1alpha1.ModelCredentialName(owner, p)], nil
}

func (f *fakeStore) Put(_ context.Context, w CredentialWrite) (*v1alpha1.ModelCredential, error) {
	name := v1alpha1.ModelCredentialName(w.Owner, w.Provider)
	rec := &v1alpha1.ModelCredential{
		Spec: v1alpha1.ModelCredentialSpec{Owner: w.Owner, OwnerLogin: w.OwnerLogin, Provider: w.Provider,
			Kind: w.Kind},
		Status: v1alpha1.ModelCredentialStatus{Fingerprint: v1alpha1.ModelCredentialFingerprint([]byte(w.Credential)),
			Health: v1alpha1.ModelCredentialUnknown},
	}
	rec.Name = name
	f.records[name], f.stored[name] = rec, w.Credential
	f.writes = append(f.writes, w)
	return rec, nil
}

func (f *fakeStore) Delete(_ context.Context, owner string, p v1alpha1.ModelCredentialProvider) error {
	name := v1alpha1.ModelCredentialName(owner, p)
	delete(f.records, name)
	delete(f.stored, name)
	f.deletes = append(f.deletes, name)
	return nil
}

func accountServer(t *testing.T, providers ...v1alpha1.ModelCredentialProvider) (*Server, *fakeStore) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).Build()
	store := newFakeStore()
	s := NewServer(c, "maul", stubAuth{}, stubGranter{grants: allGrants()}, nil).
		WithAccount(AccountOptions{Self: &accountGranter{}, Store: store, Providers: providers})
	return s, store
}

// post sends a POST with the browser's same-origin header and contentType,
// unless site or contentType is empty.
func post(t *testing.T, ts *httptest.Server, path, site, contentType, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

const putPath = "/api/account/credentials/anthropic"

func TestAccountRoutesAreOffByDefault(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).Build()
	s := NewServer(c, "maul", stubAuth{}, stubGranter{grants: allGrants()}, nil)
	ts := as(t, s, accountOctocat)
	if code, _ := get(t, ts, "/api/account"); code != http.StatusNotFound {
		t.Errorf("GET /api/account with the page off = %d, want 404", code)
	}
	if code, _ := post(t, ts, putPath, "same-origin", "application/json", `{}`); code == http.StatusOK {
		t.Errorf("a credential write with the page off = %d", code)
	}
}

func TestAccountRequiresASessionAndAGitHubIdentity(t *testing.T) {
	s, store := accountServer(t)
	ts := as(t, s, nil)
	if code, _ := get(t, ts, "/api/account"); code != http.StatusUnauthorized {
		t.Errorf("GET without a session = %d, want 401", code)
	}
	ts = as(t, s, accountLDAP)
	code, body := get(t, ts, "/api/account")
	if a := decode[Account](t, body); code != http.StatusOK || a.Self || !strings.Contains(a.Reason, "GitHub") {
		t.Errorf("GET as a non-GitHub identity = %d %+v", code, a)
	}
	code, body = post(t, ts, putPath, "same-origin", "application/json",
		`{"kind":"api-key","credential":"`+apiKey+`"}`)
	if code != http.StatusForbidden || !strings.Contains(body, "GitHub") || len(store.writes) != 0 {
		t.Errorf("a write as a non-GitHub identity = %d %q, writes %d", code, body, len(store.writes))
	}
}

func TestAccountRequiresTheSelfRole(t *testing.T) {
	s, store := accountServer(t)
	ts := as(t, s, accountNoRole)
	code, body := get(t, ts, "/api/account")
	if a := decode[Account](t, body); code != http.StatusOK || !a.Self || a.Allowed || len(a.Credentials) != 0 {
		t.Errorf("GET without the role = %d %+v", code, a)
	}
	for _, path := range []string{putPath, putPath + "/delete"} {
		code, body := post(t, ts, path, "same-origin", "application/json",
			`{"kind":"api-key","credential":"`+apiKey+`"}`)
		if code != http.StatusForbidden || !strings.Contains(body, "modelcredentials/self") {
			t.Errorf("POST %s without the role = %d %q", path, code, body)
		}
	}
	if len(store.writes)+len(store.deletes) != 0 {
		t.Error("the store was written without the role")
	}
}

// TestAccountStoresWriteOnly: a person sets, replaces and deletes their own
// credential, stored under their GitHub id whatever the body says, and no
// answer ever carries the credential.
func TestAccountStoresWriteOnly(t *testing.T) {
	s, store := accountServer(t)
	ts := as(t, s, accountOctocat)
	code, body := get(t, ts, "/api/account")
	a := decode[Account](t, body)
	if code != http.StatusOK || !a.Self || !a.Allowed || len(a.Credentials) != 1 || a.Credentials[0].Stored {
		t.Fatalf("GET before storing = %d %+v", code, a)
	}

	code, body = post(t, ts, putPath, "same-origin", "application/json; charset=utf-8",
		`{"kind":"api-key","credential":"  `+apiKey+`\n"}`)
	if code != http.StatusOK {
		t.Fatalf("store = %d %q", code, body)
	}
	if strings.Contains(body, apiKey) || strings.Contains(body, "the-whole-secret") {
		t.Fatalf("the answer carries the credential: %s", body)
	}
	got := decode[AccountCredential](t, body)
	if !got.Stored || got.Kind != "api-key" || got.Health != "Unknown" || !strings.HasPrefix(got.Fingerprint, "abcd:") {
		t.Errorf("stored facts = %+v", got)
	}
	name := v1alpha1.ModelCredentialName("github-id:42", v1alpha1.ModelCredentialProviderAnthropic)
	if store.stored[name] != apiKey {
		t.Errorf("stored %q, want the trimmed credential", store.stored[name])
	}
	if w := store.writes[0]; w.Owner != "github-id:42" || w.OwnerLogin != "octocat" || w.By != "github:octocat" {
		t.Errorf("write = %+v, want the signed-in person's", w)
	}

	checkAfterVerdict(t, ts, store.records[name])

	code, _ = post(t, ts, putPath+"/delete", "same-origin", "", "")
	if code != http.StatusNoContent || store.records[name] != nil {
		t.Fatalf("delete = %d, record %+v", code, store.records[name])
	}
}

// checkAfterVerdict: once the broker found rec Valid and used it, GET shows
// so, and still not the credential.
func checkAfterVerdict(t *testing.T, ts *httptest.Server, rec *v1alpha1.ModelCredential) {
	t.Helper()
	rec.Status.Health = v1alpha1.ModelCredentialValid
	used := metav1.NewTime(testClock)
	rec.Status.LastUsedAt = &used
	_, body := get(t, ts, "/api/account")
	if strings.Contains(body, apiKey) {
		t.Fatalf("GET carries the credential: %s", body)
	}
	if c := decode[Account](t, body).Credentials[0]; c.Health != "Valid" || c.LastUsedAt == "" || c.LastRejectedAt != "" {
		t.Errorf("GET after the broker's verdict = %+v", c)
	}
}

// TestAccountRefusesBadWrites: every write is held to its shape before the
// store is touched.
func TestAccountRefusesBadWrites(t *testing.T) {
	s, store := accountServer(t)
	ts := as(t, s, accountOctocat)
	for _, tc := range []struct {
		name, path, site, ctype, body string
		want                          int
	}{
		{"cross-site", putPath, "cross-site", "application/json", `{"kind":"api-key","credential":"x"}`,
			http.StatusForbidden},
		{"no Sec-Fetch-Site", putPath, "", "application/json", `{"kind":"api-key","credential":"x"}`,
			http.StatusForbidden},
		{"a form post", putPath, "same-origin", "application/x-www-form-urlencoded", "kind=api-key",
			http.StatusUnsupportedMediaType},
		{"an unknown field", putPath, "same-origin", "application/json",
			`{"kind":"api-key","credential":"` + apiKey + `","owner":"github-id:7"}`, http.StatusBadRequest},
		{"two objects", putPath, "same-origin", "application/json",
			`{"kind":"api-key","credential":"` + apiKey + `"}{}`, http.StatusBadRequest},
		{"an unknown kind", putPath, "same-origin", "application/json",
			`{"kind":"password","credential":"` + apiKey + `"}`, http.StatusBadRequest},
		{"a header injection", putPath, "same-origin", "application/json",
			`{"kind":"api-key","credential":"a\r\nX-Evil: 1"}`, http.StatusBadRequest},
		{"the other kind's token", putPath, "same-origin", "application/json",
			`{"kind":"api-key","credential":"sk-ant-oat01-xyz"}`, http.StatusBadRequest},
		{"too large", putPath, "same-origin", "application/json",
			`{"kind":"api-key","credential":"` + strings.Repeat("a", 9000) + `"}`, http.StatusRequestEntityTooLarge},
		{"an unknown provider", "/api/account/credentials/bedrock", "same-origin", "application/json",
			`{"kind":"api-key","credential":"` + apiKey + `"}`, http.StatusNotFound},
		{"a delete with a body", putPath + "/delete", "same-origin", "application/json", `{}`,
			http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := post(t, ts, tc.path, tc.site, tc.ctype, tc.body)
			if code != tc.want {
				t.Errorf("= %d %q, want %d", code, body, tc.want)
			}
		})
	}
	if len(store.writes)+len(store.deletes) != 0 {
		t.Errorf("a refused write reached the store: %+v %+v", store.writes, store.deletes)
	}
}

const (
	openaiPath = "/api/account/credentials/openai"
	openaiKey  = "sk-proj-0123456789abcdefghijwxyz"
)

// TestAccountOpenAI: with OpenAI offered (--account-providers anthropic,
// openai), the page lists both providers in order with the kinds each
// takes; an OpenAI API key is stored under the person's openai record,
// beside and apart from their Anthropic one; and an OpenAI write is held
// to OpenAI's rules (an API key only, never an Anthropic value), as an
// Anthropic one is to Anthropic's (never an OpenAI key).
func TestAccountOpenAI(t *testing.T) {
	s, store := accountServer(t, v1alpha1.ModelCredentialProviderAnthropic, v1alpha1.ModelCredentialProviderOpenAI)
	ts := as(t, s, accountOctocat)
	checkAccountListsBoth(t, ts)

	checkAccountProviderRules(t, ts, store)

	code, body := post(t, ts, openaiPath, "same-origin", "application/json",
		`{"kind":"api-key","credential":"`+openaiKey+`"}`)
	if code != http.StatusOK || strings.Contains(body, openaiKey) {
		t.Fatalf("store openai = %d %q", code, body)
	}
	got := decode[AccountCredential](t, body)
	if got.Provider != "openai" || !got.Stored || got.Kind != "api-key" || len(got.Kinds) != 1 {
		t.Errorf("stored facts = %+v", got)
	}
	name := v1alpha1.ModelCredentialName("github-id:42", v1alpha1.ModelCredentialProviderOpenAI)
	if store.stored[name] != openaiKey || store.writes[0].Provider != v1alpha1.ModelCredentialProviderOpenAI {
		t.Errorf("stored %q via %+v", store.stored[name], store.writes[0])
	}
	_, body = get(t, ts, "/api/account")
	if a := decode[Account](t, body); a.Credentials[0].Stored || !a.Credentials[1].Stored {
		t.Errorf("after storing openai: %+v", a.Credentials)
	}
	if code, _ := post(t, ts, openaiPath+"/delete", "same-origin", "", ""); code != http.StatusNoContent ||
		store.records[name] != nil {
		t.Errorf("delete openai = %d", code)
	}
}

// checkAccountListsBoth: anthropic then openai, each with its kinds.
func checkAccountListsBoth(t *testing.T, ts *httptest.Server) {
	t.Helper()
	_, body := get(t, ts, "/api/account")
	a := decode[Account](t, body)
	if len(a.Credentials) != 2 || a.Credentials[0].Provider != "anthropic" || a.Credentials[1].Provider != "openai" {
		t.Fatalf("credentials = %+v, want anthropic then openai", a.Credentials)
	}
	if k := a.Credentials[0].Kinds; len(k) != 2 || k[0] != "api-key" || k[1] != "oauth-token" {
		t.Errorf("anthropic kinds = %v", k)
	}
	if k := a.Credentials[1].Kinds; len(k) != 1 || k[0] != "api-key" {
		t.Errorf("openai kinds = %v", k)
	}
}

// checkAccountProviderRules: each provider's write is held to its own
// rules before the store is touched.
func checkAccountProviderRules(t *testing.T, ts *httptest.Server, store *fakeStore) {
	t.Helper()
	for _, tc := range []struct {
		name, path, body string
	}{
		{"an openai setup-token", openaiPath, `{"kind":"oauth-token","credential":"` + openaiKey + `"}`},
		{"an anthropic key as openai's", openaiPath, `{"kind":"api-key","credential":"sk-ant-api03-abc"}`},
		{"an openai key as anthropic's", putPath, `{"kind":"api-key","credential":"` + openaiKey + `"}`},
	} {
		if code, body := post(t, ts, tc.path, "same-origin", "application/json", tc.body); code !=
			http.StatusBadRequest {
			t.Errorf("%s = %d %q, want 400", tc.name, code, body)
		}
	}
	if len(store.writes) != 0 {
		t.Fatalf("a refused write reached the store: %+v", store.writes)
	}

}

// TestAccountOffersOnlyItsProviders: by default (no providers configured)
// the page offers Anthropic alone, as before OpenAI keys existed, and an
// OpenAI write is a 404.
func TestAccountOffersOnlyItsProviders(t *testing.T) {
	s, store := accountServer(t)
	ts := as(t, s, accountOctocat)
	_, body := get(t, ts, "/api/account")
	if a := decode[Account](t, body); len(a.Credentials) != 1 || a.Credentials[0].Provider != "anthropic" {
		t.Fatalf("credentials = %+v, want anthropic alone", a.Credentials)
	}
	if code, _ := post(t, ts, openaiPath, "same-origin", "application/json",
		`{"kind":"api-key","credential":"`+openaiKey+`"}`); code != http.StatusNotFound {
		t.Errorf("an openai write while not offered = %d, want 404", code)
	}
	if len(store.writes) != 0 {
		t.Errorf("writes = %+v", store.writes)
	}
}

// TestAccountKeepsAStoredCredentialItNoLongerOffers: a key stored while its
// runner was on stays visible and deletable after the operator turns the
// runner off (the provider drops out of --account-providers); it can no
// longer be replaced, and a provider with nothing stored stays hidden.
func TestAccountKeepsAStoredCredentialItNoLongerOffers(t *testing.T) {
	s, store := accountServer(t, v1alpha1.ModelCredentialProviderAnthropic)
	if _, err := store.Put(t.Context(), CredentialWrite{Owner: "github-id:42", OwnerLogin: "octocat",
		Provider: v1alpha1.ModelCredentialProviderOpenAI, Kind: v1alpha1.ModelCredentialAPIKey,
		Credential: openaiKey, By: "github:octocat"}); err != nil {
		t.Fatal(err)
	}
	store.writes = nil
	ts := as(t, s, accountOctocat)

	_, body := get(t, ts, "/api/account")
	a := decode[Account](t, body)
	if len(a.Credentials) != 2 || a.Credentials[0].Provider != "anthropic" || len(a.Credentials[0].Kinds) == 0 {
		t.Fatalf("credentials = %+v, want the offered anthropic card first", a.Credentials)
	}
	if c := a.Credentials[1]; c.Provider != "openai" || !c.Stored || len(c.Kinds) != 0 {
		t.Fatalf("the stored openai card = %+v, want stored with no kinds", c)
	}

	if code, _ := post(t, ts, openaiPath, "same-origin", "application/json",
		`{"kind":"api-key","credential":"`+openaiKey+`"}`); code != http.StatusNotFound {
		t.Errorf("replacing a credential no longer offered = %d, want 404", code)
	}
	if len(store.writes) != 0 {
		t.Errorf("writes = %+v", store.writes)
	}
	name := v1alpha1.ModelCredentialName("github-id:42", v1alpha1.ModelCredentialProviderOpenAI)
	if code, _ := post(t, ts, openaiPath+"/delete", "same-origin", "", ""); code != http.StatusNoContent ||
		store.records[name] != nil {
		t.Fatalf("deleting a credential no longer offered = %d", code)
	}
	_, body = get(t, ts, "/api/account")
	if a := decode[Account](t, body); len(a.Credentials) != 1 || a.Credentials[0].Provider != "anthropic" {
		t.Errorf("after the delete: %+v, want anthropic alone", a.Credentials)
	}
	if code, _ := post(t, ts, "/api/account/credentials/gemini/delete", "same-origin", "", ""); code !=
		http.StatusNotFound {
		t.Errorf("deleting an unknown provider = %d, want 404", code)
	}
}
