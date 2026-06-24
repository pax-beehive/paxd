# internal/supervisor

`internal/supervisor` reconciles SQLite desired state to actual runtime state.

It owns:

- `RemoteSupervisor`, which reconciles `remotes` into node-control WebSocket slots
- `AgentConnectionSupervisor`, which reconciles `agent_connections` into ACP tunnel slots
- interruptible backoff
- generation/restart nonce conflict handling
- runtime slot lifecycle

This package must not:

- parse local HTTP or remote WebSocket control messages
- mutate desired state except through explicit status updates
- implement ACP frame bridging itself

## Slot Model

Each desired runtime has one slot.

The slot owns the long-lived lifecycle:

- current session handle
- pending desired spec
- interruptible backoff timer
- restart and stop decisions

Sessions only own one concrete connection attempt and must return a classified exit to the slot.

## Reconcile Triggers

Supervisors should reconcile on:

- explicit wake from `control.Service`
- periodic ticker fallback
- session exit/reap events

Desired-state command success does not wait for runtime completion.

## Primary Interfaces

Supervisor entrypoints:

```go
type Supervisor interface {
    Start(ctx context.Context) error
    Wake()
    Snapshot() Snapshot
}

type Snapshot struct {
    Slots []SlotSnapshot
}

type SlotSnapshot struct {
    ID             string
    Generation     int64
    RestartNonce   int64
    Phase          string
    BackoffUntil   *time.Time
    PendingDesired bool
}
```

Remote supervisor dependencies:

```go
type RemoteStore interface {
    ListDesiredRemotes(ctx context.Context) ([]runtime.RemoteSpec, error)
    UpsertRemoteStatus(ctx context.Context, update RemoteStatusUpdate) error
    ConditionalRemoteStatusUpdate(ctx context.Context, update RemoteStatusUpdate) (bool, error)
}

type RemoteSessionFactory interface {
    NewRemoteControlSession(spec runtime.RemoteSpec) runtime.Session
}
```

Agent connection supervisor dependencies:

```go
type AgentConnectionStore interface {
    ListDesiredAgentConnections(ctx context.Context) ([]runtime.AgentConnectionSpec, error)
    UpsertAgentConnectionStatus(ctx context.Context, update AgentConnectionStatusUpdate) error
    ConditionalAgentConnectionStatusUpdate(ctx context.Context, update AgentConnectionStatusUpdate) (bool, error)
}

type AgentTunnelSessionFactory interface {
    NewAgentTunnelSession(spec runtime.AgentConnectionSpec) runtime.Session
}
```

Runtime slot shape:

```go
type RuntimeSlot[S any] interface {
    ID() string
    ApplyDesired(spec S)
    Stop(reason string)
    Snapshot() SlotSnapshot
}
```

Concrete slot types should bind `S` to `runtime.RemoteSpec` or `runtime.AgentConnectionSpec`. The important boundary is that slots own long-lived lifecycle and sessions own only one concrete attempt.

Use an injectable clock/timer abstraction for backoff:

```go
type Clock interface {
    Now() time.Time
    NewTimer(d time.Duration) Timer
}

type Timer interface {
    C() <-chan time.Time
    Stop() bool
    Reset(d time.Duration) bool
}
```
