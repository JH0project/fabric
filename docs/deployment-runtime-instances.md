# Framework and RuntimeInstance migration/deployment gates

The node registry is already the first milestone. Follow [database.md](database.md)
and explicitly apply `202611010001_runtime_instances` using `fabric-db -migrate`
before enabling publishers or the consumer. It adds the runtime table,
`node_observations`, and the computed observation view without changing `nodes`.
The secure integration fixture uses these same versioned production migrations.

This is a rollout runbook, not deployment authorization. Keep fleet-specific
addresses, credentials, startup commands, and resource identities in deployment
configuration/private operator records rather than general-purpose images.

This is an additive migration: introduce a dedicated Fabric database and runtime
SQL identity, the version-1 inventory schema, and a dedicated NATS account/users
before feature enablement. Retain existing application schemas, broker accounts
and traffic. No changefeed, JetStream stream/KV/durable consumer, desired-state
controller, or existing-data rewrite is required.

Freeze node UUIDs before creating subject ACLs. In the new NATS account, each node
may publish only its own `snapshot.<uuid>` and the request subject, while peers
subscribe to snapshot broadcasts/requests. The persistence identity publishes
requests and subscribes to snapshots, never publishing instance state. Keep other
accounts isolated, use separate secrets, and validate negative permissions on every
server before enabling publishers. Existing route TLS/replication is unchanged.

## Before release

1. Review the combined framework and observation-only RuntimeInstance changes;
   preserve unrelated changes. Run unit/regression tests, vet, Linux race tests,
   secure IPv6 CRDB/NATS certificate and password authentication/outage tests,
   and static arm64 builds of both binaries.
2. Commit the completed scope unsigned per the user's standing work rule. Record the exact release SHA,
   clean source state, image digest/platform, and rollback revision. Use the
   normal authenticated Coolify pipeline; no ad hoc managed-container replacement.
3. Capture existing application options/env/storage/destination settings through
   supported APIs and inventory. Preserve fixed private IPv6 startup, stop-before-
   start naming, existing RPC settings, ports, mounts, and managed file semantics.
4. Verify stable IPv6 aliases and the effective neighbor-notification startup
   setting. A saved option is not proof it is active in running containers.
5. Verify NATS seed IP SANs/CA, reachable private IPv6 seeds, client authentication
   and subject permissions, and no host-port publication. Broker JetStream may
   stay enabled, but this feature uses only Core NATS.
6. Verify secure CRDB's actual IPv6 SQL listener, certificate identities/CA,
   authentication policy and operator privileges. IPv4 node advertisement is not
   an IPv6 application endpoint. Do not change cluster networking automatically.

## Stage 1: framework-only release

Deploy the reviewed image with inventory disabled on all nodes. Existing DNS and
RPC remain enabled as before. Check actual image IDs, startup/shutdown logs,
Compose/configuration RPC receipts, local DNS, and IPv6 alias readiness on every
node. Consistent container naming means an agent can have a brief stop/start gap.
Normal Coolify multi-destination fan-out is not a serial fleet rollout controller.

## Stage 2: registry and consumer setup

- Create an isolated Fabric database with explicit backup/recovery ownership.
- Use an authorized operator identity only to create the database/role and run
  `fabric-db -migrate`; do not run consumers with operator/admin credentials.
- Verify the schema/version and empty-or-existing node identity mappings before
  provisioning node UUIDs. Reuse valid existing identities, never reset them.
- Grant the runtime identity only the required database/schema access and
  SELECT/INSERT/UPDATE on the inventory tables. Confirm inserts/updates cannot
  mutate dn42/Coolify schemas and that schema changes require the operator.
- Start a separate persistence application from the same pinned image with
  entrypoint `/inventoryd`. Its image-default JSON logging command is supported.
  It needs NATS and secure CRDB configuration, not a Docker socket or node epoch
  volume. Start one consumer initially; duplicate consumers are SQL-idempotent.

## Stage 3: feature canary

- Freeze one stable node UUID per server and provision a durable per-node epoch
  mount. Back it up; never reset/copy epoch state or reuse a node UUID elsewhere.
- Configure scoped CA/auth material and per-node NATS seeds. Existing deployments
  may use TLS/password or certificate auth; keep full server-identity validation.
- Resolve per-server `FABRIC_INVENTORY_ENABLED` to true only on the canary; all
  other nodes stay false. Set the new node/state/auth variables explicitly.
- Expect the normal multi-server application deploy to recreate other agent
  containers even when the feature flag changes on only one node. Do not detach
  destinations or replace containers manually to manufacture a canary.
- Verify full Docker inventory, including stopped/unlabelled containers, maps
  idempotently to RuntimeInstances. Use only temporary owned fixture containers
  for lifecycle checks. Registration must not grant management authority.
- Test stop/start identity, recreation with the same name/new ID, explicit
  removal, completed-inventory absence, and stale-node reporting. Verify DNS and
  RPC keep working and no secret-bearing labels/environment values were stored.

## Stage 4: peer and fleet qualification

Enable a second node in another region; verify full snapshots and peer AAAA/SRV,
explicit global-address reachability, node filtering, A/NODATA, and version-aware
repair. Validate broker/DB outages in the isolated lab; do not stop shared
production dependencies without an authorized maintenance exercise.

After the canary gate passes, expand to the rest of the nodes with explicit
per-node outcomes. Validate duplicate persistence consumers before increasing
consumer replicas. Check SQL inventory counts against complete Docker lists,
staleness, publication/persistence error rates, queue ages, DNS latency, RPC
receipts, effective epoch mounts and IPv6 startup after each deployment.

## Acceptance and rollback

Success requires correct live images/settings, stable UUIDs across agent
recreation, complete current inventory/history, no stale resurrection, functional
peer DNS and existing RPC, and no broadening of ports or management authority.
Readiness is not equivalent to Coolify reporting a container as running.

On canary failure, disable the feature through the supported configuration path,
stop the separate consumer if necessary, and redeploy normally. Restore the last
known good source/image revision if the framework release itself is faulty.
Preserve database/history and epoch volumes; do not drop tables, prune images,
delete volumes, or reset UUIDs as a rollback shortcut. Before a later re-enable,
retain or advance the persisted epoch above accepted watermarks.
