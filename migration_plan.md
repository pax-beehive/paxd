# transport_journal paxkit reliablemq migration plan

## Goal

Move ACP reliable transport storage from paxd-specific raw SQL helpers to
paxkit reliablemq `sqlstore`.

The paxkit `queue_id` is the durable local connection identity used for journal
replay, ACK dedupe, and cleanup. `agent_id` / `cloud_agent_id` remains optional
metadata for routing and diagnostics.

There is no customer data to preserve, so this migration is a hard migration:
old `transport_journal` rows can be dropped.

## Target journal schema

The schema is created by paxkit reliablemq `sqlstore.NewSQLite` with table name
`transport_journal`.

## paxd work

### 1. Raw SQL store migration

- Detect an existing `transport_journal` table without paxkit's `queue_id`.
- Drop the old table only in that incompatible case.
- Create the target table and indexes through paxkit reliablemq `sqlstore`.
- Keep the shared SQLite handle in `internal/store`, but do not keep a paxd
  transport journal repository API.

### 2. Repository API

- Add `Store.DB()` so paxd components can pass the shared SQLite handle to
  paxkit adapters.
- Replace paxd-specific transport journal helpers with paxkit
  `reliablemq.Engine` and `sqlstore.Store`.
- Update unit tests to prove seq and ACK scoping use paxkit `queue_id`, not
  `agent_id`.

### 3. Legacy ACP forwarder compatibility

Until the new `AgentTunnelSession` exists, the legacy `acpforwarder` must still
compile and run.

- Add `ConnectionID` to `acpforwarder.Config`.
- Validate `ConnectionID`.
- Include `connection_id` in tunnel envelopes and ACK envelopes.
- Use `ConnectionID` for all local journal operations.
- Continue passing `agent_id` as remote metadata/query parameter for current
  manager routing.
- In legacy YAML mode, derive `ConnectionID` from the local runtime agent:
  `instance_id`, then `agent_id`.

### 4. Runtime integration

When `AgentTunnelSession` is implemented:

- Use `runtime.AgentConnectionSpec.ConnectionID` directly.
- Stop deriving legacy connection ids from YAML.
- Keep journal interaction behind a narrow connection-scoped journal port.
- Replay outbound `pending/sent` frames on session start.
- Replay inbound `received` but not `applied` frames to ACP stdin.

## pax-manager work

- Accept and persist `connection_id` on ACP tunnel bind/connect.
- Include `connection_id` in manager-to-paxd data envelopes.
- Echo `connection_id` in ACK frames.
- Deduplicate manager-side received paxd frames by
  `connection_id + stream + seq + direction`.
- Keep `agent_id` / `cloud_agent_id` for business routing.

## Devops work

- No customer data backfill is required.
- Existing development databases can be deleted or allowed to hard-migrate.
- If deployments have stale dev databases, ensure paxd restart can drop the
  incompatible old `transport_journal` table automatically.
- No cross-version rolling compatibility is required unless an environment runs
  old pax-manager and new paxd together.

## Non-goals

- Do not migrate `transport_journal` to GORM in this step.
- Do not implement supervisor/runtime replay in this step unless explicitly
  continuing into `AgentTunnelSession`.
- Do not guarantee end-to-end exactly-once delivery through local ACP harness
  process crashes. The journal provides transport-level at-least-once delivery
  with duplicate suppression at the paxd/manager boundary.
