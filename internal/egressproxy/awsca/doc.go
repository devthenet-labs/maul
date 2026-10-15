// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package awsca is the egress proxy's CodeArtifact credential adapter: it
// mints a CodeArtifact authorization token per codeartifact registry with
// the proxy's own AWS identity (IRSA or EKS Pod Identity, through the
// SDK's default chain; no long-lived secret exists) and serves it behind
// egressproxy.CredentialSource, as npm's bearer token or pip's "aws" basic
// login.
//
// Minting runs in the background: the first mint at start, then a refresh
// at half the token's lifetime (or 15 minutes before expiry, if earlier,
// but never before a quarter of it), with ±10% jitter. A failure backs off
// from 30s to 5m and keeps serving the current token until it expires;
// after that the registry's route answers 503. Two replicas mint
// independently, which CodeArtifact's bearer tokens allow.
//
// The SDK is given two HTTP clients built on the proxy's guarded dialer
// (Clients): the CodeArtifact and STS client admits public IPv4 on port 443
// only, and the container-credentials client (Pod Identity) admits exactly
// 169.254.170.23:80 and nothing else; neither admits the other's address.
// A failed mint logs credential_mint_failed with the AWS error code and the
// class of the address last dialled (public, private, link-local), never a
// message or a token, so a VPC endpoint with private DNS shows plainly.
package awsca
