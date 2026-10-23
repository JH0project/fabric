# Stateless Compose generation RPC

`POST /v1/compose/generate` uses the existing Fabric bearer-token authentication.
Enable `FABRIC_RPC_ENABLED=true`, `FABRIC_RPC_TOKEN` (at least 32 bytes), and a
reachable `FABRIC_RPC_LISTEN` endpoint. HTTPS must terminate at the agent proxy
for remote callers. No `FABRIC_CONFIGURATION_ROOTS` are needed for generation;
without roots, file configuration routes return 503 `configuration_disabled`.
The normal agent observer still requires Docker, but this RPC does not use it.

This step does not build, create files, run Compose, deploy containers, or read or
write a database. Coolify still owns the helper, source preparation, environment
resolution, destination selection, file publication and deployment execution.
Repository-provided Compose stacks retain their existing Coolify path.

## Version 1

Example request (all field names are snake_case):

```json
{
  "schema_version": 1,
  "container_name": "app-20261023",
  "service_name": "app-20261023",
  "image": "example/app:commit",
  "network": "coolify",
  "restart": "unless-stopped",
  "labels": ["coolify.managed=true"],
  "aliases": ["my-app"],
  "expose": [8080],
  "limits": {"cpus": 0.5, "mem_limit": "512m"},
  "volumes": ["data:/app/data"],
  "named_volumes": {"data": {"name": "data", "external": false}},
  "custom_options": {},
  "preview": false,
  "consistent_name": false
}
```

Optional fields: `stop_grace_period` (duration string), `healthcheck` and `logging`
(Compose objects), `ports` (list), and `gpu` (one Compose device reservation).
`swarm` contains `replicas`, `cpus`, `memory`, `overrides` (top-level deploy keys),
and `worker_only`. Omit or use null for optional objects. Empty object fields
must be `{}`, not `[]`. `labels`, `aliases` and `volumes` are string lists.
`limits` permits mem_limit, memswap_limit, mem_swappiness, mem_reservation, cpus,
cpu_shares and cpuset; preserve input value types for compatibility.

`service_name` is the application UUID for consistent production naming and the
container name otherwise. Preview mode suppresses host ports and custom options,
and forces Swarm replicas to one. Production custom options are already converted
from Docker run options by Coolify. Their recursive merge semantics, including
scalar collisions and fixed-IP alias behavior, match the existing generator.
This is a compatibility contract, not a general Compose validation API.

HTTP 200 returns `schema_version: 1`, `compose` (YAML string), and `sha256` of its
UTF-8 bytes. Output is deterministic; YAML formatting/key order can differ from
Symfony YAML, while document values and types are preserved. The caller verifies
the receipt before using the content. Rendering does not interpolate `$` values.

401 means unauthenticated, 405 wrong method, 400 invalid JSON/types/unknown fields,
413 request too large, and 422 invalid specification or output limit. Input/output
are bounded to 8 MiB, and generated data to depth 32 / 100000 items. Errors and
telemetry do not contain request content, generated YAML or credentials.

## Coolify opt-in

Set `FABRIC_COMPOSE_ENABLED=true` and configure the deployment server ID in the
existing `FABRIC_NODES_JSON` endpoint/token map. This flag is independent of
`FABRIC_CONFIGURATION_ENABLED`. Generation targets the deployment server even
when the helper builds on a separate build server. RPC failures fail deployment;
there is no silent local fallback. Keep the flag false to use the legacy generator.
No configuration flag is activated by source changes.

## Cross-language compatibility check

In Fabric, run:

```powershell
$env:FABRIC_COMPOSE_TEST_SERVER = 'true'
go test ./control/internal/rpc -run '^TestComposeHTTPIntegrationServer$' -v
```

Copy the printed loopback URL into a separate Coolify test shell:

```powershell
$env:FABRIC_COMPOSE_TEST_ENDPOINT = 'http://127.0.0.1:<printed-port>'
php vendor/bin/pest tests/Unit/FabricComposeTest.php --compact
```

The optional test compares the real HTTP response with Coolify's existing PHP
generator using in-memory model stand-ins, with no database or Docker calls.
The test-only server expires after two minutes. Stop it early by posting to
`/test/shutdown` with bearer token consisting of 32 `a` characters. This route
exists only in the test harness, never in agentd.
