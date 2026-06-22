# internal/daemonstore

`internal/daemonstore` is the GORM-backed store for paxd control-plane desired state and status.

It owns tables such as:

- `remote`
- `remote_auth`
- `remote_status`
- `agent_connection`
- `agent_connection_status`
- `control_command`
- `harness_inventory`
- `local_session`
- `local_session_element`
- `setting`

This package owns:

- GORM models and migrations for new control-plane tables
- typed repository methods used by `internal/control` and supervisors
- transactional desired-state mutations
- conditional status updates guarded by generation/restart nonce

This package must not:

- manage the ACP `transport_journal`
- own live runtime handles
- parse HTTP or WebSocket requests
- resolve secrets directly except through an injected resolver where necessary

The existing `internal/store` raw SQL code should continue to own SQL-heavy ACP transport journal and message-history behavior.

## Primary Interfaces

The concrete GORM store should expose narrow repositories. The exact package may use one struct internally, but callers should depend on focused interfaces.

For `internal/control`:

```go
type ControlRepository interface {
    WithTx(ctx context.Context, fn func(Tx) error) error

    GetCommand(ctx context.Context, commandID string) (*control.CommandView, error)
    GetCommandRecord(ctx context.Context, commandID string) (*control.CommandRecord, error)
    InsertCommand(ctx context.Context, rec CommandRecord) error
    CompleteCommand(ctx context.Context, commandID string, completion CommandCompletion) error

    CreateRemote(ctx context.Context, cmd control.CreateRemoteCommand) (control.RemoteView, error)
    UpdateRemote(ctx context.Context, cmd control.UpdateRemoteCommand) (control.RemoteView, error)
    DeleteRemote(ctx context.Context, cmd control.DeleteRemoteCommand) (control.RemoteView, error)
    RestartRemote(ctx context.Context, cmd control.RestartRemoteCommand) (control.RemoteView, error)

    ConfigureRemoteAuth(ctx context.Context, cmd control.ConfigureRemoteAuthCommand) error
    ClearRemoteAuth(ctx context.Context, cmd control.ClearRemoteAuthCommand) error

    CreateAgentConnection(ctx context.Context, cmd control.CreateAgentConnectionCommand) (control.AgentConnectionView, error)
    UpdateAgentConnection(ctx context.Context, cmd control.UpdateAgentConnectionCommand) (control.AgentConnectionView, error)
    DeleteAgentConnection(ctx context.Context, cmd control.DeleteAgentConnectionCommand) (control.AgentConnectionView, error)
    RestartAgentConnection(ctx context.Context, cmd control.RestartAgentConnectionCommand) (control.AgentConnectionView, error)
}
```

For supervisors:

```go
type RemoteRepository interface {
    ListDesiredRemotes(ctx context.Context) ([]runtime.RemoteSpec, error)
    MarkRemoteRegistered(ctx context.Context, update RemoteRegistrationUpdate) (bool, error)
    UpsertRemoteStatus(ctx context.Context, update RemoteStatusUpdate) error
    ConditionalRemoteStatusUpdate(ctx context.Context, update RemoteStatusUpdate) (bool, error)
}

type AgentConnectionRepository interface {
    ListDesiredAgentConnections(ctx context.Context) ([]runtime.AgentConnectionSpec, error)
    SetAgentConnectionCloudAgentID(ctx context.Context, update AgentConnectionBindingUpdate) (bool, error)
    UpsertAgentConnectionStatus(ctx context.Context, update AgentConnectionStatusUpdate) error
    ConditionalAgentConnectionStatusUpdate(ctx context.Context, update AgentConnectionStatusUpdate) (bool, error)
}
```

Runtime-discovered fields such as `remote.node_id`, `remote.registered_at`, and `agent_connection.cloud_agent_id` must be updated through generation-guarded runtime repository methods. They must not go through desired-state update methods that increment generation.

For auth:

```go
type RemoteAuthRepository interface {
    GetRemoteAuthMaterial(ctx context.Context, remoteID string) (RemoteAuthMaterial, error)
}
```

Auth material returns stored secret refs, not resolved secret values.

For local observation:

```go
type HarnessInventoryRepository interface {
    ListHarnesses(ctx context.Context) ([]control.HarnessView, error)
    UpsertHarnesses(ctx context.Context, harnesses []control.HarnessView) error
}

type LocalSessionRepository interface {
    ListSessions(ctx context.Context, query control.ListLocalSessionsQuery) ([]control.LocalSessionView, error)
    UpsertSessions(ctx context.Context, sessions []control.LocalSessionView) error
    ReplaceSessionElements(ctx context.Context, sessionID string, elements []LocalSessionElement) error
}
```

Repository methods that update observed status from runtime exits must support conditional updates guarded by observed generation and restart nonce.
