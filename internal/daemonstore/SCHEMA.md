# paxd daemon store schema

This schema is the target SQLite model for the new paxd daemon/control architecture.

SQLite is the durable source of truth for desired state and observable runtime state. GORM should own the tables in this document, except where explicitly noted.

## Core Model

```text
remote                   desired Pax manager endpoints; one enabled remote owns one node-control WebSocket
remote_auth              optional access-gateway auth config for a remote
remote_status            observed node-control WebSocket status
agent_connection         desired per-agent ACP tunnel connections
agent_connection_status  observed ACP tunnel status
control_command          idempotency and audit for mutating desired-state commands
harness_inventory        local harness discovery cache
local_session            local-only session metadata cache for TUI/CLI
local_session_element    optional local-only timeline cache
messages                 local message history
message_parts            local message history parts
setting                  small daemon key-value settings
```

Existing raw-SQL tables that remain outside GORM:

```text
transport_journal        reliable ACP frame queue
```

Legacy tables that should not be source of truth in the target model:

```text
node_state        replaced by remote
agent_state       remove
hermes_instances  remove with Hermes HTTP model
cloud_agents      replaced by agent_connection
orphaned_messages remove with old Hermes/mailbox executor path
```

## remote

`remote.enabled = true` means paxd should maintain one node-control WebSocket for that remote.

```text
id TEXT PRIMARY KEY
name TEXT NOT NULL
cloud_api_url TEXT NOT NULL
node_control_path TEXT NOT NULL DEFAULT '/api/v1/node/control'
agent_tunnel_path TEXT NOT NULL DEFAULT '/api/v1/agent/tunnel'
node_id TEXT
cloud_api_key_ref TEXT
enabled INTEGER NOT NULL DEFAULT 1
is_default INTEGER NOT NULL DEFAULT 0
generation INTEGER NOT NULL DEFAULT 1
restart_nonce INTEGER NOT NULL DEFAULT 0
registered_at TEXT
created_at TEXT NOT NULL
updated_at TEXT NOT NULL
UNIQUE(cloud_api_url)
```

Notes:

- `cloud_api_url` defines the Pax manager endpoint.
- `node_control_path` defines the manager WebSocket path for remote node-control.
- `agent_tunnel_path` defines the manager WebSocket path for agent ACP tunnels under this remote.
- `node_id` is the Pax manager node identity for that remote.
- `cloud_api_key_ref` is a secret ref for the Pax node key used to authenticate outbound manager calls.
- Cloudflare Access and future gateway credentials do not live in this table.
- Mutating runtime-affecting fields increments `generation`.
- Manual restart increments `restart_nonce`.
- Runtime registration may update `node_id` and `registered_at` through a generation-guarded update and must not increment `generation`.

## remote_auth

`remote_auth` stores optional access-gateway auth configuration for a remote.

```text
remote_id TEXT PRIMARY KEY
kind TEXT NOT NULL                  -- none | cloudflare_access
config_json TEXT NOT NULL DEFAULT '{}'
created_at TEXT NOT NULL
updated_at TEXT NOT NULL
```

Cloudflare Access example:

```json
{
  "cloudflareAccess": {
    "clientId": "xxx",
    "clientSecretRef": "env:PAX_CF_SECRET_PROD"
  }
}
```

Secret ref schemes for the first version:

```text
env:NAME
file:/absolute/path
inline:value        -- dev/debug only; not recommended
```

Rules:

- Store secret refs, not resolved secrets, whenever possible.
- `inline:` is allowed only as a dev/debug escape hatch.
- Resolved secrets must not be logged, returned by debug APIs, shown in TUI, or persisted in command audit results.

## remote_status

Observed status for one remote node-control WebSocket.

```text
remote_id TEXT PRIMARY KEY
observed_generation INTEGER NOT NULL DEFAULT 0
observed_restart_nonce INTEGER NOT NULL DEFAULT 0
phase TEXT NOT NULL                 -- stopped | connecting | connected | backoff | failed
last_error_code TEXT NOT NULL DEFAULT ''
last_error_message TEXT NOT NULL DEFAULT ''
failure_class TEXT NOT NULL DEFAULT ''
reconnect_attempt INTEGER NOT NULL DEFAULT 0
next_retry_at TEXT
connected_at TEXT
stopped_at TEXT
updated_at TEXT NOT NULL
```

Rules:

- Status writes must be conditional on `observed_generation` and `observed_restart_nonce` when they come from a session exit.
- Stale session exits must not overwrite newer observed state.
- `phase=backoff` must include `next_retry_at` and `reconnect_attempt`.

## agent_connection

Each enabled row maps to one desired ACP tunnel WebSocket.
`id` is generated and owned by paxd. It is the durable local
`connection_id` used by supervisor slots, runtime status, and transport
journaling. `cloud_agent_id` is pax-manager-owned remote metadata and may be
missing until runtime registration/binding succeeds.

```text
id TEXT PRIMARY KEY
remote_id TEXT NOT NULL
name TEXT NOT NULL
cloud_agent_id TEXT
instance_id TEXT NOT NULL
agent_type TEXT NOT NULL
harness TEXT NOT NULL
command_json TEXT NOT NULL
working_dir TEXT NOT NULL DEFAULT ''
env_json TEXT NOT NULL DEFAULT '{}'
enabled INTEGER NOT NULL DEFAULT 1
desired_state TEXT NOT NULL         -- running | stopped | deleted
generation INTEGER NOT NULL DEFAULT 1
restart_nonce INTEGER NOT NULL DEFAULT 0
created_at TEXT NOT NULL
updated_at TEXT NOT NULL
deleted_at TEXT
UNIQUE(remote_id, name)
UNIQUE(remote_id, cloud_agent_id)
```

Rules:

- `id` must remain stable across reconnects, restarts, renames, and cloud agent
  rebinding.
- `remote_id` decides which remote/node credentials and auth headers are used.
- Agent connections must not store Cloudflare Access credentials.
- `cloud_agent_id` may be filled later by runtime registration through a generation-guarded update and must not increment `generation`.
- `command_json` is a JSON string array.
- `env_json` stores non-secret runtime environment configuration. Do not store secrets here unless they are explicit dev-only refs.
- `desired_state=deleted` should be treated as logically deleted. Runtime must stop; physical cleanup can happen later.

## agent_connection_status

Observed status for one ACP tunnel runtime.

```text
connection_id TEXT PRIMARY KEY
observed_generation INTEGER NOT NULL DEFAULT 0
observed_restart_nonce INTEGER NOT NULL DEFAULT 0
phase TEXT NOT NULL                 -- stopped | starting | running | stopping | backoff | failed
pid INTEGER
last_error_code TEXT NOT NULL DEFAULT ''
last_error_message TEXT NOT NULL DEFAULT ''
failure_class TEXT NOT NULL DEFAULT ''
reconnect_attempt INTEGER NOT NULL DEFAULT 0
next_retry_at TEXT
started_at TEXT
connected_at TEXT
stopped_at TEXT
updated_at TEXT NOT NULL
details_json TEXT NOT NULL DEFAULT '{}'
```

Rules:

- `phase=running` means the ACP tunnel WebSocket is connected and the local ACP process is started.
- `phase=backoff` means a transient exit occurred and retry is scheduled.
- `phase=failed` means retry is blocked until configuration, credentials, generation, or restart nonce changes.
- `details_json` can include non-secret diagnostics such as remote phase or process exit code.

## control_command

Only mutating desired-state commands are stored here. Read/query/cache-refresh operations such as status reads, `harness.discover`, and local session sync do not need command ACK semantics.

```text
command_id TEXT PRIMARY KEY
source TEXT NOT NULL                -- local | remote
type TEXT NOT NULL
target_type TEXT NOT NULL DEFAULT ''
target_id TEXT NOT NULL DEFAULT ''
payload_json TEXT NOT NULL DEFAULT '{}'
status TEXT NOT NULL                -- unknown | received | rejected | applied | failed
desired_generation INTEGER
error_code TEXT NOT NULL DEFAULT ''
error_message TEXT NOT NULL DEFAULT ''
result_json TEXT NOT NULL DEFAULT '{}'
received_at TEXT NOT NULL
applied_at TEXT
updated_at TEXT NOT NULL
```

Rules:

- Synchronous command success means desired state and command record were durably committed.
- Durably committed desired-state mutations use status `received`.
- Validation/business rejections use status `rejected`; processing failures use status `failed`; runtime completion may later use `applied`.
- Runtime completion is observed later through status polling or best-effort command results.
- `payload_json` and `result_json` must not contain resolved secrets.
- Duplicate `command_id` should return the existing command state/result.

## harness_inventory

Local harness discovery cache.

```text
harness TEXT PRIMARY KEY
display_name TEXT NOT NULL
state TEXT NOT NULL                 -- available | missing | degraded
capability TEXT NOT NULL DEFAULT '' -- acp | local-log | gateway
command_json TEXT NOT NULL DEFAULT '[]'
version TEXT NOT NULL DEFAULT ''
source TEXT NOT NULL DEFAULT ''     -- native | adapter | npm | local
install_hint TEXT NOT NULL DEFAULT ''
last_error TEXT NOT NULL DEFAULT ''
discovered_at TEXT NOT NULL
updated_at TEXT NOT NULL
```

Rules:

- Discovery cache refresh is not a desired-state command.
- Discovery must not silently install adapters.
- Discovery must not automatically adopt harnesses into `agent_connection`.

## local_session

Local observer cache for TUI and CLI.

```text
id TEXT PRIMARY KEY                 -- codex:sess_xxx
agent TEXT NOT NULL                 -- codex | claude-code | gemini
native_id TEXT NOT NULL
title TEXT NOT NULL DEFAULT ''
status TEXT NOT NULL DEFAULT ''
preview TEXT NOT NULL DEFAULT ''
project_id TEXT NOT NULL DEFAULT ''
updated_at TEXT
last_active TEXT
last_listed_at TEXT NOT NULL
last_synced_at TEXT
metadata_json TEXT NOT NULL DEFAULT '{}'
UNIQUE(agent, native_id)
```

Rules:

- This is pure local observation state.
- Do not model local observation as `remote=localhost`.
- TUI should read local session data through the local control API, not directly from SQLite.

## local_session_element

Optional local timeline cache for agents that support cheap local history extraction.

```text
id INTEGER PRIMARY KEY AUTOINCREMENT
session_id TEXT NOT NULL
seq INTEGER NOT NULL
kind TEXT NOT NULL
role TEXT NOT NULL DEFAULT ''
text TEXT NOT NULL DEFAULT ''
raw_json TEXT NOT NULL DEFAULT '{}'
started_at TEXT
completed_at TEXT
UNIQUE(session_id, seq)
```

Rules:

- This table is optional for agents that can expose local timeline data.
- It does not participate in supervisor decisions.

## messages

Local message history projected from ACP traffic.

```text
id INTEGER PRIMARY KEY AUTOINCREMENT
message_id TEXT UNIQUE NOT NULL
agent_id TEXT NOT NULL
session_id TEXT
source TEXT NOT NULL
direction TEXT NOT NULL
role TEXT
status TEXT
message_type TEXT
parent_message_id TEXT
turn_id TEXT
response_id TEXT
logical_key TEXT UNIQUE
raw_json TEXT
created_at TEXT NOT NULL
updated_at TEXT NOT NULL
```

Rules:

- Message history is business projection data, not reliable transport state.
- Projection failures can be repaired from reliable frame storage when needed.
- This table does not participate in supervisor decisions.

## message_parts

Message parts store text, raw JSON, and future artifact references. Streaming
deltas append to a text part instead of creating one row per token.

```text
id INTEGER PRIMARY KEY AUTOINCREMENT
message_id TEXT NOT NULL
part_index INTEGER NOT NULL
part_type TEXT NOT NULL
text TEXT
payload_json TEXT
artifact_uri TEXT
created_at TEXT NOT NULL
updated_at TEXT NOT NULL
UNIQUE(message_id, part_index)
```

## setting

Small daemon settings use key-value storage to avoid schema churn.

```text
key TEXT PRIMARY KEY
value_json TEXT NOT NULL
updated_at TEXT NOT NULL
```

Examples:

```text
daemon.control_socket_path
daemon.debug_http_addr
daemon.status_interval
supervisor.reconcile_interval
```

## transport_journal

`transport_journal` is owned by paxkit reliablemq `sqlstore`, not by
daemonstore GORM models. paxd opens the shared SQLite handle and passes it to
paxkit with table name `transport_journal`.

Rules:

- `connection_id` is paxd-owned and is the local owner for replay, duplicate
  suppression, supervisor association, and cleanup.
- `agent_id` / `cloud_agent_id` remains remote metadata because the
  manager-side protocol still identifies cloud agents for routing.
- This table should remain under the existing `database/sql` store.
