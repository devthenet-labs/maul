# The codex runner

The codex runner runs the OpenAI Codex CLI (`codex exec`) as the agent, for OpenAI models. It is off by default. Like
claude, it is **brokered**: a codex pod holds no model credential. All of its model traffic goes through the
[egress broker](egress-broker.md)'s [openai route](egress-broker.md#the-openai-route), which holds the OpenAI API key
and attaches it upstream. This page covers turning codex on, migrating from the old in-pod key, what codex changes for
intents, per-user keys, usage and limits, and how to verify it on a live cluster.

What codex can run:

- **Findings** (investigation and remediation): `agent.runners.codex.enabled`. The investigation may then pick an OpenAI
  model from the allowlist for the remediation, and that run goes to codex.
- **Intents** (plan and build), for a Project that chooses codex: `intentController.codex.enabled`. See
  [Brokered claude, and brokered codex by choice](intent-controller.md#brokered-claude-and-brokered-codex-by-choice).
- **Evaluations: refused.** evolve's codex runner cannot use the broker's provider config yet, so the chart refuses
  `evaluationController.runners.codex.enabled` and a codex evaluation unit settles `HarnessUnavailable`
  ([evaluation-controller](evaluation-controller.md)). No path mounts an OpenAI key into a pod.

Only OpenAI **API keys** are supported, for the org and per person. ChatGPT-plan sign-in (`auth.json`) and Codex access
tokens are not.

## Turning it on

1. Create the OpenAI API key Secret in the **release** namespace, beside the Anthropic key. Give the key a hard spend
   cap in the OpenAI Platform, and prefer a project key (`sk-proj-…`) for maul alone:

   ```sh
   kubectl -n maul create secret generic maul-openai --from-literal=api-key="$OPENAI_API_KEY"
   ```

2. Point the broker at it, enable the runner, and allow the OpenAI models the investigation may choose:

   ```yaml
   egressBroker:
     openai:
       apiKey:
         secret: maul-openai
         key: api-key
   agent:
     runners:
       codex:
         enabled: true
     modelAllowlist: anthropic/claude-sonnet-5,anthropic/claude-opus-5,openai/gpt-5.3-codex
   ```

   For codex intents, set `intentController.codex.enabled: true` as well (or instead). Either one deploys the broker,
   even with claude off.

3. Upgrade. Nothing else changes: codex pods get the same posture as claude pods (a projected caller token in the agent
   container only, egress to the artifact server and the broker), so codex works under `agent.networkPolicy.dns: none`
   and `broadEgress: never`. It needs no hosts, DNS patterns or FQDN policies of its own.

The chart refuses the render when a codex runner is enabled without `egressBroker.openai.apiKey`. With kustomize, mount
the Secret into `maul-egress-broker` at `/etc/maul/openai`, set
`MAUL_OPENAI_API_KEY_FILE: /etc/maul/openai/api-key`, and add `codex` to `MAUL_HARNESSES`
(`deploy/kustomize/base/secrets.example.yaml`, 2b, and `configmap.yaml`).

To turn codex off again, set `enabled: false`. The openai route can stay; with no codex runner nothing uses it.

## Migrating from the in-pod key

Codex used to run with the org OpenAI key inside the agent pod: a `maul-openai` Secret in the **agent** namespace,
named by `agent.runners.codex.secret`, `secretKey` and `secretEnv` (controller flags `--codex-secret`,
`--codex-secret-key`, `--codex-secret-env`), plus `hosts` and `dnsPatterns` for its egress. The agent, and anything the
agent ran, could read that key. That path is removed. There is no silent translation, because the Secret changes
namespace and the old one was readable by agent pods.

While codex is enabled on any fleet, setting any of the old keys fails the render, naming these steps (a leftover flag
fails the controller's startup the same way):

1. Create the OpenAI API key Secret in the **release** namespace (above).
2. Set `egressBroker.openai.apiKey.secret` (and `.key`) to it.
3. Remove `agent.runners.codex.secret`, `secretKey`, `secretEnv`, `hosts` and `dnsPatterns` (kustomize: the
   `MAUL_CODEX_SECRET*` keys).
4. After the upgrade, delete the old Secret from the agent namespace:

   ```sh
   kubectl -n maul-agents delete secret maul-openai
   ```

While codex is enabled nowhere, the old keys are ignored, so an install that copied the old defaults renders as before.
The old codex path most likely never authenticated anyway: the pinned codex CLI reads `CODEX_API_KEY`, not the
`OPENAI_API_KEY` the chart set.

## Intents on codex

With `intentController.codex.enabled: true`, a Project chooses codex for its intents with `spec.harness` (here in the
maul-config chart's `projects`; see [Projects](intent-controller.md#projects)):

```yaml
projects:
  - name: shop
    spec:
      intentRepository: https://github.com/acme/intents
      approvers:
        logins: [octocat]
      repositories:
        - name: shop
          url: https://github.com/acme/shop
      harness: codex # unset or claude: claude
```

Its plan and build then run on brokered codex (`openai/gpt-5.3-codex` by default, `intentController.codex.planModel` and
`buildModel`). Each run records the harness it was created with, so changing a Project's harness affects only its new
runs. With the switch off, a codex Project is not Ready (`HarnessNotEnabled`). Codex cannot hold a plan read-only,
repair a refused report in its session, or stream a running command's output; see
[Brokered claude, and brokered codex by choice](intent-controller.md#brokered-claude-and-brokered-codex-by-choice).

## Per-user OpenAI keys

Under [per-user model credentials](user-credentials.md), a codex run attributed to a person pays with that person's own
OpenAI API key. People store it on the [Account page](../account.md), which offers OpenAI wherever codex runs. A run is
held on, and paid by, the credential of its own harness's provider only: codex needs the person's `openai` record, never
their Anthropic one, and the reverse holds for claude. Under `enforce`, a codex remediation or intent run of a person
with no Valid OpenAI key is held, with a message that names OpenAI, until they store one. The broker probes a stored key
with `GET /v1/models`, which costs nothing.

## Usage, limits and pricing

Codex usage is **measured**. Before codex was brokered, a codex run's usage was always `reported` (the pod's own counts,
kept out of trusted totals). Now the broker reads every response's usage, and a codex run settles `measured` like a
claude run (with `attribution.mode: attribute`, which turns usage measurement on; see
[Run attribution](attribution.md)). Its tokens count in the Usage page's trusted figures and in `UsageRollup`s.

- **Pricing.** Measured tokens are priced from the model registry's OpenAI rates (`gpt-5.3-codex`: $1.75 input, $0.175
  cached input, $14 output per million). OpenAI's cached input tokens are cache reads. OpenAI reports no separate cache
  writes, so they are part of fresh input. An API key's cost is never notional.
- **Limits.** Codex requests count toward the broker's token or cost limits, per pod and per hour, and toward a run's
  budget annotation, exactly as claude's do. The ceilings are shared across providers: one spend ceiling, whichever
  model is used. Under `egressBroker.limits.unit: cost`, an OpenAI model the registry does not price is charged at the
  dearest rate, as for claude.
- **Refusals.** A limit refusal reaches codex as a 429 in OpenAI's envelope (`insufficient_quota`), and codex ends its
  turn after its one request. Codex (rust-v0.153.4) reports only the status, not the broker's message, so the stage
  reads `runtime_error`, not `budget_exceeded`; the broker's audit line has the reason
  ([agent-runner](agent-runner.md#codex-runs-brokered)).

## Verifying codex live

The end-to-end suite covers codex against a fake OpenAI upstream (`e2e/broker_cost_openai_test.go`,
`e2e/user_credentials_openai_test.go`, `e2e/intent_codex_test.go`), but no release has yet run the real codex CLI
against the real OpenAI API through the broker. Before relying on codex, run this check once on a cluster you can roll
back, with the values in your usual shape:

1. Get an OpenAI API key with a hard spend cap. Store it in a release-namespace Secret and set
   `egressBroker.openai.apiKey`. Consider a tighter per-pod cost limit for the check (`egressBroker.limits.unit: cost`,
   `costPerPod`).
2. Enable `agent.runners.codex` and add `openai/gpt-5.3-codex` to `agent.modelAllowlist`. Keep the rest of your posture
   as is (`agent.networkPolicy.dns: none` included).
3. Record the rollback point (`helm list -n maul`, `helm history`) and review a render diff of the old and new values
   (`helm diff upgrade`, or `helm template` twice and `diff`).
4. Run a fresh Finding on a test repository whose remediation runs an OpenAI model (set `agent.remediate.model` to
   `openai/gpt-5.3-codex` for the check). Then confirm:
   - the codex Job's pod has no Secret-backed env and no resolver
     (`timeout 30 kubectl -n maul-agents get pod <pod> -o yaml`);
   - the broker's audit line shows `route=openai`, the model and its tokens
     (`timeout 30 kubectl -n maul logs deploy/maul-egress-broker | grep route=openai`);
   - the Remediation's `status.usage` is `measured`, and the Usage page shows trusted figures for it;
   - the pull request opens.
5. Under `egressBroker.userCredentials.mode: enforce`:
   - a person with no OpenAI key on their Account page has their codex remediation held (`CredentialRequired`, naming
     OpenAI);
   - storing the key there releases it, and the broker's audit line carries the person and their key's fingerprint;
   - deleting the key fails their next request closed (`credential_missing`), within a minute.
6. A claude Finding still goes green.
7. If anything fails, roll back (`helm rollback maul <revision>`) and keep the broker's and the Job's logs.
