// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

// PodInfo is what the audit line says about the agent pod a source IP
// belongs to.
type PodInfo struct {
	Name    string
	Finding string
	Owner   string
	RunKind string
	Attempt string
	Harness string
	Repo    string
}

// PodLookup attributes a source IP to an agent pod. It is best-effort audit
// garnish: a miss, a stale entry or no lookup at all changes no decision,
// since the listener's port and the NetworkPolicy are the identity.
type PodLookup interface {
	Lookup(ip string) (PodInfo, bool)
}
