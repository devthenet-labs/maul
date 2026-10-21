// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"net/http"
	"strings"

	"github.com/devthenet-labs/maul/internal/provider"
)

// dialect is a route's wire format beyond its surface: how its request
// bodies are inspected, how its responses report usage, and how it answers
// an error. The Anthropic-shaped routes (anthropic, bedrock, vertex,
// foundry) share one; the OpenAI Responses route has its own (openai.go).
// Everything else — pre-authentication, TokenReview, the ledger and its
// limits, SSE pings, audit, the usage endpoint — is the same for every
// route.
type dialect struct {
	// inspect parses a buffered request body (inspection).
	inspect func(body []byte, betaDeny []string) (inspection, error)
	// usage reads a response's usage.
	usage usageDialect
	// writeError answers a refused or failed request.
	writeError func(w http.ResponseWriter, status int, kind, message string)
	// maxTokensField names the request's output bound in a refusal.
	maxTokensField string
	// refuseUpgrade answers any protocol upgrade (a websocket) 404.
	refuseUpgrade bool
}

var (
	anthropicDialect = dialect{
		inspect: inspectBody, usage: anthropicUsage{}, writeError: writeAPIError, maxTokensField: "max_tokens",
	}
	openaiDialect = dialect{
		inspect: inspectOpenAIBody, usage: openaiUsage{}, writeError: writeOpenAIError,
		maxTokensField: "max_output_tokens", refuseUpgrade: true,
	}
)

// dialectFor returns a named route's dialect.
func dialectFor(name string) dialect {
	if name == provider.OpenAI {
		return openaiDialect
	}
	return anthropicDialect
}

// isUpgrade reports whether a request asks to switch protocols.
func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, v := range r.Header.Values("Connection") {
		for tok := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}
