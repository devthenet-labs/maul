// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package previewdetect is `maul setup previews --detect`: a read-only look
// at the cluster the caller's kubeconfig names, so the wizard can fill in
// what it would otherwise ask (.claude/plans/preview-setup-design.md §4.2,
// owner decision Q1: opt-in, read-only, never a Secret).
//
// It has two halves, kept apart so the judging is pure:
//
//   - Observe reads the cluster into an Observation: the Nodes (provider
//     IDs, labels, taints), the default/kubernetes Service and its
//     EndpointSlice, the kube-system/kube-dns Service, IngressClasses,
//     GatewayClasses (gateway.networking.k8s.io, read unstructured), two
//     CRDs by name (cert-manager's Certificate, the Tailscale operator's
//     ProxyClass), the kube-system Pods' container images (and the VPC CNI
//     policy agent's command line), the kube-system/amazon-vpc-cni ConfigMap
//     (its network policy controller switch), and the maul release's
//     ConfigMaps (through previewcheck.LoadSettings). Nothing else: never a
//     Secret, and nothing is written. A read that fails or is refused is
//     recorded as a note, never an error; detection is a convenience. Only
//     when no read reached the API server at all does Observation say the
//     cluster is unreachable, which the command reports as an error.
//   - Judge turns an Observation into Findings (the platform, the API
//     server's addresses, the NetworkPolicy enforcer, what is installed, the
//     release's own preview settings) and Apply lays Findings over the
//     wizard's answers. Apply only fills what is still open (empty, the API
//     server CIDR's default auto, or a false cert-manager or Tailscale
//     operator "installed"), even when an earlier run recorded it in
//     answers.yaml, and never an answer whose flag was given: a flag always
//     wins. The VPC CNI counts as enforcing NetworkPolicy only with both of
//     its switches on, since its agent ships switched off.
//
// ApplyTailnet is the tailscale recipe's other half, off the cluster: the
// operator records the gateway device's address only in its proxy's state
// Secret, beside the device's keys, so detection asks the caller's own
// Tailscale client instead (package tailnet) and lays what it found over
// the answers as Apply does: --tailscale-address only while open, with
// the exact DNS records to create.
//
// The status page's Dex is not read: its OIDC settings live in a Secret
// (statusServer.auth), which detection never reads. The release's own
// preview-auth ConfigMap names Dex when previews are already installed.
package previewdetect
