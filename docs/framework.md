# Component lifecycle

Fabric grows through useful vertical slices. The application manages lifecycle;
features own their domain behavior and recoverable connection retries. Bootstrap
constructs clients/stores explicitly and closes them after components have joined.

```go
type Component interface {
    Name() string
    Run(context.Context) error
}
```

`app.New(components...).Run(ctx)` validates unique, nonempty component names and
non-nil registrations before starting anything. It starts the enabled components,
cancels siblings on a fatal failure or unexpected early return, and waits for all
of them before returning. Errors retain component identity and their original
cause. Parent cancellation is a normal shutdown. `Run` must honor cancellation
and join its own child goroutines; the framework cannot safely kill Go goroutines.

There is no dependency graph scheduler, dynamic plugin loader, DI framework, or
automatic restart supervisor. A component owns transient retry/backoff when that
behavior is needed; a fatal bind/configuration/observation error remains fatal.

## Current composition

```text
agentd bootstrap: config, telemetry, Docker client, optional broker client,
                 persistent epoch ownership, file writer and RPC service
    |
    +-> App
          +-> docker-observer       app.Observer
          +-> dns                   discovery.DNS (optional)
          +-> rpc                   rpc.Server (optional)
          +-> inventory-sync        inventory.Agent (optional)

inventoryd bootstrap: config, telemetry, broker client, CRDB store
    |
    +-> App
          +-> inventory-persistence inventory.Persistence

after App joins: close resources -> flush telemetry with fresh bounded context
```

The old Docker-specific `app.App` is now `app.Observer`; `app.App` is the generic
lifecycle owner. Startup no longer has a second custom `runServices` loop.
Docker observation does not own SQL or broker connectivity. DNS always serves a
local projection. The migration command is a bounded explicit operation, not a
daemon component, so `fabric-db -migrate` runs as an explicit operator command.

## Dependency boundaries

- `runtime.Runtime` hides Docker SDK objects from observation logic.
- `inventory.Publisher`, `SnapshotSource`, and `SnapshotTransport` describe
  domain snapshots, subscriptions, and snapshot requests without NATS SDK types.
- `inventory.SnapshotStore` is the persistence worker's data-access boundary.
- Concrete NATS/CRDB clients are constructed at bootstrap and injected; they are
  not registered as components merely because they represent external services.
- RuntimeInstance projection, version checks, tombstones, and DNS eligibility are
  separate from application lifecycle.

The current transport/store adapters remain ordinary Go implementations, not a
universal database/event-stream framework. Future adapters should implement the
existing domain ports when a real task needs them. Historical Oxia examples are
not an instruction to add Oxia or desired-state actuation now.

## Validation

Lifecycle tests exercise fatal failure cancellation, error identity, joined
shutdown, parent cancellation, unexpected early return, registration validation,
and cancelled bootstrap. Existing RPC/DNS/Docker tests protect behavior across
the Observer rename. The secure IPv6 CRDB/NATS integration test runs its agent and
persistence loops through `app.App`, including dependency outages and recovery.

See [RuntimeInstances](runtime-instances.md) for protocol/configuration and
[deployment gates](deployment-runtime-instances.md) before any production rollout.
