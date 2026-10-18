# Stateless configuration RPC

Configuration RPC is opt-in. The existing Docker observer still runs and shares
the agent's shutdown lifecycle. There is no operation database, queue, revision
history, background repair, or cross-node coordination.

## Enable an agent

Create an operator-owned writable root and configure:

```sh
FABRIC_RPC_ENABLED=true
FABRIC_RPC_LISTEN=127.0.0.1:9781
FABRIC_CONFIGURATION_ROOTS=/data/coolify/applications
FABRIC_RPC_TOKEN=<a-random-secret-of-at-least-32-bytes>
```

The roots are comma-separated absolute existing directories. File parents inside
these roots are created as needed. When the agent is containerized, explicitly
bind-mount these directories at the same absolute paths and grant UID/GID 65532
the necessary permissions. Supplementary Docker socket access does not grant
configuration-directory write permissions.

The RPC defaults to loopback, is disabled without opt-in, and authenticates
every request. For remote clients, terminate HTTPS at an authenticated agent
endpoint/reverse proxy; the agent listener itself uses HTTP. Do not expose an
unprotected plaintext endpoint. Tokens are supplied through the environment,
not command-line arguments.

## Contract

```http
POST /v1/configurations/apply
Authorization: Bearer <token>
Content-Type: application/json
```

```json
{
  "service_id": "application-uuid",
  "files": [{
    "path": "/data/coolify/applications/application-uuid/app/config.json",
    "content": "",
    "mode": "0640",
    "uid": 1000,
    "gid": 1000
  }]
}
```

Content is required; an empty string and "0" are valid. Numeric owner fields and
the octal mode are optional. Modes cannot include setuid/setgid/sticky bits.
Existing modes are preserved when omitted; new files default to 0644. File
ownership is the agent's OS identity unless explicitly requested. Chown requires
appropriate OS privileges; a request cannot grant those privileges.

A successful response has HTTP 200 and verified SHA-256 receipts:

```json
{
  "applied": true,
  "files": [{"path": "/data/coolify/applications/application-uuid/app/config.json",
             "applied": true, "sha256": "<sha256-of-content>"}]
}
```

HTTP 207 returns per-file failures after a partially successful batch. A batch
is not a filesystem transaction. Validation/authentication failures use 400/401;
oversized request bodies use 413. File error codes are invalid_file,
path_conflict, permission_denied, cancelled, timeout, or write_failed. Errors and
telemetry omit file content and authentication secrets.

Limits: 32 files, 5 MiB content per file, 8 MiB encoded request body.
Requests to the same normalized destination are serialized. Cancellation before
publication leaves the old file intact. Symlinks, directories and special-file
destinations are rejected; directories are never recursively deleted.
The writer uses os.Root for confined I/O and a same-directory temporary file,
sync, verification and rename. Atomic publication is intended for Linux.
Configured roots must not contain untrusted bind mounts or special filesystems.

Only the host filesystem is updated. Atomic replacement changes the file inode:
an existing container's individual-file bind mount may continue referencing the
old inode. Coolify must recreate/redeploy the container to refresh that mount;
this RPC neither restarts containers nor reloads applications.

The caller retries failures or resends configuration after files disappear.
A lost response can mean a write completed. Requests are safe to reapply, but
there is no durable deduplication or cross-node all-or-nothing guarantee.

## Fabric-owned templates

Files may specify either literal `content` or a `template`, never both.
Existing literal clients remain compatible. Template fields are:

```json
{
  "path": "/data/coolify/applications/example/keydb.conf",
  "template": {
    "engine": "go-template/v1",
    "source": "{{range .nodes}}{{if ne .id $.node.id}}replicaof {{.ip}} 6379\n{{end}}{{end}}",
    "data": {
      "node": {"id": "one"},
      "nodes": [
        {"id": "one", "ip": "192.0.2.1"},
        {"id": "two", "ip": "192.0.2.2"}
      ]
    }
  }
}
```

Fabric owns all rendering. The caller supplies JSON data, including the complete
node list; Fabric does not filter peers or infer inventory/addresses from the
host. Data is not interpreted recursively as templates. Missing fields fail.
Text output is not HTML-escaped. Ansible/Jinja syntax is not supported.

This first version uses a bounded Go text/template dialect, not the full Helm
function library. It supports field access, variables, if/range/with, break/
continue, and the built-ins and/or/not/eq/ne/lt/le/gt/ge/len. No filesystem,
environment, network, callable values, includes, named templates, printf, or
arbitrary Sprig functions are available. Context consists solely of JSON data.

POST /v1/configurations/render accepts the same authenticated request envelope.
It returns HTTP 200 with rendered=true and per-file rendered/SHA256 receipts,
or HTTP 422 with stable error codes. It does not access destination files or
return rendered contents. Preflight is not a filesystem permission/conflict
check, a reservation, or a cross-node transaction.

A caller should render on every destination first. If any destination fails,
do not apply on any node. Then resend each file to /apply with the returned
expected_sha256. The agent re-renders and rejects digest mismatches before any
file is written. Changing any template input requires a new preflight. Hashes
are per-node because rendered content can differ. Network/filesystem failures
during application can still leave a partially applied fleet.

Within each /apply request, all rendering and digest checks finish before the
first write. Rendering failures prevent every write in that batch; other files
receive not_applied. Ordinary filesystem failures retain the existing per-file
partial-result behavior.

Limits: 64 KiB template source, 5 MiB output per file, 8 MiB total rendered batch,
8192 JSON data items, depth 32, and 100000 instrumented execution steps. Budget
checks cover empty loops and cancellation. Template errors expose no source,
context values or rendered content. New codes are invalid_template,
template_limit, digest_mismatch and not_applied.

## Audit-derived regression cases

The read-only October 18 fleet audit identified three node-specific cases:
KeyDB selects every other node; Prometheus uses node name/region; Lightning
Stream uses a node-specific instance ID. Shared Garage/Thanos/Kratos config
drift is an ownership/synchronization case, not a reason to invent per-node
differences. Newline-only differences must remain byte-exact receipts rather
than being silently normalized by rendering. Templates stored by a controller
must never be replaced by loading one node's rendered file.

### Caller-supplied environment and node lists

Template data accepts env and node lists as JSON. For example:

```json
{
  "node": {"id": "one", "name": "region-a-node-1"},
  "nodes": [
    {"id": "one", "name": "region-a-node-1", "ip": "192.0.2.1"},
    {"id": "two", "name": "region-b-node-1", "ip": "192.0.2.2"}
  ],
  "env": [
    {"key": "REDIS_PORT", "value": "6379"},
    {"key": "CONFIG_VALUE", "value": "caller-supplied value"}
  ]
}
```

The renderer accepts these exactly as supplied. Filtering and selection belong
in the template, e.g. range over .env and compare .key, or range over .nodes and
exclude $.node.id. Fabric does not discover node addresses, fetch environment
variables, expand shell variables, or recursively interpret values as templates.
The caller decides which environment scope is authorized for each destination.
Never put another node's private environment into the public peer list.
