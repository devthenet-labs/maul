// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package podindex is the egress proxy's optional attribution adapter: a
// client-go informer over the agent pods of one namespace, indexed by pod
// IP, behind egressproxy.PodLookup. It lets the audit line name the pod,
// finding, owner, run kind, attempt and repository behind a source IP.
//
// Attribution is garnish and never authorises anything: the listener port
// and the NetworkPolicies are the identity. The informer's transform keeps
// only the name, creation time, the attribution labels, the repo
// annotation, the pod IP and the phase, so a Job's spec and env never reach
// the cache. Only Pending and Running agent pods are indexed; when two share
// an IP the newest wins.
package podindex
