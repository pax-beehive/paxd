# internal/control

`internal/control` is the shared business layer for paxd local and remote control.

All transports must enter paxd control behavior through this package:

- Unix socket local API
- optional `127.0.0.1` debug HTTP API
- remote node-control WebSocket

This package owns:

- typed command/query contracts
- command validation
- mutating desired-state transactions
- command idempotency
- supervisor wakeups
- read/query orchestration

This package must not:

- own WebSocket or HTTP protocol details
- start or stop goroutines directly
- own live tunnel/process handles
- pass `[]byte` or `json.RawMessage` across the service boundary

## Shape

Commands and queries should use explicit oneof-like structs:

```go
type Service interface {
    HandleCommand(ctx context.Context, src Source, cmd Command) (CommandAck, error)
    HandleQuery(ctx context.Context, src Source, query Query) (QueryResult, error)
}

type Command struct {
    CommandID string
    Type      CommandType

    CreateRemote  *CreateRemoteCommand
    UpdateRemote  *UpdateRemoteCommand
    DeleteRemote  *DeleteRemoteCommand
    RestartRemote *RestartRemoteCommand

    ConfigureRemoteAuth *ConfigureRemoteAuthCommand
    ClearRemoteAuth     *ClearRemoteAuthCommand

    CreateAgentConnection  *CreateAgentConnectionCommand
    UpdateAgentConnection  *UpdateAgentConnectionCommand
    DeleteAgentConnection  *DeleteAgentConnectionCommand
    RestartAgentConnection *RestartAgentConnectionCommand

    UpgradePaxd *UpgradePaxdCommand
}
```

Exactly one payload field should be set, and it must match `Type`.

Query structs should follow the same pattern. Transport adapters decode wire payloads into these typed structs before calling `Service`.

## Dependencies

`control.Service` may depend on narrow ports for:

- `daemonstore`
- `supervisor`
- `harnessregistry`
- `localsessions`

Transport packages should depend on `control`, not on stores or supervisors directly.

## Primary Interfaces

`control.Service` should depend on ports rather than concrete stores or runtimes:

```go
type Store interface {
    WithTx(ctx context.Context, fn func(Tx) error) error

    GetCommand(ctx context.Context, commandID string) (*CommandView, error)
    InsertCommand(ctx context.Context, command CommandRecord) error
    CompleteCommand(ctx context.Context, commandID string, completion CommandCompletion) error

    ListRemotes(ctx context.Context, filter RemoteFilter) ([]RemoteView, error)
    CreateRemote(ctx context.Context, cmd CreateRemoteCommand) (RemoteView, error)
    UpdateRemote(ctx context.Context, cmd UpdateRemoteCommand) (RemoteView, error)
    DeleteRemote(ctx context.Context, cmd DeleteRemoteCommand) (RemoteView, error)
    RestartRemote(ctx context.Context, cmd RestartRemoteCommand) (RemoteView, error)

    ConfigureRemoteAuth(ctx context.Context, cmd ConfigureRemoteAuthCommand) error
    ClearRemoteAuth(ctx context.Context, cmd ClearRemoteAuthCommand) error

    ListAgentConnections(ctx context.Context, filter AgentConnectionFilter) ([]AgentConnectionView, error)
    CreateAgentConnection(ctx context.Context, cmd CreateAgentConnectionCommand) (AgentConnectionView, error)
    UpdateAgentConnection(ctx context.Context, cmd UpdateAgentConnectionCommand) (AgentConnectionView, error)
    DeleteAgentConnection(ctx context.Context, cmd DeleteAgentConnectionCommand) (AgentConnectionView, error)
    RestartAgentConnection(ctx context.Context, cmd RestartAgentConnectionCommand) (AgentConnectionView, error)

    GetDaemonStatus(ctx context.Context) (DaemonStatus, error)
}

type Supervisors interface {
    WakeRemotes()
    WakeAgentConnections()
    WakeACPSlots()
}

type HarnessRegistry interface {
    ListCached(ctx context.Context) ([]HarnessView, error)
    Discover(ctx context.Context, req DiscoverHarnessesQuery) ([]HarnessView, error)
}

type LocalSessions interface {
    List(ctx context.Context, query ListLocalSessionsQuery) ([]LocalSessionView, error)
    Sync(ctx context.Context, query SyncLocalSessionsQuery) (LocalSessionSyncResult, error)
    Get(ctx context.Context, query GetLocalSessionQuery) (*LocalSessionView, error)
}
```

These interfaces are intentionally business-shaped. Transport-specific request structs should not leak into them.

## Harness authentication

`harness_auth.login` and `harness_auth.status` expose short-lived Claude and Codex native CLI
login sessions through the shared control layer. Login commands bypass the
durable command journal so authorization codes are never persisted there.
See [the login protocol and lifecycle](../harnessauth/README.md).
