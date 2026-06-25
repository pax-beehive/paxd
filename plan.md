# paxd Status And Liveness Plan

The previous daemon/client split plan is archived at:

```text
archive/daemon-client-split-plan.md
```

This plan records the status and liveness direction for the new paxd runtime.

## Decision

The long-term source of truth for node liveness is the authenticated
node-control WebSocket as observed by pax-manager.

```text
pax-manager receives fresh node-control lease evidence
  -> node is online

pax-manager has not received lease evidence past TTL
  -> node is degraded or offline
```

HTTP `POST /api/v1/node/status` is not the long-term authority. It may remain
only as a migration bridge while pax-manager and pax-console still depend on
`nodes.last_heartbeat`, `agents.last_heartbeat`, and legacy `status` fields.

Paxd may report what it observes locally, but pax-manager owns the effective
online/offline decision. Paxd must not try to report itself offline as an
authoritative fact, because a disconnected paxd cannot reliably send that fact.

## Status Layers

Keep these meanings separate:

- Node liveness: whether pax-manager has recently received authenticated
  node-control lease evidence for a node.
- Control-channel health: whether paxd's node-control WebSocket is connected,
  backing off, failed, reconnecting, or stopped.
- Agent runtime health: whether an `agent_connection` is starting, running,
  backing off, failed, stopping, or stopped.
- Workload/session status: whether a particular session or run is idle,
  running, waiting for approval, done, or failed.

This plan covers node liveness and agent runtime projection. Workload/session
status should use a separate contract unless a later design explicitly folds it
into runtime snapshots.

## Current Gap

Current pax-manager node-control handling authenticates and reads frames, but
does not process a control protocol or refresh node liveness.

Current paxd runtime has:

- transport ping/pong heartbeat in `internal/runtime/heartbeat.go`
- runtime phase events through `runtime.SessionEventSink`
- supervisor writes into local SQLite status tables

But production wiring does not yet push node lease reports or remote-scoped
runtime snapshots to pax-manager over the node-control WebSocket.

## Protocol Shape

Node-control WebSocket supports bidirectional control traffic:

Paxd to manager:

```text
report
command_result
```

Manager to paxd:

```text
query
command
```

`status.get` is a query type, not a top-level frame kind.

Keep top-level `kind` values as broad message families. Status concepts belong
inside typed payloads, not in new top-level frame kinds.

All paxd-originated report frames use this envelope:

```json
{
  "kind": "report",
  "version": 1,
  "report_id": "rpt_01J...",
  "report": {
    "type": "heartbeat",
    "remote_id": "remote_prod",
    "node_id": "node_123",
    "sent_at": "2026-06-24T12:00:00Z",
    "heartbeat": {}
  }
}
```

Envelope fields:

```text
kind       broad frame family, always "report" here
version    report schema version, starts at 1
report_id  paxd-generated unique id for logs and idempotency diagnostics
report     typed report payload
```

Report metadata fields:

```text
type       heartbeat | runtime.snapshot
remote_id  local paxd remote id that owns this node-control session
node_id    pax-manager node id for the authenticated node
sent_at    paxd clock, diagnostic only
```

`sent_at` must not be used to refresh node liveness. Pax-manager refreshes
leases using server receive time.

## Report Types

V1 report types:

```text
heartbeat
runtime.snapshot
```

Future report types:

```text
runtime.delta
```

Do not implement `runtime.delta` in the first version. Snapshots are simpler
and avoid ordering, gap detection, and merge complexity. Add deltas only after
snapshot size or frequency is proven to be a problem.

### heartbeat

Heartbeat report:

```json
{
  "kind": "report",
  "version": 1,
  "report_id": "rpt_01J...",
  "report": {
    "type": "heartbeat",
    "remote_id": "remote_prod",
    "node_id": "node_123",
    "sent_at": "2026-06-24T12:00:00Z",
    "heartbeat": {}
  }
}
```

Heartbeat semantics:

- It is a normal WebSocket text frame.
- It is business lease evidence, not WebSocket ping/pong.
- It contains no client-selected TTL.
- Pax-manager records lease freshness using the time the frame is accepted.
- Pax-manager may also accept `runtime.snapshot` as lease evidence, because it
  proves the authenticated node-control channel is alive.

Example server-side lease policy:

```text
received within 30s
  -> online

received within 5m
  -> degraded

otherwise
  -> offline
```

Heartbeat interval must be shorter than the online TTL. For example, send every
10s when the online TTL is 30s.

### runtime.snapshot

Runtime snapshots report paxd's last-known runtime state for the current
`remote_id`. They are cloud projection payloads, not local daemon status dumps.

Do not wrap or reuse `control.DaemonStatus` as the wire payload. That type is a
local overview and may include other remotes, harness inventory, local session
summary, or fields that are not safe or useful for pax-manager.

Snapshot report:

```json
{
  "kind": "report",
  "version": 1,
  "report_id": "rpt_01J...",
  "report": {
    "type": "runtime.snapshot",
    "remote_id": "remote_prod",
    "node_id": "node_123",
    "sent_at": "2026-06-24T12:00:03Z",
    "runtime_snapshot": {
      "snapshot_id": "snap_01J...",
      "host": {
        "cpu_percent": 21.4,
        "memory_percent": 63.2,
        "uptime_seconds": 80422,
        "collected_at": "2026-06-24T12:00:02Z"
      },
      "agents": [
        {
          "connection_id": "conn_codex",
          "cloud_agent_id": "agent_123",
          "remote_id": "remote_prod",
          "node_id": "node_123",
          "name": "work",
          "agent_type": "codex",
          "desired_state": "running",
          "runtime_phase": "running",
          "observed_generation": 7,
          "observed_restart_nonce": 0,
          "status_updated_at": "2026-06-24T12:00:02Z",
          "failure_class": "",
          "last_error_code": "",
          "last_error_message": ""
        }
      ]
    }
  }
}
```

Snapshot rules:

- Include only agent connections for the report `remote_id`.
- Include enabled, stopped, backoff, failed, and running connections.
- Include disabled or deleted connections only if manager needs tombstone or
  cleanup semantics; if included, mark `desired_state` explicitly.
- Include host CPU, memory, and uptime metrics when available.
- Do not include harness inventory, local session lists, command payloads,
  environment values, resolved secrets, or local-only details.
- `connection_id` is paxd's local stable identity. It is useful for diagnostics
  and idempotency, but manager business updates should prefer `cloud_agent_id`.
- If `cloud_agent_id` is empty, manager must not create a new cloud agent from
  the snapshot by accident. It should either ignore that entry, store it as an
  unbound local runtime entry, or return a safe protocol error according to the
  migration stage.
- Error fields must be safe for cloud storage and UI display. Paxd should
  redact secrets, auth headers, and sensitive environment/path details before
  reporting.

## Heartbeat Types

There are two heartbeat types. Do not mix them.

Transport heartbeat:

- Lives in `internal/runtime/heartbeat.go`.
- Uses WebSocket ping/pong.
- Detects a stuck or broken socket.
- Causes the runtime session to exit with a transient failure.
- Does not update pax-manager business status directly.

Node lease heartbeat:

- Lives in the node-control report protocol.
- Is a normal WebSocket text frame with `kind=report` and
  `report.type=heartbeat`.
- Refreshes pax-manager node lease using server receive time.
- Does not replace WebSocket ping/pong.

## Paxd Source Of Runtime Reports

Runtime snapshots should be built from daemonstore through the control business
layer, not from transient event payloads and not by giving `controlws` direct
database access.

```text
supervisor/runtime observes state
  -> conditional write to daemonstore
  -> status channel is poked
  -> control service reads a remote-scoped projection from daemonstore
  -> controlws encodes and writes report frames
```

Daemonstore remains the source of snapshot truth. Events only accelerate
reporting.

Relevant daemonstore tables:

```text
remote
remote_status
agent_connection
agent_connection_status
```

`local_session` is intentionally excluded from the v1 runtime snapshot unless a
separate workload/session reporting contract is added.

## Paxd Implementation Plan

### 1. Add report contracts in internal/control

Add versioned business payload types to `internal/control`:

```go
type ReportType string

const (
    ReportHeartbeat       ReportType = "heartbeat"
    ReportRuntimeSnapshot ReportType = "runtime.snapshot"
)

type Report struct {
    Type     ReportType `json:"type"`
    RemoteID string     `json:"remote_id"`
    NodeID   string     `json:"node_id,omitempty"`
    SentAt   string     `json:"sent_at"`

    Heartbeat       *HeartbeatReport       `json:"heartbeat,omitempty"`
    RuntimeSnapshot *RuntimeSnapshotReport `json:"runtime_snapshot,omitempty"`
}

type HeartbeatReport struct{}

type RuntimeSnapshotReport struct {
    SnapshotID string               `json:"snapshot_id"`
    Host       *HostMetricsReport   `json:"host,omitempty"`
    Agents     []AgentRuntimeReport `json:"agents"`
}

type HostMetricsReport struct {
    CPUPercent    float64 `json:"cpu_percent,omitempty"`
    MemoryPercent float64 `json:"memory_percent,omitempty"`
    UptimeSeconds int64   `json:"uptime_seconds,omitempty"`
    CollectedAt    string  `json:"collected_at,omitempty"`
}
```

`AgentRuntimeReport` should be a cloud-safe, remote-scoped projection. It must
not reuse local debug or status overview types wholesale.

Host metrics should come from an injected provider, not from direct system calls
inside `controlws`:

```go
type HostMetricsProvider interface {
    CurrentHostMetrics(ctx context.Context) (*HostMetricsReport, error)
}
```

The provider may use `gopsutil`, matching the legacy collector's CPU, memory,
and uptime behavior. It should prefer background sampling with a cached latest
value, because calls such as CPU percentage sampling can block. Snapshot
construction should read the latest sample and omit `host` if no sample is
available.

### 2. Keep controlws as the wire adapter

Update `internal/controlws` so one node-control session can write:

- ACK frames
- query response frames
- command result frames
- report frames

All outbound writes must go through one serialized writer path. Heartbeat,
snapshot, ACK, response, and command result pumps must not call
`conn.WriteMessage` directly.

`controlws` should still depend on business interfaces, not daemonstore. It may
depend on a narrow report provider interface implemented by `control.Service`:

```go
type RuntimeSnapshotProvider interface {
    BuildRuntimeSnapshot(ctx context.Context, remoteID string, nodeID string) (control.RuntimeSnapshotReport, error)
}
```

If this interface fits better inside `control.Service`, prefer that over adding
a second dependency.

### 3. Start read and write pumps together

After `RemoteControlSession` connects and delegates to `controlws`, `controlws`
should start:

```text
read pump
heartbeat ticker
snapshot ticker
status poke pump
command result pump
```

The read pump must start promptly so manager commands and queries are not
blocked behind snapshot construction or slow writes.

Suggested initial timing:

```text
heartbeat interval  10s
snapshot interval   60s
poke debounce       250ms to 1s
poke max wait       5s
```

These values are configuration defaults, not protocol guarantees.

### 4. Add a status poke path

Expose a small interface:

```go
type StatusPoke interface {
    Poke(remoteID string)
}
```

When supervisor status writes succeed, call `Poke(spec.RemoteID)`.

The poke must not carry status payloads. It only tells the active node-control
status pump to read a fresh remote-scoped snapshot from daemonstore through the
control layer.

Pokes are an optimization, not the reliability mechanism. Periodic snapshots
must remain so pax-manager can recover from missed pokes, restarts, or local
status drift.

### 5. Keep transport heartbeat independent

Do not move business lease logic into `internal/runtime/heartbeat.go`.

Transport heartbeat should only close broken sockets. The session exit will be
handled by the remote supervisor and then reflected in local daemonstore.

## Pax-manager Implementation Plan

### 1. Decode node-control report frames

Update `internal/manager/node_control_tunnel.go` so it decodes text frames into
the node-control protocol instead of only logging bytes.

For an authenticated node-control session:

```text
valid heartbeat report
  -> touch node lease using server receive time

valid runtime.snapshot report
  -> touch node lease using server receive time
  -> apply runtime snapshot projection

invalid report
  -> reject/log protocol error
  -> do not refresh lease
```

Do not trust `sent_at` for lease freshness. It is diagnostic metadata only.

### 2. Add explicit store methods

Do not overload old node status reporting as the long-term abstraction. Add
methods similar to:

```go
TouchNodeLease(ctx, nodeID string, receivedAt time.Time) error
ApplyNodeRuntimeSnapshot(ctx, nodeID string, snapshot RuntimeSnapshot, receivedAt time.Time) error
```

Short-term, `TouchNodeLease` may update `nodes.last_heartbeat` so existing
`computed_status(last_heartbeat)` and pax-console behavior continue to work.

Short-term, `ApplyNodeRuntimeSnapshot` may update legacy `agents.status` and
`agents.last_heartbeat` for bound agents while pax-console still reads those
fields. The projection must keep node lease freshness separate from last-known
agent runtime phase.

### 3. Make snapshot application idempotent

Runtime snapshot application must tolerate retries, duplicate frames, reconnects,
and out-of-order delivery.

Manager should condition updates using available monotonic-ish fields:

```text
node_id
cloud_agent_id
connection_id for diagnostics/local binding only
observed_generation
observed_restart_nonce
status_updated_at
server received_at
```

Older snapshots must not overwrite newer runtime state. If ordering cannot be
proven, prefer preserving the newer server-received projection and logging the
conflict.

### 4. Compute effective agent status from node liveness

Agent last-known runtime phase and effective online state are different.

Example:

```text
node online + agent runtime_phase running
  -> agent effective online/running

node offline + agent last-known runtime_phase running
  -> agent effective offline, last-known running
```

Avoid showing stale agent runtime as current online state after node lease
expiration.

### 5. Define connection uniqueness

For a given `node_id`, pax-manager should define what happens when multiple
node-control WebSockets are open at the same time:

- Prefer one active session per node.
- A newer authenticated connection may supersede and close the older one.
- Only the active session should apply runtime snapshots.

This prevents two paxd processes or stale reconnects from racing status writes.

## HTTP Compatibility Bridge

HTTP `POST /api/v1/node/status` can remain temporarily if pax-console or other
manager paths need legacy status before node-control report processing is fully
landed.

If used, keep it explicitly labeled as compatibility:

```text
daemonstore remote-scoped projection
  -> compatibility HTTP status report
  -> pax-manager legacy last_heartbeat/status fields
```

Do not make HTTP status reporter the long-term core status architecture.

During migration, maintain pax-console expectations:

- `nodes.online/status/last_heartbeat` continue to work.
- `agents.online/status/last_heartbeat` continue to work for bound agents.
- Agent effective online state is derived from node lease freshness plus
  last-known runtime phase, not from agent heartbeat alone.

## Implementation Order

1. Manager: add report frame decoding and validation on node-control WS.
2. Manager: add `TouchNodeLease` and update legacy `nodes.last_heartbeat`.
3. Paxd: send heartbeat reports over node-control WS through the serialized
   controlws writer.
4. Manager: verify node online/degraded/offline behavior through existing TTL
   logic.
5. Paxd: add remote-scoped runtime snapshot contracts and builder through the
   control layer.
6. Paxd: send runtime snapshot on connect and periodically.
7. Manager: apply runtime snapshots idempotently to bound agents and legacy
   compatibility fields.
8. Paxd: add supervisor status poke with debounce and max wait.
9. Manager: enforce one active node-control session per node.
10. Later: add `runtime.delta` if snapshots are too heavy.
11. Later: remove or disable HTTP compatibility reporter.

## Tests

Use BDD scenarios to prove the design at package boundaries before wiring the
full daemon path. The goal is not to test implementation details; it is to
prove the contracts that prevent stale liveness, wrong remote scoping, unsafe
payloads, and WebSocket lifecycle leaks.

### Paxd BDD

#### Scenario: heartbeat report is lease evidence without client TTL

Given a node-control session is connected for `remote_prod` and `node_123`
When the heartbeat ticker fires
Then `controlws` writes a text frame with `kind=report`
And the report type is `heartbeat`
And the frame includes `version`, `report_id`, `remote_id`, `node_id`, and
`sent_at`
And the frame does not include a client-selected TTL

#### Scenario: runtime snapshot is built through the control layer

Given `controlws` has a report-capable control service
And daemonstore contains agent connections for `remote_prod` and
`remote_staging`
When the `remote_prod` node-control session sends a runtime snapshot
Then `controlws` asks the control layer to build a snapshot for `remote_prod`
And the frame includes only `remote_prod` agent runtime entries
And `controlws` does not import or call daemonstore directly

#### Scenario: runtime snapshot is cloud-safe

Given daemonstore contains harness inventory, local session cache, command
payloads, environment values, and runtime error details
When a runtime snapshot report is built
Then the report includes only the cloud projection fields needed by
pax-manager
And it excludes harness inventory, local sessions, command payloads,
environment values, and resolved secrets
And error messages are redacted according to the reporting policy

#### Scenario: runtime snapshot includes cached host metrics

Given the host metrics provider has a recent CPU, memory, and uptime sample
When a runtime snapshot report is built
Then the snapshot includes `host.cpu_percent`, `host.memory_percent`,
`host.uptime_seconds`, and `host.collected_at`
And snapshot construction does not block on fresh CPU sampling

#### Scenario: host metrics failure does not block runtime reporting

Given the host metrics provider has no sample or returns an error
When a runtime snapshot report is built
Then the snapshot omits `host` or sends only the available fields
And agent runtime entries are still reported
And the node-control session stays healthy

#### Scenario: status poke emits a debounced snapshot

Given a node-control reporter is active for `remote_prod`
And supervisor status writes call `Poke("remote_prod")` many times quickly
When the debounce window closes
Then exactly one runtime snapshot is written for that burst
And if pokes continue indefinitely, a max-wait timer eventually writes a
snapshot

#### Scenario: missed pokes do not break correctness

Given no node-control reporter is active for `remote_prod`
When supervisor status writes call `Poke("remote_prod")`
Then the poke may be dropped or coalesced without durable retry
And when a later node-control session connects, it sends an initial runtime
snapshot
And periodic snapshots continue after connect

#### Scenario: node-control writes are serialized

Given heartbeat, runtime snapshot, query response, ACK, and command result
pumps can all write frames
When they become ready concurrently
Then all frames are written through one serialized writer path
And no goroutine writes directly to the WebSocket connection

#### Scenario: reporter stops with the WebSocket session

Given a node-control session has active heartbeat, snapshot, poke, and command
result pumps
When the read loop fails, a write fails, or the session context is canceled
Then all report pumps stop
And the poke subscription is unregistered
And `RunNodeControl` returns a classified runtime exit

### Pax-manager BDD

#### Scenario: heartbeat refreshes node lease using server time

Given an authenticated node-control session for `node_123`
And paxd sends a valid heartbeat report with `sent_at` far in the past or
future
When pax-manager accepts the frame
Then it updates the node lease using server receive time
And it does not use `sent_at` for liveness calculation

#### Scenario: invalid reports do not refresh lease

Given an authenticated node-control session for `node_123`
When pax-manager receives malformed JSON, an unsupported report version, a
missing report type, or a report whose `node_id` conflicts with the
authenticated node
Then it records a protocol error
And it does not refresh the node lease
And it does not apply runtime projection updates

#### Scenario: runtime snapshot applies bound agent projection

Given pax-manager has a node `node_123` and a bound agent `agent_123`
When it receives a valid runtime snapshot containing
`cloud_agent_id=agent_123` and `runtime_phase=running`
Then it refreshes the node lease using server receive time
And it records the agent last-known runtime phase
And during the compatibility period it updates legacy agent status fields
needed by pax-console

#### Scenario: unbound runtime entries do not create agents accidentally

Given pax-manager receives a runtime snapshot entry with an empty
`cloud_agent_id`
When snapshot projection is applied
Then pax-manager does not create a new cloud agent implicitly
And it either ignores the entry, stores an unbound diagnostic projection, or
returns a safe protocol error according to the migration policy

#### Scenario: older snapshots do not overwrite newer state

Given pax-manager has already applied a runtime projection for `agent_123` with
a newer generation, restart nonce, status timestamp, or server receive time
When an older duplicate or delayed snapshot arrives
Then pax-manager preserves the newer runtime projection
And logs enough metadata to diagnose the dropped stale update

#### Scenario: node lease controls effective online state

Given an agent's last-known runtime phase is `running`
And the owning node lease is stale past the offline TTL
When pax-console or API clients read effective agent state
Then the agent is effectively offline
And the last-known runtime phase remains available as historical context

#### Scenario: one active node-control session owns projection writes

Given two authenticated node-control WebSockets exist for the same `node_id`
When pax-manager chooses the newer session as active
Then the older session is closed or isolated
And only the active session can refresh lease and apply runtime snapshots

### Integration BDD

#### Scenario: fresh node-control heartbeat makes the node online

Given paxd is configured with one enabled remote
And pax-manager accepts the node-control WebSocket
When paxd connects and sends heartbeat reports
Then pax-manager marks the node online through the same fields read by
pax-console during migration

#### Scenario: agent runtime status survives reconnect

Given paxd reports an agent runtime snapshot with phase `running`
And the node-control WebSocket disconnects and reconnects
When paxd sends the connect-time snapshot on the new session
Then pax-manager keeps a consistent last-known agent runtime phase
And the node effective state follows the refreshed node lease

#### Scenario: HTTP compatibility bridge does not fight node-control reports

Given both HTTP compatibility status reporting and node-control reporting are
enabled during migration
When both paths report the same bound agent
Then legacy node and agent heartbeat/status fields remain monotonic enough for
pax-console
And neither path overwrites newer runtime projection with stale data

## Test Placement

Paxd:

- `controlws` serializes concurrent report, ACK, response, and command result
  writes through one writer.
- `controlws` starts read handling without waiting for snapshot construction.
- heartbeat reports contain no client-selected TTL.
- runtime snapshot builder filters by `remote_id`.
- runtime snapshot includes host CPU, memory, and uptime metrics when a metrics
  sample is available.
- runtime snapshot excludes harness inventory, local sessions, command/env
  values, and secrets.
- status pokes debounce bursts but eventually emit a snapshot.
- periodic snapshot still emits without pokes.

Pax-manager:

- authenticated heartbeat refreshes node lease using server receive time.
- `sent_at` in the future or past does not affect lease freshness.
- malformed reports do not refresh node lease.
- runtime snapshot refreshes node lease and applies runtime projection.
- older snapshots do not overwrite newer projected runtime state.
- duplicate snapshots are idempotent.
- unbound `cloud_agent_id=""` entries do not create accidental cloud agents.
- stale node lease makes agents effectively offline even if last runtime phase
  was running.
- a newer node-control session supersedes or isolates an older session.

Migration:

- pax-console node and agent resource pages continue to receive
  `online/status/last_heartbeat` during the bridge period.
- HTTP compatibility reporter and node-control reporter do not fight each other
  or regress status when both are enabled.

## Non-goals

- Do not use ACP tunnel WebSockets for daemon/node status.
- Do not make a single agent tunnel responsible for node liveness.
- Do not have paxd push `offline` as the authoritative node state.
- Do not let last-known runtime phase override stale node lease state.
- Do not put local session/workload state into v1 runtime snapshots.
