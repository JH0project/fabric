# Fabric agent guidance

## Architecture and incremental scope

Fabric is a polyglot infrastructure project. The original control-plane direction
is to build `agentd` as a small modular runtime that grows through tested vertical
slices. Go is the control plane; Rust and Node/TypeScript enter when there is real
work for them. Do not create empty components or speculative plugin frameworks.

The original lifecycle sketch was refined to:

```go
type Component interface {
    Name() string
    Run(context.Context) error
}
```

The application owns component concurrency, cancellation, error propagation, and
joining shutdown. Bootstrap explicitly constructs dependencies and registers the
enabled components. A component is a long-running loop/server. A Docker/CRDB/NATS
client is a resource/dependency, not automatically a component. A store owns data
access, a watcher turns external observations into events, and a controller owns
reconciliation/business logic. Keep SDK and driver types behind their adapters.

Avoid a DI framework, dynamic plugin loader, dependency-graph scheduler, or
generic restart supervisor until a current executable slice requires one.
Recoverable connection loops own bounded retry/backoff; a fatal component failure
cancels and joins its siblings. Components must honor context cancellation and
join any goroutines they start before returning. Close shared clients only after
their components have stopped. Flush telemetry last with a fresh bounded context.

The user narrowed the first slice to Docker only, superseding the early example
that started with Oxia. That was development sequencing, not a permanent ban on
new features. Current authorized RuntimeInstance work adds observation and
inventory synchronization while preserving these boundaries.

## RuntimeInstance and ownership

- Docker is authoritative for actual local runtime state. Node inventory is an
  observed projection, not desired state or permission to manage containers.
- A RuntimeInstance is one physical incarnation with a Fabric UUID separate from
  its Docker ID. Stop/restart retains it; recreation with a new Docker ID creates
  a new identity. Preserve terminal history.
- Core NATS carries best-effort actual-state updates/snapshots directly from
  agents. A separate persistence consumer writes CRDB. Periodic completed node
  inventories repair missed messages. Neither SQL nor broker calls may block the
  local observation callback or DNS query path.
- Persisted/received older state must not overwrite newer state or resurrect a
  known removal. An incomplete list or unreachable node is not proof of removal.
- Registration and imported metadata never imply ownership/adoption. Preserve
  reserved future Fabric-managed identities and nullable associations.
- Do not add Oxia, desired-state actuation, rollout execution, automatic adoption,
  or container creation/deletion without a specific task authorizing that work.

## Infrastructure, security, and operations

- Database work is nodes first. The production `nodes` model has exactly `uuid`,
  `id`, `region`, and `zone`. Preserve the existing backend UUID and Ansible
  integer ID; do not add generated IDs, names or timestamps implicitly.
- Use GORM through `internal/database` and explicit Gormigrate versions recorded
  in `schema_migrations`. No startup AutoMigrate. The dedicated database is
  `fabric`, so table names need no `fabric_` prefix. Runtime SQL stays a test
  fixture until separately versioned. Keep runtime users separate from operators.

- The new synchronization path uses IPv6-only verified TLS/mTLS. Existing secure
  CRDB runs as `securecockroachdb.service`; verify current version/configuration
  before qualifying production integration.
- Use explicit stable node UUIDs, separate node names, and persistent epoch state.
- Never put credentials, private keys, environment values, or arbitrary
  secret-bearing Docker labels in source, inventory, telemetry, or memory.
- Keep private DN42 startup settings in deployment configuration rather than
  hardcoding private topology into general-purpose images.
- Source edits, commits, pushes, image publication, and production rollout are
  separate operations. A request for a deployment plan does not authorize a
  deployment. Prefer Coolify MCP, then its authenticated normal API when needed.
  Do not operate the Coolify container or PHP/Laravel directly.
- Preserve unrelated working-tree/staged changes and keep Git signing enabled.
- Work directly in `C:\Users\Heihe\Downloads\jh0project-fabric`. Use the task's
  `work/` only for temporary binaries, fixtures, and lab data, not another source
  checkout. Prefer direct file patches in the actual repo.

## Observability and validation

Follow `docs/standards/observability.md`. Use native OTel with language-neutral
signal semantics, structured context-aware logs, bounded metric dimensions, and
operation-level spans. Keep logs on stdout when exporters are unavailable.

Run tests appropriate to the changed behavior. For lifecycle changes, verify
cancellation, fatal failure propagation, early exit, and joined shutdown. For
inventory ordering/persistence, use the opt-in secure IPv6 CRDB/NATS integration
test and Linux race tests. Distinguish simulated Docker coverage, local lab
results, and untested production behavior in the handoff.
