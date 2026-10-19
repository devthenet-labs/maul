// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/web/auth"
)

// The Account page (statusServer.account.enabled; per-user credentials
// design L3, "Dashboard"): a signed-in person sets, replaces and deletes
// their own model credential for each provider the enabled runners use
// (Anthropic, OpenAI), and sees its fingerprint, health and when it was
// last used. Write-only: no route ever returns a stored credential, and
// the store behind it (CredentialStore) cannot read one. A person is their
// numeric GitHub id, which only a Dex sign-in through the GitHub connector
// carries (auth.Identity.GitHubID); the owner is always stamped from it,
// never taken from the request, and every route is gated by an access
// review on the virtual modelcredentials/self.

// AccountSelfResource and AccountSelfSubresource name the page's grant:
// get, update and delete on modelcredentials/self in the release namespace
// (the chart's <release>-credentials-self ClusterRole). Virtual: nothing on
// the API server serves it.
const (
	AccountSelfResource    = "modelcredentials"
	AccountSelfSubresource = "self"
)

// maxCredentialBody caps a credential write's body: a credential is at most
// v1alpha1.ModelCredentialSecretMaxBytes, its JSON a little more.
const maxCredentialBody = 8 << 10

// AccountGranter answers whether an identity may verb its own credential;
// authz.ResourceReviewer (NewSubresourceReviewer on modelcredentials/self)
// is the SubjectAccessReview implementation.
type AccountGranter interface {
	Allowed(ctx context.Context, id auth.Identity, verb string) (bool, error)
}

// CredentialWrite is one person's credential to store.
type CredentialWrite struct {
	// Owner is "github-id:<id>", from the signed-in identity.
	Owner string
	// OwnerLogin is the person's GitHub login for display, "" if unknown.
	OwnerLogin string
	Provider   v1alpha1.ModelCredentialProvider
	Kind       v1alpha1.ModelCredentialKind
	// Credential is the value as the broker will send it
	// (v1alpha1.ModelCredentialValue).
	Credential string
	// By is the signed-in username, for the audit line.
	By string
}

// CredentialStore is the Account page's port to the credential store: the
// non-secret record by its derived name, and writes that never read a
// stored credential back. internal/web/credstore is the Kubernetes adapter.
type CredentialStore interface {
	// Record reads owner's ModelCredential for provider; nil when none.
	Record(ctx context.Context, owner string, provider v1alpha1.ModelCredentialProvider) (
		*v1alpha1.ModelCredential, error)
	// Put stores the credential and its record, and returns the record.
	Put(ctx context.Context, w CredentialWrite) (*v1alpha1.ModelCredential, error)
	// Delete removes owner's credential and record for provider; deleting
	// what is not there is not an error.
	Delete(ctx context.Context, owner string, provider v1alpha1.ModelCredentialProvider) error
}

// AccountOptions turn the Account page on. Turning it on also hardens the
// browser envelope of the whole server, as the intents views do.
type AccountOptions struct {
	// Self gates every route. Required.
	Self AccountGranter
	// Store is the credential store. Required.
	Store CredentialStore
	// Providers are the providers a person may store a credential for, in
	// the order the page shows them (--account-providers: the providers the
	// enabled runners use); each must be one of
	// v1alpha1.ModelCredentialProviders. Empty is Anthropic alone.
	Providers []v1alpha1.ModelCredentialProvider
}

// accountState is the server's account side; nil while the page is off.
type accountState struct {
	self      AccountGranter
	store     CredentialStore
	providers []v1alpha1.ModelCredentialProvider
}

// WithAccount enables the Account page.
func (s *Server) WithAccount(o AccountOptions) *Server {
	providers := slices.Clone(o.Providers)
	if len(providers) == 0 {
		providers = []v1alpha1.ModelCredentialProvider{v1alpha1.ModelCredentialProviderAnthropic}
	}
	s.account = &accountState{self: o.Self, store: o.Store, providers: providers}
	return s
}

// Account is what GET /api/account serves. Mirrors ui/src/types.ts.
type Account struct {
	// Name is the caller's display name.
	Name string `json:"name"`
	// Self is whether the caller has an identity a credential is kept
	// under (a GitHub sign-in); Reason says why not.
	Self bool `json:"self"`
	// Allowed is whether the caller holds get on modelcredentials/self;
	// Reason says why not.
	Allowed bool `json:"allowed"`
	// Reason explains a refused Self or Allowed, "" otherwise.
	Reason string `json:"reason"`
	// Credentials is one entry per provider; empty unless Self and Allowed.
	Credentials []AccountCredential `json:"credentials"`
}

// AccountCredential is what the page shows of one provider's credential:
// never the credential, only its non-secret facts. Mirrors ui/src/types.ts.
type AccountCredential struct {
	Provider string `json:"provider"`
	// Kinds are the kinds the provider takes, in the order the page offers
	// them (v1alpha1.ModelCredentialKinds): an Anthropic API key or
	// setup-token, an OpenAI API key.
	Kinds []string `json:"kinds"`
	// Stored is whether a credential is stored for the provider.
	Stored bool `json:"stored"`
	// Kind is "api-key" or "oauth-token"; "" when none is stored.
	Kind string `json:"kind"`
	// Fingerprint is the stored credential's last four characters and a
	// SHA-256 prefix.
	Fingerprint string `json:"fingerprint"`
	// Health is Valid, Rejected or Unknown (the egress broker's probe).
	Health string `json:"health"`
	// LastUsedAt and LastRejectedAt are RFC 3339, "" when never.
	LastUsedAt     string `json:"lastUsedAt"`
	LastRejectedAt string `json:"lastRejectedAt"`
}

// credentialRequest is the body of a credential write.
type credentialRequest struct {
	Kind       string `json:"kind"`
	Credential string `json:"credential"`
}

// notGitHubAccountReason is the one explanation for a caller without a
// GitHub id.
const notGitHubAccountReason = "Credentials are kept per GitHub account. Sign in with GitHub (through the GitHub " +
	"connector) to manage yours; if you already did, sign out and in again so your session carries your GitHub id."

// registerAccount mounts the account routes. Every write is a POST: the
// middleware caps its body and refuses it cross-origin, and the hardened
// envelope requires Sec-Fetch-Site.
func (s *Server) registerAccount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/account", s.handleAccount)
	mux.HandleFunc("POST /api/account/credentials/{provider}", s.handleCredentialPut)
	mux.HandleFunc("POST /api/account/credentials/{provider}/delete", s.handleCredentialDelete)
}

// accountCaller identifies the caller, holds them to a GitHub identity and
// to verb on modelcredentials/self, answering the request itself on
// failure.
func (s *Server) accountCaller(w http.ResponseWriter, r *http.Request, verb string) (auth.Identity, bool) {
	id, ok := s.identify(w, r)
	if !ok {
		return id, false
	}
	if id.GitHubID < 1 {
		http.Error(w, notGitHubAccountReason, http.StatusForbidden)
		return id, false
	}
	allowed, err := s.account.self.Allowed(r.Context(), id, verb)
	if err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "account access review", slog.Any("error", err))
		http.Error(w, "authorization failed", http.StatusInternalServerError)
		return id, false
	}
	if !allowed {
		http.Error(w, "Permission denied. User "+quoteName(id.Display())+" may not "+verb+" "+
			AccountSelfResource+"/"+AccountSelfSubresource+" in namespace "+quoteName(s.namespace)+".",
			http.StatusForbidden)
		return id, false
	}
	return id, true
}

func quoteName(s string) string { b, _ := json.Marshal(s); return string(b) }

// handleAccount serves the caller's credentials' facts, or why the page
// cannot show them.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	out := Account{Name: id.Display(), Self: id.GitHubID > 0, Credentials: []AccountCredential{}}
	if !out.Self {
		out.Reason = notGitHubAccountReason
		writeJSON(w, out)
		return
	}
	allowed, err := s.account.self.Allowed(r.Context(), id, "get")
	if err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "account access review", slog.Any("error", err))
		http.Error(w, "authorization failed", http.StatusInternalServerError)
		return
	}
	out.Allowed = allowed
	if !allowed {
		out.Reason = "You may not manage a model credential here: ask an administrator to bind the " +
			"credentials-self role to you."
		writeJSON(w, out)
		return
	}
	owner := v1alpha1.ModelCredentialOwner(id.GitHubID)
	for _, p := range s.account.providers {
		rec, err := s.account.store.Record(r.Context(), owner, p)
		if err != nil {
			s.log.LogAttrs(r.Context(), slog.LevelError, "read model credential", slog.Any("error", err))
			http.Error(w, "failed to load credentials", http.StatusInternalServerError)
			return
		}
		out.Credentials = append(out.Credentials, accountCredential(p, rec))
	}
	writeJSON(w, out)
}

// accountCredential is what the page shows of rec, nil when none.
func accountCredential(p v1alpha1.ModelCredentialProvider, rec *v1alpha1.ModelCredential) AccountCredential {
	out := AccountCredential{Provider: string(p), Kinds: []string{}}
	for _, k := range v1alpha1.ModelCredentialKinds(p) {
		out.Kinds = append(out.Kinds, string(k))
	}
	if rec == nil {
		return out
	}
	out.Stored = true
	out.Kind = string(rec.Spec.Kind)
	out.Fingerprint = rec.Status.Fingerprint
	out.Health = string(rec.Status.Health)
	if out.Health == "" {
		out.Health = string(v1alpha1.ModelCredentialUnknown)
	}
	if t := rec.Status.LastUsedAt; t != nil {
		out.LastUsedAt = t.UTC().Format(time.RFC3339)
	}
	if t := rec.Status.LastRejectedAt; t != nil {
		out.LastRejectedAt = t.UTC().Format(time.RFC3339)
	}
	return out
}

// accountProvider reads the {provider} path value, answering one the page
// does not offer itself.
func (s *Server) accountProvider(w http.ResponseWriter, r *http.Request) (v1alpha1.ModelCredentialProvider, bool) {
	p := v1alpha1.ModelCredentialProvider(r.PathValue("provider"))
	if slices.Contains(s.account.providers, p) {
		return p, true
	}
	notFound(w)
	return "", false
}

// credentialFormats says, per provider, what a stored value looks like,
// for the refusal of one that does not.
var credentialFormats = map[v1alpha1.ModelCredentialProvider]string{
	v1alpha1.ModelCredentialProviderAnthropic: "an Anthropic API key starts sk-ant-api, a setup-token sk-ant-oat",
	v1alpha1.ModelCredentialProviderOpenAI:    "an OpenAI API key starts sk- and never sk-ant-",
}

// loginRE is the GitHub login grammar the record's ownerLogin admits.
var loginRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// handleCredentialPut sets or replaces the caller's credential for the
// provider: a JSON body {kind, credential} of at most maxCredentialBody
// bytes, Content-Type application/json, no other field. The answer is the
// stored credential's facts, never the credential.
func (s *Server) handleCredentialPut(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.accountProvider(w, r)
	if !ok {
		return
	}
	id, ok := s.accountCaller(w, r, "update")
	if !ok {
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "the body must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var req credentialRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCredentialBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "the body is too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "the body must be {\"kind\": ..., \"credential\": ...}", http.StatusBadRequest)
		return
	}
	if dec.More() {
		http.Error(w, "the body must be one JSON object", http.StatusBadRequest)
		return
	}
	kind := v1alpha1.ModelCredentialKind(req.Kind)
	if !v1alpha1.ModelCredentialKindAllowed(provider, kind) {
		kinds := make([]string, 0, 2)
		for _, k := range v1alpha1.ModelCredentialKinds(provider) {
			kinds = append(kinds, string(k))
		}
		http.Error(w, "kind must be "+strings.Join(kinds, " or ")+" for "+string(provider), http.StatusBadRequest)
		return
	}
	value, ok := v1alpha1.ModelCredentialValue(req.Credential, provider, kind)
	if !ok {
		http.Error(w, "the credential must be one token of printable characters, at most 3072 bytes, of the "+
			"kind and provider chosen ("+credentialFormats[provider]+")", http.StatusBadRequest)
		return
	}
	login := ""
	if loginRE.MatchString(id.DisplayName) {
		login = id.DisplayName
	}
	owner := v1alpha1.ModelCredentialOwner(id.GitHubID)
	rec, err := s.account.store.Put(r.Context(), CredentialWrite{
		Owner: owner, OwnerLogin: login, Provider: provider, Kind: kind, Credential: value, By: id.Username,
	})
	if err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "store model credential", slog.String("owner", owner),
			slog.String("provider", string(provider)), slog.Any("error", err))
		http.Error(w, "failed to store the credential", http.StatusInternalServerError)
		return
	}
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "model credential stored", slog.String("by", id.Username),
		slog.String("owner", owner), slog.String("provider", string(provider)), slog.String("kind", string(kind)),
		slog.String("fingerprint", rec.Status.Fingerprint))
	writeJSON(w, accountCredential(provider, rec))
}

// handleCredentialDelete deletes the caller's credential for the provider.
// It takes no body.
func (s *Server) handleCredentialDelete(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.accountProvider(w, r)
	if !ok {
		return
	}
	id, ok := s.accountCaller(w, r, "delete")
	if !ok {
		return
	}
	var probe [1]byte
	if n, _ := r.Body.Read(probe[:]); n > 0 {
		http.Error(w, "delete takes no body", http.StatusBadRequest)
		return
	}
	owner := v1alpha1.ModelCredentialOwner(id.GitHubID)
	if err := s.account.store.Delete(r.Context(), owner, provider); err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "delete model credential", slog.String("owner", owner),
			slog.String("provider", string(provider)), slog.Any("error", err))
		http.Error(w, "failed to delete the credential", http.StatusInternalServerError)
		return
	}
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "model credential deleted", slog.String("by", id.Username),
		slog.String("owner", owner), slog.String("provider", string(provider)))
	w.WriteHeader(http.StatusNoContent)
}
