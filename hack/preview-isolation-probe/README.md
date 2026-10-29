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

## The evaluator run: the broker and nothing else

`run.sh evaluator` checks the agent evaluator's row of the security model (`intentController.verification.evaluator`) on
**any cluster, with no cloud API**: that a Pod labelled as an evaluator Job's Pod is reaches the egress broker and
nothing else. It is part of the S0 live gate; run it on the desktop (with `--refused-is-blocked`) and wherever the
evaluator is to be turned on, after any CNI or policy-controller upgrade, and in every network mode the cluster runs.

Each run, in a free slot:

1. requires the evaluator's egress policy (`maul-agents-evaluator` in the agents namespace: the value is on) and the
   slot policy, a slot holding no Deployment, and no probe Job left over;
2. creates a netprobe listener (`listen`) in the slot as a slot component, and requires it to answer on its own loopback
   before and after the run, the same Pod throughout, so its refusal is never a dead listener's;
3. creates, **as intent-controller's ServiceAccount** (`kubectl create --as`), a Job in the agents namespace labelled as
   an evaluator Job is: the agent selector, `maul.run/kind: intent`, `maul.run/harness: claude`,
   `maul.run/role: evaluator` and a `maul.run/preview-verification` UID of the probe's own, on the Job and its Pod
   alike, with no resolver (`dnsPolicy: None`). The `maul-evaluator-labels` admission policy admits those labels from
   intent-controller alone, and the Pod only from the Job controller, so the caller needs the right to impersonate that
   ServiceAccount (cluster-admin has it); nothing else is created as it;
4. the Job runs `netprobe --as evaluator`, which first waits up to a minute to reach the broker on 8080 (`REACHED`;
   otherwise `NOT_REACHED`, a failure: a policy not yet programmed proves nothing) and only then makes the egress run's
   burst (four workers, four attempts) at the internet (`1.1.1.1:443`), the cluster resolver (the `kube-dns` Service on
   TCP 53, the address the evaluator's strict sandbox probe also finds closed), the API server, the instance metadata
   service (a token PUT, as above), the artifact server (source-controller on 9790) and the slot component on 8080,
   every one of which must be `BLOCKED`. Its summary is `PASS`, `SECURITY_FAILURE`, `INCONCLUSIVE` or `NOT_REACHED`;
5. deletes the Job and the component.

The evaluator's own tool pod on 8090 and the matrix's other rows (DESIGN 4.2) are the tools run below. By default a
refused connection is `INCONCLUSIVE` and fails the run; on k3s and k3d pass `--refused-is-blocked`: a refusal then
counts as blocked, sound because the broker has answered first (the Pod's policy is in force), the slot component
answers on its loopback, and the release's Services, the API server, kube-dns and the internet are assumed to answer.

```sh
bash hack/preview-isolation-probe/run.sh evaluator \
  --image registry.example/maul/previews/netprobe:sha-0123456789abcdef0123456789abcdef01234567 \
  --node-selector maul.example.com/previews=true \
  --taint-key maul.example.com/preview-only \
  --refused-is-blocked
```

| Flag                     | Environment                  | Default                  | Meaning                                                                                  |
| ------------------------ | ---------------------------- | ------------------------ | ---------------------------------------------------------------------------------------- |
| `--image`                | `PROBE_IMAGE`                | required                 | The netprobe image under the preview image prefix, tagged `sha-<40 hex>` (see above)     |
| `--node-selector`        | `PROBE_NODE_SELECTOR`        | required                 | `label=value[,label=value]`, the chart's `preview.nodeIsolation.selector`                |
| `--taint-key`            | `PROBE_TAINT_KEY`            | required                 | The preview nodes' `NoExecute` taint key                                                 |
| `--no-node-isolation`    |                              |                          | The install runs `preview.nodeIsolation.mode: none`                                      |
| `--refused-is-blocked`   |                              |                          | Count a refused connection as blocked, once the broker is reached: k3s and k3d           |
| `--slot`                 | `PROBE_SLOT`                 | `0`                      | The free slot the component runs in                                                      |
| `--release-namespace`    | `PROBE_RELEASE_NAMESPACE`    | `maul`                   | Where release `maul`'s Services run                                                      |
| `--agents-namespace`     | `PROBE_AGENTS_NAMESPACE`     | `maul-agents`            | The agents' namespace, where the probe Job runs                                          |
| `--intent-controller-sa` | `PROBE_INTENT_CONTROLLER_SA` | `maul-intent-controller` | intent-controller's ServiceAccount in the release namespace, which the Job is created as |
| `--timeout`              | `PROBE_TIMEOUT`              | `300`                    | Seconds the component may take to start, and the probe Job to finish                     |

`--dry-run` validates the values and prints the site; `--print-manifests` prints the component's Deployment and the
probe Job (its addresses as `192.0.2.x`). Neither calls `kubectl`. A failed run prints the probe's log, deletes what it
created and stops: keep the evaluator off and investigate. Rebuild the netprobe image from the same commit as the
script: an older netprobe has no `--as evaluator` and prints no summary (`NO_SUMMARY`).

## The tools run: the evaluator and tool-pod matrix, live

`run.sh tools` is the whole of DESIGN 4.2's matrix on **one running evaluation**, on any cluster with no cloud API. It
is the isolation half of the S1-28 live demo; nothing runs it automatically, and no test runs it against a cluster. Run
it from a workstation whose kubeconfig can create `pods/ephemeralcontainers` in the slot and the agents namespace, while
an evaluation's evaluator and tool pods are alive.

It creates no stand-ins. It adds an **ephemeral container** of the netprobe image to each real pod
(`kubectl debug --profile=restricted`). An ephemeral container shares only the pod's network namespace, so its traffic
is that pod's: to the NetworkPolicies, and to the application gates on localhost, alike. A pod's own image needs no
shell and no tools. The containers stay until the pod is deleted (Kubernetes cannot remove one); they are named
`probe-<row>-<unix time>`. The netprobe image is the one `Dockerfile` here builds, tagged `sha-<40 hex>`, and must be
pullable on the preview and agent nodes.

| Row         | netprobe mode    | Runs in                                                     | REACHED                                             | BLOCKED                                                                                                                                            |
| ----------- | ---------------- | ----------------------------------------------------------- | --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `evaluator` | `--as evaluator` | the evaluator pod (agents namespace)                        | the egress broker `:8080`, its own tool pod `:8090` | the internet, IMDS, DNS `:53`, the API server, the artifact server `:9790`, a slot component, another evaluation's tool pod, a pod in another slot |
| `component` | `--as peers`     | a component pod of the same Preview                         | the cluster resolver `:53` (the control)            | the tool pod `:8090`, the evaluator, the broker `:8080`                                                                                            |
| `tools`     | `--as tools`     | the tool pod                                                |                                                     | DNS `:53`, the API server, IMDS, the internet                                                                                                      |
| `sandbox`   | `--as sandbox`   | the tool pod                                                |                                                     | `:8090` on loopback and on the pod IP; the CDP shim `:9222` with no key and with a wrong one                                                       |
| `page`      | `--as page`      | the tool pod, loaded by Chromium in its `browser` container |                                                     | page JS fetching `:8090` and the CDP shim                                                                                                          |
| `agents`    | `--as tools`     | an agents-namespace tool pod (`--agents-tool-pod`)          | the artifact server `:9790`                         | the broker, DNS `:53`, the API server, IMDS, the internet                                                                                          |

The `tools`, `sandbox` and `browser` containers share one network namespace, which is the point of the `sandbox` and
`page` rows: the policy cannot separate containers of one pod, so the application gates do.

How each mode judges:

- **`--as evaluator`, `--as tools`, `--as peers`** share one engine. Every REACHED target is dialled first, for up to a
  minute, and only if all connect is the BLOCKED burst made (four workers, four attempts per target, the egress run's).
  A refusal is inconclusive unless `--refused-is-blocked`, which is sound because the control answered first.
  `PROBE_REACH` and `PROBE_DENY` (`name=<ipv4>:<port>` lists) carry the run's own addresses, and `peers` refuses to run
  without a control and a target to deny. The summary is `PASS`, `SECURITY_FAILURE`, `INCONCLUSIVE` or `NOT_REACHED`.
- **`--as sandbox`** makes plain HTTP requests, with no `Origin`, to `tools`' `:8090` (loopback and the pod's address,
  `PROBE_POD_IP`) and to the shim's `/json/version` with no key and with a wrong `X-Maul-Cdp-Key`. A 4xx status, a
  connection closed at accept (how `tools` refuses local callers) or a timeout is `blocked`; a 2xx or 3xx is `REACHED`;
  a refused connection or a 5xx is inconclusive, because nothing proves a gate there. The `evaluator` row reaching the
  same `:8090` is what shows the listener is up.
- **`--as page`** serves a page on `127.0.0.1:8099` whose script fetches the same targets, and Chromium loads it. Page
  JS sends an `Origin` and cannot set the key, so each fetch must fail or be unreadable; only a readable answer is
  `REACHED`. The page posts what it saw back to the server that served it, which prints it. A page that never reports is
  inconclusive. It cannot see a side effect a gate allowed that the page could not read; the gates' refusals are covered
  by their own unit tests (`internal/cdpshim`, `internal/previewtools`).

```sh
bash hack/preview-isolation-probe/run.sh tools \
  --image registry.example/maul/previews/netprobe:sha-0123456789abcdef0123456789abcdef01234567 \
  --tool-pod maul-preview-0/preview-ev-0123456789-tools \
  --evaluator-pod maul-agents/evaluator-0123 \
  --component-pod maul-preview-0/preview-pr-12-web-5d9f7 \
  --other-tool-pod maul-preview-1/preview-ev-abcdef0123-tools \
  --other-slot-pod maul-preview-1/preview-pr-13-web-77c8d \
  --refused-is-blocked
```

| Flag                   | Environment               | Default                    | Meaning                                                                                                                                |
| ---------------------- | ------------------------- | -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `--image`              | `PROBE_IMAGE`             | required                   | The netprobe image, `<registry>/<path>:sha-<40 hex>`                                                                                   |
| `--tool-pod`           | `PROBE_TOOL_POD`          | required                   | `<namespace>/<pod>`: the evaluation's tool pod                                                                                         |
| `--evaluator-pod`      | `PROBE_EVALUATOR_POD`     | required                   | The evaluator pod of the same evaluation                                                                                               |
| `--component-pod`      | `PROBE_COMPONENT_POD`     | required                   | A component pod of the same Preview                                                                                                    |
| `--other-tool-pod`     | `PROBE_OTHER_TOOL_POD`    | required                   | Another evaluation's tool pod (not `--tool-pod`)                                                                                       |
| `--other-slot-pod`     | `PROBE_OTHER_SLOT_POD`    | required                   | A pod in another slot                                                                                                                  |
| `--agents-tool-pod`    | `PROBE_AGENTS_TOOL_POD`   |                            | A tool pod in the agents namespace; adds the `agents` row                                                                              |
| `--component-port`     | `PROBE_COMPONENT_PORT`    | `8080`                     | The port the component and the other slot pod listen on                                                                                |
| `--release-namespace`  | `PROBE_RELEASE_NAMESPACE` | `maul`                     | Where the broker's and source-controller's Services run                                                                                |
| `--chromium`           | `PROBE_CHROMIUM`          | `/usr/lib/chromium/chrome` | Chromium in the browser image (`Dockerfile.preview-browser`)                                                                           |
| `--refused-is-blocked` |                           |                            | Count a refusal as blocked once the row's control is reached (k3s, k3d); the evaluator listens on nothing, so a REJECTing CNI needs it |
| `--timeout`            | `PROBE_TIMEOUT`           | `120`                      | Seconds each row may take                                                                                                              |

`--dry-run` checks every value and prints exactly what each row would run in which pod and what it must find REACHED and
BLOCKED, then exits before `kubectl` is called. A row that does not hold is printed with the probe's log, the remaining
rows still run so the matrix is whole, and the script exits 1: keep `intentController.verification.evaluator` off and
investigate.
