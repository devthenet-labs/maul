// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package httpguard holds the small, credential-free HTTP guards shared by
// patchy's in-cluster reverse proxies (the egress broker today, the
// egress proxy next): the caller's source IP, a per-source-IP token bucket
// with an in-flight cap, and a response writer that counts what it wrote
// for the audit line.
//
// None of it is a security decision. The source IP is an attribution and
// rate key, never an identity; the limiter only bounds how much work one
// source can cause. The package imports the standard library and
// golang.org/x/time/rate only (a test pins that), so a pure engine can use
// it without linking a Kubernetes client.
package httpguard
