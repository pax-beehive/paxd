# internal/localsessions

`internal/localsessions` manages the local-only session observation cache used by TUI and CLI.

It is not a remote manager connection and must not be represented as `remote=localhost`.

This package owns:

- local session listing
- local session sync
- optional local timeline extraction for agents that support it
- cache reads/writes for `local_sessions` and `local_session_elements`

This package must not:

- start ACP tunnel WebSockets
- mutate `agent_connections`
- directly implement TUI rendering
- require a Pax manager remote

The TUI should consume this behavior through the local control API over Unix socket.

## Primary Interfaces

```go
type Service interface {
    List(ctx context.Context, query ListQuery) ([]SessionView, error)
    Sync(ctx context.Context, query SyncQuery) (SyncResult, error)
    Get(ctx context.Context, query GetQuery) (*SessionDetail, error)
}

type Store interface {
    ListSessions(ctx context.Context, query ListQuery) ([]SessionView, error)
    UpsertSessions(ctx context.Context, sessions []SessionView) error
    GetSession(ctx context.Context, sessionID string) (*SessionDetail, error)
    ReplaceSessionElements(ctx context.Context, sessionID string, elements []SessionElement) error
}

type Scanner interface {
    ListSessions(ctx context.Context, agent string, limit int) ([]SessionView, error)
    LoadElements(ctx context.Context, session SessionView) ([]SessionElement, error)
}

type ScannerRegistry interface {
    ScannerFor(agent string) (Scanner, bool)
    AvailableAgents(ctx context.Context) ([]string, error)
}

type ListQuery struct {
    Agent string
    Limit int
}

type SyncQuery struct {
    Agent   string
    Limit   int
    Timeout time.Duration
}

type GetQuery struct {
    SessionID string
}

type SessionView struct {
    ID           string
    Agent        string
    NativeID     string
    Title        string
    Status       string
    Preview      string
    ProjectID    string
    UpdatedAt    *time.Time
    LastActive   *time.Time
    LastListedAt time.Time
    LastSyncedAt *time.Time
}

type SessionDetail struct {
    Session  SessionView
    Elements []SessionElement
}

type SessionElement struct {
    Seq         int64
    Kind        string
    Role        string
    Text        string
    RawJSON     string
    StartedAt   *time.Time
    CompletedAt *time.Time
}

type SyncResult struct {
    Synced int
    Failed int
    Errors []error
}
```

## Boundary

Local session scans are local observation. They should not require a remote, start ACP tunnel WebSockets, or mutate desired agent connection state.
