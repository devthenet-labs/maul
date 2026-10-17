// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package previewsetup is the engine behind `patchy setup previews`: it turns
// an operator's answers into the files that expose previews and sign viewers
// in to them, and writes them without ever touching a cluster
// (.claude/plans/preview-setup-design.md §4).
//
// The parts, each its own seam:
//
//   - Answers is the model: every answer the command takes, as answers.yaml
//     records it (never a secret). Defaults fills what the operator need not
//     say, Missing names the flags still unanswered, and Validate reports
//     every answer the chart or the edge would refuse, all at once.
//   - The validators are pure: CheckHosts holds the preview host suffix and
//     the sign-in host to the gateway's grammar, keeps the sign-in host
//     outside the suffix, and refuses a sign-in host or Dex issuer on the
//     suffix's registrable domain (golang.org/x/net/publicsuffix decides it),
//     so preview code is never same-site with sign-in (owner decision Q3).
//   - Secrets carries the one secret the core generates, the relay's Dex
//     client secret (32 bytes from crypto/rand, base64url), in a type whose
//     formatting is redacted. ResolveSecrets keeps it stable across re-runs:
//     a secret file already written is read back, never regenerated, unless
//     the operator rotates it.
//   - Plan renders the tree for a recipe: the patchy values layered after
//     the operator's own, the manifests, the two Dex client Secret
//     manifests, answers.yaml and NEXT-STEPS.md. It is byte-stable for the
//     same answers and CLI version. Three recipes have landed: the
//     existing edge (R4: an Ingress class or a Gateway API parent),
//     gateway-tls (R2: the gateway's own LoadBalancer Service terminating
//     TLS) and tailscale (R3: that Service as a tailnet device through the
//     Tailscale operator). cloudflare-tunnel reports ErrRecipeNotReady
//     until it lands.
//   - R2 and R3 share the certificate pieces: the wildcard Certificate, the
//     namespaced ACME Issuer with one DNS-01 solver (dnsProviders, every
//     setting non-secret; a ClusterIssuer when the solver takes no stored
//     credential, since cert-manager lends its own identity to no
//     namespaced Issuer by default; --dns manual names the operator's own
//     issuer instead), and the DNS credential the operator hands over in a file or
//     on stdin, held in a redacted Credential and written only into its
//     Secret manifest (read back on a re-run, as the Dex client secret is).
//   - Pins are the upstream charts the recipes write values for
//     (cert-manager, the Tailscale operator), each version a constant under
//     a renovate comment; TestLivePins (`mise run preview-pins`, CI's
//     preview-pins job) checks each exists and accepts what is written.
//   - Compare reads what is on disk and says, per file, whether it is new,
//     changed or unchanged (with a unified diff for every file that is not
//     a secret), and which files an earlier run wrote that this one no
//     longer generates. Write applies a comparison: nothing is written
//     while a changed file is not accepted, and every file is written with
//     mode 0600 through internal ghapp's writer, in a directory created
//     0700.
//
// Nothing here applies anything to a cluster or calls a provider API: the
// operator, or their GitOps, applies the files, and NEXT-STEPS.md lists the
// steps patchy cannot take (DNS records, a Dex client, preview nodes).
package previewsetup
