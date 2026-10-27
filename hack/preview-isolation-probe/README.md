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
the chart's preview policy envtest (`charts/maul/preview_probe_envtest_test.go`) runs the script's own command on it
against the rendered policies.

The probe runs from an app repository that already publishes preview images through maul's trusted publisher, and it is
**never merged into that repository's default branch**. Prepare a disposable same-repo PR on a `test/preview-*` branch
based on the current default branch and open into it; the script reads the repository's default branch from GitHub and
refuses a PR into any other. Copy this directory's `cmd/netprobe/` to the app repository's `cmd/netprobe/`, and replace
its runtime `Dockerfile` and `.dockerignore` with the two files here. Do not alter its agent-toolchain image,
build/publish workflow, or application source. Verify the PR diff contains only those files; open the PR and wait for
the uncredentialed build and the trusted publisher to succeed. The publisher must push
`<preview path prefix>/<app>:sha-<full PR head SHA>`; the script confirms that tag and its digest exist in ECR. Never
run the probe from a fork PR or from an image tagged from the default branch.

The script takes the site as flags or from its environment and sets nothing else: `gh` must be signed in with read
access to the app repository, `kubectl`'s current context must be the cluster, and the AWS CLI must reach the registry's
account with the credentials already in your environment (`AWS_PROFILE` or the rest). The image is looked up in the
account named in `--image`, the one the probe Pod pulls from, whichever account those credentials belong to; from
another account, that repository's policy must allow them `ecr:DescribeImages`. From this maul checkout, check the
values first with `--dry-run`, which validates them and prints the site the run would probe without calling `gh`, `aws`
or `kubectl`, then run:

```sh
bash hack/preview-isolation-probe/run.sh \
  --repository acme/hello-web \
  --image 123456789012.dkr.ecr.us-east-1.amazonaws.com/maul/previews/hello-web \
  --taint-key preview.example.com/preview-only \
  <disposable-PR-number>
```

| Flag           | Environment        | Required | Meaning                                                                                                                       |
| -------------- | ------------------ | -------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `--repository` | `PROBE_REPOSITORY` | yes      | `owner/name` of the app repository the disposable PR is open on                                                               |
| `--image`      | `PROBE_IMAGE`      | yes      | The ECR repository its trusted publisher pushes preview images to; the registry host, its account and region are read from it |
| `--taint-key`  | `PROBE_TAINT_KEY`  | yes      | The preview NodePool's `NoExecute` taint key, the chart's `preview.nodeIsolation.taintKey`                                    |
| `--node-pool`  | `PROBE_NODE_POOL`  | no       | The preview NodePool, the chart's `preview.nodeIsolation.nodePool`; default `maul-preview`                                    |
| `--node-class` | `PROBE_NODE_CLASS` | no       | The preview NodeClass, the chart's `preview.nodeIsolation.nodeClass`; default `maul-preview`                                  |

A flag wins over its environment variable; `--help` prints the same table. Each value is checked for its shape before
anything runs, because each lands in a command argument or in the probe's manifest. The script still assumes a release
named `maul` in the namespace `maul` (it reads the Services `maul-egress-broker`, `maul-integration-controller`,
`maul-source-controller` and `maul-status-server` there), and the chart's slot namespaces `maul-preview-0` and
`maul-preview-1`.

The script discovers the current Kubernetes and maul Service ClusterIPs (rather than trusting stale IPs), verifies the
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
lets only the preview gateway open a connection to a slot Pod. `maul check previews --isolation` runs the same probe
from the CLI (its `preview-netpol` line), always in the `--refused-is-blocked` order below, with the same four Pods (a
test pins them to `--print-manifests`), reading the placement, the namespaces and the image prefix from the install. Run
it on the desktop and on EKS once `previewAuth.gateway` is on, and again before release N+1 turns the legacy ALB path
off. It never calls `gh` or `aws`.

The run creates four Pods from one netprobe image (`cmd/netprobe`, the same binary, in its `listen` and `connect`
modes):

| Pod                        | Where                                       | Must be                                 |
| -------------------------- | ------------------------------------------- | --------------------------------------- |
| `preview-ingress-listener` | the slot (`maul-preview-<slot>`), port http | listening on 8080, on the preview nodes |
| `preview-ingress-agent`    | the agents namespace                        | refused (`BLOCKED`)                     |
| `preview-ingress-release`  | the release namespace, no gateway label     | refused (`BLOCKED`)                     |
| `preview-ingress-gateway`  | the release namespace, gateway label        | admitted (`REACHED`)                    |

Each client makes the egress run's burst (four workers, four attempts) at the listener's Pod IP. A refusal is a timeout
or an unreachable route on every attempt; by default a connection refused is `INCONCLUSIVE`, which fails the run. The
admitted client also proves the listener answers, so the two refusals are the policy's and not a dead listener's. The
gateway label is `maul.run/preview-gateway: "true"`, the one the slot policy names; the script refuses to run when the
slot policy does not name it (the gateway is off). The agents namespace's own default-deny egress may refuse its client
before the slot policy does; either fence is a pass, and the release-namespace client, which no egress policy holds
back, is the one that proves the slot policy. Because the agents' refusal cannot show that the slot policy itself
refuses them, the script also reads every NetworkPolicy in the slot namespace first and refuses to run when any admits a
peer other than the gateway's exact one (the release namespace AND the gateway's label) or an `ipBlock`: a rule with no
`from`, another namespace or Pod selector, a match expression, or a `preview-isolation` that selects only some Pods or
does not govern ingress. While `preview.legacyIngress` is `true` the slot policy also admits the preview ALB's subnets
by `ipBlock`; no client here is in them, so the run prints a `WARNING` naming those ranges instead of judging them. They
are gone once `preview.legacyIngress` is `false`.

With acceptance verification on (`preview.verification.enabled`), a slot also holds preview-controller's edge policies,
one per part of a verifying Preview (`preview-<p>-np-<part>`). They admit only that Preview's own Pods, by its UID and a
part, so the static check accepts a policy of exactly the shape the `maul-preview-networkpolicies` admission policy
admits preview-controller (the same predicate, in jq: its name and managed-by label, a selector of one Preview's UID and
one part, both policy types, every rule naming a peer and a numeric TCP port without an end, every peer a part of the
same Preview by those two labels alone) and refuses any other policy so named or so labelled, whoever wrote it: the
operator may write any slot policy, so the script cannot rely on admission. `maul check previews --isolation` judges
edge policies the same way, and a test holds the two to the same verdicts. `--check-policies` runs this static check
alone and creates nothing; the chart's preview probe envtest runs it against policies preview-controller wrote through
the rendered admission policies, and against each shape they refuse it.

### A policy controller that refuses: `--refused-is-blocked`

Some policy controllers drop a denied connection, so the client times out. kube-router, the network policy controller
built into k3s (and so k3d), rejects it instead: its per-Pod chain ends in `REJECT --reject-with icmp-port-unreachable`,
which a TCP client sees as `ECONNREFUSED`, the very error a dead listener gives. Without the flag, every refused client
on k3s is `INCONCLUSIVE` and the run cannot pass. Confirmed on the desktop k3d cluster (k3s v1.36.5+k3s1) by reading the
preview node's rules:

```sh
docker exec k3d-preview-0 iptables-save | grep 'REJECT traffic destined for POD'
# -A KUBE-POD-FW-... -m comment --comment "rule to REJECT traffic destined for POD name:... namespace: maul-preview-0"
#   -m mark ! --mark 0x10000/0x10000 -j REJECT --reject-with icmp-port-unreachable
```

`--refused-is-blocked` counts a refusal as blocked, and only where it can be nothing but the policy's:

1. The gateway client runs first, alone, and must be `REACHED`: the listener answers. If it is not, the run stops before
   the other clients exist.
2. The two clients that must be refused then run with `connect --refused-is-blocked`, which logs each refused attempt as
   `refused` and sums a burst of refusals and timeouts as `BLOCKED`. Any other error is still inconclusive, and a
   connection is still `REACHED`.
3. The listener must still be `Running` once they finish (it never restarts), or the run fails: a refusal from a
   listener that stopped would be its own.

Use it on k3s and k3d. Where the policy drops connections, as on EKS, leave it off: the default, stricter verdict passes
there. The egress run (`run.sh`) has no such flag; it runs on EKS only.

The same REJECT meets every new Pod until kube-router has programmed its policy, about a second after the Pod starts.
The acceptance tester is a new Pod on every run, so it waits for each component's Service to accept a TCP connection
before its first check, for at most a minute, and records the wait; a Service still refusing after that is the change's
failure, `component-unreachable` (docs/intents/acceptance.md). A probe client that starts sending at once can likewise
see its first attempts refused on k3s before the policy that would admit it exists.

### The listener's image

The listener runs in a slot, so it must pass the slot admission policies: its image must be under preview-controller's
image prefix as `<leaf>:sha-<40 hex>`. Build the image from this directory's `Dockerfile` (any build, no trusted
publisher needed) and give it such a tag. **Rebuild it from the same commit as the script** (and as the CLI, for
`maul check previews --isolation`): an older netprobe has no `connect --refused-is-blocked`, prints its usage instead of
a summary, and every client it runs then fails as `NO_SUMMARY`, quoting that first log line. Either push it to that
registry, or, on k3d, import it straight onto the nodes: the Pods pull `IfNotPresent`, so a node that holds the image
never contacts the registry, and the prefix needs no repository of that name there. On the desktop the prefix is an ECR
one, and the netprobe image ran from an import alone:

```sh
# <imageRegistry>/<imagePathPrefix>/netprobe:sha-<40 hex>; the commit keeps the tag traceable
image=377946145366.dkr.ecr.us-east-1.amazonaws.com/maul/previews/netprobe:sha-$(git rev-parse HEAD)
ctx=$(mktemp -d) && cp -R hack/preview-isolation-probe/. "$ctx" # the Dockerfile wants a go.mod beside cmd/
printf 'module netprobe\n\ngo 1.26\n' >"$ctx/go.mod"
docker build -t "$image" "$ctx"
k3d image import "$image" --cluster maul
```

Then:

```sh
bash hack/preview-isolation-probe/ingress.sh \
  --image registry.example/maul/previews/netprobe:sha-0123456789abcdef0123456789abcdef01234567 \
  --node-selector maul.example.com/previews=true \
  --taint-key maul.example.com/preview-only
```

| Flag                   | Environment               | Default       | Meaning                                                                                              |
| ---------------------- | ------------------------- | ------------- | ---------------------------------------------------------------------------------------------------- |
| `--image`              | `PROBE_IMAGE`             | required      | The netprobe image under the preview image prefix, tagged `sha-<40 hex>`                             |
| `--node-selector`      | `PROBE_NODE_SELECTOR`     | required      | `label=value[,label=value]`, the chart's `preview.nodeIsolation.selector`                            |
| `--taint-key`          | `PROBE_TAINT_KEY`         | required      | The preview nodes' `NoExecute` taint key, the chart's `preview.nodeIsolation.taintKey`               |
| `--no-node-isolation`  |                           |               | The install runs `preview.nodeIsolation.mode: none`; neither of the two above is given               |
| `--refused-is-blocked` |                           |               | Count a refused connection as blocked, once the gateway client has reached the listener: k3s and k3d |
| `--release-namespace`  | `PROBE_RELEASE_NAMESPACE` | `maul`        | Where the gateway runs                                                                               |
| `--agents-namespace`   | `PROBE_AGENTS_NAMESPACE`  | `maul-agents` | The agents' namespace                                                                                |
| `--slot`               | `PROBE_SLOT`              | `0`           | The slot the listener runs in                                                                        |
| `--timeout`            | `PROBE_TIMEOUT`           | `300`         | Seconds each Pod may take to start or finish                                                         |
| `--check-policies`     |                           |               | Judge the slot's NetworkPolicies, then exit before any Pod is created                                |

`--dry-run` validates the values and prints the site; `--print-manifests` prints the four Pods (the listener's address
as `192.0.2.1`). Neither calls `kubectl`. The chart's preview policy envtest creates `--print-manifests`' Pods with a
server-side dry run against the rendered slot policies, so the listener stays admissible. The script deletes its Pods on
exit, and refuses to start while any of them exists.

On EKS Auto Mode use the NodePool and NodeClass labels as the selector
(`karpenter.sh/nodepool=maul-preview,eks.amazonaws.com/nodeclass=maul-preview`). On the desktop, isolation is scheduling
only; the run still proves the NetworkPolicy, which k3d's bundled network policy controller enforces. Run it there with
`--refused-is-blocked` (above).

## The edges run: declared edges admitted, undeclared refused

`run.sh edges` checks acceptance verification's per-part policies on **any cluster, with no cloud API** (it calls
neither `gh` nor `aws`): that a Preview's declared edges connect and nothing else in or out of the slot does. Run it
before the desktop live gate counts (three runs, slots 0, 1, 0), after any CNI or policy-controller upgrade, and again
before turning `preview.verification.enabled` on anywhere.

Its fixture is `edges.yaml`: the policies preview-controller renders for a Preview named `edges` whose tester calls
`web`, `api` and `render` on 8080 (the slice-1 environment). `internal/controller/preview/probe_edges_test.go` holds the
file to the controller's render, name for name and spec for spec; only the managed-by label differs
(`maul-isolation-probe`), so preview-controller's orphan sweep leaves the probe's policies alone, and the Preview UID is
the probe's own. Each run, in a slot that holds no Deployment and with the other slot free too:

1. creates the fixture's policies, a ClusterIP Service for each component (port 80 to 8080, as a component's is) and a
   netprobe listener (`listen`) for each part, labelled with the fixture's Preview UID and the part, plus a listener in
   the other slot, all at once;
2. reads the declared edges back from the policies the API server holds: a pair whose caller's egress and callee's
   ingress both name it;
3. runs every check as a `connect` exec'd in a stand-in, so it is that Pod's own traffic: each declared edge must reach
   the called part through its Service; every other ordered pair of parts (each reverse edge, each component pair, any
   part to the tester), every part to the other slot's Pod, and the tester to the API server, the egress broker,
   integration-controller, source-controller, the status server and the instance metadata service must be `BLOCKED`;
4. requires each listener to answer on its own loopback before and after the checks, and every stand-in to be the same
   Pod, never restarted, so a refusal can never be a dead listener's;
5. deletes everything it created.

A declared edge gets `--timeout` seconds to connect while the policy is programmed; the refusals get the egress run's
burst (four workers, four attempts). By default a refused connection is `INCONCLUSIVE` and fails the run; on k3s and
k3d, whose policy controller rejects, pass `--refused-is-blocked` (see above): a refusal then counts as blocked, which
step 4 makes sound for the stand-ins. The release's Services are assumed to answer.

```sh
bash hack/preview-isolation-probe/run.sh edges \
  --image registry.example/maul/previews/netprobe:sha-0123456789abcdef0123456789abcdef01234567 \
  --node-selector maul.example.com/previews=true \
  --taint-key maul.example.com/preview-only \
  --refused-is-blocked
```

| Flag                   | Environment               | Default  | Meaning                                                                              |
| ---------------------- | ------------------------- | -------- | ------------------------------------------------------------------------------------ |
| `--image`              | `PROBE_IMAGE`             | required | The netprobe image under the preview image prefix, tagged `sha-<40 hex>` (see above) |
| `--node-selector`      | `PROBE_NODE_SELECTOR`     | required | `label=value[,label=value]`, the chart's `preview.nodeIsolation.selector`            |
| `--taint-key`          | `PROBE_TAINT_KEY`         | required | The preview nodes' `NoExecute` taint key                                             |
| `--no-node-isolation`  |                           |          | The install runs `preview.nodeIsolation.mode: none`                                  |
| `--refused-is-blocked` |                           |          | Count a refused connection as blocked, against a listener proven alive: k3s and k3d  |
| `--slots`              | `PROBE_SLOTS`             | `0,1,0`  | The runs' slots, in order: two distinct slots, each run's other slot the other one   |
| `--cold-node-pool`     | `PROBE_COLD_NODE_POOL`    |          | A Karpenter NodePool to wait to reach zero nodes before each run (EKS Auto Mode)     |
| `--release-namespace`  | `PROBE_RELEASE_NAMESPACE` | `maul`   | Where release `maul`'s Services run                                                  |
| `--timeout`            | `PROBE_TIMEOUT`           | `300`    | Seconds each stand-in may take to start, and each declared edge to connect           |

`--dry-run` validates the values and prints the site; `--print-manifests` prints the Services and Deployments a run in
the first slot creates. Neither calls `kubectl`. The chart's preview probe envtest creates `--print-manifests`' objects
and the fixture with a server-side dry run against the rendered slot policies with verification on, so they stay
admissible. A failed run names each pair that connected or did not, deletes what it created, and stops: keep
verification off and investigate.
