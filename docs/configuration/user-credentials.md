# Per-user model credentials

Per-user model credentials let each person's own Anthropic API key or Claude setup-token pay for the runs attributed to
them. A run with no human behind it (a Finding investigation, an unheld remediation, a plan triggered by someone who is
not an approver) keeps using the org credential. It builds on [run attribution](attribution.md): whose credential pays
is read from the run's attribution.

The feature arrives in stages. The credential store (the `ModelCredential` kind, the credentials namespace, its RBAC and
its admission policies) and the broker's per-request resolution and health probes have shipped. The status page's
Account page, where people store their own credential, follows in a later release; until then a credentials admin writes
the records and Secrets.

It is off by default. With it off, the chart renders nothing new. The `ModelCredential` CRD is installed, as every
patchy kind is, but nothing grants access to it and no policy judges it.

## Turning it on

| Helm                                                   | Default              | Values                                             |
| ------------------------------------------------------ | -------------------- | -------------------------------------------------- |
| `egressBroker.userCredentials.mode`                    | `off`                | `off`, `attribute`, `enforce`                      |
| `egressBroker.userCredentials.namespace`               | `patchy-credentials` | the namespace the credential Secrets live in       |
| `egressBroker.userCredentials.createNamespace`         | `true`               | create that namespace with the chart               |
| `egressBroker.userCredentials.orgCredentialIsPersonal` | `""`                 | `acknowledged` admits a setup-token org credential |

- `attribute` renders the store and has the broker read each run's attribution. People can register their credentials,
  and their health is probed, but the org credential still pays for every run; the audit line records the person.
- `enforce` makes a run attributed to a person use that person's own credential or none. A run attributed to the org
  keeps the org credential. See [What the broker does](#what-the-broker-does).

Either mode is refused at render time unless all of the following hold:

- `attribution.mode` is `attribute`.
- The egress broker is deployed (a claude runner is enabled).
- `agent.runners.claude.provider.name` is `anthropic`. Bedrock, Vertex and Foundry use the broker's one cloud identity,
  so they get attribution and usage only.
- The credentials namespace is neither the release namespace nor the agent namespace.
- The subscription guard passes (below).

### The subscription guard

With `egressBroker.anthropicAuth: token`, the org credential is a Claude subscription's setup-token, and a subscription
belongs to a person. Turning on per-user credentials while that token pays for every run with no human behind it would
bill that one person for the org's work. So the chart refuses `attribute` and `enforce` while the org route uses a
setup-token, unless the operator records that they know:

```yaml
egressBroker:
  anthropicAuth: token
  userCredentials:
    mode: attribute
    orgCredentialIsPersonal: acknowledged
```

An org API key (`anthropicAuth: key`) needs no acknowledgement. Switching to one is a configuration change alone: swap
the `patchy-anthropic` Secret's `api-key` for the API key and set `anthropicAuth: key`.

## The store

Each person's credential is one Secret per provider in the credentials namespace, and one `ModelCredential` record in
the release namespace holds the non-secret facts about it. Both names derive from the owner's GitHub numeric id, so no
caller ever chooses one:

| Object              | Namespace   | Name                           |
| ------------------- | ----------- | ------------------------------ |
| `ModelCredential`   | release     | `<owner hash>-<provider>`      |
| credential `Secret` | credentials | `cred-<owner hash>-<provider>` |

The owner hash is the first 16 hex digits of SHA-256 over the owner value, `github-id:<id>`. For GitHub id 42 the record
is `bf39649e29052e0e-anthropic` and the Secret `cred-bf39649e29052e0e-anthropic`. The Secret carries the label
`patchy.bitwisemedia.uk/credential-owner: <owner hash>`, equal to the hash in its own name, and one data key,
`credential`, of 1 to 3072 bytes.

A `ModelCredential` (short name `mcred`) holds:

- `spec.owner`: `github-id:<id>`, immutable;
- `spec.ownerLogin`: the GitHub login, for display only;
- `spec.provider`: `anthropic`, immutable;
- `spec.kind`: `api-key` or `oauth-token` (a setup-token);
- `status.fingerprint`: the credential's last four characters and a SHA-256 prefix, never more of it;
- `status.health`: `Valid`, `Rejected` or `Unknown` (empty is `Unknown`), from probes the broker issues itself;
- `status.secretVersion`: the credential Secret's `resourceVersion` that health was found for; a health is `Unknown` for
  any other version;
- `status.lastUsedAt` and `status.lastRejectedAt`.

There is no Secret pointer. A record's name must be its provider behind a 16-hex-digit hash, enforced by the CRD. It is
deliberately not in the `patchy` category, so `kubectl get patchy` does not list whose credentials exist.

## What the broker does

With either mode on, the chart passes the broker `--user-credentials` (the mode), `--credential-records-namespace` (the
release namespace) and `--credentials-namespace`, and `--org-credential-is-personal acknowledged` when it is set. The
broker itself refuses to start in either mode while the org credential is a setup-token unless that acknowledgement is
given, or beside any route but anthropic.

For each request, after the TokenReview:

1. The broker reads the token's own pod (by name, in the agent namespace) and checks that its UID is the token's. It
   reads the pod's `patchy.bitwisemedia.uk/attributed-to` annotation. This is cached with the verdict, one read per
   token per `--verdict-ttl` (a minute).
2. A run attributed to `org` uses the org credential, in both modes.
3. Under `attribute`, every run uses the org credential.
4. Under `enforce`, a run attributed to `github-id:<id>` uses that person's credential: the `ModelCredential` named for
   them (from an informer), and the Secret named for them. The Secret's version comes from a metadata-only informer, and
   its data from a live read, kept for at most a minute while its version holds. The Secret must carry the owner label
   equal to the hash in its name, be `Opaque`, hold only `credential`, and the credential must be one token of printable
   ASCII. An API key is sent as `x-api-key`, a setup-token as an `Authorization` bearer.

Under `enforce` nothing falls back to the org credential. A request that cannot be resolved is refused before it reaches
the provider, and the reason is in the error the agent sees, the audit line and the request counter:

| Reason                      | Status | Meaning                                                                                |
| --------------------------- | ------ | -------------------------------------------------------------------------------------- |
| `attribution_unverified`    | 403    | the token is bound to no pod UID, or the pod is gone, or its UID is another pod's      |
| `attribution_missing`       | 403    | the pod carries no `attributed-to` annotation                                          |
| `attribution_invalid`       | 403    | the annotation is outside its grammar                                                  |
| `credentials_unsynced`      | 503    | the broker's informers have not listed yet (a retry succeeds once they have)           |
| `credential_missing`        | 403    | the person has no `ModelCredential`                                                    |
| `credential_mismatch`       | 403    | the record found is not the person's (owner, provider or kind)                         |
| `credential_secret_missing` | 403    | the person has a record but no Secret                                                  |
| `credential_secret_invalid` | 403    | the Secret breaks the store's contract (label, type, keys or value)                    |
| `credential_unavailable`    | 503    | the Secret could not be read from the API server                                       |
| `credential_rejected`       | 403    | the provider rejected the stored credential: its health is `Rejected` for this version |

Holding a refused run as Blocked, and telling the person, is the run controllers' part and ships with the Account page.

**Revocation.** Deleting or replacing a credential Secret reaches the broker as soon as its informer sees the change. If
the informer's watch stalls, the broker still reads the Secret again at least once a minute, so a revoked credential is
used for at most a minute. Deleting the `ModelCredential` revokes it too.

**The audit line** gains `user` (the attributed person, `org`, or `unverified`, `none` or `invalid`) and `credential`
(the paying credential's fingerprint, or `org`; empty on a refused request). Neither ever holds a credential. With the
mode off, the line is unchanged.

**The usage ledger** records the kind of credential that paid, per request: a person's setup-token is `oauth-token`
(notional cost) even when the org credential is an API key, and a pod whose person switched kinds midway reports the two
apart.

### Health

A credential's `status.health` comes only from probes the broker issues itself, never from a run's traffic:

- An API key is probed with `GET /v1/models?limit=1`, which costs nothing. A setup-token is probed by asking
  `--credential-probe-model` (`claude-haiku-4-5`) for one output token, with the OAuth beta header, because a
  subscription authorises messages.
- A 2xx is `Valid`, a 401 is `Rejected`; anything else is no verdict, retried a few times, a minute apart.
- `status.secretVersion` records the Secret version a health was found for. A health speaks only for that version: once
  the Secret changes, the broker resets health to `Unknown` and probes the new credential.
- A verdict is written as a patch conditional on the record's `resourceVersion`, and only while the Secret is still the
  version probed (read again just before the write), so a verdict on a replaced credential never lands.
- Probes are throttled: one per credential version, a recheck at most every ten minutes, and at least two seconds
  between any two probes. A 401 on a run's request asks for a recheck; the run's answer is never the verdict.
- `status.lastUsedAt` is written at most once an hour per credential.
- The broker's readiness never depends on any of this. It stays tied to the org credential alone.

Each health write is logged with the record, the health, the Secret version and the fingerprint.

## Who may do what

| Who                   | Grants                                                                                                                                                                                                                                            |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| status server         | Secrets in the credentials namespace: `create`, `update`, `delete`. Never `get`, `list`, `watch` or `patch`: a stored key is never read back, and a PATCH response is the whole stored object. Records and their status in the release namespace. |
| egress broker         | `get` pods in the agent namespace; `get`, `list`, `watch` Secrets in the credentials namespace; read records and write their status.                                                                                                              |
| everyone who signs in | `<release>-credentials-self`: `get`, `update`, `delete` on the virtual `modelcredentials/self`.                                                                                                                                                   |
| credentials admins    | `<release>-credentials-admin`: every record, and `get`, `update`, `delete` on the virtual `modelcredentials/all`.                                                                                                                                 |

The virtual subresources are served by nothing on the API server, so they grant kubectl nothing. The status server's
access reviews read `modelcredentials/self`, and the admission policies and the status server recognise an admin by
`update` on `modelcredentials/all`. Bind both ClusterRoles with a RoleBinding in the release namespace, for example:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: credentials-self
  namespace: patchy
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: patchy-credentials-self
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: github:my-org
```

The admin role grants no Secret access. A credential is never readable through patchy, by anyone.

## Admission policies

Three `ValidatingAdmissionPolicies` fail closed:

- **`<release>-credentials-records`**: a `ModelCredential` is created or changed only by the status server (which stamps
  the signed-in person as the owner) or by a credentials admin. A broad grant, such as a role over every patchy kind, is
  not enough. Deleting one is a revocation, so RBAC alone decides it.
- **`<release>-credentials-status`**: each status field has one writer. The broker writes health, the Secret version it
  was found for, and the timestamps, and never the fingerprint. The status server writes the fingerprint, may only reset
  health to `Unknown` and clear the Secret version, and never moves the timestamps. Nobody else writes status, an admin
  included.
- **`<release>-credential-secrets`**: a Secret in the credentials namespace is written only by the status server or a
  credentials admin. It must be named `cred-<owner hash>-<provider>`, carry the owner label with the same hash, be
  `Opaque` and hold exactly one key, `credential`, of 1 to 3072 bytes.

## Security notes

- The status server gains Secret writes, the first from an internet-facing component. They are confined to one namespace
  that holds nothing else, write-only, and every Secret it writes is held to the derived name and label.
- The broker gains reads beyond TokenReview: its callers' pods, the records and the credential Secrets. A broker
  compromise already meant an org-credential compromise; with this on, it also means every stored key. Agent pods still
  hold no credential at all: a person's credential is attached by the broker on the way out, exactly as the org's is.
- The broker keeps no credential in memory that no request needed: its Secret informer is metadata only, and a Secret's
  data is read when a run of its owner calls, then kept for at most a minute.
- Which credential pays is decided by the run's attribution, which only the run controllers may set (the attribution
  policies), read from the caller's own pod, UID-checked. A prompt-injected agent cannot choose another person's.
- A setup-token probe spends one output token of that person's subscription per new credential version, and at most one
  more every ten minutes while their runs are refused.
- Under both modes the namespace and grants exist before anything uses them, so people can register ahead of `enforce`.

## Turning it off

Set `egressBroker.userCredentials.mode: off`. The chart removes the grants and the policies. It keeps the credentials
namespace it created (`helm.sh/resource-policy: keep`), and with it every stored credential, because deleting them is
the operator's decision, never a side effect of an upgrade. With the grants gone nothing reads or writes them. To remove
them, delete the namespace. The `ModelCredential` records stay in the release namespace until deleted.
