# ACP Slot Pool Implementation Plan

Status: proposed

Scope: paxd ACP process pool, sticky session routing, process recovery, and the
minimum pax-manager changes required to safely enable concurrency.

## 1. Outcome

One pax-manager-to-paxd agent tunnel serves a pool of local ACP stdio
processes. Each local process is represented by one `ACPSlot`. A native ACP
session is sticky to one live slot while that slot process remains alive. If
the slot process is gone, paxd selects another slot and restores the session
with `session/resume` before forwarding the next session operation.

This evolves the existing single-ACP model rather than introducing an
unrelated kind of slot. Today one supervisor `runtimeSlot` effectively bundles
one agent connection, one tunnel lifecycle, and one persistent ACP worker. The
migration splits that composite into one connection runtime for the single
tunnel and N process-level `ACPSlot` workers behind it.

The first production rollout keeps the existing pax-manager single-active-run
gate. Multi-slot routing is exercised and stabilized behind that gate before
pax-manager allows multiple concurrent conversations for one agent.

## 2. Decisions

These decisions are part of the plan and are not left to implementation-time
interpretation.

1. There remains exactly one physical ACP WebSocket tunnel per
   `agent_connection`.
2. The existing `AgentConnectionSupervisor` runtime slot is the single-ACP
   model's composite slot: because connection, tunnel, and ACP process are
   currently 1:1:1, it effectively acts as both connection runtime and ACP
   worker. The migration retains it as the connection-level runtime that owns
   the one tunnel's retry lifecycle; it must not be multiplied to create ACP
   capacity, because that would create multiple tunnels.
3. The ACP-process side of that composite is extracted and expanded into N
   process-level `ACPSlot` instances. A typed `ACPSlotSupervisor` reuses the
   existing generic `baseSupervisor/runtimeSlot` machinery to own them; it is
   not a second, unrelated supervisor framework. Tunnel reconnects do not
   restart healthy ACP slot processes.
4. `agent_connection.desired_acp_slots` is an exact, hot-updatable SQLite
   value in v1. There is no
   load-based autoscaling, consistent hashing, or automatic idle reap in the
   first rollout.
5. Slot selection uses an explicit route table, not consistent hashing.
6. The authoritative sticky key is `(connection_id, native_session_id)`.
   Native session IDs are not assumed to be globally unique across agents.
7. `session/new` is the only normal request without a native session ID. paxd
   selects a slot before forwarding it and persists the native-ID route before
   forwarding the successful response to pax-manager.
8. paxd is the ACP client of every local worker and owns the initialize
   descriptor. It derives that descriptor from paxd's implemented capabilities,
   version, and local configuration, then initializes every slot exactly once
   per process epoch with an internal request ID. pax-manager neither supplies
   nor confirms worker initialization; it receives a capability report.
9. One slot permits at most one active prompt turn. Cancellation and replies
    to ACP-originated client requests may pass through to that active turn.
10. v1 does not queue a prompt behind a busy slot inside an in-memory queue.
    It returns a typed retryable busy error. The reliable frame may already be
    received and applied at the transport boundary; the busy error is the
    explicit business result and is not hidden behind another paxd prompt
    queue.
11. ACP slot metrics and resource limits are not implemented in this plan.
    The slot identity and snapshot ports required by metrics are established.
12. `reliablemq` remains one transport queue per connection. Its identity stays
    `queue_id + stream + seq + direction`; `queue_id` is not rebound to a slot
    or session, and no per-session transport stream is added in v1.
13. `reliablemq.Engine.Send` becomes asynchronous acceptance. It validates and
    enqueues an outbound message, but does not wait for a journal flush, a
    WebSocket write, or a durable `sent` patch. It returns no `Frame`; transport
    sequence allocation and sending belong to the queue owner.
14. The reliable producer is the outbound queue. paxd must not add a second
    ACP-specific outbox in front of it. All slots publish into the same
    producer, whose journal and network workers advance independently over one
    ordered log.
15. There is one network send path for both backlog and newly accepted frames.
    A reconnect resets that path from the cumulative peer ACK and never opens a
    direct live-send bypass.
16. pax-manager performs ACP business routing by manager session after a frame
    reaches it. This application-level demultiplexing does not change
    reliablemq sequence, ACK, journal, or reconcile identity.
17. Neither ACP nor the manager conversation API gains a new idempotency
    promise. Journaled/peer-recorded transport remains ordered and replayable;
    the unflushed producer tail has the explicitly documented send-first crash
    window. Ambiguous application side effects across a full process crash
    remain visible to callers.

ACP v1 requires initialization before session creation. The stabilized resume
contract requires `sessionId`, `cwd`, and `mcpServers`:

- https://agentclientprotocol.com/protocol/v1/session-setup
- https://agentclientprotocol.com/announcements/session-resume-stabilized

## 3. Terminology and ownership

### Existing composite runtime slot / connection runtime

The existing generic supervisor `runtimeSlot` is keyed by `connection_id`.
In the current single-ACP topology it starts one `AgentTunnelSession` attempt
at a time, and that attempt acquires the connection's one persistent ACP
process. It is therefore reasonable to treat the existing runtime slot as the
old ACP slot: connection runtime and ACP worker are currently collapsed into
one effective unit.

After this migration, the same supervisor unit becomes only the connection
runtime. It owns the one tunnel's reconnect and backoff lifecycle, while the
extracted process-level `ACPSlot` instances own individual ACP processes. One
existing runtime slot is retained per connection; N copies of it are never
created.

### Reuse of the existing supervisor

The existing generic supervisor remains the lifecycle framework and receives a
second typed specialization:

```text
baseSupervisor/runtimeSlot
  -> AgentConnectionSupervisor
       key: connection_id
       session: one AgentTunnelSession attempt
  -> ACPSlotSupervisor
       key: slot_id
       session: one ACPSlot process epoch
```

There is one daemon-level `ACPSlotSupervisor`, analogous to the existing
daemon-level `AgentConnectionSupervisor`; it owns a map of runtime slots keyed
by `slot_id`. `connection_id` groups those slots through `ACPPoolRegistry`, but
does not create one supervisor goroutine per connection.

The two typed supervisors are siblings coordinated by SQLite desired state and
the pool registry, not parent and child cancellation scopes. In particular,
an `AgentTunnelSession` attempt ending must not cancel an `ACPSlot` runtime.
Stopping or deleting an `agent_connection` removes both its tunnel desired spec
and all synthesized ACP slot specs; the tunnel closes independently while the
slot supervisor drains and stops the processes.

The current generic implementation can be reused for desired-list reconcile,
coalesced wake, periodic reconcile, start attempts, backoff, and in-memory
stale-attempt rejection. It needs these narrow extensions before it can manage
ACP processes correctly:

1. Desired-change detection must be an injected operation. Existing tunnel
   slots continue comparing generation/restart nonce; ACP slots compare stable
   `slot_id` plus `CommandFingerprint`. A slot-count-only change adds or removes
   specs and does not invent a pool generation.
2. Reconcile entry must be serialized even when `Reconcile` is called directly,
   not only when invoked by the `Start` loop. Every pass reads the latest
   `desired_acp_slots` before synthesizing slot specs.
3. Stop/replacement must support a drain transition. Removing an active ACP
   slot first closes admission and marks it `draining`; process cancellation
   and termination happen after its lease is released or an explicit daemon
   shutdown/cancel policy takes over. The current immediate `runtimeSlot.Stop`
   behavior is insufficient.
4. A new ACP process attempt creates and persists a new `process_epoch` before
   it becomes ready. The existing in-memory attempt token rejects late
   callbacks inside the daemon; database status and route writes additionally
   use `(slot_id, process_epoch)` predicates so an old process exit cannot
   overwrite a replacement.
5. `ACPSlotSupervisor` supplies an ACP-specific session factory, status writer,
   exit handler, and stop/drain hooks. It does not reuse the tunnel status
   writer or transport-queue rotation exit handler.

### ACP pool

One pool per `agent_connection`. It owns routing state, the canonical paxd
client-init descriptor and worker initialize result, worker-request response
correlation, and access to all `ACPSlot` instances for that connection.

The pool's in-memory `slots map[slot_id]*ACPSlot` is the live runtime registry;
there is no separate PID registry. Each entry contains the current
`process_epoch`, the `LocalACPProcess` handle, phase, and admission/lease state.
PID and process-group ID are process metadata used for metrics and termination,
not routing identity.

Conceptually:

```go
type ACPSlot struct {
    SlotID       string
    ProcessEpoch string
    Process      LocalACPProcess // live handle; nil when stopped
    Phase        SlotPhase
    // admission, prompt lease, and lifecycle lease state
}
```

`ACPPoolRegistry` maps `connection_id -> *ACPPool`; `ACPPool.slots` then maps
`slot_id -> *ACPSlot`. Route resolution uses those two maps and compares the
UUID process epoch. It never looks up a slot by PID.

### ACP slot

One logical slot with a stable `slot_id` and ordinal. Each time its stdio ACP
process starts, it receives a new `process_epoch`. A slot may host multiple
idle ACP sessions, but it has only one active prompt turn.

`process_epoch` is an opaque UUID generated for each process start. It is the
incarnation ID of that particular ACP process, not a timestamp, counter, or OS
PID. Restarting a slot keeps the same `slot_id` but always creates a different
`process_epoch`.

A session is bound to `(slot_id, process_epoch)`, never to `slot_id` alone. A
new process in the same logical slot does not inherit any session residency
from the previous process and must resume those sessions before use.

### Hot route

A session route whose `slot_id` is ready and whose `process_epoch` matches the
route's bound process epoch. The next session operation can be delivered
without resume.

### Cold route

A durable native-session route with no matching live process epoch. Before a
prompt or other stateful operation is delivered, paxd must execute
`session/resume` using the stored lifecycle descriptor.

## 4. Target component boundary

The structural change is:

```text
Current:  runtimeSlot(connection_id) = one tunnel lifecycle + one ACP worker
Target:   ConnectionRuntime(connection_id) = one tunnel lifecycle
            -> ACPPool(connection_id) = N ACPSlot workers
```

```text
AgentConnectionSupervisor
  -> AgentTunnelSession (one WebSocket attempt)
       -> ACPPoolRegistry.Get(connection_id)
       -> reconcile/replay barrier
       -> ACPRouter.HandleManagerFrame(...)

ACPSlotSupervisor
  -> desired ACPSlotSpec rows synthesized from SQLite desired_acp_slots
  -> ACPSlot process lifecycle
       -> initialize once
       -> stdin/stdout
       -> process_epoch and process identity
       -> ACPRouter.HandleSlotFrame(slot_id, ...)

ACPRouter
  -> durable session route store
  -> slot admission/selection
  -> pending worker-response source validation
  -> one shared reliablemq producer

reliablemq producer (one per connection + stream)
  -> ordered in-memory hot log
  -> asynchronous journal flush cursor
  -> asynchronous network cursor
```

The router must not open WebSockets. The tunnel session must not start ACP
processes. The slot supervisor must not parse reliablemq envelopes. There is no
separate `ACPTransportOutbox`: reliablemq owns sequence allocation, queueing,
journaling, replay, ACK state, and the single network send path.

## 5. Runtime invariants

The implementation and tests must enforce all of these invariants.

1. A `slot_id` is stable across process restarts; `process_epoch` is not.
2. A slot becomes `ready` only after initialization succeeds.
3. At most one prompt lease exists per slot.
4. At most one prompt lease exists per native session across the local pool.
5. A hot session route points to exactly one `(slot_id, process_epoch)` and is
   effectively hot only while that exact process epoch is the slot's live
   epoch. A matching `slot_id` alone is insufficient.
6. A slot process exit synchronously closes that epoch's in-memory admission;
   indexed SQLite binding cleanup follows eagerly but is not the sole
   correctness guard.
7. A scale-down never kills an active slot. It marks the slot draining and
   stops it after the active turn ends.
8. A successful `session/new` route is durable before its response becomes
   visible to pax-manager.
9. A cold session is never forwarded a prompt until `session/resume` succeeds.
10. Resume failure never creates a replacement session.
11. Frames generated by different slots enter one reliablemq producer and
    receive one monotonically increasing transport sequence in producer
    acceptance order. Session ordering is therefore preserved; cross-session
    total ordering is stronger than the product requires but remains the v1
    transport behavior.
12. The network cursor sends only its current head sequence. It never skips a
    failed or unavailable head to send a later frame, regardless of session.
13. Tunnel recovery finishes reconcile before the network cursor is enabled.
    Inbound replay completes before new manager work is admitted, while new
    slot output may continue to enqueue behind the outbound backlog.
14. A disconnected tunnel does not stop healthy ACP slots. Slot stdout remains
    accepted into the reliable producer and is asynchronously journaled while
    the network cursor is unavailable.
15. A worker-originated JSON-RPC request ID is scoped by native session. It is
    never used alone to route a response across slots.
16. No command line, environment value, MCP secret, or raw lifecycle descriptor
    is written to logs or status reports.

## 6. SQLite model

### `agent_connection` extension

```text
desired_acp_slots INTEGER NOT NULL DEFAULT 1
```

Rules:

- `desired_acp_slots` belongs to the existing `agent_connection` row because
  the pool and the physical tunnel share the same `connection_id` lifecycle.
- It has a bounded validation range. Start with `1..16`.
- Updating it must not increment `agent_connection.generation`, rotate the
  transport queue, or restart the physical tunnel.
- Existing agent connections receive the default value of one slot during the
  migration.
- There is no separate pool generation. The single daemon-level
  `ACPSlotSupervisor` serializes reconcile passes, coalesces wakeups, and
  rereads all latest desired counts before applying a reconcile.

### `acp_slot_status`

```text
slot_id TEXT PRIMARY KEY
connection_id TEXT NOT NULL REFERENCES agent_connection(id)
ordinal INTEGER NOT NULL
process_epoch TEXT
pid INTEGER
process_group_id INTEGER
process_start_token TEXT
phase TEXT NOT NULL
failure_class TEXT NOT NULL DEFAULT ''
last_error_code TEXT NOT NULL DEFAULT ''
last_error_message TEXT NOT NULL DEFAULT ''
started_at TEXT
ready_at TEXT
active_since TEXT
stopped_at TEXT
updated_at TEXT NOT NULL
UNIQUE(connection_id, ordinal)
```

`slot_id` is deterministically derived from `connection_id + ordinal` using a
single helper, for example `slot_` plus a truncated base32 SHA-256 digest. It
must not change after scale down and later scale up of the same ordinal. Do not
expose a second random slot-ID creation path.

### `acp_session_route`

```text
connection_id TEXT NOT NULL REFERENCES agent_connection(id)
native_session_id TEXT NOT NULL
bound_slot_id TEXT
bound_process_epoch TEXT
last_slot_id TEXT
resume_params_json TEXT NOT NULL
created_at TEXT NOT NULL
last_used_at TEXT NOT NULL
updated_at TEXT NOT NULL
version INTEGER NOT NULL DEFAULT 1
PRIMARY KEY(connection_id, native_session_id)
CHECK ((bound_slot_id IS NULL) = (bound_process_epoch IS NULL))
```

Add the process-binding lookup index used by process exit and scale down:

```sql
CREATE INDEX idx_acp_session_route_process_binding
ON acp_session_route(connection_id, bound_slot_id, bound_process_epoch)
WHERE bound_process_epoch IS NOT NULL;
```

The table stores the last committed process binding, not a real-time health
status:

- both binding columns are non-null: a persisted binding candidate exists;
- both binding columns are null: the route is cold;
- a candidate is effectively hot only when `ACPPool.slots` contains that exact
  ready `(slot_id, process_epoch)`;
- `resuming` is an in-memory per-session recovery lease;
- a resume failure is an error/event and leaves the route cold. It is not a
  durable route state.

Process exit and scale down eagerly clear only the matching incarnation:

```sql
UPDATE acp_session_route
SET last_slot_id = bound_slot_id,
    bound_slot_id = NULL,
    bound_process_epoch = NULL,
    updated_at = :now,
    version = version + 1
WHERE connection_id = :connection_id
  AND bound_slot_id = :slot_id
  AND bound_process_epoch = :process_epoch;
```

The daemon-start transaction clears every binding before supervisors start:

```sql
UPDATE acp_session_route
SET last_slot_id = COALESCE(bound_slot_id, last_slot_id),
    bound_slot_id = NULL,
    bound_process_epoch = NULL,
    updated_at = :now,
    version = version + 1
WHERE bound_process_epoch IS NOT NULL;
```

Neither operation deletes the route row or its resume descriptor.

`resume_params_json` contains the session lifecycle descriptor required to
perform `session/resume`:

```json
{
  "cwd": "/absolute/path",
  "mcpServers": [],
  "additionalDirectories": []
}
```

The descriptor is captured from `session/new` or an explicit successful
`session/resume`. It is sensitive local data:

- never log it;
- never put it in node-control status or metrics;
- preserve field names and values needed by the negotiated protocol version;
- apply the same local database permissions and retention policy as the ACP
  reliable transport journal, which already carries lifecycle request bodies;
- add encryption-at-rest as a separate security hardening task if arbitrary
  secret-bearing MCP configs are supported.

paxd stores no manager `sess_*` ID in the route table. pax-manager remains the
owner of `manager session ID <-> native session ID` translation.

### `acp_client_init_profile`

The pool must retain how paxd, as the ACP client, initialized its workers. Use a
separate connection-scoped record rather than hiding this state in an arbitrary
slot:

```text
connection_id TEXT PRIMARY KEY REFERENCES agent_connection(id)
params_json TEXT NOT NULL
profile_hash TEXT NOT NULL
source TEXT NOT NULL
paxd_version TEXT NOT NULL
command_fingerprint TEXT NOT NULL
canonical_result_json TEXT
result_hash TEXT
created_at TEXT NOT NULL
updated_at TEXT NOT NULL
```

`params_json` is canonical JSON for the complete worker `initialize.params`
generated by paxd, including extension fields. Known fields such as
`protocolVersion`, `clientCapabilities`, and `clientInfo` are parsed for
validation, but the stored descriptor is not a lossy projection.
`profile_hash` is computed from the canonical JSON. `source` identifies the
local descriptor builder/config revision; it is never an external manager
request. The canonical worker result is tied to `command_fingerprint` and is
invalidated when the ACP command changes.

This record exists for recovery, diagnostics, and future capability-aware
routing. It is never logged or exposed through status/metrics. Apply the same
database permissions and retention policy as lifecycle descriptors.

Within one paxd lifetime the pool also holds an immutable snapshot of the local
profile. On startup, paxd rebuilds the descriptor from current code/config and
compares its hash with the persisted row. A changed profile invalidates the
cached result and causes normal per-slot process replacement/initialization;
manager approval is not involved.

### Configuration mutation

Extend the existing typed `agent_connection.update` control command with an
optional `desired_slots` field. A desired-slots-only update:

1. validates `1..16`;
2. updates `agent_connection.desired_acp_slots`;
3. does not increment `agent_connection.generation`;
4. does not rotate `transport_queue_id`;
5. wakes only `ACPSlotSupervisor`.

Wakeups may be coalesced. The daemon-level slot reconcile loop is serialized
and must reread `desired_acp_slots` immediately before calculating scale
up/down deltas, so an older queued reconcile cannot overwrite newer desired
state. The periodic supervisor ticker remains the missed-wake fallback.

If the same command also changes the ACP command, working directory, or
environment, normal agent-connection generation rules still apply and both
the tunnel and slot supervisors are woken.

### Status ownership

`agent_connection_status` remains tunnel-level status after this migration.
Its single `pid` is cleared/deprecated because one connection can have several
ACP process IDs. Process phase and PID move to `acp_slot_status`.

Runtime snapshots may summarize `desired_slots`, `ready_slots`, and
`active_slots`, but they must not flatten one arbitrary slot PID back into the
connection status row.

## 7. Slot state machine

```text
stopped
  -> starting
  -> initializing
  -> ready
  -> active
  -> ready
  -> draining
  -> stopping
  -> stopped

starting/initializing/ready/active
  -> backoff      on transient process failure
  -> failed       on non-retryable configuration failure
```

Semantics:

- `ready`: initialized and no prompt lease.
- `active`: one `session/prompt` is in progress.
- `draining`: accepts cancellation and responses required by its active turn,
  but no new session or prompt.
- `backoff`: retry timer owned by `ACPSlotSupervisor`.

`session/new` and internal `session/resume` take an exclusive lifecycle lease.
They may be represented as `active` with an operation kind, but must not overlap
a prompt.

## 8. Process ownership

Refactor `LocalACPProcess` to expose a non-secret identity:

```go
type ProcessIdentity struct {
    PID          int
    ProcessGroup int
    StartToken   string
    StartedAt    time.Time
}

type LocalACPProcess interface {
    Identity() ProcessIdentity
    Stdin() io.WriteCloser
    Stdout() io.Reader
    Stderr() io.Reader
    Wait() error
    Terminate(ctx context.Context) error
}
```

On Unix, start the process in a new process group. Normal shutdown signals the
group so ACP-created tool subprocesses do not leak. Forced termination also
targets the group after the grace period. `StartToken` is an OS-derived process
birth identity used to prevent PID-reuse mistakes during an orphan sweep; a
persisted PID integer alone is never sufficient authority to signal a process.

paxd is the single owner of its local data directory. Before any orphan cleanup,
it must acquire a process-lifetime exclusive daemon lock; if another paxd holds
the lock, startup fails. Once that lock is held, every persisted slot process
identity is necessarily from the previous owner, so no daemon-owner epoch needs
to be stored. ACP process lifetime never crosses a paxd lifetime and old stdio
processes are never reattached.

### Daemon lock: exact cross-platform contract

Implement a small `internal/daemonlock` package with platform files and one
process-lifetime object:

```go
var ErrAlreadyRunning = errors.New("another paxd owns this database")

type Lock interface {
    Release() error
}

func Acquire(ctx context.Context, dbPath string) (Lock, error)
```

Before `Acquire` is called, `cmdRun` performs only path preflight: expand and
canonicalize the daemon SQLite path, create the database parent directory if it
does not exist, and reject known network/remote filesystems. This preflight must
not open, migrate, query, or write the SQLite database. The lock path is then the
canonical absolute daemon SQLite path plus `.lock`, for example
`~/.paxd/paxd.db.lock`. Resolve the parent directory before deriving the path so
relative and symlinked DB paths do not create independent locks. The v1
guarantee applies only to local filesystems because NFS/SMB locking semantics
vary.

Common rules:

1. `cmdRun` may create and canonicalize the database parent directory first, but
   it must acquire the lock before opening/migrating SQLite, orphan cleanup,
   local APIs, or supervisors.
2. Open/create the file without truncation and with owner-only permissions.
   After acquisition, PID/start metadata may be rewritten for diagnostics, but
   file content is never lock authority.
3. Keep the descriptor/handle strongly owned by the `Lock` object for the
   entire daemon lifetime. Do not use a finalizer.
4. Never unlink the lock file while holding it or during startup. On Unix,
   unlinking allows another process to create and lock a different inode.
5. The lock descriptor/handle must not be inherited by ACP or tool child
   processes. Otherwise a child could retain the lock after paxd crashes.
6. Graceful shutdown stops traffic and all ACP processes first, then explicitly
   unlocks and closes the daemon lock last.
7. Crash recovery relies on the kernel closing the owning descriptor/handle.
   The stale file remains and is harmless.

On macOS, Linux, and supported BSD systems, use a build-tagged Unix file:

```go
f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) // no O_TRUNC
unix.CloseOnExec(int(f.Fd()))
err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
```

Map `EWOULDBLOCK`/`EAGAIN` to `ErrAlreadyRunning`; every other error is a
startup failure. `Release` performs `LOCK_UN` and closes the file. `flock`
ownership follows the open file description and is released after all copies
of that descriptor close, which is why close-on-exec and no `ExtraFiles`
propagation are mandatory.

On Windows, use a separate build-tagged file with `golang.org/x/sys/windows`:

1. `CreateFileW(..., OPEN_ALWAYS, ...)` opens the lock file for read/write,
   allows read/write sharing but not delete sharing, and passes nil security
   attributes so the handle is not inheritable.
2. Defensively call `SetHandleInformation` to clear `HANDLE_FLAG_INHERIT`.
3. Call `LockFileEx` for byte range `[0,1)` with
   `LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY`.
4. Map `ERROR_LOCK_VIOLATION` to `ErrAlreadyRunning` after a bounded startup
   retry window; Microsoft documents that automatic unlock after termination
   can be delayed briefly by resource pressure.
5. `Release` calls `UnlockFileEx` with the same range and then closes the
   handle.

`Acquire` uses a bounded context-driven retry (initial 50 ms, capped at 500 ms,
maximum 5 seconds) to smooth service restarts on both platforms. Expiry returns
`ErrAlreadyRunning`; it never steals, deletes, or replaces a held lock. A hung
but still-live paxd correctly continues to own the lock and must be terminated
before another instance can start. After SIGKILL, `TerminateProcess`, or a
machine reboot, the kernel lock disappears automatically even though the file
remains.

References:

- https://pkg.go.dev/golang.org/x/sys/unix#Flock
- https://man7.org/linux/man-pages/man2/flock.2.html
- https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-lockfileex
- https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-createfilew

Before starting slot or tunnel supervisors, the daemon-start barrier must:

1. acquire the exclusive daemon lock;
2. terminate and wait for every safely identified persisted process/process
   group;
3. transactionally clear every session `bound_slot_id` and
   `bound_process_epoch`, preserving session IDs and resume descriptors;
4. mark previous slot-status rows stopped and clear their process identity;
5. start an empty in-memory pool, then create desired slots;
6. only after this barrier allow tunnel reconcile, replay, and live traffic.

The process runner must provide an OS-specific containment or verified orphan
reaper that makes step 2 safe. A platform that cannot guarantee this must fail
the startup barrier rather than risk signaling an unrelated reused PID.

`process_epoch` is a fresh UUID created by `ACPSlot` before every process
start. It is never inferred from PID because operating systems can reuse PIDs.
If a process exits and the exit transaction has not yet marked its routes
cold, the router still treats an epoch mismatch as cold. On paxd startup, no
persisted binding is retained: the daemon-start barrier clears all bindings
before any new slot or tunnel traffic starts.

Within one paxd lifetime, `ACPPool.slots` registers an `ACPSlot` immediately
after `Start` returns a process handle. The entry becomes routable only after
initialization changes it to `ready`. When `Wait` returns for any reason,
`ACPSlot` first closes admission and removes that process epoch from the live
entry under lock, then performs best-effort bulk binding cleanup in SQLite.

## 9. Initialization policy

paxd is the protocol client connected to each local ACP worker. Therefore paxd,
not pax-manager or a manager-side conversation caller, owns the worker
`initialize.params` contract.

At daemon startup and whenever relevant local configuration changes, paxd
builds and canonicalizes `ACPClientInitDescriptor` from:

- the ACP protocol version implemented by paxd;
- paxd client capabilities that are actually implemented locally;
- paxd client identity/version;
- explicit local extension/config fields supported by the worker adapter.

The descriptor preserves the complete generated JSON, including extension
fields, and has a deterministic profile hash. It is connection-scoped, not
session-scoped and not slot-scoped. No manager request can mutate it.

Every new process epoch performs this internal handshake before becoming ready:

1. Snapshot the current local descriptor and command fingerprint.
2. Synthesize `initialize` with a reserved paxd request ID.
3. Send it to exactly one worker process and consume its response in an internal
   waiter before the outbound reliable producer.
4. For the first ready worker, persist the successful result as the pool's
   canonical worker capability result.
5. Require later worker results to be semantically compatible for negotiated
   protocol/capability fields. A failed or incompatible worker remains
   non-ready and is reported as a slot failure.

This is per-worker initialization, not broadcast. Each process epoch has its
own request ID and receives `initialize` at most once. No worker initialize
request or response is routed through a manager session, SSE subscriber, or
conversation response waiter.

After the canonical result exists, paxd emits a sanitized, typed
`ACPPoolCapabilityReport` through the existing authenticated paxd node-status
reporting path, outside the ACP reliablemq stream. The report contains
connection ID, profile/result hashes, protocol version, non-secret client/agent
capability fields, command fingerprint, and report generation. Full raw
descriptor/result JSON stays local. The report is an upsertable observation,
not a request for approval, and periodic/reconnect reporting must not
reinitialize workers.

Conceptually:

```go
type ACPPoolCapabilityReport struct {
    ConnectionID       string
    ReportGeneration   int64
    ClientProfileHash  string
    WorkerResultHash   string
    ProtocolVersion    json.RawMessage
    ClientCapabilities json.RawMessage // sanitized
    AgentCapabilities  json.RawMessage // sanitized
    CommandFingerprint string
}
```

The structured manager conversation path does not send ACP `initialize` at
all. It waits for pool readiness/capability status, then starts lifecycle work
with `session/new` or `session/resume`. Legacy raw ACP proxy compatibility may
answer a client-facing initialize from the reported canonical result after an
explicit compatibility check, but that request never controls or reaches a
worker and remains outside the first concurrent path.

## 10. Routing policy

### Manager-to-paxd classification

| Frame | Routing action |
|---|---|
| `initialize` request | Structured manager path: reject as unexpected because paxd already owns worker initialization. Legacy proxy compatibility may answer from the canonical capability report, but never forwards or broadcasts it to workers. |
| `session/new` request | Select a ready slot and remember request-to-slot pending state. |
| Explicit `session/resume` | Select/validate a slot, forward, and update route on success. |
| Request/notification with `sessionId` | Look up `(connection_id, native_session_id)`. |
| JSON-RPC response without method | Require durable `native_session_id` dispatch metadata, then match `(native_session_id, raw request ID)` to the pending source slot/process epoch. Forward the payload unchanged. |
| Unknown sessionless method | Fail closed with a proxy JSON-RPC error in v1. |

### Slot selection

Candidate slots must be ready and not draining. Selection is:

1. Existing matching hot route, if any.
2. Otherwise fewest hot routes.
3. Then oldest `last_assigned_at`.
4. Then lowest ordinal for deterministic tie-breaking.

No consistent hash is used. Durable explicit routes remain authoritative.

### `session/new`

1. Validate and extract the resumable lifecycle descriptor.
2. Acquire a lifecycle lease on a selected ready slot.
3. Record pending request ID to `(slot_id, process_epoch, descriptor)` in pool
   memory. Pool memory survives tunnel reconnects.
4. Forward the request to the slot.
5. On an error response, clear pending state and release the lease.
6. On success, extract the native session ID.
7. In one SQLite transaction, insert the route with `bound_slot_id` and
   `bound_process_epoch` set to the current slot process.
8. Only after the transaction commits, forward the response to pax-manager and
   release the lease.

If route persistence fails, do not forward success. Return a proxy error. The
newly created ACP session is an orphan candidate and may be closed later if the
agent advertises `sessionCapabilities.close`.

### Existing effectively-hot session

For a request carrying a native session ID:

1. Load the route binding.
2. Resolve `bound_slot_id` in `ACPPool.slots` and verify the ready slot's
   current process epoch matches `bound_process_epoch`.
3. Acquire the appropriate slot lease.
4. Forward without resume.
5. Update `last_used_at` after successful delivery.

If either binding column is null, or the in-memory entry does not match, the
route is effectively cold. A stale non-null binding is cleared with version
compare before recovery.

### Existing effectively-cold session

1. Select a ready slot and acquire its lifecycle lease.
2. Acquire the in-memory per-session recovery lease. This is the derived
   `resuming` state; it is not written to SQLite.
3. Ensure any stale binding is cleared with version compare, then send an
   internal `session/resume` with native ID and stored lifecycle
   descriptor.
4. Consume the internal response locally. It is not exposed as the response to
   the user's original operation.
5. On success, set `bound_slot_id` and `bound_process_epoch` to the selected
   process with version compare.
6. Forward the original operation only after the route commit succeeds.
7. On failure, leave both binding columns null, release the lease, emit a
   sanitized recovery-failure event, and return the resume error for the
   original request. Never call `session/new`.

Concurrent cold resumes for the same native ID are serialized using route
version compare plus the in-process per-session recovery lease. A loser reloads
the route instead of issuing a second resume.

### Missing route during migration

A native session may predate the slot-pool tables, so pax-manager can know its
native ID while paxd has neither a route nor a resume descriptor. paxd must not
guess a slot and forward the prompt.

It returns a typed proxy error:

```json
{
  "code": -32002,
  "message": "ACP session route requires resume",
  "data": {
    "kind": "session_route_missing",
    "requiresResume": true
  }
}
```

The manager conversation path handles this once by reconstructing the
lifecycle descriptor from its session configuration, sending an explicit
`session/resume`, and retrying the original prompt with a new request ID. A
successful explicit resume creates the durable route. Raw legacy tunnel users
must issue `session/resume` themselves.

This is also the recovery path if a route row is administratively removed. It
never falls back to `session/new`.

### Active prompt

The prompt lease begins when `session/prompt` is accepted for delivery and ends
only when the matching JSON-RPC response is read from that slot or the slot
process exits. Streaming `session/update` notifications do not end the lease.

`session/cancel` and responses to permission/file/terminal requests are routed
to the owning active slot even while the prompt lease is held.

If the required slot is busy with another session, v1 returns a typed error:

```json
{
  "code": -32001,
  "message": "ACP slot is busy",
  "data": {
    "kind": "slot_busy",
    "retryable": true
  }
}
```

No in-memory deferred prompt is acknowledged as applied.

## 11. Responses to worker-originated requests

JSON-RPC request IDs are correlation values, not global routing keys. Ordinary
manager-to-worker requests need no ID translation: manager owns those IDs and
paxd already knows the selected slot when it forwards each request.

The case that needs an explicit route is a request initiated by an ACP worker
that expects the ACP client to answer. `session/request_permission` is the
primary example. Its later JSON-RPC response normally contains only `id`; it
does not repeat the method, session ID, or slot identity.

Every ACP data frame that can be attributed to a native session must carry
`native_session_id` in reliablemq envelope metadata. The envelope already has a
generic metadata field; the ACP adapters are responsible for populating it. It
is not extracted automatically by reliablemq. Consequently,
`(native_session_id, raw request ID)` is the correlation identity for a
worker-originated request, and the raw ID needs no translation.

The existing approval model already preserves the worker's original JSON-RPC
ID and associates the approval with a manager session. The durable session
record resolves that manager session to its native session ID; the raw
permission payload also contains `params.sessionId`. Manager therefore does
not need a second request ID or a new global correlation namespace.

```text
worker request payload:
  id = 1
  params.sessionId = native-A

manager approval/client-request state:
  manager_session_id = manager-A
  raw_request_id = 1

durable session record:
  manager-A -> native-A

manager response:
  payload.id = 1
  reliablemq metadata.native_session_id = native-A

paxd pending source:
  (native-A, raw JSON id 1) -> slot_id + process_epoch
```

Flow:

1. When paxd receives an ID-bearing worker request, it requires a native
   session ID and records `(native_session_id, raw request ID)` to the source
   `(slot_id, process_epoch)` before publishing it. The payload is unchanged,
   and the outbound reliablemq envelope carries `native_session_id` metadata.
2. Manager stores the original ID in the approval/client-request state already
   associated with its manager session. The current approval model's `NativeID`
   or original `RawPayload`, plus `RequestSessionID`, already represents this
   relationship. No tunnel ID or additional approval correlation field is
   added.
3. When manager sends the response, it keeps the original JSON-RPC ID in the
   payload, resolves the manager session to its native session, and adds
   `native_session_id` to reliablemq outbound metadata. That metadata is
   journaled and replayed with the payload.
4. paxd looks up the pair, verifies the exact slot process epoch is still live,
   forwards the response unchanged to that worker, and removes the pending
   entry.

Manager must expose a typed worker-response send path that requires session
context, resolves the native session, and sets this metadata before calling
reliablemq. Manual approval, auto-approval, and reusable-grant responses all
use that path; none may call the generic raw tunnel writer directly. Ordinary
manager-originated request sending remains unchanged.

Two sessions may both have a pending request with numeric ID `1`; the native
session ID disambiguates them. Numeric `1` and string `"1"` remain distinct by
storing the ID as raw JSON. Reusing an ID while it is already pending in the
same native session is rejected as a protocol error.

The paxd pending entry is only a source/epoch guard, not an ID translator. It
lives in pool memory and is removed on response, worker exit, or bounded
expiry. A transient tunnel reconnect keeps it because the pool and worker stay
alive. A full paxd restart kills the worker, so it need not be persisted. A
late response for an exited epoch fails closed and is never delivered to a
replacement process.

Any future worker-originated request that expects a response must provide a
native session identity. A sessionless request fails closed in v1 rather than
introducing a general JSON-RPC ID translation layer.

## 12. Reliable outbound queue

### Boundary and API

`persistentACPProcess.sendOutbound` currently appends a frame itself, reads an
optional sender, writes the WebSocket, and then calls `MarkSent`. The current
`reliablemq.Engine.Send` similarly appends, synchronously calls `Sender.Send`,
and returns the allocated `Frame`. Neither shape is the target.

The target API is acceptance-only:

```go
type Producer interface {
    Send(ctx context.Context, msg reliablemq.OutboundMessage) error
}
```

This changes the local paxkit producer API, not the reliablemq wire protocol.
Producer call sites in both paxd and pax-manager must compile against the new
return contract when they update paxkit, but a peer receiver does not need to
understand asynchronous acceptance. Old and new deployed peers remain wire
compatible because the envelope format and receive semantics do not change.
This transport compatibility does not enable multi-session routing on an old
peer: concurrent mode remains gated on the session-metadata/pool capability.

### Receiver impact

`Engine.Receive(ctx, env)` keeps its durable receive, deduplication, ordered
dispatch, and replay contract. It does not call or interpret `Engine.Send`.
Receiver-side work is limited to these integration changes:

1. The inbound `Frame.Metadata` must be preserved and passed to the ACP router
   or manager session mux. paxd uses `native_session_id` plus the raw JSON-RPC
   ID to route a worker-response frame; manager uses the same metadata to
   classify worker frames by session. The current single-process dispatcher
   that discards metadata and writes payload directly to one stdin is replaced
   by `ACPRouter`.
2. After an inbound data frame is durably recorded, `Receive` submits the
   cumulative ACK to the connection-owned network writer. It must no longer
   call `Sender.Send` directly and wait for a WebSocket write. ACK submission is
   non-blocking and coalesces to the greatest contiguous `through` value.
3. Receiving an ACK advances the producer owner's in-memory `ackedThrough`,
   wakes journal/eviction work, and batch-persists the cumulative checkpoint.
   It does not synchronously patch every frame.
4. Reconcile messages bind or reset that same producer's network cursor. They
   do not create a receiver-owned replay sender.

Only item 1 is required by multi-session ACP routing. Items 2-4 are required by
the asynchronous producer and single-WebSocket-writer design; none changes the
wire-visible meaning of a received envelope.

`Send` validates the message and enqueues an owned copy into the producer. A
successful return means only "accepted by this live producer". It does not
mean journaled, written to the socket, received, applied, or ACKed. It returns
no `Frame` because callers must not use a transport sequence or status for ACP
business routing.

Caller cancellation is observed only before acceptance. After enqueue, the
producer lifecycle context owns the message; cancellation of the slot/request
context must not remove it. Outbound middleware executed inside `Send` must be
bounded, local, and free of I/O. Any hook that needs network or database access
must run before `Send` or in the producer owner path so it cannot reintroduce
blocking under the acceptance API.

The call must not wait on SQLite/Postgres, a journal batch, WebSocket I/O,
reconnect, or the consumer ACK. A closed/not-started producer and invalid input
are synchronous errors. Queue bootstrap, including loading the initial
sequence/checkpoint, happens before the producer is published for use, so the
first `Send` does not perform lazy database I/O.

The producer is process-owned, not tunnel-attempt-owned. After reconcile, a
tunnel attempt binds its connection generation and socket sender to the
producer; disconnect removes only that same generation. A late cleanup from an
old attempt cannot detach a newer connection. Binding enables the existing
network cursor rather than creating another replay loop or sender.

The producer ingress must be multi-producer safe because multiple slots can
emit concurrently. Its public enqueue path uses no explicit mutex and never
sends into a bounded channel that can fill and block slot stdout. The intended
shape is a lock-free MPSC append plus a best-effort non-blocking wake signal;
one owner goroutine drains the queue and owns mutable producer state. Memory is
not an unbounded durability policy: accepted entries are continuously moved to
the transport journal in batches, and degraded flush age/bytes must be bounded
by an explicit fatal/degraded policy before rollout. Normal network outage does
not itself grow memory indefinitely because persisted entries can be evicted
from the hot log and reread from the journal.

All slot output first passes through `ACPRouter` for response state transitions
and pending worker-response source registration, then calls this shared producer. paxd does not
introduce `ACPTransportOutbox`, per-slot senders, or direct WebSocket writes.

`slot_id` and `process_epoch` may be stored as diagnostic metadata. Native
session ID is business dispatch metadata, including on manager responses to
worker-originated requests. None is part of `FrameKey` or creates a transport
partition, and pax-manager must not depend on slot metadata for routing.

Keeping the queue connection-scoped is deliberate. A slot-scoped queue would
add independent sequence/ACK/reconcile state for every process worker, expose
slot identity to pax-manager, and become awkward when a cold session resumes on
a different slot/process epoch. A session-scoped queue would require the
manager to discover the partition before decoding some lifecycle responses and
would multiply durable queue state without helping paxd's local sticky routing.
Neither is needed to meet the product requirement of same-session order. The
cost of the simpler v1 choice is cross-session transport head-of-line blocking,
which is accepted and explicitly deferred.

### One log and four positions

After acceptance, one sequencer assigns the next `seq` and appends the frame to
the producer's ordered hot log. Atomic ingress append is the linearization point
for acceptance order; an accepted entry may briefly be waiting for the owner to
assign its sequence. The log then has these logical positions:

```text
tail             highest sequence accepted and assigned
persistedThrough highest contiguous sequence durably stored in the journal
nextToSend       next sequence the current network connection may write
ackedThrough     highest cumulative sequence durably received by the peer
```

The journal flusher and network sender are independent consumers of the same
log:

- The network cursor normally follows `tail`, sending each new head as soon as
  the socket is available. It does not wait for `persistedThrough`; this
  preserves reliablemq's send-first fast path.
- The journal cursor advances in batches. It persists payload, metadata, and
  the latest cumulative ACK/checkpoint state without delaying network output.
- If the network cursor stalls, the journal cursor may pass it. Once those
  frames are persisted they may be evicted from hot memory; the sender reads an
  evicted head back from the journal when the network becomes writable.
- If the journal cursor temporarily trails the network cursor, frames remain in
  hot memory until journaled or cumulatively ACKed. A daemon crash can lose this
  unflushed tail; reconnect reconcile handles the peer-ahead case by advancing
  the producer sequence/checkpoint. This is an explicit consequence of the
  send-first contract, not an exactly-once guarantee.

Only `ackedThrough` is the durable delivery high-water mark. A per-frame
`sent` state is optional in-memory diagnostics and must not generate one
database patch per network write. Batch persistence coalesces a newly inserted
frame with the latest known status, and cumulative ACK persistence supersedes
stale pending/sent row state. A database row that still says pending after it
was sent is safe: it may be replayed, and the peer deduplicates it by transport
key.

`MarkSent` is removed from the required durable-store correctness path;
`StatusSent` may remain temporarily for schema compatibility but replay must
treat pending and legacy sent rows identically. Per-frame send failures are
connection telemetry/in-memory diagnostics, not synchronous database patches.
The producer-owned network cursor replaces public concurrent calls to
`ReplayOutbound`; tunnel code asks the cursor to resume after reconcile instead
of running a second replay sender.

Each journal batch must atomically converge newly persisted frames, the next
outbound sequence, and cumulative ACK/checkpoint state. It must be valid for an
ACK to arrive before its frame batch flushes: the eventual batch stores the
frame already covered by the high-water mark or omits/collects it according to
the journal retention policy, without resurrecting it as unacked work.

The implementation must define hot-entry eviction and flush-failure limits in
bytes and age. It must never silently drop an accepted frame merely to stay
within a memory limit. If the journal is unavailable long enough to cross the
configured safety limit, the owning connection/daemon enters an explicit
degraded or fatal state; this is operational backpressure, not a hidden block
inside `Send`.

### Single network writer

Exactly one connection-owned network writer serializes WebSocket writes. The
data sender advances `nextToSend` one sequence at a time; a write failure keeps
that sequence at the head and disables the cursor until reconnect. Later data
cannot bypass it.

ACK envelopes are not entries in the outbound ACP sequence, but they use the
same connection-owned socket writer so data and ACK writes never race on the
WebSocket. ACK scheduling may have priority over data, but it must not mutate
data ordering.

## 13. Tunnel reconnect and replay barrier

Reconnect does not have separate "replay sender" and "live sender" phases.
Both backlog and new output are read by the same network cursor:

```text
WebSocket connected
  -> reliablemq producer reconcile
  -> replay manager-to-paxd received/unapplied frames through ACPRouter
  -> set outbound nextToSend = peer ackedThrough + 1
  -> enable the one network writer/cursor
       -> read exact head from hot log, otherwise journal
       -> send head; advance only after successful socket write
       -> continue through backlog and then follow tail
  -> declare tunnel ready for new manager work
```

Requirements:

- Outbound slot frames continue to enqueue and journal while disconnected.
- No slot, replay helper, or reconnect path writes ACP data directly to the new
  WebSocket.
- Journal reads loop until the cursor catches `tail`; a read limit such as 1000
  is only a batch size.
- There is no replay/live handoff and no write lock protecting such a handoff.
  Newly accepted frames append after the existing head and are naturally sent
  later by the same cursor.
- Reconnect never prefers in-memory "live" data over an older journal row. For
  each `nextToSend`, the producer resolves that exact sequence from hot memory
  or journal before it can advance.
- Manager must not publish the ACP tunnel into the claimable hub until its
  reconcile side has completed. This is the manager-side recovery barrier.
- A new prompt arriving before the barrier is either held by manager's turn
  queue or receives a transport-recovering response. It is never dispatched
  into paxd early.

Tunnel disconnect, slot process exit, and paxd daemon crash are separate events:

- tunnel disconnect: slot processes and their committed bindings are retained;
- slot process exit: admission closes immediately and that process epoch's
  bindings are cleared eagerly;
- daemon restart: the daemon-start barrier guarantees all prior ACP processes
  are gone and clears every persisted binding before replay, while retaining
  native session IDs and resume descriptors.

Exactly-once continuation of an operation across a full daemon crash is not
claimed. A frame is replayable once it is journaled, and a frame durably
received by the peer is covered by reconcile even if the producer's local
journal was behind. A frame accepted only into the producer's unflushed hot
tail can be lost if the whole daemon crashes before either side durably records
it. This is the explicit availability/durability tradeoff required by the
non-blocking send-first API; callers must not describe successful `Send` as a
durable application commit. Ambiguous application side effects must be
surfaced rather than silently creating a replacement session.

## 14. Desired slot reconciliation

`ACPSlotSupervisor` is the ACP-specific wrapper around the existing generic
supervisor. Its store reads enabled/running `agent_connection` rows and
synthesizes exact `ACPSlotSpec` values from each row's
`desired_acp_slots`:

```go
type ACPSlotSpec struct {
    SlotID               string
    ConnectionID         string
    Ordinal              int
    CommandFingerprint   string
    Command              []string
    WorkingDir           string
    Env                  map[string]string
}
```

### Scale up

If desired count changes from N to M where M > N:

1. Create/reuse logical ordinals `[N, M)`.
2. Start each process independently.
3. Initialize the process.
4. Add the slot to routing only after it is ready.

The tunnel remains connected throughout.

### Scale down

If M < N:

1. Select highest ordinals first.
2. Mark them draining.
3. Stop idle draining slots immediately.
4. Let active turns finish or be explicitly canceled; do not kill them merely
   because desired count changed.
5. Before process termination, clear that process epoch's indexed bindings.
6. Retain slot status/ordinal identity for later scale up.

### Command/config change

Restart affected slots when `CommandFingerprint` changes. The generic
supervisor's injected desired-change operation triggers the replacement; it
does not use a pool generation. A
rolling replacement is preferred: keep at least one ready slot when
possible. Each restarted slot gets a new process epoch and its old bindings are
cleared.

## 15. pax-manager session demultiplexing

### Existing behavior and exact gap

pax-manager already has useful session-aware pieces:

- ACP middleware translates manager session IDs and native session IDs;
- `session/new` pending state binds a native result to a manager session;
- SSE subscribers carry a manager session ID;
- reliablemq dispatch is ordered and deduplicated for the connection queue.

Those pieces do not yet make one shared paxd tunnel multi-session safe. The
current live state still has one mutable `sessionID`, one tunnel-wide `paired`
bit, one raw `userWS`, and response waiters that do not retain session context.
`withSessionContext` temporarily overwrites the tunnel session. SSE delivery
also treats a frame without `sessionId` as matching every subscriber. That is
safe only while the tunnel-wide single-active gate prevents overlap.

The manager change in this plan is an ACP-layer session mux after reliablemq
dispatch. It is not a transport partition and does not reorder or independently
ACK sessions.

### Routing key resolution

Every manager-originated request must register its manager session before it is
sent:

```text
request_id -> manager_session_id + response_waiter + request_kind
```

Manager-originated request IDs remain globally unique within one physical
agent tunnel. The mux
resolves an inbound frame's manager session in this order:

1. An explicit native `sessionId`, translated by the existing middleware to a
   manager session ID.
2. For a JSON-RPC response without `sessionId`, the pending request ID entry.
3. For `session/new`, the existing pending-session-new entry and the native ID
   returned in the result.

`ACPPoolCapabilityReport` does not enter this frame-resolution chain. It arrives
through the existing node-status API, where manager upserts it by connection ID
and report generation. Manager uses it for readiness, compatibility, and
future routing decisions, but does not reply with or synthesize worker
initialization. It has no response waiter or SSE delivery path.

An unclassified sessionless frame fails closed and is logged without payload.
The mux must never use mutable tunnel `currentSessionID` as a fallback and must
never broadcast a missing-session response to every SSE subscriber.

For `session/new`, the manager session ID comes from that call's explicit
request context and is stored in pending state before send. Concurrent code
must not allocate or discover it through tunnel-global
`ensureManagerSessionID` state.

For a worker-originated client request, manager stores the unchanged raw request
ID with manager session identity and resolves the corresponding native session
when sending the response. The response carries that native session ID in
durable reliablemq dispatch metadata. Manager does not allocate a replacement
ID for this path.

### Per-session state

Replace tunnel-wide business state with explicit session-owned state:

```text
sessions[manager_session_id]
  -> active turn/admission state
  -> SSE subscribers
  -> optional conversation/raw consumer

pending[request_id]
  -> manager_session_id
  -> response waiter / request kind

worker_client_requests[native_session_id, raw_request_id]
  -> manager_session_id + native_session_id
  -> approval / client-request state
```

The physical tunnel and reliablemq engine remain connection-owned. There is
still one connection network writer. Session consumers receive immutable frame
copies after middleware translation; one slow session consumer must not hold
the network writer or another session's registry operation.

No request goroutine writes the WebSocket under `agentWriteMu`/`userWriteMu` in
the concurrent path. It submits to the connection-owned producer or session
consumer; the relevant single owner performs the write/delivery.

Each per-session consumer preserves reliablemq dispatch order for that session.
A bounded SSE/client mailbox may disconnect only its own slow subscriber with
an explicit retry/resync signal; it must not silently drop a frame, block the
transport dispatcher, or affect another session.

The existing `paired bool` is replaced for conversation/SSE traffic by
per-session turn admission:

- two different manager sessions may own active turns concurrently;
- one manager session may own at most one prompt turn;
- release, timeout, and disconnect clean up only that session and its pending
  waiters, not unrelated sessions;
- paxd `slot_busy` becomes a typed retryable conversation result or enters an
  explicit manager turn queue; the tunnel writer never spins or sleeps to
  retry it.

History projection, logging context, waiter delivery, and SSE delivery use the
resolved manager session ID carried by the frame dispatch context. They do not
read `ACPTunnelAgent.sessionID`.

Legacy raw user WebSocket tunnels remain single-claim until their request-ID
and session-context isolation is separately proven. The first concurrent path
is the structured conversation/SSE API.

### Recovery and compatibility

The paxd pool is first exercised with the existing one-active-agent gate. The
gate is removed only after the mux tests pass. A newly reconnected tunnel is not
claimable for new turns until manager-side reliablemq reconcile and inbound
replay complete and the current pool readiness/capability report is available.

The manager/native session middleware remains the only cloud-side ID
translation authority. `session_route_missing` is handled once by rebuilding
the lifecycle descriptor, sending explicit `session/resume`, and retrying the
original operation with a new request ID. It never falls back to
`session/new`.

This mux guarantees same-session routing and admission order at the manager
business boundary. Because reliablemq remains one connection-level sequence,
an absent/stalled transport head can still delay all sessions during recovery.
Removing that cross-session transport head-of-line behavior would require a
future partition key in envelopes, journals, ACKs, queue state, reconcile, and
both services; it is explicitly outside this plan.

## 16. Implementation slices

Each slice should be a small reviewable change that leaves tests green and
produces a named, runnable pax-manager+paxd version combination. Breaking
changes between combinations are acceptable; do not spend complexity preserving
old/new peer interoperability beyond the explicitly named bridge points. Write
the BDD scenario first, then the failing test, then implementation.

### Slice 0: Characterization and rollout contracts

- Add this plan's core scenarios to module `BDD.md` files.
- Characterize current persistent-process survival across tunnel reconnect.
- Characterize current reconcile/replay order and the 1000-frame batch behavior.
- Add narrow interfaces for clock, ID generation, route store, process runner,
  and ACP JSON-RPC inspection.
- Add a visible compatibility/version gate for the structured manager path:
  manager may require a paxd capability generation before starting lifecycle
  work.
- No production behavior change.

Stable combination:

- `slot-pool-baseline`: current manager + current paxd behavior.

Acceptance:

- Existing tests remain green.
- New characterization tests demonstrate current ordering and lifecycle.
- The compatibility gate exists but remains permissive until later slices.

### Slice 1: paxd-owned initialize on the existing single worker

- Move worker `initialize` ownership into paxd while still using the current
  single persistent ACP process path.
- Build the connection-scoped paxd client-init descriptor locally, canonicalize
  it, compute `client_profile_hash`, send an internal initialize request to the
  one worker, consume the response inside paxd, and compute `worker_result_hash`.
- Store the local init profile/result record and emit a sanitized
  `ACPPoolCapabilityReport` through node-status/control reporting. The report
  must include enough non-secret detail to prove how initialization happened:
  connection ID, report generation, paxd version, command fingerprint, profile
  hash, result hash, protocol version, sanitized client capability keys,
  sanitized worker capability keys, initialization phase, initialized-at time,
  and last error code/message.
- Keep full raw initialize params/result local only; never put command env, MCP
  secrets, or raw descriptor JSON in manager status.
- Update pax-manager structured conversation code to stop sending ACP
  `initialize`. It waits for a compatible `ACPPoolCapabilityReport`, then starts
  with `session/new` or `session/resume`.
- Legacy raw ACP proxy behavior may be removed or may answer `initialize` from
  the cached result, but it must not forward manager initialize to the worker.

Stable combination:

- `paxd-owned-init-v1`: updated paxd + updated manager. One tunnel, one worker,
  manager sends no worker initialize, sessions still flow through the old process
  owner.

Acceptance:

- A worker receives exactly one initialize per process start, generated by paxd.
- Structured manager session creation sends no ACP `initialize`.
- Manager can show that paxd initialized the worker from the capability report
  without seeing raw secrets.
- Reconnect/report refresh does not reinitialize a healthy worker.
- Incompatible or failed initialize leaves the agent unavailable with a typed,
  sanitized status.

### Slice 2: Process identity, daemon lock, and safe startup barrier

- Add `ProcessIdentity`, OS start token, exclusive daemon lock, and `Identity()`.
- Start Unix ACP processes in a dedicated process group.
- Terminate the process group with graceful then forced escalation.
- Add the safe daemon-start orphan termination barrier; never signal from a
  persisted PID without verifying its process birth identity.

Stable combination:

- `single-worker-owned-init-locked`: same single-worker behavior as Slice 1, with
  daemon ownership and orphan cleanup safety.

Acceptance:

- A subprocess holding the daemon lock makes a second acquisition return
  `ErrAlreadyRunning` before SQLite open/migrate.
- Explicit release permits immediate reacquisition.
- Force-killing the owner permits bounded reacquisition while an ACP-like child
  remains alive, proving the lock descriptor/handle was not inherited.
- A stale lock file with no kernel lock does not block startup.
- Unix and Windows CI run the subprocess crash-release tests; unsupported OS
  builds fail explicitly rather than silently using a PID file.
- A spawned child shares the ACP PGID on Unix.
- Group termination stops root and child.
- Daemon restart cannot retain or reattach a prior ACP process.
- PID reuse cannot cause an unrelated process to be signaled.
- Tests use testify `assert`/`require`.

### Slice 3: SQLite slot and route state

- Add models, migrations, repository ports, and transactional route operations.
- Backfill/default every existing agent connection to one desired slot.
- Add desired-slot update validation and supervisor wake, but keep values above
  one rejected or feature-gated until the real slot path is active.
- Add route version compare, indexed bulk process-binding clear, and the startup
  transaction that clears every binding.
- Reuse the Slice 1 init profile store instead of adding a second copy.

Stable combination:

- `single-worker-with-slot-state`: production still runs one worker, but the
  durable state required by slot routing exists and can be inspected.

Acceptance:

- Store tests cover create, bind, clear by process epoch, clear all on daemon
  start, conflict, scale count validation, and descriptor persistence.
- Sensitive lifecycle data never appears in status projections.
- Desired-slot-only updates do not rotate the transport queue or restart the
  physical tunnel.

### Slice 4: Standalone `ACPSlot` and router core with fakes

- Extract process/stdin/stdout ownership from `persistentACPProcess` into a
  standalone `ACPSlot`.
- Add slot state machine, process epoch, paxd-owned initialize, prompt/lifecycle
  lease, and event sink.
- Implement ACPRouter classification, pending `session/new`, sticky routes, cold
  resume, active prompt tracking, cancellation bypass, and pending
  `(native_session_id, raw request ID) -> slot/process epoch` validation against
  fake slots first.
- Add an internal RPC waiter for initialize/resume responses.
- Do not connect the slot/router to the real tunnel yet.

Stable combination:

- `router-core-dark`: no production traffic uses the router yet; unit and fake
  integration tests prove routing semantics.

Acceptance:

- Process exit closes waiters and emits one terminal epoch event.
- One active prompt lease is enforced under race tests.
- Two sessions on different fake slots may both issue request ID `1`; each
  unchanged response returns only to its source slot.
- A new-session response is not emitted before route commit.
- A cold prompt performs resume first with the original lifecycle descriptor.
- Resume failure never emits `session/new`.
- An unknown pre-migration route returns `session_route_missing` and does not
  reach ACP stdin.

### Slice 5: `ACPSlotSupervisor` and count-one real slot path

- Refactor the existing generic supervisor to inject desired-change detection,
  serialize all reconcile entry points, and support drain-aware stop/replacement.
- Implement `ACPSlotSupervisor` as a typed specialization of that generic
  supervisor, keyed by `slot_id`; do not introduce a separate reconcile loop
  framework.
- Reconcile exact desired slot count from SQLite, but enable the real path first
  at count one.
- Make one `ACPPool` discoverable by `connection_id` through a registry.
- Wire `AgentTunnelSession` to the pool/router at desired count one and remove or
  disable the old persistent-process owner for that version combination.
- Keep manager single-active.

Stable combination:

- `slot-path-count-one`: one tunnel, one `ACPSlot`, paxd-owned initialize,
  sticky route table active, manager still single-active.

Acceptance:

- Existing remote and tunnel supervisor behavior remains covered by its current
  tests after the generic refactor.
- Count one works end to end through the new slot/router path.
- Restarting the one slot creates a new process epoch, clears old bindings, and
  resumes on first use.
- Active scale-down/drain behavior is tested even if production count remains
  one.
- Stale slot exits cannot overwrite a newer process epoch.

### Slice 6: Reliable producer and single network writer

- Change `reliablemq.Engine.Send` and its host-facing interface to return only
  acceptance error, not `Frame`.
- Update `paxkit/docs/reliablemq-design.md`, reliablemq BDD scenarios, and both
  host adapters to the send-first acceptance/crash-window contract in the same
  slice; do not leave the old persist-before-send promise in package docs.
- Bootstrap producer sequence/checkpoint before publishing it to callers.
- Add the non-blocking multi-producer ingress, single sequencer, asynchronous
  journal flusher, and single network cursor to reliablemq. Do not add an ACP
  outbox package.
- Remove synchronous `Sender.Send` and per-frame durable `MarkSent` from the
  caller path. Keep cumulative ACK persistence and batch-coalesced frame state.
- Route all data and ACK WebSocket writes through one connection-owned writer.
- Route reliable inbound frames through `ACPRouter` and route all translated slot
  stdout into the shared reliable producer.
- Implement cursor-based reconnect, full journal batching, and the
  recovery-ready barrier.

Stable combination:

- `slot-path-count-one-async-producer`: still one slot in production, but the
  final transport architecture is active and observable.

Acceptance:

- `Send` returns while the journal sink and socket writer are deliberately
  blocked, and accepted messages retain producer order.
- Concurrent fake slot producers cannot block on a full ingress channel and
  produce unique, contiguous sequence numbers.
- The network cursor does not wait for journal flush in the healthy send-first
  path.
- A failed head is never bypassed by a later frame.
- `sent` does not cause one durable patch per frame; cumulative ACK and batch
  flush converge stale pending rows safely.
- Slot processes survive transient tunnel reconnect.
- More than 1000 frames replay fully and in order.
- Output created during replay cannot overtake the backlog.
- A prompt cannot enter before the recovery barrier.

### Slice 7: Multi-slot under manager single-active gate

- Allow desired slots above one.
- Create multiple sessions and verify deterministic distribution/stickiness.
- Restart one slot and verify only its sessions become cold and resume.
- Scale down and back up without reconnecting the physical tunnel.
- Keep pax-manager's single-active-agent gate in place, so this validates pool
  lifecycle and recovery before business concurrency is opened.

Stable combination:

- `multi-slot-single-active`: one tunnel, N slots, one active manager turn at a
  time.

Acceptance:

- No existing manager API behavior regresses at default slot count one.
- Sticky routing and resume work end to end.
- Tunnel and slot status remain independently observable.
- desired slot count changes through SQLite without restarting the physical
  tunnel.

### Slice 8: pax-manager session mux, still gated

- Introduce the connection-owned session mux and carry resolved manager session
  identity in every frame/call context.
- Add native session ID to reliablemq metadata when manager sends a response to
  a worker-originated request; preserve it through journal and replay.
- Route manual approval, auto-approval, and reusable-grant responses through one
  typed session-aware worker-response sender; prohibit direct raw tunnel writes
  for these responses.
- Change response waiter registration to retain request ID, manager session ID,
  and request kind.
- Route explicit-session notifications exactly to that session; route
  sessionless responses through request-ID correlation; enumerate sessionless
  control frames and fail closed for unknown ones.
- Remove `withSessionContext` and all routing/history fallbacks to mutable
  tunnel `sessionID` from the structured conversation path.
- Key SSE/conversation consumers by manager session and remove the
  `sessionID == ""` broadcast-to-all fallback.
- Keep legacy raw tunnels single-claim.
- Keep the per-agent concurrency gate enabled.

Stable combination:

- `session-mux-gated`: manager has correct per-session routing internals, but
  still admits only one active turn per agent.

Acceptance:

- Interleaved responses without `sessionId` reach only the waiter/session that
  owns each request ID in tests.
- Interleaved notifications with explicit session IDs reach only matching SSE
  subscribers.
- Unknown sessionless frames are never broadcast to all sessions.
- Starting two manager sessions sends no worker initialize request; both use the
  same reported pool capability generation.
- Closing or timing out one session does not release or cancel another.

### Slice 9: Enable concurrent turns

- Replace the agent-wide `paired`/single-active gate with per-session turn
  admission for the structured conversation/SSE API.
- Add busy handling and reconnect-ready gating.
- Add one-shot `session_route_missing` resume-and-retry for pre-migration
  sessions before enabling the new path broadly.
- Enable only for internal/dev nodes first.

Stable combination:

- `multi-slot-concurrent`: one tunnel, N slots, multiple structured manager
  sessions may prompt concurrently.

Acceptance:

- Two manager sessions assigned to different slots can prompt concurrently.
- Two prompts targeting one session remain serialized.
- A busy sticky slot cannot corrupt or reroute another active session.
- Closing or timing out one session does not release or cancel another.

### Slice 10: Metrics integration point

- Expose read-only `ACPSlotSnapshot` values containing slot ID, process epoch,
  process identity, phase, active native session ID, active tool-call ID, and
  last ACP activity time.
- Do not collect or upload CPU/memory in this slice.

Acceptance:

- Metrics work can consume snapshots without importing router internals or
  mutating slot state.

## 17. Test strategy

All new backend packages and materially changed packages require at least 80%
unit-test coverage for new code. Prefer BDD scenario naming and testify
`assert`/`require`.

### Unit tests

- daemon-lock contention, explicit release, stale-file behavior, and error
  classification;
- process group identity and termination;
- slot state transitions and invalid transitions;
- initialize once per process epoch;
- canonical paxd client-init persistence and hash stability with extension
  fields;
- distinct internal initialize request IDs for separate process epochs;
- reconnect capability re-reporting does not reinitialize workers;
- manager initialization input cannot mutate the local client profile;
- an incompatible worker initialize result keeps only that slot non-ready;
- request-ID string/number preservation;
- slot selection and deterministic tie-breaks;
- route version conflicts;
- worker-request `(native session, raw ID)` source registration, response
  cleanup, expiry, raw ID type preservation, and stale process-epoch rejection;
- process-epoch binding clear;
- daemon-start clear-all binding transaction;
- prompt lease and cancellation bypass;
- busy error behavior;
- resume descriptor reconstruction;
- no success forwarding before route commit;
- scale-up/down reconciliation with fake clock;
- reliable producer bootstrap before first acceptance;
- non-blocking concurrent producer acceptance and contiguous sequence
  assignment;
- independent journal/network cursor progress;
- network-head failure without later-frame bypass;
- cumulative ACK coalescing over stale pending/sent journal rows;
- hot-memory eviction followed by exact-sequence journal read;
- reconnect cursor reset to peer `ackedThrough + 1`;
- manager response-to-session correlation by request ID;
- exact-session SSE delivery and fail-closed sessionless dispatch;
- per-session turn admission and cleanup isolation.

### Integration tests

Use controllable fake ACP stdio servers, not sleeps:

- a lock-owner helper process is force-killed and a replacement daemon acquires
  the same lock within the bounded retry window;
- a child process kept alive after its lock-owning parent is killed does not
  retain the lock;
- two slots return identical native-looking UUIDs scoped to different test
  connections;
- two slots issue permission requests with original ID `1`, and interleaved
  manager responses return to the correct workers;
- one locally built profile initializes N workers with distinct internal IDs,
  while no initialize frame/response reaches session/SSE routing;
- a worker added after scale-up receives the same current paxd descriptor
  before becoming ready;
- manager receives one sanitized capability report/upsert and sends no ACP
  initialize request;
- a prompt streams updates, performs a long tool call, then completes;
- tunnel disconnects during that tool call while the process remains alive;
- slot exits during a prompt;
- cold route resumes with cwd and MCP config;
- a pre-migration native session has no paxd route, manager explicitly resumes
  it, and the original prompt is retried only once;
- more than 1000 transport frames are pending;
- slot output is produced while reconnect replay is reading journal backlog and
  cannot overtake it;
- journal flush is blocked while healthy network sending continues;
- network is blocked while journal batching advances and hot entries are
  evicted;
- two manager sessions share one blank-session paxd tunnel, issue interleaved
  prompts, and receive only their own responses and updates;
- desired slot count changes while one slot is active;
- paxd restart kills prior ACP processes, clears all bindings, retains resume
  descriptors, and resumes on first use.

### Concurrency checks

Run relevant packages under `go test -race`. Tests must use channels/fake clocks
instead of timing-sensitive sleeps wherever possible.

## 18. Rollout

Breaking version combinations are acceptable, but every rollout step must name a
known-good manager+paxd pair and keep a rollback target.

1. Ship `paxd-owned-init-v1`: paxd initializes the existing single worker,
   manager no longer sends structured ACP `initialize`, and manager surfaces the
   sanitized capability report. This is the first operator-verifiable breaking
   pair.
2. Ship `single-worker-owned-init-locked`: add daemon lock, process identity, and
   safe startup cleanup without changing manager behavior.
3. Ship `single-worker-with-slot-state`: add desired-slot, route, slot status,
   and startup binding-clear storage while production still runs one worker.
4. Ship `router-core-dark`: merge router/slot unit behavior behind fakes only.
5. Ship `slot-path-count-one`: route real traffic through one `ACPSlot` and the
   route table, keep manager single-active, and remove or disable the old
   persistent-process owner for that pair.
6. Ship `slot-path-count-one-async-producer`: switch to the final reliable
   producer/single-writer transport while still running one production slot.
7. Ship `multi-slot-single-active`: allow desired slot count above one, validate
   stickiness, resume, scale up/down, and tunnel reconnect while manager still
   admits only one active turn per agent.
8. Ship `session-mux-gated`: manager uses the per-session mux internally but the
   external concurrency gate remains enabled.
9. Ship `multi-slot-concurrent` for internal/dev nodes.
10. Roll out broadly only after reconnect, replay, demux, and race tests are
    stable.

Useful diagnostics, with no sensitive payloads:

```text
connection_id
slot_id
slot_ordinal
process_epoch
phase
native_session_id_hash
request_direction
request_method
transport_seq
recovery_phase
```

## 19. Explicitly deferred

- CPU/memory/IO metrics collection and pax-manager metrics tables;
- automatic killing based on resource use;
- Linux cgroup and Windows Job Object accounting;
- automatic min/max load-based scaling;
- automatic slot idle reap and warm-pool policy;
- session archive/release policy;
- consistent hashing;
- multiple physical ACP tunnels for one agent connection;
- aggregation of `session/list` across slots;
- arbitrary legacy client capability negotiation;
- exactly-once continuation across a full paxd daemon crash;
- ACP request or conversation API idempotency guarantees;
- changing `queue_id` ownership from connection to slot or session;
- per-session reliablemq sequence, ACK, journal, reconcile, or replay
  partitions;
- allowing one session to bypass another session's missing transport sequence;
- concurrent legacy raw user WebSocket claims.

Idle reap can be added after the exact-count pool is stable. The later policy
should stop only a ready slot with no active lease, mark its routes cold, and
rely exclusively on `session/resume` when those sessions are used again.

## 20. Definition of done

The ACP slot foundation is complete when:

1. one agent tunnel can route multiple native sessions across multiple ACP
   slots;
2. each slot process epoch is initialized once from the paxd-owned connection
   client-init descriptor; manager receives a sanitized capability report and
   no initialize request/response is session-broadcast;
3. committed bindings survive tunnel reconnect, while paxd restart clears all
   bindings but retains native IDs and resume descriptors;
4. slot process restart clears its bindings and the next operation resumes
   before prompting;
5. one slot never runs two prompts concurrently;
6. colliding worker-originated request IDs cannot misroute permission/client
   responses across slots;
7. replay completes before new prompts are admitted;
8. slot count changes through SQLite without restarting the physical tunnel;
9. the old persistent-process owner has been removed;
10. reliablemq has one asynchronous producer queue and one cursor-based network
    path for replay and live output, with no ACP-specific outbox;
11. `Engine.Send` never waits for journal or network I/O, the network never
    bypasses its head, and cumulative ACK persistence makes stale sent state
    replay-safe;
12. two structured manager sessions can share one paxd tunnel without mutable
    session fallback, response/SSE cross-delivery, or tunnel-wide turn
    admission;
13. new backend code meets the 80% unit coverage requirement and relevant tests
    pass under the race detector.
