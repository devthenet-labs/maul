// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import "github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewcheck"

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
	keyAuthRequired         = "PATCHY_PREVIEW_AUTH_REQUIRED"
	keyAuthIssuer           = "PATCHY_PREVIEW_AUTH_ISSUER"
	keyAuthDexIssuer        = "PATCHY_PREVIEW_AUTH_DEX_ISSUER_URL"
	keyAuthDexClientID      = "PATCHY_PREVIEW_AUTH_DEX_CLIENT_ID"
	keyAuthGateway          = "PATCHY_PREVIEW_AUTH_GATEWAY"
	keyAuthGatewayInbound   = "PATCHY_PREVIEW_AUTH_GATEWAY_INBOUND_CIDRS"
	keyPreviewLegacyIngress = "PATCHY_PREVIEW_LEGACY_INGRESS"
	publishedAnnotation     = "patchy.bitwisemedia.uk/preview-gateway-published-addresses"
)

// The chart's placeholder Ingress in slot 0.
const (
	placeholderNamespace = "patchy-preview-0"
	placeholderName      = "patchy-preview-placeholder"
)

// dexMaxHops is how many redirects on Dex's own host the Dex check follows.
const dexMaxHops = 5

// The labels and controller names the controllers' ConfigMaps carry.
const (
	labelName         = "app.kubernetes.io/name"
	labelPartOf       = "app.kubernetes.io/part-of"
	partOf            = "patchy"
	sourceController  = "source-controller"
	intentController  = "intent-controller"
	previewController = "preview-controller"
	previewAuth       = "preview-auth"
	sharedName        = "patchy"
)
