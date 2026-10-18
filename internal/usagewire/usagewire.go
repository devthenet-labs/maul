// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagewire

import (
	"regexp"
	"time"
)

// Audience is the projected-token audience the run controllers present to
// the usage endpoint. It differs from the agent pods' broker audience, so an
// agent token never authenticates here and a controller token never
// authenticates on the proxy.
const Audience = "patchy-egress-broker-usage"

// PathPrefix is the endpoint: GET PathPrefix + "<job name>".
const PathPrefix = "/v1/usage/jobs/"

// MaxJobName is the longest Job name the endpoint answers for. The Job
// controller names a pod GenerateName "<job>-" plus five characters and
// truncates the base past 58 characters, so beyond 57 a pod name no longer
// carries its whole Job name and a prefix could match another Job's pods.
const MaxJobName = 57

// jobNameRE is a DNS-1123 label, which every Job name is.
var jobNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidJobName reports whether name is a Job name the endpoint answers for.
func ValidJobName(name string) bool {
	return len(name) <= MaxJobName && jobNameRE.MatchString(name)
}

// podSuffixRE is the five characters the Job controller's name generator
// appends: lower-case alphanumerics, without vowels and some lookalikes in
// practice, but any of [a-z0-9] is accepted here.
var podSuffixRE = regexp.MustCompile(`^[a-z0-9]{5}$`)

// PodOfJob reports whether pod is a pod name the Job controller generates
// for job: "<job>-" and five characters.
func PodOfJob(pod, job string) bool {
	if len(pod) != len(job)+6 || pod[:len(job)] != job || pod[len(job)] != '-' {
		return false
	}
	return podSuffixRE.MatchString(pod[len(job)+1:])
}

// Credential kinds: what kind of credential paid for a model's tokens. An
// oauth-token is a Claude subscription (setup-token): its list-price cost is
// notional, never money spent.
const (
	CredentialAPIKey        = "api-key"
	CredentialOAuthToken    = "oauth-token"
	CredentialCloudIdentity = "cloud-identity"
)

// Reserved model keys: a pod's ledger keeps at most MaxModels distinct model
// ids, each matching ModelPattern. A model id outside the grammar is kept
// under ModelInvalid and any beyond the cap under ModelOther; neither can
// collide with a real id, since the grammar admits no parentheses.
const (
	MaxModels    = 8
	ModelInvalid = "(invalid)"
	ModelOther   = "(other)"
	ModelPattern = `^[a-z0-9][a-z0-9._:@-]{0,127}$`
)

// Response is the endpoint's answer.
type Response struct {
	// BootTime is when this broker process started its ledger: usage from
	// before it is not in any entry.
	BootTime time.Time `json:"bootTime"`
	// Pods are the ledger entries of the Job's pods, normally one.
	Pods []Pod `json:"pods"`
}

// Pod is one pod's ledger entry.
type Pod struct {
	Name string `json:"name"`
	// UID is the pod UID TokenReview reported, empty on an API server that
	// reports none.
	UID string `json:"uid,omitempty"`
	// FirstSeen is when the ledger first saw the pod.
	FirstSeen time.Time `json:"firstSeen"`
	// Inflight is the pod's requests not yet settled.
	Inflight int64 `json:"inflight"`
	// Models is the pod's usage per bare model id and credential kind:
	// one entry per model, two when the pod's requests were paid with two
	// kinds of credential (under per-user credentials the kind is each
	// request's, not the route's).
	Models []Model `json:"models"`
}

// Model is one pod's usage of one model, as the broker observed it in the
// responses, plus the shortfall it charged for responses whose usage it
// never saw in full.
type Model struct {
	// Model is the bare model id (provider prefixes stripped, lower case,
	// a dated suffix kept), or ModelInvalid or ModelOther.
	Model string `json:"model"`
	// Route is the broker route that served it.
	Route string `json:"route"`
	// Credential is the kind of credential that paid for these tokens:
	// the route's, or under per-user credentials the attributed person's
	// own (an oauth-token is a subscription: its cost is notional).
	Credential string `json:"credential"`
	// Observed tokens, from the responses' usage objects.
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheRead    int64 `json:"cacheRead"`
	CacheWrite5m int64 `json:"cacheWrite5m"`
	CacheWrite1h int64 `json:"cacheWrite1h"`
	// EstimatedShortfall is the worst-case tokens the broker charged for
	// responses cut short or without usage, on top of what it observed.
	// Never observed, so never priced.
	EstimatedShortfall int64 `json:"estimatedShortfall"`
}
