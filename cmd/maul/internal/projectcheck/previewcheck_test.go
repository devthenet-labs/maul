// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import "github.com/devthenet-labs/maul/cmd/maul/internal/previewcheck"

// The preview path's checks moved to previewcheck unchanged; these tests
// still drive them through Run, against the wire contract they read: the
// chart's ConfigMap keys and annotation, and the placeholder Ingress.

// The seams Run passes on to previewcheck.
type (
	Resolver  = previewcheck.Resolver
	TLSDialer = previewcheck.TLSDialer
	HTTPDoer  = previewcheck.HTTPDoer
)

// The relay's and preview-controller's settings the preview checks read.
const (
	keyAuthRequired         = "MAUL_PREVIEW_AUTH_REQUIRED"
	keyAuthIssuer           = "MAUL_PREVIEW_AUTH_ISSUER"
	keyAuthDexIssuer        = "MAUL_PREVIEW_AUTH_DEX_ISSUER_URL"
	keyAuthDexClientID      = "MAUL_PREVIEW_AUTH_DEX_CLIENT_ID"
	keyAuthGateway          = "MAUL_PREVIEW_AUTH_GATEWAY"
	keyAuthGatewayInbound   = "MAUL_PREVIEW_AUTH_GATEWAY_INBOUND_CIDRS"
	keyPreviewLegacyIngress = "MAUL_PREVIEW_LEGACY_INGRESS"
	publishedAnnotation     = "maul.run/preview-gateway-published-addresses"
)

// The chart's placeholder Ingress in slot 0.
const (
	placeholderNamespace = "maul-preview-0"
	placeholderName      = "maul-preview-placeholder"
)

// dexMaxHops is how many redirects on Dex's own host the Dex check follows.
const dexMaxHops = 5

// The labels and controller names the controllers' ConfigMaps carry.
const (
	labelName         = "app.kubernetes.io/name"
	labelPartOf       = "app.kubernetes.io/part-of"
	partOf            = "maul"
	sourceController  = "source-controller"
	intentController  = "intent-controller"
	previewController = "preview-controller"
	previewAuth       = "preview-auth"
	sharedName        = "maul"
)
