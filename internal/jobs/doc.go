// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package jobs creates and observes the ephemeral Kubernetes Jobs that run
// agent-runner: one Job per Investigation/Remediation attempt,
// deterministically named, labelled by kind/owner/finding (the two job
// controllers share the agents namespace and filter on the kind label), and
// garbage collected via TTL plus an owner-referenced per-Job Secret.
//
// The Job shape carries the isolation model: no credential of any kind
// reaches the pod except the model API key. The init container fetches the
// repository as a digest-verified tarball from source-controller's artifact
// server and synthesizes the local git base; the per-Job Secret holds only
// the handoff markdown. Results come back on the agent container's stdout
// as envelope events, read once at Job completion. Brokered claude pods also
// carry a fixed, non-secret placeholder ANTHROPIC_AUTH_TOKEN (the CLI will not
// start without one); the egress broker strips it and injects the real key.
//
// A Job may run a repository-declared image instead of the harness's runner
// image (Spec.RunnerImage, honoured only under Config.AllowRepositoryImages
// and for a Runner with binaries to Inject). The isolation model does not
// bend for it. Create refuses such a Job outright for a runner that would
// carry a model credential into the pod, for a reference not pinned to a
// sha256 digest, and without an ephemeral-storage limit. Otherwise the
// trusted prepare init still runs the runner image and, after the fetch,
// copies patchy's static agent-runner and the harness CLI into a read-only
// emptyDir the agent container starts from by absolute path, then runs the
// sandbox probe (`agent-runner sandbox-probe`) and refuses to hand over —
// exit ExitSandboxUnenforced — while the pod can still reach the internet,
// the API server or the metadata service. The agent container gets a
// controller-owned PATH, git told to ignore the image's config, and an
// explicit empty value for the known shell, loader, interpreter, git,
// proxy, gateway and PATCHY_* redirection points the Job does not set. That
// is a backstop against configuration an image carries by accident, not a
// boundary: the image supplies git, bash and the libc the CLI runs on, so
// its ENV is exactly as untrusted as its binaries, and the trust boundaries
// are agent-runner and the broker. Names an empty value cannot neutralise
// (git's repository redirections) are reserved instead, so the resolver
// refuses an image that sets them. Such a Job carries the runner-image
// audit annotations and label; a default Job carries none. Create returns
// the image and source that actually apply, and that value is what the
// launching controller records.
//
// A default-image Job's prepare init ends with the same probe in its wait
// mode (`agent-runner sandbox-probe --mode=wait --targets=k8s,imds`): it
// holds the agent container back until the API server and the metadata
// service, which every chart posture blocks, have stayed unreachable for
// the probe's confirming rounds, so the agent does not start inside the
// window before a late-attaching NetworkPolicy applies. When the window
// closes with either still open it logs one loud line and hands over
// anyway: it never exits ExitSandboxUnenforced and never fails the init,
// so a default-image run's attempts and the breaker are untouched. An
// evaluation Job runs neither probe: its evolve-runner image ships no
// agent-runner.
//
// Under Config.DNS DNSNone a pod has no working resolver, which closes the
// DNS channel out of the sandbox: dnsPolicy None, its own loopback as its
// only nameserver, and the hosts its URLs name (the artifact server, the
// egress broker) pinned in its hosts file, resolved through Config.Resolver
// as Create builds the Job. A host that does not resolve fails the launch
// (ErrUnresolved) before anything is created, and a runner that dials its
// model API by name (NeedsResolver) is refused. DNSCluster, the default,
// leaves every Job byte-for-byte what it was.
//
// A multi-repository intent's plan Job reads more than its own tree
// (Spec.Trees): the per-Job Secret then carries a fetch list and a
// repositories manifest, and the prepare init fetches, digest-verifies and
// extracts each tree under /workspace/repos, with no git, before staging
// the manifest for agent-runner. Create refuses trees anywhere but on a plan
// Job on the default runner image (ErrTreesRefused), so N trees never meet
// a repository-declared image, and every Job without them is byte-for-byte
// what it was before.
//
// One exception to the offline sandbox is the operator's to grant: a network
// profile (Spec.NetworkProfile, network.go). It applies only under
// Config.NetworkProfiles, only to a Job that runs the repository's image,
// and only to a remediate Job of kind remediation or a build Job of kind
// intent (revise and check-fix are build Jobs); Create refuses it on any
// other (Phase, Kind) pair or beside trees (ErrNetworkProfileRefused).
// Applied, it adds exactly one destination: the profile label on the Job
// and its pod (netprofile.LabelKey, which the NetworkPolicies on both sides
// of the egress proxy's profile port select), the proxy's host in the hosts
// file under DNSNone, and the go command's environment pointing at the
// proxy, every go variable a profile owns set non-empty so the image's
// $GOROOT/go.env cannot route around it. No credential, no resolver and no
// proxy variable comes with it, and the per-Job Secret is unchanged. Create
// reads the profile back off the Job that exists, so what the caller
// records is what runs.
package jobs
