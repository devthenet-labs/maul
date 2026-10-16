# Sizing limits for large features

patchy's default limits are sized for a security fix or a small intent: a build of at most 150 turns in an hour. A
feature-sized intent, one that adds a subsystem across many files, runs for longer, over more turns and with a much
larger context. This guide shows what such runs consume, why a token limit is the wrong measure for them, and how to
switch an install to cost limits with a "large" preset that fits them.

Everything here is opt-in. Until an operator sets `egressBroker.limits.unit: cost`, the broker counts tokens exactly as
before, no run carries a budget, and every intent Job keeps its 90 minute deadline.

## What a feature-sized run consumes

These are measured agent runs that each implemented one feature-sized pull request in patchy itself, on the same model
family patchy's intents use:

| Agent                           | Turns | Minutes | Output tokens | Total tokens (incl. cache reads) | Peak context |
| ------------------------------- | ----: | ------: | ------------: | -------------------------------: | -----------: |
| implement attribution PR        |   232 |      55 |           32k |                            67.7M |         479k |
| implement preview-controller PR |   108 |      43 |           19k |                            23.5M |         302k |
| implement usage ledger PR       |   164 |       — |           29k |                            46.1M |         431k |
| implement preview chart PR      |   161 |       — |           25k |                            49.5M |         451k |

A whole feature track, with an implementer, three reviewers, verifiers, a fix round and CI for each of about three pull
requests, came to about 6M tokens without cache reads, but **about 140M to 160M tokens with them**: roughly \$110 per
track at API list prices (\$5 input, \$6.25 cache write, \$0.50 cache read and \$25 output per million tokens). A
client's token counter often leaves cache reads out. The broker does not: it counts every token the model API reports.

The largest single run, the attribution implementer, prices at about \$42. It is the run the large preset below is sized
around.

## Why cache reads dominate long-context runs

An agent sends its whole conversation with every turn. With prompt caching, the part already seen is a cache read rather
than fresh input, so each turn costs little in money, but its token count is the full context size. Over a run, cache
reads therefore grow with **context size × turns**: the attribution run's 232 turns, at an average context near 300k,
re-read about 65M tokens, around 97% of its 67.7M total. Output, the part a reviewer reads, was 32k tokens.

Cache reads cost a tenth of the input price, so the same run looks very different in each unit:

- **As tokens**, it is 67.7M. A `tokensPerPod` limit of 15M counts a cache read the same as an output token and would
  end this run about 22% of the way through, though the 15M it had consumed cost about \$9.
- **As cost**, it is about \$42, of which cache reads are about \$33. A cost limit of \$60 admits it with room to spare,
  and still ends a run that loops or runs away.

The broker's own e2e suite checks this end to end (`e2e/broker_cost_test.go`): with a fake model API answering every
request with a 413k-token turn of 400k cache reads, a 2M tokens-per-pod limit cuts the run on its sixth turn, while a
\$5 cost-per-pod limit admits all ten turns (\$3.15), and a \$1 run budget still stops its own pod on the fourth.

Raising a token limit high enough for the largest run raises it for every pod in the cluster, at every model's price.
Cost limits price each model's tokens at its own rates, and a run's budget can differ from the cluster's per-pod cap.

## The large preset

The preset is values only; it adds no kind or mode. It raises intent-controller's stage ceilings so a Project may use
them, then lets each large Project opt in through its own limits. A Project that sets nothing keeps the defaults.

| Stage                 | Turns | Timeout | Run budget (\$)       | Output tokens | Evidence                                |
| --------------------- | ----: | ------: | --------------------- | ------------: | --------------------------------------- |
| build                 |   300 |     90m | 60 (`60000000` micro) |          200k | largest run: 232 turns, 55m, about \$42 |
| revise (or check fix) |   200 |     45m | 20 (`20000000` micro) |          200k | a review round on the same context      |
| plan                  |    40 |     20m | 10 (`10000000` micro) |          200k | the defaults; a plan is read-only       |

Output tokens fall to 200k because even the largest run wrote 32k. `tokenBudget` is the runner's output budget and makes
a good tripwire, not a spend measure. The Job deadline follows each stage: 90m plus 15 minutes for a build, under the
default `jobDeadlineMax` of 4h30m.

### patchy chart values

```yaml
egressBroker:
  limits:
    unit: cost
    # The cap of a pod without a budget annotation (Findings, evaluations) and
    # the most any run's budget can be: a pod is held to the smaller of the
    # two, so keep this at or above the largest stage budget.
    costPerPod: 60000000 # $60
    # At least twice the largest run budget, or a Project using it is not
    # Ready (RunBudgetOverHourlyLimit); a stage without its own budget
    # counts the per-intent ceiling. Size it for your concurrent runs.
    costPerHour: 200000000 # $200
    # Token limits stay as backstops under cost. Unset them, or raise them
    # well above 70M a pod: a 15M backstop would still cut the largest run.
    tokensPerPod: 0
    tokensPerHour: 0
intentController:
  config:
    build:
      maxTurns: 300
      timeout: 90m
    revise:
      maxTurns: 200
      timeout: 45m
agent:
  resources:
    # Optional: a larger class for the repositories that build large
    # features (spec.repositories[].agentResourceClass).
    classes:
      large:
        requests: { cpu: 4, memory: 8Gi }
        limits: { memory: 10Gi }
```

Values not shown keep their defaults. A Helm values file replaces only the keys it sets, so the stages' `model`,
`tokenBudget` and `idleTimeout` keep the chart's values.

### A large Project (patchy-config)

```yaml
projects:
  - name: shop-web
    spec:
      repositories:
        - name: web
          url: https://github.com/acme/Shop.Web
          agentResourceClass: large # only if you defined the class above
      limits:
        # The per-intent ceiling covers the plan, the build and every revise
        # round together, and each stage budget must fit under it. Every
        # stage sets its own budget: one that sets none runs on this ceiling,
        # more than half the hourly limit above, and the Project is not Ready.
        maxCostMicroUSD: 150000000 # $150
        plan: { maxCostMicroUSD: 10000000 }
        build: { maxTurns: 300, timeout: 90m, maxCostMicroUSD: 60000000, tokenBudget: 200000 }
        revise: { maxTurns: 200, timeout: 45m, maxCostMicroUSD: 20000000, tokenBudget: 200000 }
```

The rest of the Project (trigger, approvers, previews) is as in [deploying intents](deploying.md). A stage limit above
the controller's ceiling is clamped to it, so the patchy chart values above must come first.

## Switching an install to cost

1. **Measure first, still under tokens.** The broker prices every request under either unit. Its audit line carries
   `cost_micro_usd` and the pod's running `pod_cost_micro_usd`, and the `patchy.broker.cost` counter sums micro-USD by
   route. Run a few intents of the size you expect, then read the largest pod totals. They tell you whether \$60 and
   \$20 fit your Projects.
2. **Upgrade the patchy chart with the values above.** Under `unit: cost` the chart requires `costPerPod`, renders a
   Role that lets the broker `get` pods in the agent namespace (for the budget annotation; no list or watch), gives
   intent-controller the unit and the hourly limit, and renders the admission policies that let only intent-controller
   set a run's budget. It refuses `costPerPod` or `costPerHour` under `unit: tokens`.
3. **Upgrade patchy-config with each large Project's limits.** Projects that set no stage budget get the per-intent
   ceiling (`limits.maxCostMicroUSD`, \$10 by default) as every run's budget.
4. **Check it took.** `kubectl get project <name>` shows the Project Ready, and not `RunBudgetOverHourlyLimit`. A new
   run's IntentRun records `spec.grant.budgetMicroUSD`, its Job and pod carry the
   `patchy.bitwisemedia.uk/budget-micro-usd` annotation, and the broker's audit line for that pod shows
   `pod_cost_limit_micro_usd` at the run's budget.

A run that reaches its budget settles as `budget_exceeded`, and its detail names the limit:
`cost per pod (60000000 micro-USD)`. A run granted before the switch recorded no budget; it launches with its Project's
budget for its stage as it stands then, so no intent Job runs without one.

To roll back, set `egressBroker.limits.unit: tokens` and remove the cost values. The broker stops reading pods, new runs
carry no budget, and the token limits apply again.

## A bigger lever than limits

Cache reads grow with context × turns, so the cheapest large feature is one split into smaller ones. A plan that splits
a feature into pull-request-sized intents of about 150 to 250 turns each keeps every context under about 300k. That, and
a post-build review round, are a separate design and not part of this preset.

See also: [cost limits](../configuration/egress-broker.md#cost-limits) in the broker reference,
[stage limits](../configuration/intent-controller.md#stage-limits) in intent-controller's, and the chart's
[egress broker limits](../deployment/helm.md#egress-broker-limits).
