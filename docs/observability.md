# Observability

Every binary logs structured `log/slog` to **stderr** — stdout is reserved, because the agent-runner's `MAUL-EVENT:`
stream lives there — and ships OpenTelemetry traces, metrics, and logs when an exporter is configured. Telemetry never
blocks startup: if initialisation fails, the binary falls back to stderr-only logging and keeps serving.

The pipeline itself is observable without any of this: the custom resources **are** the state
(`kubectl get maul -n maul`, `kubectl describe finding <name>` for conditions and the phase log), and the
`FindingRollup` objects are the durable statistics record. The metrics below are the same numbers, exported.

## Modes

Each controller resolves its telemetry mode at startup, in this order:

| Mode     | Selected when                    | Behavior                                                                                  |
| -------- | -------------------------------- | ----------------------------------------------------------------------------------------- |
| file     | `MAUL_TELEMETRY_DIR` is set      | One JSON file per signal (`traces.json`, `metrics.json`, `logs.json`) under the directory |
| env      | Any active `OTEL_*` exporter var | Standard OTel autoexport / OTLP                                                           |
| disabled | Neither                          | Stderr logging only; no providers installed                                               |

`MAUL_TELEMETRY_DIR` is environment-only (no flag) and always wins — it exists for tests and local debugging, where a
file you can `jq` beats a collector.

## OTLP configuration

Env mode honours the standard OpenTelemetry variables; any of these being set (to a value other than `none`) activates
it:

```text
OTEL_TRACES_EXPORTER            OTEL_EXPORTER_OTLP_ENDPOINT
OTEL_METRICS_EXPORTER           OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
OTEL_LOGS_EXPORTER              OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
                                OTEL_EXPORTER_OTLP_LOGS_ENDPOINT
```

Point the controllers at a collector by adding the variables to the ConfigMaps via `config.extra` (Helm) or a ConfigMap
patch (kustomize):

```yaml
config:
  extra:
    OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.observability:4317
    OTEL_TRACES_EXPORTER: otlp
    OTEL_METRICS_EXPORTER: otlp
    OTEL_LOGS_EXPORTER: otlp
```

Autoexport's own protocol/endpoint variables work as usual. The resource carries the binary name as `service.name` and
the build version; the instrumentation scope root is `github.com/devthenet-labs/maul`, and W3C TraceContext + Baggage
propagators are installed. Logs are bridged — every `slog` record fans out to both stderr and the OTel logger.

## Signals worth alerting on

**Ingestion** (integration-controller):

- `maul.webhook.deliveries` — counter, attributed by event type and result. A rising `bad-signature` rate means the
  App's webhook secret and the Integration's disagree; `queue-full` means GitHub is redelivering.
- `maul.webhook.delivery` — the span wrapping each delivery's handling.

**Pipeline outcomes** (emitted at rollup time, exactly once per run/finding, aligned with the CR counters — anyone with
an OTLP backend gets the same numbers the `FindingRollup` objects carry):

- `maul.stage.runs` — agent runs by `stage`, `harness`, `model`, `outcome`, and `repo`. Alert on a rising non-`ok`
  outcome share.
- `maul.stage.tokens` — token counts by `stage` and `class` (`input`, `output`, `cache_read`, `cache_creation`).
- `maul.stage.cost` — agent spend in USD, same attributes as `maul.stage.runs`. The budget alarm.
- `maul.finding.completed` — findings reaching a terminal phase, by `phase`, `recommendation`, and `repo`. A rising
  `Failed` share is the pipeline-health signal.
- `maul.finding.deleted` — finding deletions by `reason` (`ttl` or `manual`).

`repo` is the only high-cardinality attribute (~estate size); constrained backends can drop it with a metric view.

**Per-user usage** (only under `attribution.mode: attribute`; emitted by each run's own controller when it folds the run
into its user's `UsageRollup`, exactly once per run, aligned with the rollup counters; see
[usage rollups](configuration/attribution.md#usage-rollups)):

- `maul.usage.tokens` — tokens by `user` (`org` or `github-id:<id>`), `project` (empty for Finding and evaluation runs),
  `kind` (`investigation`, `remediation`, `plan`, `build`, `revise`, `check-fix`, `evaluation`), `source` (`measured`,
  `partial`, `reported`) and `class` (`input`, `output`, `cache_read`, `cache_write`; `reported` for a reported run's
  own total).
- `maul.usage.cost` — USD by `user`, `project`, `kind` and `pricing`: `list` is money spent at list rates,
  `subscription` the notional list-price equivalent of subscription-paid tokens, `reported` a pod's own untrusted
  figure. Sum `list` alone for spend.

`user` is bounded by headcount.

**Preview sign-in** (preview-auth and preview-controller, only while the chart's `previewAuth` is on):

- `maul.preview_auth.unprotected_hosts` — Ready preview hosts that answered the relay's credential-less probe with
  anything but the redirect to the relay for their own slot. Anything above zero is an incident: a preview is served
  without sign-in. `maul.preview_auth.probe.unreachable_hosts` counts hosts that did not answer at all, which is all of
  them while the preview allowlist leaves out the cluster's NAT addresses.
- `maul.preview.ingress.unauthenticated{slot}` — slot Ingresses without their pinned sign-in annotations;
  `maul.preview.ingress.refused{slot}` — Ingress writes the API server refused.
- The relay's request counters and latency are listed in [preview-auth](configuration/preview-auth.md#observability).

**Probes:** `GET /healthz` / `GET /readyz` on every controller's `--health-addr` (`:8081`) feed the Deployments'
liveness and readiness probes.

## Verbosity

`--log-level` / `MAUL_LOG_LEVEL` sets the stderr handler (and the log bridge) level: `debug`, `info`, `warn` (the
default), or `error`. At `debug`, the webhook server's dedup, queue, and signature decisions all log — the first place
to look when deliveries seem to vanish. The dev overlay ships `info`.
