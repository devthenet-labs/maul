# preview-controller

`preview-controller` is the default-off, namespaced runtime renderer for intent PRs. It uses `Preview` CRs as its only
work queue; intent-controller is the sole writer of their spec, and preview-controller is the sole writer of their
status. No GitHub, ECR or AWS call is made by this binary. It never runs an agent and never receives a Secret. See
[the accepted preview design](../design/intent-driven-development.md#previews-slice-2-d5) and the Helm security
foundation in `charts/maul/README.md`.

Before rendering, the controller re-derives the Preview from the operator's Project config and the Intent's recorded
state (`v1alpha1.DesiredPreviewComponents`, the function the writer uses too) and checks every component against it. A
mismatched spec fails closed and removes an exposed Ingress before releasing the slot.

!!! warning "Off EKS Auto Mode, previews need the preview gateway"

    The legacy path (the default, `preview.legacyIngress: true`) works only on EKS Auto Mode: it depends on its
    `alb-preview` load balancer class, on the AWS load balancer's built-in sign-in (through
    [preview-auth](preview-auth.md)) and on Auto Mode's readiness-gate label. On any other cluster (GKE, AKS, k3d,
    on-premises), turn on the [preview gateway](preview-gateway.md) and set `preview.legacyIngress: false`: the slots
    then render no Ingress, and the gateway serves every preview host behind whatever edge the cluster has.

`preview.enabled` first creates guarded, empty slot namespaces and the non-default `alb-preview` class.
`previewController.enabled` then adds the controller, one namespaced Role in the release namespace and one in each slot,
and turns on intent-controller's Preview projector. Both switches default to `false`; the chart refuses to enable the
renderer without the foundation and the intent-controller. The controller's own NetworkPolicy allows only DNS and the
Kubernetes API Service `/32` (with `apiServerCIDR: auto`, also the API server's endpoint addresses, for CNIs that judge
the policy after the Service address is translated, such as kube-router on k3s). The slot Roles allow Deployment,
Service and Ingress CRUD plus Pod and ReplicaSet reads, and nothing cluster-scoped. The security foundation still
applies its fail-closed admission, NetworkPolicy, quota and dedicated-node rules to every workload.

To opt in one benign application Project, set a fixed runtime contract on its `Project`:

```yaml
spec:
  preview:
    imageRepository: 123456789012.dkr.ecr.us-west-2.amazonaws.com/maul/previews/shop-web
    port: 8080
    readinessPath: /healthz
```

The image repository sits exactly one leaf under the operator's preview image prefix,
`<preview.imageRegistry>/<preview.imagePathPrefix>/` in the chart (`--preview-image-prefix` on the binary), which is
`<registry>/maul/previews/` unless the operator sets `preview.imagePathPrefix`. The leaf is a lowercase DNS label
(letters, digits and inner hyphens, so not `app-` or `-app`). The `Project` schema checks only that shape, a registry
host, one or more lowercase path segments and the leaf; the preview-controller refuses any component whose repository is
not the configured prefix plus one leaf (another registry, a nested path, a tag, the agent image path), and the slot
admission policy denies a Pod whose image is outside the prefix. The controller refuses to start with a prefix that is
not `<registry>/<path>/`. The chart keeps the prefix disjoint from `agent.repositoryImages.registries` (see
[Helm](../deployment/helm.md)), so an image built from a pull request can never run as an agent sandbox.

`spec.preview` is the one-repository shorthand: it needs exactly one application repository. Do **not** add this block
to a Project whose application must never be exposed, such as a deliberately vulnerable test target. The runtime
publisher must publish the immutable `sha-<full PR head SHA>` image before the preview can become Ready.
[Deploying intents and previews](../intents/deploying.md) and [Onboarding an application](../intents/onboarding-app.md)
cover the whole setup. No fork PR may publish; the uncredentialed build has no `id-token` access, and the trusted
main-context publisher never checks out or executes PR code. The Preview's host is
`<project>-<issue>.<preview.hostSuffix>`; its spec contains the Intent UID, host label, each component's name/image
repository/full head SHA/port/readiness path/route path, and a 72-hour TTL.

A Project with several repositories previews them per repository instead, at most four, each under its own path on the
one host:

```yaml
spec:
  repositories:
    - name: web
      url: https://github.com/acme/Acme.Web_App
      preview:
        imageRepository: 123456789012.dkr.ecr.us-west-2.amazonaws.com/maul/previews/acme-web
        port: 8080
        readinessPath: /healthz
        path: / # the default
    - name: lib # never previewed: no runtime
      url: https://github.com/acme/shared-lib
    - name: api
      url: https://github.com/acme/api
      preview:
        imageRepository: 123456789012.dkr.ecr.us-west-2.amazonaws.com/maul/previews/acme-api
        port: 8080
        readinessPath: /api/healthz
        path: /api
```

Each previewed repository becomes one component, named by its key, with its own Deployment and Service: the first keeps
the single-component name `preview-<project>-<issue>`, and each further one is `preview-<project>-<issue>-<key>`. Their
selectors carry the component's name, so no Service reaches a sibling's Pods. The one Ingress routes a `Prefix` path per
component, longest first; the load balancer does not rewrite paths, so an API under `/api` serves `/api/...`. Each
component Service carries its own health-check path; the Ingress carries the `/` component's. The repository name is
free: the key and the image leaf are the operator's. intent-controller writes every Preview through the derivation the
preview-controller checks it against, so a one-repository Project may use either form. A Project with several
repositories needs intent-controller's `--intent-multi-repo` (`intentController.config.multiRepo`); without it the
Project reports `UnsupportedRepositories` and its intents are held, with no Preview.

!!! warning "Previewed apps call each other from the browser, on the same host"

    The slot NetworkPolicy admits only the load balancer and DNS, so one component can never reach another
    server-side: a web component's server cannot call the API component, inside the slot or by the preview's URL. An
    app built to be previewed with its siblings calls them from the browser, same-origin and by path (`fetch("/api/...")`
    from a page served at `/`), and each component serves under its own `path`, which the load balancer passes through
    unrewritten. Server-side calls between components are not supported.

Which revision each component runs:

- **A repository the intent changed** runs its pull request's recorded head: the `sha-<head SHA>` image its trusted
  publisher pushed for that commit. A new push to the pull request redeploys it.
- **A previewed repository the intent did not change** runs its default branch as it was when review began:
  intent-controller reads that repository's default-branch head once, on the intent's first review pass, and records it
  in the Intent's `status.previewBases`, never rewriting it, so the preview does not move when main does. The component
  runs that commit's `sha-<SHA>` image.
- There is no Preview until every component has a revision and at least one comes from a pull request, and none once no
  pull request is open.

So an application repository previewed beside others has a contract with its CI: main's CI must publish a `sha-<SHA>`
runtime image for **every** main commit (never cancel or skip a main build), and the registry must keep those images for
as long as an intent may be in review (tag them `main-<SHA>` as well, and keep that tag longer than the PR images'
expiry). A main image that was never published, or has expired, fails its component after the rollout retries; an image
still being published is covered by the rollout's retries.

`Pending` takes a free slot, or `Queued` waits in creation order behind other Previews. `Deploying` first prunes the
objects of components no longer rendered, so a renamed component's Service fits the slot quota, then creates a fixed
ClusterIP Service and a single-replica, restricted Deployment per component on the dedicated `DefaultDeny` preview pool;
a component whose spec did not change is not rolled. With `targetHealth` off, only after every component has a Ready Pod
of its current spec (the requested image, port and readiness path) and an image ID does it create the fixed
`alb-preview` Ingress and mark the Preview `Ready` with its URL and each component's revision and image ID, and a
PR-head change removes the old Ingress before updating the runtime images. With `targetHealth` on (the chart's default),
the Ingress comes first and stays (below). Cleanup and pruning find a Preview's objects by its UID label, so a component
the Project stops previewing is removed. Timed-out rollouts are retried at most `previewController.config.maxRetries`
times (default three) with a 10-minute per-attempt deadline; `Failed` frees the slot after cleanup. `Expired` is reached
72 hours after the last successful deployment (or after creation/attempt start if none ever succeeds); a later new PR
head may revive it. An Intent merge/close or Project opt-out deletes the Preview. Its finalizer waits until its rendered
resources and Pods/ReplicaSets are gone. A periodic sweep deletes owned orphans left by a lost CR, while the queue
refuses to reuse a slot that still contains owned resources. Reducing `slotCount` while a Preview owns a removed slot
deliberately blocks finalizer removal: restore the count and drain first.

A new spec on a Preview that holds its slot, such as a revision round's new PR head, redeploys it there at once: the
Preview is `Deploying`, with no URL, its `observedRevision` the new head, until the new revision is `Ready`. Each
Deployment rolls out with `maxSurge: 1` and `maxUnavailable: 0`: the new revision's Pod starts beside the serving one,
which stops only once the new one is Ready. The slot quota (8 Pods, 2 CPU and 2Gi of limits) holds that surge Pod for
each of four components. With `targetHealth` on, the Ingress stays, so the previous revision keeps answering on the host
until the new one's target is healthy. The pull request's runtime image may not be published yet (its trusted publisher
runs after the pull request's checks), so the new Pod waits for it while the kubelet retries the pull, and nothing fails
before the rollout deadline. The kubelet's pull back-off grows to five minutes, so an image published more than about
five minutes after its Pod started is pulled only at about ten, past the default deadline. If the new revision is not
Ready by the deadline, the retry restarts every component as before, by deleting its Deployment, which stops the
previous revision too; after `maxRetries` attempts the Preview is `Failed` and its slot is released. With `targetHealth`
off, the host is still withdrawn for a redeploy.

Upgrading from a release that rendered the `Recreate` strategy (0.12.18 or earlier) patches each live Deployment to the
rolling one in place. Its Pod template is unchanged, so no Pod restarts, and a `Ready` Preview stays `Ready` while the
Deployment controller observes the patch. Rolling back to such a release is not as quiet: its controller patches the
strategy back and counts the moment before the Deployment controller observes that against the rollout deadline, so a
Preview `Ready` for longer than `rolloutTimeout` is retried at once. Its Pods restart, its host is down until they are
Ready again, and one already on its last retry ends `Failed`.

intent-controller reads a Preview's status, never writes it, to link the preview from the intent's issue and its pull
requests once it is `Ready` at their heads; it never posts the status `message`. See
[The preview link](intent-controller.md#the-preview-link).

With `previewController.config.targetHealth: true` (the chart's default; `--preview-target-health` on the binary, whose
own default is still off), `Ready` also means the load balancer's target is healthy, so the host does not answer 404 or
nothing for the seconds after `Ready` while a new target registers. The chart then labels the slot namespaces
`eks.amazonaws.com/pod-readiness-gate-inject: enabled`, EKS Auto Mode's opt-in to inject a target-health readiness gate
into the slot's Pods (the upstream AWS Load Balancer Controller's label is `elbv2.k8s.aws/pod-readiness-gate-inject`,
but the `alb-preview` class is Auto Mode's). The gate is injected only into a Pod created after its target group binding
exists, which follows the Ingress, so in this mode the Ingress is created with the Services, before any Deployment; the
Deployments wait until the load balancer has published the Ingress's address; and a component is Ready only once its Pod
carries a readiness gate and every gate is True. The Ingress stays across a PR-head redeploy and a retry (the new Pods
need its binding): the rolling update keeps the previous revision's Pod serving until the new Pod is Ready, which here
means its target is healthy, and the load balancer routes nothing to a target until it is healthy. A spec that adds a
component (or renames one) replaces the Ingress instead, withdrawing the host until `Ready`: the load balancer keeps an
Ingress's address once published, so only a new Ingress's address shows that the new component's binding exists. A
Preview already `Ready` when the mode is switched on keeps serving on its Pods rather than being restarted to grow a
gate. The Auto Mode label comes from AWS (an EKS Auto Mode blog post and a maintainer's answer on
aws/containers-roadmap#2511), not from the EKS user guide. It has been checked on a live preview (maul 0.12.14): Auto
Mode injected the gate, and the host answered 200 to all 60 requests made once a second from the moment the Preview was
`Ready`, where the ungated `Ready` gave about 15 seconds of 404s and empty replies. Previews require EKS Auto Mode, and
a preview whose host does not answer yet is not ready to review, so the chart turns the mode on by default. That default
was off up to 0.12.15, so an upgrade from there that never set the value turns it on, and a Preview deploying during
that upgrade may spend one retry: its Pods predate the label and carry no gate (see
[the upgrade note](../deployment/helm.md#eks-auto-mode-and-previews)). If Auto Mode ever injects no gate, every rollout
times out and retries, the retry's message naming the missing gate, and `targetHealth: false` restores the ungated
behaviour. A Pod that has the gate but whose target never turns healthy (a readiness path the component does not serve,
or a security group or network policy keeping the load balancer's health checks out) is retried with a message naming
the unhealthy target instead.

Before enabling any Project preview, complete the separate ALB, placeholder Ingress and wildcard DNS check-in, then run
the cold-start isolation gate in `hack/preview-isolation-probe/README.md` with a disposable PR image. Repeat that gate
after any EKS, Auto Mode or VPC CNI upgrade and before relying on previews again. The direct Auto Mode network-policy
agent-log check is still open; absence of errors in Kubernetes events is not a substitute.

## Node isolation

Every slot Pod selects the dedicated preview nodes and tolerates their `NoExecute` taint (`<taint key>=true`), which
keeps everything else off those nodes. The binary takes the selector in generic form:

- `--preview-node-selector` (`MAUL_PREVIEW_NODE_SELECTOR`) is a JSON object of node label to value.
- `--preview-node-pool` and `--preview-node-class` fill its EKS Auto Mode keys, `karpenter.sh/nodepool` and
  `eks.amazonaws.com/nodeclass`, so an install configured by them renders exactly the Pods it always did and no live
  preview restarts. A selector that gives either key another value is refused.
- `--preview-taint-key` is the taint every slot Pod tolerates.
- `--preview-node-isolation` is `required` (the default) or `none`. Required refuses to start without a non-empty
  selector and a taint key; none renders neither, refuses either if set, and is an explicit opt-out for a cluster with
  no dedicated preview nodes, where preview code then shares nodes with everything else.

Every key must be a label name and every value a label value. Changing the selector or the taint changes every slot
Pod's template, so each live preview rolls once. The chart sets these from `preview.nodeIsolation` (`selector`,
`taintKey`, `mode`, and the EKS `eksAutoMode.nodePool` and `nodeClass`), and its slot admission policy compares the same
selector.

## The serving snapshot

`status.serving` records what a Preview serves, for the preview gateway to route by: the Preview's generation and UID
when it turned `Ready`, and per component its name, its Service (`preview-<project>-<issue>` or
`preview-<project>-<issue>-<key>`), its path (`/` written out) and the Service's port (80). It is written when the
Preview turns `Ready`, and backfilled onto a Preview that was `Ready` before the field existed. Unlike `url` and
`components`, it stays while the Preview redeploys (`Deploying` at a new generation), because the rolling update keeps
the previous revision's Pods serving until the new ones are Ready; it moves to the new generation once that is `Ready`.
A redeploy that stops rendering a component (a repository dropped from the Project, or a component whose Service name
moves to another) drops that component's entry at once, before its Service is pruned or repurposed, just as the legacy
path drops its route from the kept Ingress, and clears the snapshot if no entry is left. It is cleared when nothing may
serve any more: a retry that deletes the Deployments, `Failed`, `Expired`, the slot's release, and a deleting Preview. A
router serves a Preview only from this snapshot plus its UID, slot and host label, never from its spec. The Service-name
derivation (`v1alpha1.PreviewComponentServiceName`) and the path matcher (`v1alpha1.PreviewPathMatches` and
`PreviewRoute`, the Ingress `Prefix` semantics: element by element, longest prefix first) live in `api/v1alpha1`, and a
seeded property pins them to the Ingress this controller renders.

## Without the legacy Ingress

`--preview-legacy-ingress` (`MAUL_PREVIEW_LEGACY_INGRESS`, chart `preview.legacyIngress`) defaults to `true`: everything
above, exactly as before the preview gateway. Set to `false`, for an install whose preview hosts the preview gateway
serves:

- No slot Ingress is rendered, and none precedes the Pods.
- `Ready` means every component has a Ready Pod of its current spec with an image ID. There is no load balancer to wait
  for, so `--preview-target-health` is ignored (the controller logs a warning when both are set) and no readiness gate
  is expected. The chart sets it false there, whatever `previewController.config.targetHealth` says, so a gateway-only
  install starts without that warning.
- On every deploy pass the controller deletes the slot Ingresses its Preview owns (by its UID, name and managed-by
  labels), so a Preview made `Ready` on the legacy path loses its Ingress without restarting; a foreign Ingress is left
  alone. Cleanup deletes them too before a slot is freed, and the orphan sweep deletes every managed slot Ingress, a
  live Preview's included.
- The slot Role keeps its Ingress verbs, which the cleanup needs.

Turning it off before the gateway serves the preview hosts takes every preview off the network. The order on an EKS
install is in [Preview gateway](preview-gateway.md#moving-off-the-preview-alb-eks-release-n).

## Sign-in on preview Ingresses (off by default)

The chart sets these flags from `previewAuth` in its require stage only (see the chart README's preview sign-in
section); off, and in the permit stage, they stay unset and every slot Ingress renders exactly as before. The
controller's side:

- `--preview-auth-required` (`MAUL_PREVIEW_AUTH_REQUIRED`) renders each slot's pinned sign-in annotations onto every
  Ingress it writes, beside the health-check path. It names the slot's client Secret there and never reads it.
- `--preview-auth-annotations` (`MAUL_PREVIEW_AUTH_ANNOTATIONS`) is a JSON object mapping each slot namespace
  (`maul-preview-<n>`) to its six `alb.ingress.kubernetes.io/auth-*` values. The chart renders it once, for both this
  controller and the slot admission policy, so the controller writes exactly the bytes the policy compares. The policy,
  and the sweeper below, admit any shorter `auth-session-timeout` than the set's, so an Ingress written before a
  `sessionTimeout` change stays conforming while it waits for its reconcile. The controller refuses to start unless
  every configured slot, and only those, has exactly the six keys, the slot's own cookie name, `auth-type: oidc`,
  `authenticate` on an unauthenticated request, the `openid` scope, a session timeout the load balancer accepts, and one
  https issuer whose endpoints sit under it.
- `--preview-auth-previous-annotations` is the previous key generation's sets during a rotation. An Ingress still on
  them conforms. Its own reconcile patches it to the current set, and the sweeper never deletes it.

An Ingress write that the API server refuses (at admission, or for want of RBAC) is not a failed deploy attempt. The
Preview keeps its Deployments, its retries and the Ingress it has, and a `Ready` one keeps serving. The controller
records a Warning Event `IngressRefused` on the Preview, counts `maul.preview.ingress.refused{slot}`, and tries again at
the next poll. A Preview still deploying notes the refusal in its message, and its attempt restarts once the write is
admitted.

With auth required, the orphan sweep also lists every Ingress of the `alb-preview` class in each slot, labelled or not,
and reports each one without either generation's pinned set (its `auth-session-timeout` at most the set's, as the policy
judges it): the gauge `maul.preview.ingress.unauthenticated{slot}`, a log line, and a Warning Event on the Preview that
owns it. It deletes such an Ingress only after it has stayed unauthenticated for three poll intervals, which gives the
owning reconcile time to patch it first, and counts the deletion in `maul.preview.ingress.unauthenticated.deleted`. It
never deletes the chart's placeholder Ingress. The Events use the `events.k8s.io` API in the release namespace.

## Acceptance verification (off by default)

With `preview.verification.enabled` (chart; needs `preview.enabled`, `previewController.enabled` and a
`preview.verification.testerImage` pinned by digest) preview-controller also answers `PreviewVerification` requests from
intent-controller: it runs the approved acceptance criteria of an intent against that intent's own Preview, from a
tester Job inside the Preview's slot. See [Acceptance verification](../intents/acceptance.md) for what people see. The
controller's side:

- **Flags.** `--preview-tester-image` (`MAUL_PREVIEW_TESTER_IMAGE`; empty, the default, is verification off) is the
  digest-pinned `preview-tester` image, never under the preview image prefix or an agent image registry.
  `--preview-verify-deadline-max` (`MAUL_PREVIEW_VERIFY_DEADLINE_MAX`, default `30m`, 1m to 1h) is the most a tester Job
  may run; a longer request is clamped. The chart sets it from `preview.verification.deadlineMaxSeconds` (60 to 3600,
  default 1800), the bound the slot Job admission policy enforces, and renders the same value as intent-controller's
  `--intent-verify-timeout-max`.
- **Re-checks before anything runs.** The request must name this Preview's UID and generation, which must be Ready at
  exactly the requested heads with its rollout settled (no pod of the Preview still on an older image); the criteria
  must equal the automatable criteria of the Intent's approved list, by digest and in order; and the Preview's
  `environment` must re-derive from the Project. Otherwise the verification is `Stale` and nothing renders.
- **Per-part NetworkPolicies.** A Preview created with `spec.environment` (set once, immutable) gets one NetworkPolicy
  per part, `preview-<project>-<issue>-np-<part>`, admitting only the edges derived from the Project beside the chart's
  `preview-isolation`, which it never touches. In this release the one edge is the tester to each component, on its
  numeric port. A Preview without an environment renders exactly what it always did.
- **The tester Job**, `preview-<…>-vt-<hash>`: one pod, `backoffLimit: 0`, `activeDeadlineSeconds` the clamped timeout,
  no service-account token, restricted security context, small requests and limits, a termination message in a file. It
  requests only `http://<component Service>:80`, built from the declared component, never from criterion text.
- **The result.** The controller reads the tester's log through `pods/log`, accepts only a well-formed `MAUL-VERIFY:`
  stream whose summary count and hash match, and writes observations, never verdicts, to the PreviewVerification's
  status, with component container facts (restarts, waiting reasons) and bounded, redacted log tails. A Preview that
  fails while a verification runs has its failure written onto the request before the slot is released. Unclear output
  is `Infra`, not a pass.
- **Cleanup.** Jobs and NetworkPolicies are found and removed by the Preview's UID label like every rendered object, and
  the orphan sweep removes a tester pod whose Job is gone. It also removes the Job of a request that is gone and, once
  it is 30 seconds old, an evaluator ingress policy whose request is gone (intent-controller deletes a request in flight
  when its evaluate run fails, and no owner reference can reach the policy across namespaces), logging each.

### Grants

Everything below renders only under `preview.verification.enabled`, and each is fenced by an admission policy
(`maul-preview-jobs`, `maul-preview-networkpolicies`, `maul-preview-verifications`):

| Where                 | Resource                                                       | Verbs                             |
| --------------------- | -------------------------------------------------------------- | --------------------------------- |
| Each slot (Role)      | `jobs` (batch)                                                 | create, get, list, delete         |
| Each slot (Role)      | `networkpolicies` (networking.k8s.io)                          | create, get, list, update, delete |
| Each slot (Role)      | `pods` (delete, beside the list it already had) and `pods/log` | delete; get                       |
| The release namespace | `previewverifications`                                         | get, list, watch                  |
| The release namespace | `previewverifications/status`                                  | update                            |

intent-controller gets `previewverifications` create, get, list, watch and delete beside it; it is the only writer of a
request, and preview-controller the only writer of its status. preview-controller still has no Secret, ConfigMap,
namespace, cluster-scope, GitHub or cloud access, and the slot quota gains room for one Job and the edge policies
(`preview.quota`). To check a cluster's NetworkPolicy enforcement for the edges, run
`hack/preview-isolation-probe/run.sh edges` (see its README); `ingress.sh --check-policies` judges the rendered policies
without creating a pod.

## The agent evaluator's tool pod

With the [agent evaluator](../intents/evaluator.md) on as well (chart `intentController.verification.evaluator: true`
beside `preview.verification.enabled`), an outcome verification does not run the tester. preview-controller runs the
slot **tool pod** instead: the hands of the evaluator, which itself runs in the agents namespace with no route to the
application. Off, an outcome verification is `Infra` and renders nothing.

- **Flags**, all set by the chart under the evaluator. `--preview-evaluator` (`MAUL_PREVIEW_EVALUATOR`, default `false`)
  needs `--preview-tester-image`, the three images and `--preview-agents-namespace`. `--preview-tools-image`,
  `--preview-browser-image` and `--preview-toolbox-image` (`MAUL_PREVIEW_TOOLS_IMAGE`, `MAUL_PREVIEW_BROWSER_IMAGE`,
  `MAUL_PREVIEW_TOOLBOX_IMAGE`) are the digest-pinned `preview-tools` (maul's relay, Node and the pinned Playwright
  CLI), `preview-browser` (Chromium behind maul's keyed CDP shim) and `preview-toolbox` (the sandbox: curl, grpcurl,
  websocat, node, python, go, and maul's exec shim) images, never under the preview image prefix; the chart takes them
  from `preview.verification.tools.image`, `.browserImage` and `.toolboxImage`, which the release workflow stamps.
  `--preview-agents-namespace` (`MAUL_PREVIEW_AGENTS_NAMESPACE`, the chart's `agent.namespace`) is the one namespace a
  tool pod admits its evaluator from. `--preview-max-evaluations` (`MAUL_PREVIEW_MAX_EVALUATIONS`, default `0`, meaning
  the slot count) bounds the tool pods running at once; a verification waiting past its own timeout for room is `Infra`
  (`tools-not-ready`).
- **Re-checks.** As for criteria, plus: the outcomes must equal the Intent's approved outcomes under their digest, and
  the saved checks the check store's, each valid and bound to an approved outcome.
- **The pod.** A Job named as a tester Job is (so the cleanup and the sweep cover it), whose main container `tools` runs
  the relay, with `browser` (only when the Project declares `surfaces.browser`) and `sandbox` as native sidecars. Each
  container runs as its own user, read-only, with no capability, no service-account token and no host namespace. Only
  `tools` mounts the tool ConfigMap (its configuration, the outcomes and saved checks, the captured logs and events);
  only `tools` and `browser` mount the per-verification CDP key Secret; `tools` and `sandbox` share only the exec shim's
  socket. The Secret and ConfigMap are owned by the Job and never read back. Requests equal limits: 500m/384Mi for
  `tools` and `sandbox`, 1500m/1280Mi for `browser`, about **2.5 CPU and 2 GiB** in all.
- **Network.** The pod is the slot's tester part with role `tools` (`maul.run/preview-role`), so the Preview's edge
  policies give it exactly the tester's edges to the components. The chart's `preview-tools-isolation` (kept, Helm's)
  isolates role `tools` both ways with no rule of its own, and `preview-isolation` no longer selects it, so it has no
  DNS and no gateway. preview-controller adds one ingress policy per verification, admitting on port 8090 only pods of
  role `evaluator` carrying this verification's UID in `maul.run/preview-verification`, from the agents namespace;
  `maul-preview-networkpolicies` admits no other shape.
- **`status.tools`.** Once `tools` is Ready, and every sidecar the round asks for (`browser`, `sandbox`) is running and
  Ready, the controller writes the pod's address and port to the request's `status.tools`; intent-controller creates the
  evaluator only then, because the evaluator reaches it through a host entry fixed when its pod starts.
- **Logs and events.** While the pod runs, each poll refreshes the tool ConfigMap with a bounded log tail of each
  component since the round began, redacted (JWT-shaped strings, `Bearer` and `Authorization` values, the round's keys),
  and the slot's pod events and container states. That is how `maul-tool logs` and `maul-tool events` see them: the
  evaluator and the tool pod hold no Kubernetes access. The CDP key lives only in the memory of the controller process
  that wrote its Secret (it never reads a Secret back), so after a controller restart a running browser round's capture
  is no longer refreshed: the last one written stands, and the key is never captured unredacted.
- **The result.** Once the Job finishes, the controller reads the `tools` container's log (that container only) and its
  termination message, holds the ledger to the count and sha256 both carry, and writes an index of every entry to
  `status.ledger`, with the replay and confirmation runs and the confirmed checks' bodies taken from the ledger's own
  dry-run records, never from the evaluator's report. The full ledger goes to the ConfigMap `<verification>-ledger` and
  the cited screenshots (kept only on a sha256 match with their ledger entry, checked as PNG and re-encoded, at most
  1280 pixels wide) to `<verification>-evidence`, both in the release namespace and owned by the request. A ledger that
  does not hold is `Infra` (`ledger-mismatch`), never a verdict. So is a full round whose ledger holds no call from the
  evaluator (no agent entry, no agent-runner finish), as when the Job ran to its deadline with no evaluator:
  `tools-not-ready` when `status.tools` was never written, else `evaluator-not-run`. A pod that is never created,
  scheduled or pulled is `Infra` past its bound (`tools-not-ready`, `tools-unscheduled`, `tools-image-pull`; a quota
  refusal `quota-refused`). So is one whose container, a browser or sandbox sidecar above all, keeps exiting in error
  (`CrashLoopBackOff`, or three restarts with the last exit non-zero): `tools-crashloop`, settled past the same bound
  rather than at the Job's deadline, its `status.environment` fact naming the container, its last termination and, as
  its `logTail`, the redacted termination message (also logged as `preview tool pod container crash-looping`). Settling
  deletes the Job, its Secret and ConfigMap, and the ingress policy.
- **Timing.** A round's request asks for the evaluate timeout plus five minutes. The chart refuses
  `intentController.verification.evaluateTimeout` unless it is shorter than `preview.verification.deadlineMaxSeconds`,
  so the slot Job policy never refuses the tool pod.
- **Capacity.** With the evaluator on, the chart sizes each slot for a tool pod beside a full Preview: the slot quota
  becomes pods 11, requests 3750m/3328Mi, limits 5 CPU/4608Mi, jobs 2 (a finished Job counts until deleted) and
  networkPolicies 9, and the LimitRange's per-container maximum 1500m/1536Mi. Preview nodes need room for one tool pod
  per concurrently evaluated slot; lower `--preview-max-evaluations` (`previewController.config.extra`) where they do
  not have it.

### Grants under the evaluator

Rendered only with the evaluator and verification both on, beside the grants above, and each fenced by an admission
policy (`maul-preview-tool-objects` in the slots, `maul-preview-ledgers` in the release namespace):

| Where                 | Resource     | Verbs                  | For                                                             |
| --------------------- | ------------ | ---------------------- | --------------------------------------------------------------- |
| Each slot (Role)      | `events`     | get, list, watch       | the pod events a tool pod reports                               |
| Each slot (Role)      | `secrets`    | create, delete         | the per-verification CDP key (never read)                       |
| Each slot (Role)      | `configmaps` | create, update, delete | the tool ConfigMap (never read; each refresh replaces it whole) |
| The release namespace | `configmaps` | create, delete         | each verification's ledger and evidence (never read)            |

In a slot, `maul-preview-tool-objects` holds the Secret and ConfigMap writes to preview-controller's own objects (a
`preview-` name and its managed-by label), since a slot also holds Secrets that are not its own. The evaluator labels
are fenced too: `maul-evaluator-labels` lets only preview-controller set role `tools` and
`maul.run/preview-verification` on a slot Job, and only the Job controller on its Pod. `hack/preview-isolation-probe`
probes the evaluator's rows of the isolation matrix live.
