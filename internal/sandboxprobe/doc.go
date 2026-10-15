// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package sandboxprobe is the negative-connectivity check a repository-image
// Job runs before any untrusted code does: from inside the pod, the trusted
// prepare init container tries to reach the addresses the agent's
// NetworkPolicy must block (the public internet, the Kubernetes API server,
// the cloud metadata service) and refuses to hand over to the agent
// container while any of them answers.
//
// The probe retries rather than judging its first attempt: on some CNIs a
// new pod's traffic is open for a few seconds until its policy attaches (EKS
// Auto Mode among them), so a first successful connection proves nothing.
// Nor does a first failed one: enforcement is concluded only after several
// consecutive rounds in which nothing answered. Egress is declared
// unenforced when a target is still reachable at the end of the window,
// and the process then exits ExitUnenforced, which
// the job controllers map to a SandboxUnenforced failure. The window, the
// clock and the dialer are injectable so the retry behaviour is tested
// without a network or a wall clock.
//
// A default-image Job runs the same probe in its wait mode (ModeWait,
// `sandbox-probe --mode=wait --targets=k8s,imds`): only the targets every
// chart posture blocks (the public internet is open on purpose under
// broadEgress), the same rounds and window, and exit 0 whatever the
// verdict, logging one loud line when a target is still open at the end of
// the window. It narrows the window in which a trusted agent could run
// before its policy attaches; it never refuses a run.
//
// The probe is stdlib-only and lives in the static agent-runner binary
// (`agent-runner sandbox-probe`, the subcommand named by Command, which
// internal/jobs writes into the prepare script and agent-runner dispatches
// on), the same trusted binary the prepare step copies into the pod, so it
// never depends on what the untrusted image carries. Main is that
// subcommand's whole body, so its exit statuses are tested here rather
// than in package main.
package sandboxprobe
