# Direct Coolify environment reads

Managed-file template RPCs can resolve application runtime environment values
directly from Coolify PostgreSQL. Compose generation does not receive an `env`
map and is unchanged. This feature reads existing rows only: no migrations,
configuration writes, container adoption, or deployment execution are added.

The dependencies are explicit:

```text
RPC -> configuration.Service -> environment.Resolver -> environment.Store
                                                       ^
                                                  coolify.Store
                                                       |
                                           database / GORM / pgx
```

`coolify.Store` owns SQL, a consistent read-only snapshot, destination/team checks,
and Laravel ciphertext decoding. `environment.Resolver` owns runtime filtering
and shared-reference substitution, using plain Go data and a store interface.
The template renderer/file writer has no SQL, GORM, or encryption dependency.

## RPC contract

Enable `FABRIC_CONFIGURATION_DB_ENV_ENABLED=true` in Coolify only after every
destination supports this contract. Instead of `template.data.env`, Coolify sends:

```json
{
  "service_id": "application-public-id",
  "files": [{
    "path": "/allowed/application/config.txt",
    "template": {
      "engine": "go-template/v1",
      "source": "{{range .env}}{{.key}}={{.value}}{{end}}",
      "environment": {
        "application_uuid": "application-public-id",
        "server_id": 12,
        "team_id": 7
      },
      "data": {"node": {"id": "12"}, "nodes": [], "application": {"uuid": "application-public-id"}}
    }
  }]
}
```

The application ID must match `service_id`; the server ID must match `data.node.id`
and the configured local Coolify server ID. The adapter checks the application's
project/team, both primary and target servers' teams, and primary/additional
deployment membership. Deleted applications/servers and unsupported destination
types fail closed. Authentication remains the existing bearer-token boundary.

Inline `data.env` and an environment reference cannot be combined. Legacy inline
requests remain supported for staged rollout. A reference on an unconfigured
agent returns `environment_unavailable`; there is no fallback to inline values.

One snapshot is loaded per unique reference per request. Preflight resolves the
environment and returns hashes only. Apply resolves again and verifies the
existing `expected_sha256`: environment drift that changes output prevents the
entire local batch from writing. Coolify still preflights all nodes before any
apply. As before, apply is not a distributed transaction across nodes.

## Agent configuration

The agent opt-in is `FABRIC_COOLIFY_DB_ENV_ENABLED=true`; RPC and configuration
roots must also be enabled. These settings are separate from Fabric's CRDB URL:

| Variable | Meaning |
| --- | --- |
| `FABRIC_COOLIFY_DB_HOST` | TLS identity hostname or certificate-matching IPv6 address, without brackets |
| `FABRIC_COOLIFY_DB_PORT` | PostgreSQL port, default 5432 |
| `FABRIC_COOLIFY_DB_NAME` | Existing Coolify database |
| `FABRIC_COOLIFY_DB_USERNAME` / `FABRIC_COOLIFY_DB_PASSWORD` | Login for the legacy database |
| `FABRIC_COOLIFY_DB_CA` | Mounted trusted CA PEM path |
| `FABRIC_COOLIFY_DB_CERT` / `FABRIC_COOLIFY_DB_KEY` | Optional client certificate/key PEM paths |
| `FABRIC_COOLIFY_SERVER_ID` | This destination's Coolify integer server ID; **not** the Ansible ID or Fabric UUID |
| `FABRIC_COOLIFY_APP_KEY` | Coolify's encryption key, supplied through secret deployment configuration |
| `FABRIC_COOLIFY_APP_PREVIOUS_KEYS` | Optional comma-separated previous encryption keys for rotation |
| `FABRIC_COOLIFY_APP_CIPHER` | Laravel cipher; default AES-256-CBC; AES-128-CBC/AES-128-GCM/AES-256-GCM supported |

Database connections require verified TLS and IPv6. There is no insecure switch.
Saving a DB host/login alone does not enable this feature: configure PostgreSQL
TLS, trusted CA/identity and the encryption key before activation. No secrets,
DSNs, SQL parameters, decrypted values or rendered content are logged. Queries
use a ten-second deadline, bounded row/value sizes and repeatable-read read-only
transactions; the pool also defaults transactions to read-only. No SQL role or
schema is changed by the adapter.

Application variables use Coolify's serialized `encrypt()` format; shared
variables use Eloquent encrypted-string casts. CBC authentication is checked
before decryption, GCM tags are authenticated, and PHP parsing accepts strings
only. No general PHP object deserialization is performed.

Runtime behavior matches the current template caller: exclude preview and
build-only variables, retain Coolify's runtime buildpack allowlist, PHP-style
trim application values, and substitute team/project/environment/target-server
references once. Unknown/missing shared references remain literal. No shell or
Compose escaping, recursive expansion, or remote-secret fetching is introduced.

## Validation

Unit tests cover real Laravel-generated synthetic CBC/GCM payloads, key rotation,
tampering, shared scope resolution, filtering, identity rejection, failed lookup,
request immutability and changed-environment digest rejection before writes.

`TestStoreCurrentCoolifySchema` is opt-in with
`FABRIC_TEST_COOLIFY_DATABASE_URL`. It requires a disposable database named
`fabric_coolify_test`, verified TLS/IPv6, and the current Coolify schema provisioned
externally. It creates synthetic fixtures only, verifies primary/additional and
cross-team membership, resolves all shared scopes and tests read-only enforcement.
It never migrates an external Coolify database. Production connectivity and
activation remain separate rollout work.
