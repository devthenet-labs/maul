// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package rbactest is test support for suites that grant RBAC on a real
// kube-apiserver (envtest) and then act on the grant.
//
// Creating a Role and RoleBinding returns as soon as etcd has them, but the
// API server's RBAC authorizer answers from its own informers, which observe
// the new objects a moment later. A request made in that window is refused
// although the grant exists: a flake that shows up only on a loaded machine.
// WaitAllowed closes the window deterministically by polling
// SubjectAccessReviews until the authorizer itself allows every request the
// suite is about to make. It never grants anything and never relaxes a
// denial a suite expects: it waits only for permissions the suite granted.
package rbactest
