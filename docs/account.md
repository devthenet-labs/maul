# Your model credential: the Account page

When your patchy operator turns on [per-user model credentials](configuration/user-credentials.md), the agent runs that
are attributed to you are paid for with your own credential rather than the organisation's: an Anthropic one for runs on
claude, an OpenAI API key for runs on codex. The **Account** page of the [status page](status-ui.md) is where you store
each credential, check that its provider accepts it, replace it and delete it. It shows one card per provider your
operator runs.

The page appears in the top bar once you are signed in, and only when the operator has it on. If it says "Account
unavailable", the reason is on the page: usually you signed in with something other than GitHub, or no one has granted
you the credentials role yet. Ask your operator.

## Which runs are yours

A run is attributed to the person whose decision started it:

- **Intent plans** you trigger, if you are one of the Project's approvers. A plan triggered by anyone else is the
  organisation's.
- **Intent builds** of a plan you approved, and the check-fix runs that follow them.
- **Revisions** you ask for with `/patchy revise`, or that answer the review you left last.
- **Finding remediations** you approve with `/patchy approve` on the tracking issue.

Investigations of Findings, remediations nobody approved, and approvals made on the status page or with the CLI are the
organisation's: patchy cannot tie them to your GitHub account, so they never use your credential.

## Storing a credential

A run uses the credential of its own provider only: a claude run your Anthropic one, a codex run your OpenAI one. If
your operator runs both, store both.

For Anthropic you need one of:

- **An Anthropic API key** (`sk-ant-api…`), from the Anthropic Console's API keys page. Runs on it are billed to that
  key's Anthropic organisation at API prices.
- **A Claude setup-token** (`sk-ant-oat…`), which `claude setup-token` prints after you sign in to your Claude
  subscription. Store only your own: it is used only for runs attributed to you, and they count against your
  subscription's limits. Usage shows such runs as an equivalent API cost, not as money spent.

For OpenAI you need **an OpenAI API key** (`sk-proj-…` or `sk-…`), from the OpenAI Platform's API keys page. Runs on it
are billed to that key's OpenAI project at API prices. A ChatGPT sign-in cannot be used.

On the Account page, choose the kind on the Anthropic card (pasting a credential picks it for you from its prefix),
paste the credential into the password field and press **Save**. It must be a single token of printable characters, at
most 3072 of them; patchy refuses one whose prefix says it is the other kind, or the other provider's.

patchy never shows a stored credential again, to you or anyone else, administrators included. The page shows only:

| Field         | What it means                                                                                       |
| ------------- | --------------------------------------------------------------------------------------------------- |
| Kind          | API key or setup-token (OpenAI: API key)                                                            |
| Fingerprint   | the credential's last four characters and the start of its SHA-256, so you can tell which one it is |
| Health        | `Valid` (the provider accepted it), `Rejected` (it refused it) or `Unknown` (not checked yet)       |
| Last used     | when a run of yours last used it, to the hour                                                       |
| Last rejected | when the provider last refused it, if ever                                                          |

A new credential is `Unknown` until patchy checks it, which happens within moments: an API key (either provider's) by
listing models, which costs nothing, a setup-token by asking for a single output token.

## When a run waits for you

If the operator enforces per-user credentials and a run would be yours while you have no valid credential, patchy does
not start it. It waits instead, and tells you where you work:

- On an intent, its status comment says that you must add a credential, for which provider, and links the Account page.
- On a Finding, its tracking issue gets a comment that names you and the provider and links the page.

Store or replace your credential, and the run starts on its own once patchy finds it `Valid`. Nothing is replanned and
nothing needs re-approving. An approver who already holds a valid credential can also approve the build or the
remediation again, which makes the run theirs instead.

## Replacing and deleting

- **Replace** a credential that is `Rejected`, expiring or that you are rotating: paste the new one and press
  **Replace**. Runs move to the new one within a minute at most, and it is checked as any new credential is.
- **Delete** removes it. Within a minute at most patchy stops paying with it: a run of yours already going is refused
  from then on and is likely to fail, and a new one waits until you add another.

Revoking the key at Anthropic as well is up to you: patchy deleting its copy does not revoke the key itself.
