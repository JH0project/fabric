# Embedded local service DNS

`agentd` can serve an authoritative service zone directly from an immutable local
index. DNS is disabled by default. This milestone discovers only containers on
the local Docker Engine. It has no database, message broker, remote catalog,
forwarder, or recursive resolver.

```text
Docker list + container/network events + periodic refresh
    -> runtime.View -> discovery index -> UDP/TCP DNS
```

Queries never call Docker or wait for observation. Before the first successful
Docker snapshot, queries inside the zone return SERVFAIL. A successful empty
snapshot returns NXDOMAIN for absent services. Each subsequent successful
observation atomically replaces the index; invalid updates retain the last valid
snapshot. An observed stop, unhealthy/starting healthcheck, removal, or invalid
registration withdraws that container's records. Missed Docker events are repaired
by the existing periodic observation (30 seconds by default).

The existing agent lifecycle is unchanged: a Docker list/inspect/watch failure
stops the agent, which also stops DNS. There is no retry supervisor or disk cache.
Later CRDB/NATS synchronization must remain independent of DNS query handling.

## Start

```powershell
$env:FABRIC_NODE_ID = 'node-1'
go run ./control/cmd/agentd -dns-enabled
```

| Flag | Environment/default |
| --- | --- |
| `-dns-enabled` | `FABRIC_DNS_ENABLED=true`; default false |
| `-dns-listen` | `FABRIC_DNS_LISTEN`; default `127.0.0.1:8600` |
| `-dns-zone` | `FABRIC_DNS_ZONE`; default `fabric.internal` |
| `-dns-node` | `FABRIC_NODE_ID`; default lowercase hostname |
| `-dns-ttl` | default `5s`; whole seconds from `1s` through `1m` |

Node and service names must be lowercase DNS labels (letters, digits, internal
hyphens, at most 63 characters). Node name `local` is reserved for the local alias.
The zone may end in a dot and is normalized to lowercase; it is limited to 40
characters to leave room for full-length service, node, and SRV labels.

Both TCP and UDP bind the same address/port. A bind failure exits the agent.
For standard DNS use `-dns-listen 127.0.0.1:53` where the port is available and the
process has permission. In a container, bind to `0.0.0.0:8600` and explicitly publish
both `8600/tcp` and `8600/udp`, or map them to host port 53. The default loopback
bind inside a container is accessible only in its own network namespace.
Configure a host resolver to forward this zone to the agent if ordinary system
lookups should resolve these names. The agent does not alter host/Docker DNS
configuration or firewall rules.

## Register services through Docker labels

```yaml
services:
  api:
    image: your-api-image
    networks: [mesh]
    labels:
      fabric.discovery.service: api
      fabric.discovery.network: mesh
      fabric.discovery.port: "8080"
      fabric.discovery.port-name: http
      fabric.discovery.protocol: tcp
networks:
  mesh:
    name: mesh
```

Use the actual Docker network name, including any Compose project prefix.
Only explicitly labelled running containers are registered. If Docker defines a
healthcheck, its status must be `healthy`; containers without a healthcheck are
eligible as soon as they run. Startup/full refresh inspects labelled running
containers for current health and network details. Network connect/disconnect and
health events trigger inspect, just like lifecycle events.

| Label | Meaning |
| --- | --- |
| `fabric.discovery.service` | Required service DNS label; multiple replicas share it |
| `fabric.discovery.network` | Explicit Docker network to read IPv4/IPv6 addresses from |
| `fabric.discovery.addresses` | Optional comma-separated literal advertised IPs; overrides network selection |
| `fabric.discovery.port` | Optional port 1–65535; enables SRV records |
| `fabric.discovery.port-name` | SRV port label, defaults to service name |
| `fabric.discovery.protocol` | `tcp` (default) or `udp` for SRV |

Provide either a network or advertised addresses. No arbitrary default bridge is
selected. Advertised IPs must be reachable from clients: the agent does not add
routes, publish Docker ports, or translate container/host port numbers. Loopback,
unspecified, multicast, scoped, and link-local addresses are rejected. A malformed
label omits that registration and logs a warning when the invalid set changes.

## Names and replies

For service `api`, node `node-1`, and the default zone:

| Query | Result |
| --- | --- |
| `api.service.fabric.internal` | Local replicas' A / AAAA addresses |
| `api.service.node-1.node.fabric.internal` | Same, explicitly scoped to this node |
| `api.service.local.node.fabric.internal` | Alias for this node's replicas |
| `_http._tcp.api.service.fabric.internal` | SRV records for labelled HTTP ports |
| `_http._tcp.api.service.node-1.node.fabric.internal` | Node-scoped SRV records |

There is no cross-node/global aggregation yet. Queries scoped to another node
return NXDOMAIN. SRV targets use a stable opaque hash of node ID and container ID
under `*.endpoint.fabric.internal`; their A/AAAA records are also resolvable and
included in the SRV additional section. Service names combine addresses from
eligible replicas and deduplicate identical addresses.

Replies are authoritative with recursion unavailable. Out-of-zone names and
non-IN classes are refused. Unknown names return NXDOMAIN; existing names with
no matching record type return NOERROR/NODATA. SOA records support bounded
negative caching. The apex answers SOA; this is a forwarding zone, not a public
delegation or zone-transfer service. UDP respects the client's EDNS size (capped
at 1232 bytes), or 512 bytes without EDNS; oversized responses set TC so clients
can retry over TCP. ANY does not enumerate records. DNSSEC is not implemented.

```sh
dig @127.0.0.1 -p 8600 api.service.fabric.internal AAAA
dig @127.0.0.1 -p 8600 _http._tcp.api.service.fabric.internal SRV
dig @127.0.0.1 -p 8600 api.service.fabric.internal A +tcp
```

The module emits `dns.query` spans and bounded `fabric.dns.queries` and
`fabric.dns.query.duration` metrics. Query names, IPs, container IDs, and arbitrary
labels are excluded from metric dimensions.

## Validation

The tests use simulated Docker HTTP responses and real local UDP/TCP sockets.
They cover health/network mapping, opt-in registration, invalid labels, event
updates, periodic repair, removal, snapshot ownership, bootstrap, DNS response
semantics, SRV targets, truncation/TCP fallback, and service shutdown. A running
Docker Engine is still required for the existing opt-in Docker integration test.
