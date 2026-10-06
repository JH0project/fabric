# JH0project Fabric

Fabric will eventually contain a Go control plane (`agentd`) and a Rust L4
data plane (`routerd`). This repository currently implements only a read-only,
local Docker observation slice in Go. There are no external services, deployment
actions, desired-state reconciliation, or data-plane scaffolds.

## Run

Requires Go 1.24+ and a running Docker Engine accessible to the current user.

```sh
go run ./control/cmd/agentd
go build -o bin/agentd ./control/cmd/agentd
go test ./...
go test -race ./...
go vet ./...
```

The Docker SDK honors `DOCKER_HOST`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH`,
and `DOCKER_API_VERSION`; otherwise it uses its platform default endpoint and
negotiates the API version. It does **not** resolve Docker CLI contexts. When
using a non-default context, set `DOCKER_HOST` to that context's Engine endpoint.

Flags: `-request-timeout` (default `10s`) bounds list/inspect calls;
`-refresh-interval` (default `30s`) controls full observations. Both must be
positive. Ctrl+C or SIGTERM cancels the process, joins the event reader, and
closes the Docker client. Startup/request/stream failures exit nonzero; restart
the process after restoring Docker connectivity. There is no retry supervisor.

## Architecture

```text
control/cmd/agentd                 explicit construction, flags, signals, cleanup
control/internal/app              Run(ctx), snapshot/event observation lifecycle
control/internal/runtime          own Container/Event types, Runtime boundary, View
control/internal/runtime/docker   Docker SDK adapter and type translation

Docker -- full list + event-triggered inspect --> in-memory View
```

`agentd` lists all containers, including stopped ones, then watches container
events replayed from the timestamp immediately before the initial list. Every
event is an invalidation hint: the agent inspects the affected container and
upserts current Docker state, or removes it only if Docker returns not found.
It does not infer current state from event action names, which may be delayed.

Docker remains the source of truth. The view is a disposable, concurrency-safe
map of ID, name, image reference, state, and labels; readers receive sorted copies.
It is eventually consistent, not an atomic Docker transaction. Docker retains
only a bounded event history, and clocks can differ, so periodic full lists repair
missed events and remove stale entries. These are read-only observations, not a
desired-state controller. The view is internal; there is no public API or persistence.

Tests use fake runtimes and a local HTTP Engine simulator. They cover bootstrap,
event-driven updates/deletion, delayed event hints, snapshot repair, request
deadlines, stream failures, shutdown/join, SDK mapping, and copy/concurrency safety.
An opt-in real-daemon smoke test creates, starts, stops, and removes its own
temporary container, verifying the view and clean shutdown. Set
`FABRIC_DOCKER_INTEGRATION=1` and optionally `FABRIC_TEST_IMAGE` (default
`alpine:latest`) before running `go test ./control/internal/runtime/docker
-run TestDockerIntegration -v`. The image must already exist locally and provide
`sleep`; the test does not pull it.

Docker event semantics: https://docs.docker.com/reference/cli/docker/system/events/
