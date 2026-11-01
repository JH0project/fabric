# Observed RuntimeInstances

Production SQL currently stops at the four-column node registry described in
[database.md](database.md). RuntimeInstance SQL below describes the deferred
integration fixture; do not enable the persistence consumer against the nodes-only
database. No runtime tables or observation view are part of the first migration.

This initial registry is observation-only. Docker owns the actual containers;
Fabric assigns resource identities and records what agentd observes. No container
creation, deletion, rollout execution, desired-state controller, Oxia integration,
or automatic adoption is implemented.

```text
Docker list/inspect -> agentd local inventory -> local DNS
                                  |
                                  +-> Core NATS snapshots -> peer DNS projections
                                                       |
                                                       +-> inventoryd -> CRDB registry
```

The feature is opt-in. Existing agentd, local DNS, file configuration RPC, and
stateless Compose RPC behavior remain available without inventory configuration.
agentd has no database connection. `inventoryd` is the separate persistence
subscriber; stopping CRDB must not stop Docker observation, publication, or DNS.

## Identity and lifecycle

Each physical incarnation has a Fabric UUID distinct from its Docker ID. Observed
IDs use UUIDv5 (SHA-1) with the stable node UUID as namespace and the versioned
name `fabric/runtime-instance/v1/docker/<full-container-id>`. Preserve this
algorithm once data is registered. Node UUID and Docker ID, not container name or
process startup, determine identity. Stops, restarts of the same container ID,
agent restarts, and repeated messages retain the UUID. A recreated container ID
produces a different UUID even if its name is unchanged.

The deferred lab-only `fabric_nodes` supplies the initial Node UUID reference and tracks the accepted
publisher epoch, newest revision, newest complete revision, and observation time.
`runtime_instances` contains:

- Fabric UUID, node UUID, nullable service/deployment/rollout UUIDs.
- Runtime type and runtime ID, name, image reference and image ID.
- Allowlisted labels and network name/address attachments.
- Runtime/health state, origin, creation/update/observation timestamps.
- Removal timestamp and epoch/revision metadata.

`(node_id, runtime_type, runtime_id)` is unique. Runtime IDs are nullable only for
future `fabric_managed` preallocations; multiple unbound managed instances can be
created without inventing Docker IDs. Current observations always propose
`origin=observed` and null management associations. Metadata claiming a Fabric
identity is not trusted or adopted. Updates to an already-bound managed row
preserve its Fabric UUID, origin, associations, and creation timestamp.

The schema allows future Fabric-generated records to be allocated before Docker
creation. The trusted binding/label-validation path for that future lifecycle is
not implemented yet. No existing observed identities need to be reassigned.

Removed containers retain their rows and UUIDs with `runtime_state=removed` and
`removed_at`. Only explicit Docker-not-found observations, or absence in a
successful completed full list, can remove them. A failed/partial list is not an
empty server. Unbound future managed records are excluded from absence removal.

`runtime_instance_observations` exposes a computed `observation_stale` flag after
60 seconds without a node observation, or when a row belongs to an older node
epoch. Staleness never fabricates an `exited` or `removed` state. History is
retained; this milestone does not automatically prune it.

## Transport, ordering, and repair

The portable snapshot envelope has schema version, node UUID/name, epoch,
revision, observation time, completeness, instances, and explicit removed IDs.
An event-triggered publication includes the current known inventory but is not
authorized to remove merely absent rows. A complete publication follows a full
successful Docker list. Periodic Docker refresh (30 seconds by default) repairs
missed events and produces complete node inventories.

Each publisher holds an OS lock on its node's persistent state directory for its
lifetime. Startup atomically increments and syncs the epoch file before publishing.
The directory must survive agent/container recreation. Do not share one node's
state directory with another node, run independent copies under the same UUID,
delete/reset the epoch file, or clone it into another server. Restore lost state
from backup, or explicitly recover an epoch above the persisted/known watermark;
silently resetting the epoch is unsafe. A corrupt file fails closed.

Version comparison is `(epoch, revision)`. Persistence uses serializable
transactions with bounded SQLSTATE 40001 retries. Duplicate/older messages do not
replace newer state. A delayed complete inventory can still reconcile older rows
at or below its revision without deleting or overwriting newer observations.
Peer views retain deletion versions until a complete watermark makes them safe
to discard, preventing late snapshots from resurrecting removed endpoints.
Persistence also creates a terminal tombstone when a removal arrives before the
first upsert, preserving that protection even when the create message was lost.

Core NATS subjects, using the configured cluster name, are:

```text
fabric.inventory.v1.<cluster>.snapshot.<node-uuid>
fabric.inventory.v1.<cluster>.request
```

Every peer and persistence subscriber receives snapshots independently; there is
no queue group. Agents and consumers request source snapshots at startup,
reconnect, and every 15 seconds. Publishers retain their latest complete and
latest partial inventories, retry unavailable transport, and answer requests.
These are best-effort Core messages: a flush confirms transport acceptance, not
durable storage or exactly-once processing. Periodic complete lists repair lost
updates and removals. JetStream and CRDB changefeeds are not required.

Persistence holds bounded coalescing per-node mailboxes, retaining a pending
complete snapshot separately from a newer partial update. During a DB outage the
newest inventory remains pending. Multiple persistence subscribers may safely
write the same registry because SQL version checks and uniqueness are idempotent.
This release bootstraps peer DNS from live NATS source snapshots; it does not
expose a database-backed peer-bootstrap/query API yet. Last-known durable state
is inspectable directly through SQL.

## Security and IPv6

Transport is IPv6-only and TLS-verified. CRDB requires `sslmode=verify-full`, an
explicit CA path, and either a password or a client certificate/key pair. NATS
requires `tls://` seed URLs and a CA, with either `FABRIC_NATS_USERNAME` /
`FABRIC_NATS_PASSWORD` or a client certificate/key pair. Both dialers use `tcp6`, including discovered NATS
peers. IPv4 and IPv4-mapped endpoint URLs are rejected. Use IPv6 SANs or independent
AAAA-resolved SAN hostnames; do not depend on Fabric DNS to bootstrap NATS/CRDB.

Broker authorization must restrict publishers to their own node UUID subject and
permit the required subscriptions/request subject. TLS alone does not establish
which node identity a trusted client is authorized to publish. Keep the cluster
private, use scoped identities, and supply additional reachable seed URLs if
cold-start failover beyond a local Docker alias is needed.

Inventory preserves factual Docker network metadata, including observed IPv4
addresses, but synchronized DNS publishes only native IPv6. Remote DNS records
require `fabric.discovery.scope=global`; optional
`fabric.discovery.global-addresses` overrides the advertised addresses. Other
existing discovery labels retain their meaning. The operator must verify that
advertised addresses and ports are reachable between nodes.

Remote DNS data expires after 60 seconds from the source observation. DNS TTLs
are capped to its remaining validity. A local observation supersedes stale
received copies of the same node. Inventory-enabled DNS uses IPv6-only sockets;
existing names answer AAAA/SRV and return NODATA for A queries. The unchanged
DNS-only mode keeps its existing IPv4 compatibility.

The inventory label allowlist includes discovery settings, Compose project/service
identity, and Coolify application identity. Raw environment values, arbitrary
labels, injected management UUIDs, credentials, and Docker inspect documents are
not exported. Metadata limits are explicit: 2048 instances per message, at most
1 MiB JSON, 256 remote nodes/pending node mailboxes, and bounded peer tombstones.
Large inventories require protocol chunking in a future revision; they must not
be silently persisted as authoritative partial inventories.

## Configure and run

Allocate and persist a distinct `FABRIC_NODE_UUID` per server. `FABRIC_NODE_ID`
remains the stable DNS/telemetry node name and must be a lowercase DNS label.
The UUID and name are separate identities.

```sh
export FABRIC_NODE_UUID='<stable-node-uuid>'
export FABRIC_NODE_ID='node-one'
export FABRIC_INVENTORY_STATE_DIR='/var/lib/fabric/inventory'
export FABRIC_INVENTORY_CLUSTER='fabric'
export FABRIC_NATS_URLS='tls://[<private-ipv6>]:4222'
export FABRIC_NATS_CA='/certs/nats/ca.crt'
export FABRIC_NATS_CERT='/certs/nats/client.crt'
export FABRIC_NATS_KEY='/certs/nats/client.key'
go run ./control/cmd/agentd -inventory-enabled -dns-enabled -dns-listen '[::]:8600'
```

For a server configured for TLS/password clients, omit the optional NATS client
certificate/key variables and supply the username/password through protected
runtime environment configuration. Keep passwords out of URLs/arguments and
source. For password-authenticated CRDB, use its protected environment URL with
`sslmode=verify-full&sslrootcert=...` and omit `sslcert`/`sslkey`. Construct that URL
securely with proper percent-encoding; do not paste secrets into shell history.

Provision a separate Fabric database and least-privilege SQL identity on the
existing secure CockroachDB cluster. Run migrations explicitly with appropriate
operator credentials, then run the consumer using a scoped identity with
SELECT/INSERT/UPDATE on `fabric_nodes` and `runtime_instances` (plus normal
database/schema access). No runtime container authority or dn42 schema access is
needed. A compatible Node name/UUID must not be reused for an unrelated server.

```sh
export FABRIC_INVENTORY_DATABASE_URL='postgresql://fabric_inventory@[<private-ipv6>]:26257/fabric?sslmode=verify-full&sslrootcert=/certs/crdb/ca.crt&sslcert=/certs/crdb/client.fabric_inventory.crt&sslkey=/certs/crdb/client.fabric_inventory.key'
go run ./control/cmd/fabric-db -migrate   # nodes-only operator setup; runtime SQL deferred
go run ./control/cmd/inventoryd            # normal scoped consumer
```

The agent image also includes `/inventoryd`; select it with an explicit
entrypoint for a separately configured consumer. No default image startup or
existing deployment is changed by this source implementation. Mount the node
state directory persistently and grant the configured agent UID write access.
The consumer does not require a Docker socket. Keep private keys read-only and
out of images, arguments, and logs.

The application lifecycle is shared through [named components](framework.md).
See the [deployment runbook](deployment-runtime-instances.md) for staged enablement,
credential/epoch provisioning, verification and rollback gates.

Publication/persistence emit native `inventory.publish`/`inventory.persist` spans
and `fabric.inventory.{publish,persist}.{operations,duration}` metrics with only
bounded `result` dimensions.

## Validation

```sh
go test ./...
go vet ./...
go test -race ./...

FABRIC_INVENTORY_LAB=1 \
FABRIC_LAB_CRDB_BINARY=/path/to/cockroach \
FABRIC_LAB_NATS_BINARY=/path/to/nats-server \
go test -race ./control/internal/inventory -run TestSecureInventoryIntegration -v -timeout=240s
```

The opt-in integration fixture starts its own secure IPv6 database and TLS/mTLS
broker on local ephemeral ports and stops only those processes. It creates
temporary certificates/data, never connects to production, and exercises schema
idempotence, duplicate delivery, lifecycle/history, delayed full reconciliation,
managed-record preservation, wrong-CA rejection, real peer DNS, DB outage/restart,
broker outage/reconnect, and missed-removal repair. It needs enough SQL memory for
CRDB schema-change jobs; the fixture uses 256 MiB SQL memory.

Production ACLs, existing cluster certificates, six-host partitions, very large
inventories, power-loss/epoch-volume recovery, and a production rollout remain
separate qualification work. Passing a two-agent local lab does not prove those
conditions. No GitOps desired-state reconciliation or lifecycle management is
enabled by this feature.
