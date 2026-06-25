# internal/controlws

`internal/controlws` is the remote node-control WebSocket adapter.

Each enabled remote owns one node-control WebSocket between pax-manager and paxd. This transport is for daemon and machine-level control, not ACP data-plane traffic.

This package owns:

- WebSocket frame parsing
- command ACK encoding
- best-effort command result encoding
- query response encoding
- remote source attribution (`source=remote`, `remote_id=...`)

This package must not:

- write desired-state tables directly
- start or stop agent tunnels directly
- own ACP tunnel payload handling
- implement business validation independently from `internal/control`

All business behavior should be delegated to `control.Service`.

## Primary Interfaces

The adapter should expose a session-level runner called by `RemoteControlSession`:

```go
func Run(
    ctx context.Context,
    conn WebSocketConn,
    src control.Source,
    control control.Service,
) runtime.Exit
```

Use a narrow WebSocket interface so tests can use fakes:

```go
type WebSocketConn interface {
    ReadMessage() (messageType int, payload []byte, err error)
    WriteMessage(messageType int, payload []byte) error
    Close() error
}
```

Frame structs should be typed at the adapter boundary:

```go
type Frame struct {
    Kind      string
    RequestID string

    Command *control.Command
    Query   *control.Query
}

type AckFrame struct {
    Kind      string
    CommandID string
    Accepted  bool
    Error     *control.ControlError
}
```

`controlws` should decode frames into typed `control.Command` or `control.Query` before calling `control.Service`.

## Message Families

- Query/request frames are direct request/response operations and do not enter `control_commands`.
- Command frames mutate desired state, enter `control_commands`, and receive an immediate ACK after durable acceptance.

Runtime completion is observed through status polling or best-effort `command_result` frames.
