# internal/daemonstore BDD test cases

These scenarios define the expected behavior of the GORM-backed daemon/control store.

`daemonstore` owns desired-state tables, status tables, command audit/idempotency, local harness/session cache, local message history, settings, and migrations for the new control-plane model. It does not own the paxkit ACP transport journal.

## Module boundaries

Upstream callers:

- `internal/control` uses this module for desired-state transactions and command idempotency.
- `internal/supervisor` uses this module to list desired runtimes and update observed status.
- `internal/harnessregistry` may use this module to refresh `harness_inventory`.
- `internal/localsessions` may use this module to refresh `local_session` and `local_session_element`.
- daemon/bootstrap code uses this module for migrations and YAML import.

Downstream dependencies:

- SQLite through GORM for target control-plane tables.
- Existing `internal/store` raw SQL store remains separate for legacy daemon state and the shared SQL handle used by paxkit `transport_journal`.

What to mock in `daemonstore` tests:

- Prefer real temporary SQLite databases and real migrations.
- Do not mock GORM for repository behavior tests.
- Mock only external collaborators such as clocks or ID generators if needed.

Who uses `daemonstore` test helpers:

- `control` tests may use an in-memory/test daemonstore implementation or a temporary SQLite daemonstore.
- `supervisor` tests may use daemonstore fixtures to seed desired remotes and agent connections.

Boundary rule:

- If a test asserts SQL schema, migrations, transaction behavior, generation increments, idempotent command records, or conditional status updates, it belongs here.
- If a test asserts business validation or when to wake supervisors, it belongs in `internal/control`.
- If a test asserts runtime start/stop/backoff behavior, it belongs in `internal/supervisor`.

## Migrations

### Scenario: creates target control-plane tables

Given an empty SQLite database  
When daemonstore migrations run  
Then the database contains `remote`, `remote_auth`, `remote_status`, `agent_connection`, `agent_connection_status`, `control_command`, `harness_inventory`, `local_session`, `local_session_element`, `messages`, `message_parts`, and `setting`

### Scenario: migrations are idempotent

Given migrations have already run successfully  
When migrations run again  
Then they complete without error  
And existing rows are preserved

### Scenario: raw SQL transport journal remains outside GORM migration ownership

Given daemonstore migrations run  
When the store inspects migration ownership  
Then daemonstore does not attempt to recreate or rewrite `transport_journal` behavior  
And raw-SQL journal tests remain under `internal/store`

## Remotes

### Scenario: creates a remote

Given no remote exists for `https://api.example.test`  
When the store creates a remote  
Then the remote has generation `1`  
And restart nonce `0`  
And enabled/default fields match the request

### Scenario: allows duplicate cloud API URL

Given a remote already exists for `https://api.example.test`  
When the store creates another remote with the same URL  
Then both remotes are persisted under their distinct IDs

### Scenario: updates runtime-affecting remote fields

Given a remote exists with generation `4`  
When the store updates its cloud API URL or enabled state  
Then generation becomes `5`  
And restart nonce is unchanged  
And updated_at changes

### Scenario: restarts remote

Given a remote exists with restart nonce `2`  
When the store records a remote restart intent  
Then restart nonce becomes `3`  
And generation is unchanged

## Remote auth

### Scenario: upserts remote auth config

Given a remote exists  
When the store upserts Cloudflare Access auth with client id and secret ref  
Then `remote_auth` contains kind `cloudflare_access`  
And `config_json` contains the secret ref  
And `config_json` does not contain a resolved secret value

### Scenario: clears remote auth config

Given a remote has auth config  
When the store clears remote auth  
Then `remote_auth` is removed or set to kind `none` according to repository policy  
And no resolved secret value remains

## Agent connections

### Scenario: creates an agent connection

Given a remote exists  
When the store creates an agent connection under that remote  
Then the connection has generation `1`  
And restart nonce `0`  
And desired state `running` or the requested desired state  
And `(remote_id, name)` is unique

### Scenario: rejects duplicate agent connection name within a remote

Given remote `remote_prod` has agent connection name `codex-main`  
When the store creates another connection named `codex-main` under `remote_prod`  
Then the operation fails  
And the existing connection is unchanged

### Scenario: allows same agent connection name under different remotes

Given remote `remote_prod` has agent connection name `codex-main`  
When the store creates connection name `codex-main` under `remote_local`  
Then the operation succeeds  
And both rows exist

### Scenario: updates runtime-affecting agent connection fields

Given an agent connection exists with generation `8`  
When the store updates command, working directory, harness, tunnel path, env, enabled, or desired state  
Then generation becomes `9`  
And restart nonce is unchanged

### Scenario: restarts agent connection

Given an agent connection exists with restart nonce `6`  
When the store records restart intent  
Then restart nonce becomes `7`  
And generation is unchanged

### Scenario: logically deletes agent connection

Given an enabled agent connection exists  
When the store deletes the connection  
Then desired state becomes `deleted` or enabled becomes false according to repository policy  
And deleted_at is set  
And generation is incremented

## Command idempotency

### Scenario: inserts accepted command record in same transaction as desired-state mutation

Given a mutating command is accepted  
When the desired-state mutation commits  
Then the command record is inserted with status `received`  
And the desired-state row is updated in the same transaction

### Scenario: rolls back command record when desired-state mutation fails

Given a mutating command targets a missing remote  
When the desired-state mutation fails  
Then no received command record is committed  
And no partial desired-state row is written

### Scenario: detects duplicate command id

Given `control_command` contains command id `cmd_1`  
When the store attempts to insert command id `cmd_1` again  
Then the store returns the existing command record or duplicate status according to repository policy  
And it does not apply a second mutation

### Scenario: command audit does not persist resolved secrets

Given a remote auth command includes secret ref `env:PAX_CF_SECRET_PROD`  
And the resolved secret value is `super-secret`  
When the command audit row is stored  
Then `payload_json` contains `env:PAX_CF_SECRET_PROD`  
And does not contain `super-secret`

## Status updates

### Scenario: writes initial remote status

Given a remote exists  
When the remote supervisor writes status `connecting` for generation `1` and restart nonce `0`  
Then `remote_status` has observed generation `1`  
And phase `connecting`

### Scenario: prevents stale remote status overwrite

Given `remote_status` observes generation `3`  
When an old session attempts to write status for generation `2`  
Then the conditional update affects zero rows  
And the generation `3` status remains unchanged

### Scenario: writes initial agent connection status

Given an agent connection exists  
When the agent supervisor writes status `starting` for generation `1` and restart nonce `0`  
Then `agent_connection_status` has observed generation `1`  
And phase `starting`

### Scenario: prevents stale agent connection status overwrite

Given `agent_connection_status` observes restart nonce `4`  
When an old session attempts to write exit status for restart nonce `3`  
Then the conditional update affects zero rows  
And the restart nonce `4` status remains unchanged

### Scenario: records backoff state

Given a transient runtime exit occurs  
When the supervisor writes backoff status  
Then status contains phase `backoff`  
And reconnect attempt  
And next retry timestamp  
And safe error code/message

### Scenario: runtime-discovered binding is generation guarded

Given an agent connection exists at generation `3` without a cloud agent id  
When runtime discovers cloud agent id `agent_123` while observing generation `3`  
Then `cloud_agent_id` is persisted  
And generation remains `3`

Given the desired row advances to generation `4` before an older runtime returns  
When the old runtime tries to persist cloud agent id for observed generation `3`  
Then the update affects zero rows  
And the newer desired row is unchanged

### Scenario: remote registration is generation guarded

Given a remote exists at generation `2`  
When runtime registers the node and observes generation `2`  
Then `node_id` and `registered_at` are persisted  
And generation remains `2`

Given the remote desired row advances to generation `3` before an older runtime returns  
When the old runtime tries to persist registration for observed generation `2`  
Then the update affects zero rows  
And the newer desired row is unchanged

## Harness and local session cache

### Scenario: upserts harness inventory

Given harness discovery returns Codex as available  
When the store upserts harness inventory  
Then `harness_inventory` contains Codex state, capability, command JSON, and timestamps

### Scenario: upserts local sessions

Given local session scan returns Codex sessions  
When the store upserts local sessions  
Then `local_session` contains unique `(agent, native_id)` rows  
And last listed timestamp is updated

### Scenario: replaces local session elements for a sync version

Given a local session has existing cached elements  
When the store completes a new local timeline sync  
Then `local_session_element` reflects the new element set  
And stale elements for that session are not returned

## Settings

### Scenario: upserts setting

Given no setting exists for `daemon.debug_http_addr`  
When the store upserts a JSON value for that key  
Then the setting exists  
And updated_at is set

### Scenario: reads missing setting with default

Given no setting exists for a requested key  
When the caller reads the setting with a default value  
Then the default value is returned  
And no row is created unless explicitly requested
