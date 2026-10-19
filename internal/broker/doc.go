// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package broker is the egress credential broker's engine: a reconciler-less
// reverse proxy that agent pods address directly (base-URL env overrides in
// the pod point the claude CLI at it) and that injects or signs the model
// credential outbound. It is what makes agent pods fully credential-less —
// the pod authenticates to the broker with an audience-bound projected
// ServiceAccount token (an identity document, not a capability), and the
// broker alone holds the Anthropic or OpenAI API key or the cloud workload
// identity that signs Bedrock/Vertex/Foundry traffic.
//
// One route per upstream, keyed by path prefix. The route set is closed:
// Config.validate admits only the model providers in provider.Names
// (anthropic, bedrock, vertex, foundry, openai), because every route is a
// positive method+path surface (surface.go) and a metered model-inference
// contract, not an open proxy. Each route also has a dialect (dialect.go):
// how its bodies are inspected, how its responses report usage and how it
// answers an error. The four Anthropic-shaped routes share one; the openai
// route (openai.go: the Responses API, POST /v1/responses and GET
// /v1/models only, no websocket) has its own, reading usage from the
// terminal response.completed, response.incomplete or response.failed
// event, which carries the whole response and so is bounded at 8 MiB
// rather than 64 KiB, and refusing OpenAI's server tools, server-side state
// (background, previous_response_id, conversation, stored prompts) and
// remote files and images. Its errors use OpenAI's envelope, a spend-limit
// refusal as insufficient_quota. Brokering another upstream (forge-minted GitHub tokens,
// package registries) means adding its name and a surface for it, not only a
// route. Every request is audited as a single slog line (never bodies or
// headers) and streamed through with immediate flushing so SSE responses
// survive multi-minute runs.
//
// The broker is also the enforcement point for what a pod may ask of the
// model API and how much, because under a repository-declared agent image
// the in-pod kill switch is advisory and the caller token is readable by
// anything in the pod. Enforcement runs in layers before any upstream
// contact: pre-authentication (a syntactic token check, a per-source-IP
// bucket and a global TokenReview limiter, so a token-header flood degrades
// only its source); the TokenReview with its verdict cache; a positive
// method+path surface per route, 404 elsewhere, so Files, Batches and the
// rest of the upstream API are unreachable; body inspection that refuses
// server-side tools, MCP servers, containers and file references and strips
// denied anthropic-beta entries; the model allowlist, read from the path on
// bedrock and vertex and from the body elsewhere; and the spend ledger —
// per-pod request, concurrency and token counters plus a broker-wide hourly
// token ceiling, charged from every usage field the response reports and
// from a request-size estimate when a stream is cut. Over any limit is a 429
// whose message carries provider.LimitMessagePrefix, which the in-pod runtime
// maps to budget_exceeded. The ledger is in-memory per replica.
//
// The limits are in tokens by default (Limits.Unit). Under the cost unit the
// per-pod and hourly limits are in micro-USD instead (pricing.go: the shared
// model.MeasuredCostUSD over the built-in registry, an unpriced model at the
// dearest rate of each class, a shortfall at the output rate), a request
// reserves its output bound at its model's output price, and the token
// limits stay as optional backstops. A pod's cap is then the smaller of
// Limits.CostPerPod and its provider.BudgetAnnotation, read with the verdict
// by one get of the token's own pod in the agent namespace, UID-checked
// (budget.go); that read, under the cost unit only, is the broker's one
// Kubernetes access beside TokenReview. Cost is counted, audited and
// metered under either unit, so a cost limit can be sized before switching.
//
// The ledger also keeps what each pod consumed per bare model id: the tokens
// observed in the responses (input, output, cache reads, and cache writes
// split into their five-minute and one-hour parts), apart from the
// worst-case shortfall charged for a response whose usage never became
// final. The limits count both; only the observed tokens are ever priced.
// With Config.Usage set, UsageHandler serves that record per Job on a
// listener of its own (usagewire) to the run controllers alone, which read
// it when a run settles; it reports the process's boot time, so a ledger
// that restarted mid-run is never taken for a whole one. The broker runs as
// a single replica, and the usage figures depend on it: a second replica
// would hold half of a pod's traffic.
//
// Per-user model credentials (Config.UserCredentials, design L3; off by
// default) let a run attributed to a person be paid with that person's own
// Anthropic API key or setup-token on the anthropic route, or OpenAI API key
// on the openai route (usercred.go): one resolver and one health probe per
// route, each reading only its own provider's records, so a request is
// never paid by another provider's credential. The caller pod's
// attributed-to annotation is read with the verdict (the same UID-checked
// get as the budget); under attribute the org credential still pays and the
// audit line records the person, and under enforce a person's run carries
// that person's credential, found by derived names through a
// CredentialStore (informers on the ModelCredential records and the
// credential Secrets' versions, a live read of the Secret at most once a
// minute while its version holds), or is refused with a 403 naming which
// link broke, never sent with the org credential. A run attributed to the
// org keeps the org credential in every mode. The credential travels to the
// route's CredentialFunc in the request context; the audit line carries
// the person and the credential's fingerprint, and the ledger the kind that
// paid. Each record's health comes only from probes the broker issues
// itself (credhealth.go), throttled, written by a patch conditional on the
// record's resourceVersion and only while the probed Secret version is
// still current; readiness stays tied to the org credential alone.
package broker
