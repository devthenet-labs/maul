// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package projectcheck is the engine behind `maul check project`: a
// workstation preflight that tells an operator whether a Project is ready
// for its first intent, and, when it is not, which part is missing.
//
// It is not a second controller. Every fact the in-cluster controllers
// already prove is read off the cluster rather than proven again: the
// Project's Ready condition (which intent-controller sets only after
// minting the App's scoped tokens on every repository and ensuring the
// labels), its IntentNameConflict condition, and each covering Forge's
// Ready condition. It never reads a Secret, so it never holds the App's
// private key: the cluster reads are Projects, Forges, ConfigMaps, the
// placeholder Ingress, the gateway's Ingress and Service, and Previews, with
// the caller's own kubeconfig (the last three, and the ConfigMaps, through
// previewcheck).
//
// What the controllers cannot report before an intent runs, it checks with
// the operator's own identity, and says so in each reason:
//
//   - forge: forge.Resolve over the namespace's Forge CRs, the same pure
//     function the controllers call, for the intent repository and every
//     app repository;
//   - agent-image: each repository's declaration at its default-branch head
//     (read from GitHub with GH_TOKEN, else GITHUB_TOKEN, else anonymously;
//     an enterprise token goes only to the host GH_HOST names, since the
//     Project, not the caller, names the hosts), regular files only, as
//     source-controller's archive read; with runnerimage.Declare's
//     precedence, then imagecheck.Static under source-controller's live
//     policy, read from its ConfigMap (found by the app.kubernetes.io labels,
//     over kustomize's shared maul-config); registry reads use
//     resolve.NewKeychain, so an entry in the caller's docker config naming
//     the registry host wins (for ECR too), and an ECR host it does not name
//     is read with the caller's AWS credentials. A signature source-controller requires but whose key is
//     not found here makes an otherwise passing image a SKIP;
//   - previews: that intent-controller writes Previews and preview-controller
//     is configured, then, per previewed repository, that its image
//     repository sits under preview-controller's prefix and that
//     sha-<default-branch head> is published there (a FAIL only with more
//     than one previewed repository, since only then does a preview run it);
//   - acceptance-rules: only while spec.verification.enabled, per app
//     repository, whether the rulesets on its default branch require the
//     maul/acceptance status from a named App: a WARN when not (or from any
//     source), a SKIP when the rules cannot be read; classic branch
//     protection is not read;
//   - preview-dns, preview-tls, preview-auth, preview-auth-dex,
//     preview-auth-host and preview-gateway-allowlist: previewcheck's
//     checks of the preview path (the probe host <project>-0.<host suffix>,
//     the gateway's or the legacy load balancer's edge, and preview sign-in),
//     run for this Project and its Ready Previews; previewcheck's package
//     documentation says what each judges.
//
// What it cannot prove: that source-controller's or the preview nodes' own
// registry credentials work from inside the cluster (a Repository's
// status.runnerImage and a Preview's status are the evidence for those).
//
// Every check is a Check line keyed by repository key (which also names the
// repository's preview component), or by IntentRepository, and is never
// limited to the first repository. Network and cluster reads go through
// seams (client.Reader, GitHub, the registry Keychain, and previewcheck's
// Resolver, TLSDialer and HTTPDoer) so tests need no network at all.
package projectcheck
