# Per-user model credentials

Per-user model credentials let each person's own Anthropic API key or Claude setup-token pay for the runs attributed to
them. A run with no human behind it (a Finding investigation, an unheld remediation, a plan triggered by someone who is
not an approver) keeps using the org credential. It builds on [run attribution](attribution.md): whose credential pays
is read from the run's attribution.

The feature arrives in stages. This release ships the credential store: the `ModelCredential` kind, the credentials
namespace, its RBAC and its admission policies. The broker's per-request resolution and the status page's Account page
follow in later releases. Until they ship, the org credential pays for every run under either mode.

It is off by default. With it off, the chart renders nothing new. The `ModelCredential` CRD is installed, as every
patchy kind is, but nothing grants access to it and no policy judges it.

## Turning it on

| Helm                                                   | Default              | Values                                             |
| ------------------------------------------------------ | -------------------- | -------------------------------------------------- |
| `egressBroker.userCredentials.mode`                    | `off`                | `off`, `attribute`, `enforce`                      |
| `egressBroker.userCredentials.namespace`               | `patchy-credentials` | the namespace the credential Secrets live in       |
| `egressBroker.userCredentials.createNamespace`         | `true`               | create that namespace with the chart               |
| `egressBroker.userCredentials.orgCredentialIsPersonal` | `""`                 | `acknowledged` admits a setup-token org credential |

- `attribute` renders the store. People can register their credentials, and the org credential still pays for every run.
- `enforce` is the mode in which a run attributed to a person uses that person's credential or none. It renders the same
  store as `attribute` today; the broker side comes with its release.

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
- `status.lastUsedAt` and `status.lastRejectedAt`.

There is no Secret pointer. A record's name must be its provider behind a 16-hex-digit hash, enforced by the CRD. It is
deliberately not in the `patchy` category, so `kubectl get patchy` does not list whose credentials exist.

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
- **`<release>-credentials-status`**: each status field has one writer. The broker writes health and the timestamps and
  never the fingerprint. The status server writes the fingerprint, may only reset health to `Unknown`, and never moves
  the timestamps. Nobody else writes status, an admin included.
- **`<release>-credential-secrets`**: a Secret in the credentials namespace is written only by the status server or a
  credentials admin. It must be named `cred-<owner hash>-<provider>`, carry the owner label with the same hash, be
  `Opaque` and hold exactly one key, `credential`, of 1 to 3072 bytes.

## Security notes

- The status server gains Secret writes, the first from an internet-facing component. They are confined to one namespace
  that holds nothing else, write-only, and every Secret it writes is held to the derived name and label.
- The broker gains reads beyond TokenReview: its callers' pods, the records and the credential Secrets. A broker
  compromise already meant an org-credential compromise; with this on, it also means every stored key. Agent pods still
  hold no credential at all.
- Under both modes the namespace and grants exist before anything uses them, so people can register ahead of `enforce`.

## Turning it off

Set `egressBroker.userCredentials.mode: off`. The chart removes the grants and the policies. It keeps the credentials
namespace it created (`helm.sh/resource-policy: keep`), and with it every stored credential, because deleting them is
the operator's decision, never a side effect of an upgrade. With the grants gone nothing reads or writes them. To remove
them, delete the namespace. The `ModelCredential` records stay in the release namespace until deleted.
