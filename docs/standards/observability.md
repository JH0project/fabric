# Fabric observability standard

This contract applies to current and future Go, Rust, and Node/TypeScript server
executables in the Fabric monorepo. Each language uses its native OpenTelemetry
SDK and idioms. Share signal semantics and OTLP, not a cross-language helper
framework. Browser telemetry will need a separate privacy/transport policy.

## Signal flow and identity

Applications send OTLP to an OpenTelemetry Collector. The intended destinations
are Loki for logs, Tempo for traces, and Prometheus for metrics. Applications
must not depend on backend-specific clients. This repository does not deploy
the Collector or these backends.

All signals MUST share these resource attributes:

| Attribute | Meaning |
| --- | --- |
| `service.name` | Executable identity, currently `agentd` |
| `service.version` | Release or source revision; `dev` when unavailable |
| `service.instance.id` | Unique process instance, changes on restart |
| `deployment.environment.name` | Deployment environment |
| `jh0project.node.id` | Stable operator-assigned node identity |
| `jh0project.node.name` | Human-readable node name |
| `jh0project.node.region` | Operator-assigned region |
| `jh0project.node.zone` | Operator-assigned zone |

Local defaults may use the hostname for node ID/name and `unknown` for
region/zone. Production operators MUST configure stable node identity and real
topology. Do not persist instance identity as node identity. Use standard OTel
semantic conventions where available, `jh0project.*` for project-wide concepts,
and `fabric.*` for Fabric-specific concepts. Do not include secrets or arbitrary
container labels in telemetry.

## Logs and errors

Logs MUST be structured, with a stable message and typed attributes. Pass the
operation context to logging calls. Active trace context MUST be carried in OTLP
log trace/span fields and as `trace_id`/`span_id` in console output. Lifecycle
logs outside a span need no synthetic trace ID.

| Level | Use |
| --- | --- |
| DEBUG | Event mechanics, successful periodic observations |
| INFO | Process lifecycle and meaningful container state/metadata transitions |
| WARN | Recoverable degradation, including unavailable telemetry transport |
| ERROR | An operational failure that causes an operation/process to fail |

Return wrapped errors from lower layers. Log a failure once at the layer that
decides its significance; spans may still record it and set error status.
Expected not-found deletion and intentional cancellation are not errors.
Record actual request timeouts as failures. Do not log a successful refresh at
INFO, and do not create an endless span for an event stream.

## Traces and propagation

Trace units of work with stable names, currently `docker.snapshot` and
`docker.inspect`. Pass `context.Context` throughout Go operations. Use W3C
Trace Context and Baggage propagation at future network boundaries. Container
events have no upstream trace context, so their inspect operation starts a new
trace. Container IDs and event details may be span/log attributes, never span
names. Do not trace trivial conversion functions. Configure sampling with
`OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`; defaults follow the SDK.

## Metrics

Use OTel dotted names and explicit units. Do not encode implementation language
or executable names in shared Fabric measurements. Backend exporters may
translate these to Prometheus names and counter suffixes.

| Instrument | Kind/unit | Attributes |
| --- | --- | --- |
| `fabric.runtime.containers` | Observable gauge, `{container}` | `container.state` |
| `fabric.runtime.events` | Counter, `{event}` | `action` |
| `fabric.runtime.snapshots` | Counter, `{operation}` | `result` |
| `fabric.runtime.snapshot.duration` | Histogram, `s` | `result` |
| `fabric.runtime.inspects` | Counter, `{operation}` | `result` |
| `fabric.runtime.inspect.duration` | Histogram, `s` | `result` |

Results are `success`, `error`, or `cancelled`; expected missing containers are
successful observations. States are `created`, `running`, `paused`,
`restarting`, `removing`, `exited`, `dead`, or `unknown`. Emit zero for absent
states. Event actions are allowlisted lifecycle actions; all `health_status:*`
collapse to `health_status`, all `exec_*` to `exec`, and unknown values to `other`.

Metric dimensions MUST be bounded. Never use container IDs/names, deployment/job
IDs, service UUIDs, arbitrary labels, or error messages as metric attributes.
Resource identity is attached by the SDK; configure backend resource-to-label
mapping selectively rather than copying every resource attribute into labels.

## Bootstrap and shutdown

Bootstrap owns providers, exporter construction, resource identity, global
registration, and shutdown. Feature packages use native OTel APIs. Exporters
MUST use bounded asynchronous queues and SDK transport timeouts. Collector
absence must not prevent Docker observation; failures are reported to console,
without recursively sending exporter errors to the failed log exporter. A
Collector outage can drop telemetry after retries/queue limits; this is not a
durable audit log.

On shutdown, stop/join application work and close its resources first, then
flush all signal providers using a fresh bounded context (currently 5 seconds).
A cancelled application context cannot be reused to flush. Invalid explicit
telemetry configuration fails bootstrap; network unavailability is non-fatal.

## Current Go configuration

Export is opt-in: with no endpoint or explicit exporter settings, console logs
work and no OTLP network requests are made. Set `OTEL_EXPORTER_OTLP_ENDPOINT`
to enable all three signals, or a signal-specific endpoint to enable only that
signal. `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, and `OTEL_LOGS_EXPORTER`
accept `otlp` or `none`. Explicit `otlp` uses the SDK's localhost endpoint if
none is supplied. `OTEL_SDK_DISABLED=true` disables export and span recording.

`OTEL_EXPORTER_OTLP_PROTOCOL` defaults to `http/protobuf` (normally port 4318).
`grpc` is also supported (normally port 4317). Per-signal
`OTEL_EXPORTER_OTLP_{TRACES,METRICS,LOGS}_PROTOCOL` overrides it. SDK exporters
handle standard endpoint overrides, headers, certificates, compression and
timeouts. With HTTP, a global base URL appends `/v1/traces`, `/v1/metrics`, or
`/v1/logs`; signal-specific URLs must include their full signal path.

Use `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` for standard overrides.
`FABRIC_NODE_ID`, `FABRIC_NODE_NAME`, `FABRIC_NODE_REGION`, `FABRIC_NODE_ZONE`,
and `FABRIC_ENVIRONMENT` provide convenient defaults; OTel resource overrides
take precedence. `-log-level` selects debug/info/warn/error; `-log-format`
selects text/json. Production console output SHOULD be JSON.

References: [OTel exporter configuration](https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/),
[OTLP specification](https://opentelemetry.io/docs/specs/otel/protocol/exporter/),
[Go SDK exporters](https://opentelemetry.io/docs/languages/go/exporters/).
