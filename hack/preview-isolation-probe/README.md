# Re-running the preview cold-start isolation gate

Run this after **every EKS, EKS Auto Mode, or VPC CNI upgrade, and before relying on previews again**. It reproduces the
2026-09-30 accepted test: three fresh Deployment-managed starts in slots 0, 1, 0, each after the dedicated pool has
reached zero nodes. The binary's first network operation is a concurrent burst: four workers make four attempts against
each of eight forbidden targets. All 128 connections must be blocked; DNS may succeed after the policy is programmed. A
connection refusal is inconclusive, not a pass. The IMDSv2 test issues only a token PUT, reads only the status line, and
treats an established TCP connection as a failure; it never reads a token or credentials.

A fourth, **sibling run** covers multi-component previews, where several components share one slot. Two more probe Pods
stand in for a sibling component (with its own ClusterIP Service) in slot 0 and for a Pod in slot 1, then the probe runs
in slot 0 with three more forbidden targets: the sibling's Pod, the sibling's Service and the other slot's Pod. All 176
connections must be blocked. The stand-ins listen on nothing, so a policy that let a connection through shows as a
refused, inconclusive attempt, which fails the run too. The pool is warm by then: the sibling run checks the slot policy
between Pods, while the three runs before it check the cold start.

The script **creates** every probe object (`kubectl create`) and never client-side applies one: `kubectl apply` adds a
`kubectl.kubernetes.io/last-applied-configuration` annotation, and a slot Service may carry no annotation but a safe
health-check path, so admission would refuse the sibling's Service. The sibling Service is `sibling-service.yaml`, and
the chart's preview policy envtest (`charts/patchy/preview_probe_envtest_test.go`) runs the script's own command on it
against the rendered policies.

The probe runs from an app repository that already publishes preview images through patchy's trusted publisher, and it
is **never merged into that repository's default branch**. Prepare a disposable same-repo PR on a `test/preview-*`
branch based on the current default branch and open into it; the script reads the repository's default branch from
GitHub and refuses a PR into any other. Copy this directory's `cmd/netprobe/` to the app repository's `cmd/netprobe/`,
and replace its runtime `Dockerfile` and `.dockerignore` with the two files here. Do not alter its agent-toolchain
image, build/publish workflow, or application source. Verify the PR diff contains only those files; open the PR and wait
for the uncredentialed build and the trusted publisher to succeed. The publisher must push
`<preview path prefix>/<app>:sha-<full PR head SHA>`; the script confirms that tag and its digest exist in ECR. Never
run the probe from a fork PR or from an image tagged from the default branch.

The script takes the site as flags or from its environment and sets nothing else: `gh` must be signed in with read
access to the app repository, `kubectl`'s current context must be the cluster, and the AWS CLI must reach the registry's
account with the credentials already in your environment (`AWS_PROFILE` or the rest). The image is looked up in the
account named in `--image`, the one the probe Pod pulls from, whichever account those credentials belong to; from
another account, that repository's policy must allow them `ecr:DescribeImages`. From this patchy checkout, check the
values first with `--dry-run`, which validates them and prints the site the run would probe without calling `gh`, `aws`
or `kubectl`, then run:

```sh
bash hack/preview-isolation-probe/run.sh \
  --repository acme/hello-web \
  --image 123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/hello-web \
  --taint-key preview.example.com/preview-only \
  <disposable-PR-number>
```

| Flag           | Environment        | Required | Meaning                                                                                                                       |
| -------------- | ------------------ | -------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `--repository` | `PROBE_REPOSITORY` | yes      | `owner/name` of the app repository the disposable PR is open on                                                               |
| `--image`      | `PROBE_IMAGE`      | yes      | The ECR repository its trusted publisher pushes preview images to; the registry host, its account and region are read from it |
| `--taint-key`  | `PROBE_TAINT_KEY`  | yes      | The preview NodePool's `NoExecute` taint key, the chart's `preview.nodeIsolation.taintKey`                                    |
| `--node-pool`  | `PROBE_NODE_POOL`  | no       | The preview NodePool, the chart's `preview.nodeIsolation.nodePool`; default `patchy-preview`                                  |
| `--node-class` | `PROBE_NODE_CLASS` | no       | The preview NodeClass, the chart's `preview.nodeIsolation.nodeClass`; default `patchy-preview`                                |

A flag wins over its environment variable; `--help` prints the same table. Each value is checked for its shape before
anything runs, because each lands in a command argument or in the probe's manifest. The script still assumes a release
named `patchy` in the namespace `patchy` (it reads the Services `patchy-egress-broker`, `patchy-integration-controller`,
`patchy-source-controller` and `patchy-status-server` there), and the chart's slot namespaces `patchy-preview-0` and
`patchy-preview-1`.

The script discovers the current Kubernetes and patchy Service ClusterIPs (rather than trusting stale IPs), verifies the
NodeClass is `DefaultDeny` and both slot NetworkPolicies exist, and refuses to overwrite an existing probe Deployment.
It uses the immutable PR-head image in a **Deployment-managed Pod**, with the existing admission rules unchanged. It
deletes each Deployment after its result and waits for the preview NodePool and NodeClaims to return to zero. A
forbidden success, inconclusive attempt, DNS failure, refused admission, missing image, or timeout stops the run; keep
previews unused and investigate. Afterward, close the disposable PR **without merging** it.

This tests observed connectivity, **not** the Auto Mode network-policy agent logs. The `no bpf context registered`
agent-log check remains open until node diagnostics or managed-agent log delivery make those logs directly available.
Record that distinction with every re-run. Before enabling previews, also verify that the preview ALB, DNS alias,
admission guards and slot quotas match the approved configuration for that rollout.

## The ingress direction: only the preview gateway reaches a slot

`ingress.sh` checks the other half of slot isolation on **any cluster, with no cloud API**: that the slot NetworkPolicy
lets only the preview gateway open a connection to a slot Pod. Run it on the desktop and on EKS once
`previewAuth.gateway` is on, and again before release N+1 turns the legacy ALB path off. It never calls `gh` or `aws`.

The run creates four Pods from one netprobe image (`cmd/netprobe`, the same binary, in its `listen` and `connect`
modes):

| Pod                        | Where                                         | Must be                                 |
| -------------------------- | --------------------------------------------- | --------------------------------------- |
| `preview-ingress-listener` | the slot (`patchy-preview-<slot>`), port http | listening on 8080, on the preview nodes |
| `preview-ingress-agent`    | the agents namespace                          | refused (`BLOCKED`)                     |
| `preview-ingress-release`  | the release namespace, no gateway label       | refused (`BLOCKED`)                     |
| `preview-ingress-gateway`  | the release namespace, gateway label          | admitted (`REACHED`)                    |

Each client makes the egress run's burst (four workers, four attempts) at the listener's Pod IP. A refusal is a timeout
or an unreachable route on every attempt; a connection refused is `INCONCLUSIVE`, which fails the run. The admitted
client also proves the listener answers, so the two refusals are the policy's and not a dead listener's. The gateway
label is `patchy.bitwisemedia.uk/preview-gateway: "true"`, the one the slot policy names; the script refuses to run when
the slot policy does not name it (the gateway is off). The agents namespace's own default-deny egress may refuse its
client before the slot policy does; either fence is a pass, and the release-namespace client, which no egress policy
holds back, is the one that proves the slot policy.

The listener runs in a slot, so it must pass the slot admission policies: its image must be under preview-controller's
image prefix as `<leaf>:sha-<40 hex>`. Build the image from this directory's `Dockerfile` (any build, no trusted
publisher needed) and push it to that registry with such a tag. On k3d, import it into the cluster's registry the way
preview images are. Then:

```sh
bash hack/preview-isolation-probe/ingress.sh \
  --image registry.example/patchy/previews/netprobe:sha-0123456789abcdef0123456789abcdef01234567 \
  --node-selector patchy.example.com/previews=true \
  --taint-key patchy.example.com/preview-only
```

| Flag                  | Environment               | Default         | Meaning                                                                                |
| --------------------- | ------------------------- | --------------- | -------------------------------------------------------------------------------------- |
| `--image`             | `PROBE_IMAGE`             | required        | The netprobe image under the preview image prefix, tagged `sha-<40 hex>`               |
| `--node-selector`     | `PROBE_NODE_SELECTOR`     | required        | `label=value[,label=value]`, the chart's `preview.nodeIsolation.selector`              |
| `--taint-key`         | `PROBE_TAINT_KEY`         | required        | The preview nodes' `NoExecute` taint key, the chart's `preview.nodeIsolation.taintKey` |
| `--no-node-isolation` |                           |                 | The install runs `preview.nodeIsolation.mode: none`; neither of the two above is given |
| `--release-namespace` | `PROBE_RELEASE_NAMESPACE` | `patchy`        | Where the gateway runs                                                                 |
| `--agents-namespace`  | `PROBE_AGENTS_NAMESPACE`  | `patchy-agents` | The agents' namespace                                                                  |
| `--slot`              | `PROBE_SLOT`              | `0`             | The slot the listener runs in                                                          |
| `--timeout`           | `PROBE_TIMEOUT`           | `300`           | Seconds each Pod may take to start or finish                                           |

`--dry-run` validates the values and prints the site; `--print-manifests` prints the four Pods (the listener's address
as `192.0.2.1`). Neither calls `kubectl`. The chart's preview policy envtest creates `--print-manifests`' Pods with a
server-side dry run against the rendered slot policies, so the listener stays admissible. The script deletes its Pods on
exit, and refuses to start while any of them exists.

On EKS Auto Mode use the NodePool and NodeClass labels as the selector
(`karpenter.sh/nodepool=patchy-preview,eks.amazonaws.com/nodeclass=patchy-preview`). On the desktop, isolation is
scheduling only; the run still proves the NetworkPolicy, which k3d's bundled network policy controller enforces.
