// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package agentshost is intent-controller's agents host (SPEC-S2 S2-13,
// Q20; DESIGN 2.4, 2.5): the reconciler that runs an agents
// PreviewVerification's tool pod in the agents namespace and is the single
// writer of that verification's status (O1). The intent reconciler only
// creates the verification (placement agents, its workspace); this host
// renders, observes and collects it, as preview-controller does a slot
// verification, and never writes its spec.
//
// For each agents verification it:
//
//   - re-checks the request on every pass against its Intent (the
//     controller, the approved outcomes and saved checks, the open pull
//     request still at the workspace head), its Project's surface
//     repository and the two commit-pinned trees (UIDs, commits, resolved
//     SHAs, artifacts): one that no longer holds is Stale, dropped unjudged;
//   - waits, never failing, while --intent-max-tool-pods tool pods are live
//     (Q31: reason tool-pods-saturated, Infra only past its own timeout);
//   - re-derives the build's runner image and network profile through the
//     very calls a build launch makes (runnerguard PinFor and Profile on the
//     latest build's own Repository), holds them to the workspace, and
//     renders the relay's config from the sandbox environment jobs renders
//     (jobs.Client.ToolPodSandboxEnv) plus the image's Go directories
//     (status.runnerImage.goEnv), so a program sees the build's network and
//     nothing wider (7.8);
//   - creates the <job>-tools Secret and the Job through jobs.CreateToolPod,
//     one Job per verification (JobName), reusing the key of a Secret an
//     earlier pass created;
//   - observes the pod: prepare's exit 78 is sandbox-unenforced and trips
//     the sandbox breaker; an eviction, an unschedulable pod, an image that
//     will not pull, a crash loop or a deadline long past are Infra; it
//     writes status.tools only once tools and both sandboxes are Ready;
//   - collects the finished pod: only the tools container's log, verified
//     against its termination message (toolledger.Parse); a log or
//     termination message holding the tools key is ledger-mismatch (U23);
//     the index, the builds, the head's replay, confirmations and confirmed
//     checks, the base's runs (toolledger.BaseRuns, tree base),
//     status.base, the head build's part (previewtools.BuildPart) and the
//     <pv>-ledger ConfigMap; a head that did not build is Infra
//     tools-build-failed, never a fail, and a tree built but not kept for
//     its check runs (its tree, output and HOME over toolexec.MaxKeptBytes,
//     or a full work volume) Infra kept-copy-failed;
//   - tears the Job and its Secret down once the verification is settled,
//     after a linger for a finished pod so the isolation probe's ledger row
//     can still read it (S2-21), and at once for one still running.
//
// The Sweeper deletes tool Jobs (selected by maul.run/role tools, never by
// kind) whose verification is gone or settled past the linger, and every
// one while the agents host is off. Nothing here imports the intent
// package: intent-controller wires it.
package agentshost
