// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package credgate is the run controllers' side of per-user model
// credentials (the per-user credentials design, L3, §4 "Fallback" and
// revision 1's re-attribution rule): whether a run about to be created may
// be, given whom it would be attributed to and that person's stored
// credential.
//
// The egress broker is what attaches a person's credential to a request,
// and under enforce it refuses a person's run whose credential cannot be
// used. A run created anyway would spend its attempt on a 403. So under
// enforce (Mode, the controllers' --user-credentials, the chart's
// egressBroker.userCredentials.mode) a controller asks Check before it
// creates a run attributed to a person: while the person has no
// ModelCredential whose health is Valid, the run is held (Verdict.Hold),
// the controller says so where a human reads it (Message), and it asks
// again later. Nothing is replanned: the hold lifts on its own once the
// credential is Valid, or once a later approval by someone who holds a
// Valid one changes whom the run is for. A run attributed to the org is
// never held, and under off or attribute nothing is held at all.
//
// Only the record's own health is read, never the Secret: the controllers
// hold no Secret access, and the record's health speaks for the Secret
// version it was judged against (the status server resets it to Unknown
// whenever it writes the Secret, the broker probes the new one). A health
// that is stale for any other reason is the broker's to refuse.
//
// The package imports only the standard library and api/v1alpha1 (a test
// pins that); kubecred is the Lookup over a Kubernetes reader.
package credgate
