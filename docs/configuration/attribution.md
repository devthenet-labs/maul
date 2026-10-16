# Run attribution

Run attribution records whom every agent run is for. It is the first layer of per-user credentials and usage. The org
credential still pays for every run. Attribution only records who caused each one. It is off by default. With it off,
run specs, agent Jobs, issue comments and the chart's admission policies are exactly what they were before it existed.

## Turning it on

| Helm                          | Flag                 | Env                       | Default |
| ----------------------------- | -------------------- | ------------------------- | ------- |
| `attribution.mode: attribute` | `--attribution-mode` | `PATCHY_ATTRIBUTION_MODE` | `off`   |

The flag is shared by the four controllers that launch agent Jobs: investigation-, remediation-, intent- and
evaluation-controller. The chart sets it on all four from `attribution.mode`. It also renders the admission policies
described [below](#admission-policies), which need Kubernetes 1.30 or later. An unknown mode stops the controller at
startup.

Set the chart value rather than the flag on its own. Under `attribute` the Job policy requires every agent Job to carry
the annotation, so a controller left on `off` would have its launches refused. The chart's ConfigMap change rolls the
four controllers. A run launched during that roll by a pod still running the old configuration can be refused. An intent
run refused that way ends `launch_refused` and is retried as its next attempt. To avoid that window, pause the agents
first (`agent.paused`), upgrade, and then resume them.

## What is recorded

Each new `Investigation`, `Remediation`, `IntentRun` and `EvaluationUnit` gets `spec.attributedTo` when it is created.
It names a user (`login` and GitHub numeric `id`) and a `reason`. Every run spec is immutable, so the stamp is written
once and never changes.

| Run                     | Attributed to                                                     | `reason`    |
| ----------------------- | ----------------------------------------------------------------- | ----------- |
| Intent plan             | whoever opened the plan's revision, if they are an approver       | `requester` |
| Intent plan             | otherwise, the organisation                                       | `org`       |
| Intent build            | the plan's approver                                               | `approver`  |
| Intent revise (command) | the approver whose `/patchy revise` or `/patchy retry` started it | `commenter` |
| Intent revise (review)  | the author of the newest approver review the round consumed       | `reviewer`  |
| Intent check-fix        | the plan's approver                                               | `approver`  |
| Finding investigation   | the organisation: no human is behind an investigation             | `org`       |
| Finding remediation     | the Finding's approver when its approval is held, else the org    | `approver`  |
| Evaluation unit         | its Evaluation's submitter                                        | `submitter` |

A plan's revision is opened by the trigger that created the Intent, or by the replan or revival that started it again.
Every attempt of that revision is attributed to the same person. A trigger action consumed while it runs, a refused or
unavailable one included, starts no plan and changes no attempt's attribution.

A person is identified by their GitHub numeric id. A login can be renamed, and a freed login can be taken by someone
else, so the login is kept for display only. patchy records the id wherever GitHub supplies it:

- an Intent's `spec.requestedBy.userID`;
- every consumed trigger action (`status.lastTrigger.userID`);
- the trigger action that opened the current plan revision (`status.input.trigger.userID`);
- the accepted approval (`status.approval.byID`);
- a Finding's approval, retry and expedite made by a `/patchy` comment (`spec.approval.byID` and the same field on
  `retry` and `expedite`).

**Re-attribution.** A person the table names may have no patchy identity, meaning no GitHub id is on record for them.
The run is then attributed to the organisation, with reason `approver-unregistered`, or `submitter-unregistered` for an
evaluation unit. This applies to:

- an approval made on the status page or in the CLI, whose usernames are not GitHub accounts;
- an Intent created before ids were recorded;
- every evaluation submitter, for now, since the evaluation API verifies a username and no GitHub id.

No one is attributed a run on the strength of a login alone.

**On the Job.** Every agent Job, and the pod template its pods are made from, carries the annotation
`patchy.bitwisemedia.uk/attributed-to`. Its value is `org`, or `github-id:<id>` for a person. The launching controller
takes it from the run's own stamp. A run with no stamp (one created before the mode was turned on) is annotated `org`.
investigation-controller only ever writes `org`.

**On the issue.** The intent issue's status comment gains a line beside the cost:
`Attributed to: @login (reason), for the latest run`, or the organisation. The login is shown as code, like every login
in that comment, so it notifies no one.

## Admission policies

With `attribution.mode` other than `off`, the chart renders up to six ValidatingAdmissionPolicies. Each fails closed.

- **`<release>-attribution-jobs`** applies in the agent namespace. A Job created by one of the launching controllers
  must be of that controller's own run kind (`investigation`, `remediation`, `intent`, `evaluation`). It must carry the
  annotation in the grammar on the Job and its pod template alike, and investigation-controller may only write `org`.
  Anyone else's Job may not carry the annotation at all. No update may set, change or remove it.
- **`<release>-attribution-pods`** applies in the agent namespace. A pod carrying the annotation is admitted only when
  the Kubernetes Job controller (`admissionPolicy.jobControllerUsernames`) creates it, with a controlling owner
  reference to a `batch/v1` Job. No update, nor a `pods/status` write, may set, change or remove it.
- **`<release>-attribution-finding-stamps`** applies in the release namespace. Anyone but patchy's controllers, the
  status server and `admissionPolicy.extraExemptSubjects` who sets or changes a Finding's `spec.approval`, `spec.retry`
  or `spec.expedite` must name themselves in `by` (their Kubernetes username) and must not set `byID`. The
  [finding-actions policy](../cli.md#permissions) still decides whether they may take the action at all.
- **`<release>-attribution-evaluation-submitter`** applies in the release namespace, and only with the
  evaluation-controller. An Evaluation created by anyone but the evaluation-controller (whose API stamps the verified
  submitter) must name its creator in `spec.submitter`.

- **`<release>-attribution-run-stamps`** applies in the release namespace. A new `Investigation`, `Remediation`,
  `IntentRun` or `EvaluationUnit` may carry `spec.attributedTo` only when the controller of that kind creates it:
  investigation-, remediation-, intent- or evaluation-controller respectively. Anyone else, another of those controllers
  included, creates a run without a stamp, and its Job is then annotated `org`.
- **`<release>-attribution-intent-requester`** applies in the release namespace, and only with the intent-controller. An
  Intent created by anyone but the intent-controller (which reads the requester from the issue's events) must not carry
  `spec.requestedBy.userID`, so a requester named by login alone is never attributed a run.

Together with the immutable run specs, these mean that only the run controllers decide attribution. A prompt-injected
agent, or anyone holding `create` on pods or Jobs, cannot bill a run to someone else.

## Turning it off

Set `attribution.mode: off`. The controllers stop stamping new runs and annotating new Jobs, and the policies are
removed. Stamps already written stay on their runs: the specs are immutable, and the stamps are only records.
