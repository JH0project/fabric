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
