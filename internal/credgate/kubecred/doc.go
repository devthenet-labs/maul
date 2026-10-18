// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package kubecred is credgate's Lookup over a Kubernetes reader: one get
// of a person's ModelCredential by its derived name, in the release
// namespace. Controllers pass their uncached API reader, so holding runs
// needs only get on modelcredentials (no list, no watch, no informer), which
// the chart grants them only while egressBroker.userCredentials.mode is on.
package kubecred
