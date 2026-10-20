// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UsageScopeType is a UsageRollup dimension.
// +kubebuilder:validation:Enum=user;user-project;project
type UsageScopeType string

// Usage rollup scopes.
const (
	// UsageScopeUser is one person's (or the org's) usage over every kind of
	// run.
	UsageScopeUser UsageScopeType = "user"
	// UsageScopeUserProject is one person's usage of one Project's intents.
	UsageScopeUserProject UsageScopeType = "user-project"
	// UsageScopeProject is one Project's intents' usage, whoever ran them.
	UsageScopeProject UsageScopeType = "project"
)

// UsageScope identifies one UsageRollup. Rollups are sharded, one object per
// scope value, so no object grows with headcount or with the estate.
// +kubebuilder:validation:XValidation:rule="has(self.user) == (self.type in ['user', 'user-project'])",message="scope.user is set exactly for the user and user-project scopes"
// +kubebuilder:validation:XValidation:rule="has(self.project) == (self.type in ['user-project', 'project'])",message="scope.project is set exactly for the user-project and project scopes"
type UsageScope struct {
	// Type of the dimension.
	Type UsageScopeType `json:"type"`
	// User is whom the runs were attributed to, as the attributed-to
	// annotation says it: "org", or "github-id:<numeric id>" (the identity;
	// a login is display only).
	// +optional
	// +kubebuilder:validation:Pattern=`^(org|github-id:[1-9][0-9]{0,18})$`
	User string `json:"user,omitempty"`
	// Project is the Project the intents' runs belonged to.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Project string `json:"project,omitempty"`
}

// UsageRollupSpec pins the scope; it is immutable.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.scope is immutable"
type UsageRollupSpec struct {
	// Scope this rollup aggregates.
	Scope UsageScope `json:"scope"`
}

// UsageAggregate accumulates runs' usage. Trusted figures (the tokens, the
// unmetered shortfall and the two costs) come only from runs whose usage the
// egress broker metered (source measured or partial); a run with only its
// pod's own report (source reported: a non-brokered harness, no broker
// figure, or measurement off) is counted apart, in the Reported fields, and
// never in a trusted total. All costs are exact integer micro-USD.
type UsageAggregate struct {
	// Runs counted, whatever their source.
	// +optional
	Runs int64 `json:"runs,omitempty"`
	// PartialRuns are the runs whose metered figure is partial (the broker
	// saw only part of the pod); their tokens are in the trusted totals.
	// +optional
	PartialRuns int64 `json:"partialRuns,omitempty"`
	// ReportedRuns are the runs with only a pod-reported figure.
	// +optional
	ReportedRuns int64 `json:"reportedRuns,omitempty"`
	// UnpricedRuns are metered runs with tokens of a model that has no known
	// price: their tokens are counted, their cost is not.
	// +optional
	UnpricedRuns int64 `json:"unpricedRuns,omitempty"`
	// InputTokens metered.
	// +optional
	InputTokens int64 `json:"inputTokens,omitempty"`
	// OutputTokens metered.
	// +optional
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// CacheReadTokens metered.
	// +optional
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	// CacheWriteTokens metered (five-minute and one-hour).
	// +optional
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	// UnmeteredTokens is the broker's worst-case shortfall: never priced,
	// never part of a cost.
	// +optional
	UnmeteredTokens int64 `json:"unmeteredTokens,omitempty"`
	// CostMicroUSD is the metered tokens' cost at list rates, paid on an API
	// key or a cloud identity.
	// +optional
	CostMicroUSD int64 `json:"costMicroUSD,omitempty"`
	// NotionalCostMicroUSD is the list-rate equivalent of metered tokens a
	// Claude subscription paid for: never money spent.
	// +optional
	NotionalCostMicroUSD int64 `json:"notionalCostMicroUSD,omitempty"`
	// ReportedTokens sums every token class the reported runs' pods said
	// they used. Untrusted.
	// +optional
	ReportedTokens int64 `json:"reportedTokens,omitempty"`
	// ReportedCostMicroUSD sums the reported runs' pod-reported cost.
	// Untrusted.
	// +optional
	ReportedCostMicroUSD int64 `json:"reportedCostMicroUSD,omitempty"`
}

// UsageKindTotals is one run kind's share of a bucket, for a chart stacked by
// kind: trusted figures only, beside the run count.
type UsageKindTotals struct {
	// Runs of this kind, whatever their source.
	// +optional
	Runs int64 `json:"runs,omitempty"`
	// Tokens metered, every class.
	// +optional
	Tokens int64 `json:"tokens,omitempty"`
	// CostMicroUSD at list rates.
	// +optional
	CostMicroUSD int64 `json:"costMicroUSD,omitempty"`
	// NotionalCostMicroUSD for a subscription.
	// +optional
	NotionalCostMicroUSD int64 `json:"notionalCostMicroUSD,omitempty"`
}

// UsageBucket is one day's or month's usage within a scope.
type UsageBucket struct {
	UsageAggregate `json:",inline"`
	// Kinds splits the bucket by run kind: investigation, remediation, plan,
	// build, revise, check-fix, evaluation.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	Kinds map[string]UsageKindTotals `json:"kinds,omitempty"`
}

// MaxTopIntents bounds a month bucket's TopIntents.
const MaxTopIntents = 10

// UsageTopIntent is one intent's usage within a month, for a user's "top
// intents".
type UsageTopIntent struct {
	// Intent is the Intent's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Intent string `json:"intent"`
	// Project the intent belonged to.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Project string `json:"project,omitempty"`
	// Runs counted.
	// +optional
	Runs int64 `json:"runs,omitempty"`
	// Tokens metered, every class.
	// +optional
	Tokens int64 `json:"tokens,omitempty"`
	// CostMicroUSD at list rates.
	// +optional
	CostMicroUSD int64 `json:"costMicroUSD,omitempty"`
	// NotionalCostMicroUSD for a subscription.
	// +optional
	NotionalCostMicroUSD int64 `json:"notionalCostMicroUSD,omitempty"`
}

// UsageMonthBucket is one month's usage within a scope, and on a user scope
// the month's most expensive intents.
type UsageMonthBucket struct {
	UsageBucket `json:",inline"`
	// TopIntents is the user scope's bounded list of the month's most
	// expensive intents (cost, then tokens), at most MaxTopIntents. It is
	// kept as runs arrive: an intent enters a full list only when its runs
	// so far outrank the last entry, so a long tail of cheap intents is
	// not listed, and an evicted intent's earlier runs are forgotten here
	// (never in the bucket's totals).
	// +optional
	// +kubebuilder:validation:MaxItems=10
	// +listType=atomic
	TopIntents []UsageTopIntent `json:"topIntents,omitempty"`
}

// Retention bounds for a UsageRollup: day buckets for the last
// UsageRetentionDays days (at most that many), month buckets for at most
// UsageRetentionMonths months. Every run is written to its day and its month,
// so dropping an old day loses only the day's detail, never its month's
// total.
const (
	UsageRetentionDays   = 400
	UsageRetentionMonths = 120
)

// UsageRollupSchemaVersion is the current bucket layout.
//
//	1 — the original layout.
const UsageRollupSchemaVersion int32 = 1

// UsageRollupStatus is the accumulated usage. All data lives in status; the
// ledger makes application exactly-once per run.
type UsageRollupStatus struct {
	// SchemaVersion of the bucket layout; see UsageRollupSchemaVersion.
	// +optional
	SchemaVersion int32 `json:"schemaVersion,omitempty"`
	// Login is the GitHub login last seen for a person's user or
	// user-project scope, for display only (the scope's id is the
	// identity).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Login string `json:"login,omitempty"`
	// FirstProcessed is the usage epoch for this scope.
	// +optional
	FirstProcessed *metav1.Time `json:"firstProcessed,omitempty"`
	// LastProcessed is the most recent aggregation.
	// +optional
	LastProcessed *metav1.Time `json:"lastProcessed,omitempty"`
	// Days are the day buckets keyed "2026-10-16" (UTC, the run's
	// completion), the last UsageRetentionDays only.
	// +optional
	// +kubebuilder:validation:MaxProperties=400
	Days map[string]UsageBucket `json:"days,omitempty"`
	// Months are the month buckets keyed "2026-10" (UTC), at most
	// UsageRetentionMonths.
	// +optional
	// +kubebuilder:validation:MaxProperties=120
	Months map[string]UsageMonthBucket `json:"months,omitempty"`
	// Recent is the exactly-once ledger: the last 512 applied run keys
	// ("u:<run uid>").
	// +optional
	// +kubebuilder:validation:MaxItems=512
	Recent []string `json:"recent,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ur,categories=maul
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope.type`
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.spec.scope.user`
// +kubebuilder:printcolumn:name="Login",type=string,JSONPath=`.status.login`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.scope.project`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// UsageRollup accumulates the durable per-day and per-month usage of one
// scope: one user (a person or the org), one user's runs in one Project, or
// one Project. Each run's own controller folds the run in once it settles
// (with its usage final), before the run can be deleted; the rollups outlive
// the runs' TTL. They live only in etcd, so a cluster rebuild resets them.
type UsageRollup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec UsageRollupSpec `json:"spec"`
	// +optional
	Status UsageRollupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UsageRollupList contains a list of UsageRollup.
type UsageRollupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UsageRollup `json:"items"`
}
