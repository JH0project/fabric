# JH0project Fabric

Fabric is a Go + Rust + Node/TypeScript monorepo. Only the Go control plane,
`agentd`, exists today; the future Rust L4 data plane (`routerd`) and Node services
will follow the same language-neutral observability contract. No empty language
scaffolds, external state systems, deployment actions, or desired-state
reconciliation are implemented.

## Run

Requires Go 1.26+ and a running Docker Engine accessible to the current user.

```sh
go run ./control/cmd/agentd
go run ./control/cmd/agentd -log-level debug -log-format json
go build -o bin/agentd ./control/cmd/agentd
go test ./...
go test -race ./...
go vet ./...
```

The Docker SDK honors `DOCKER_HOST`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH`,
and `DOCKER_API_VERSION`; otherwise it uses its platform default endpoint and
negotiates the API version. It does not resolve Docker CLI contexts. Set
`DOCKER_HOST` when using a non-default context.

Flags: `-request-timeout` (default `10s`), `-refresh-interval` (default `30s`),
`-log-level` (default `info`), and `-log-format` (`text` by default, or `json`).
Ctrl+C or SIGTERM stops and joins observation, closes Docker, and flushes telemetry
with a fresh five-second context. Docker failures exit nonzero; there is no retry
supervisor. Telemetry transport failures warn without stopping observation.

## Architecture

```text
control/cmd/agentd                 construction, flags, signals, cleanup
control/internal/app              Run(ctx), observations, OTel instrumentation
control/internal/runtime          own Container/Event types, Runtime boundary, View
control/internal/runtime/docker   Docker SDK adapter and type translation
control/internal/observability    native OTel providers, logging bridge, bootstrap
docs/standards/observability.md    shared Go/Rust/Node signal contract

Docker -- full list + event-triggered inspect --> in-memory View
```

The root Go module currently covers `control/`; other language manifests will be
added with real code. `agentd` lists all containers, including stopped ones, then
watches events replayed from before the initial list. Events are invalidation
hints: inspect refreshes current Docker state, or removes a container only when
Docker says not found. A periodic full list repairs missed events from Docker's
bounded event history. This is read-only observation, not reconciliation.

Docker remains the source of truth. The disposable, concurrency-safe view stores
ID, name, image reference, state, and labels. Readers receive copies. It is
internal and eventually consistent; there is no public API or persistence.

## Observability

The [shared standard](docs/standards/observability.md) defines resource identity,
structured logs, operation spans, metric naming/cardinality, propagation, and
error/shutdown handling. Intended signal flow is OTLP to an OTel Collector,
then Loki (logs), Tempo (traces), and Prometheus (metrics). No monitoring
infrastructure is deployed by this repo.

Local execution needs no Collector and makes no telemetry export requests by
default. INFO logs show lifecycle and container state/metadata changes. DEBUG
shows event processing and successful refreshes. Active operation logs include
trace/span IDs. Production console logs can use `-log-format json`.

To send all three signals to an existing Collector from PowerShell:

```powershell
$env:OTEL_EXPORTER_OTLP_ENDPOINT = 'http://localhost:4318'
$env:OTEL_EXPORTER_OTLP_PROTOCOL = 'http/protobuf'
$env:FABRIC_NODE_ID = 'your-stable-node-id'
$env:FABRIC_NODE_REGION = 'your-region'
$env:FABRIC_NODE_ZONE = 'your-zone'
go run ./control/cmd/agentd -log-format json
```

For gRPC use `grpc` and port `4317`. Per-signal endpoint/protocol overrides and
`OTEL_{TRACES,METRICS,LOGS}_EXPORTER=none` are supported. `OTEL_SERVICE_NAME` and
`OTEL_RESOURCE_ATTRIBUTES` override defaults. See the standard for the complete
configuration contract. Build version comes from Go VCS metadata, falls back to
`dev`, or can be supplied with `-ldflags '-X main.version=<release>'`.

## Tests

Fake runtimes and local HTTP simulators verify observations, deadlines, failures,
shutdown, SDK mappings, view ownership, OTLP signals, trace/log correlation,
identity, and bounded metrics. Race tests require a working C compiler on Windows.

The opt-in Docker test creates, starts, stops, and removes its own temporary
container, checking the view and clean shutdown. The image must already exist
locally and provide `sleep`; the test does not pull it.

```powershell
$env:FABRIC_DOCKER_INTEGRATION = '1'
$env:FABRIC_TEST_IMAGE = 'node:24-alpine' # or another local image providing sleep
go test ./control/internal/runtime/docker -run TestDockerIntegration -v
```
