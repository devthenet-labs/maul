// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package previewcheck is the preview path's checks, shared by the `check`
// verbs so their verdicts cannot drift: `patchy check project` runs them
// for one Project, and `patchy check previews` (Install) runs them for the
// install as a whole, before any Project exists.
//
// Install names the probe host previewauth.UnknownProbeLabel (the relay's
// own background probe's, ProbeProject), probes every Ready Preview whatever
// its Project (Config.AllPreviews), and adds three checks of its own:
//
//   - preview-config: previews and sign-in are on and agree on the host
//     suffix, the preview gateway alone serves preview hosts (the legacy
//     path still serving is a WARN, and serving without sign-in a FAIL), the
//     sign-in host sits outside the
//     suffix, and the suffix is a registrable domain (publicsuffix) shared
//     with neither the sign-in host, the relay's issuer nor Dex (a WARN);
//   - preview-viewers: a RoleBinding in the release namespace or a
//     ClusterRoleBinding binds the chart's <release>-preview-viewer
//     ClusterRole to at least one User or Group carrying the relay's claim
//     prefix (a SelfSubjectAccessReview cannot ask for someone else, so the
//     bindings are read; none is a WARN, since another role may grant the
//     same);
//   - preview-nodes: with node isolation required, at least one node
//     carries preview-controller's node selector and its
//     <taint key>=true:NoExecute taint, and none carries the selector alone
//     (the taint alone, another install's node or a typo, is a WARN; an EKS
//     Auto Mode NodePool with no node yet is a SKIP; isolation off is a
//     WARN);
//   - preview-upstreams (upstreams.go): each Deployment running an upstream
//     the preview recipes pin (cert-manager's controller, the Tailscale
//     operator, cloudflared; previewsetup's pin table, design §5.3), found
//     by its image's repository path, is compared with its pin by the
//     image's tag, else its app.kubernetes.io/version label. Older is a
//     WARN, no readable version a SKIP, none running no line at all;
//   - preview-netpol (isolation*.go), only with Config.Isolation (`check
//     previews --isolation`): the Go port of
//     hack/preview-isolation-probe/ingress.sh --refused-is-blocked, and the
//     one check that writes to the cluster. A netprobe listener starts in
//     a slot (in ingress.sh's exact Pod shape, pinned by a test that runs
//     its --print-manifests); the gateway-labelled client runs alone and
//     must REACH it; only then do the agents' client and an unlabelled
//     release client run, both of which must be BLOCKED (a refusal counted
//     as blocked); and the listener must still be the same running Pod with
//     no restart. judgeIsolation, pure and property-tested, passes nothing
//     else, so a dead listener never passes as a block. Every Pod the run
//     created is deleted, UID-checked, on a context that outlives an
//     interrupt.
//
// It reads the controllers' settings from their ConfigMaps (LoadSettings:
// each controller's own over kustomize's shared one; never a Secret), and
// checks, with the caller's own network and kubeconfig:
//
//   - preview-dns and preview-tls (Checker.Host): that <project>-0.<host
//     suffix> resolves to the preview load balancer (the placeholder
//     Ingress's address) and serves a certificate trusted for it. An Intent
//     is <project>-<issue> and no issue is numbered 0, so the probe host is
//     one the wildcard record and certificate cover and no Preview owns.
//     Only NXDOMAIN fails the DNS check; a lookup that times out is a SKIP.
//     The load balancer admits only the chart's inbound CIDRs, so a TLS
//     timeout is a SKIP, not a FAIL;
//   - preview-auth, preview-auth-dex and preview-auth-host (Checker.Auth),
//     only while the chart's preview sign-in is on (the relay's ConfigMap
//     exists, or preview-controller requires sign-in): the relay answers its
//     discovery document at its issuer; Dex accepts the relay's client with
//     its one redirect URI (asked of Dex's authorization endpoint without a
//     session, following only redirects on Dex's own host, such as a
//     single-connector Dex's hop to its connector endpoint, never one off it,
//     since Dex's clients are not readable); and, once sign-in is required,
//     the placeholder host and every Ready Preview of the Project answer
//     without credentials with the load balancer's redirect to the relay for
//     their own slot's client (previewauth.JudgeProbe, the relay's own probe
//     verdict). A timeout from outside the inbound CIDRs is a SKIP;
//   - in gateway mode (the relay's PATCHY_PREVIEW_AUTH_GATEWAY on; the mode
//     is previewauth.ProbeModeFor of that and preview-controller's
//     PATCHY_PREVIEW_LEGACY_INGRESS), preview-dns compares the probe host
//     with the gateway's edge instead: the chart's publishedAddresses (an
//     annotation on the relay's ConfigMap), else the status of the
//     gateway's Ingress or LoadBalancer Service (<release>-preview-gateway),
//     else a Cloudflare Tunnel (tunnel.go: the cloudflare-tunnel recipe's
//     cloudflared ConfigMap, found by its labels in any namespace, names the
//     tunnel routing *.<suffix>; a record that is a CNAME to
//     <tunnel id>.cfargotunnel.com passes; one resolving to Cloudflare's
//     proxy alone hides its target and is a SKIP that leaves routing to
//     preview-auth-host; any other answer is a WARN or a SKIP
//     saying what to look at), else a SKIP (an HTTPRoute, or a tunnel the
//     check cannot identify); a private edge (RFC 1918, ULA,
//     CGNAT, so a tailnet) that this resolver does not answer with is a SKIP;
//     while both paths serve, the legacy load balancer passes too.
//     preview-auth-host asks every host as a navigation: the probe host,
//     which no Preview owns, must be the gateway's own 404 page
//     (previewauth.JudgeGatewayUnknown) and each Ready Preview the redirect to
//     sign in for its own label (previewauth.ProbeExpect.Live, either path's
//     while both serve). preview-gateway-allowlist, with inboundCIDRs set,
//     sends the probe host a spoofed X-Forwarded-For naming an admitted
//     address from an address the gateway refuses, which must still be
//     refused; from an admitted address it is a SKIP. The Dex check is the
//     same in both modes.
//
// Every line is about the install as a whole. Network and cluster reads go
// through seams (client.Reader, Resolver, TLSDialer and HTTPDoer) so tests
// need no network at all. Cluster reads are ConfigMaps, the placeholder and
// gateway Ingresses, the gateway's Service, Previews and, for Install,
// RoleBindings, ClusterRoleBindings, Nodes and Deployments; never a Secret.
package previewcheck
