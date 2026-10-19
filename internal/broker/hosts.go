// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

// The model APIs' default base URLs, as egress-broker builds its routes
// when no base-URL override is given. They are constants, not flag-default
// literals, so the egress proxy's hard-deny table can be pinned to the
// hosts the broker really reaches (cmd/egress-broker's TestModelHostsDenied
// reads this file). Each Format takes one value: the region for Bedrock
// and Vertex, the resource name for Foundry.
const (
	DefaultAnthropicBaseURL = "https://api.anthropic.com"
	DefaultOpenAIBaseURL    = "https://api.openai.com"
	BedrockBaseURLFormat    = "https://bedrock-runtime.%s.amazonaws.com"
	VertexBaseURLFormat     = "https://%s-aiplatform.googleapis.com"
	FoundryBaseURLFormat    = "https://%s.services.ai.azure.com"
)
