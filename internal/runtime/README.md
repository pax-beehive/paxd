# internal/runtime

`internal/runtime` contains one-shot runtime session contracts and shared runtime types.

It should model a session as one concrete connection attempt, not a long-lived reconnect loop.

Example session types:

- `RemoteControlSession`: one node-control WebSocket session
- `AgentTunnelSession`: one ACP tunnel WebSocket plus local ACP process session

This package owns:

- runtime specs passed from supervisors
- classified exit types
- one-shot session interfaces
- WebSocket heartbeat/read-deadline behavior for the live connection
- ACP tunnel read/write pumps for one live session

This package must not:

- decide whether to retry
- sleep and reconnect after a broken tunnel
- mutate desired state
- write command records

The slot/supervisor owns long-lived lifecycle and retry decisions.

## Tunnel Handoff

During `Run(ctx, spec)`, a session owns the live WebSocket/process handles.
For `AgentTunnelSession`, this includes the ACP process stdin/stdout/stderr
pipes and the WebSocket tunnel for that one attempt.

When it exits, it must:

- close or release those handles
- return a classified exit
- leave retry/backoff/fail/stop decisions to the slot

The session may be long-lived. "One-shot" means one concrete WebSocket/process
attempt, not a short operation. If that live connection lasts for days,
`Run(ctx)` may run for days. When the connection dies or is canceled, `Run`
returns and the supervisor decides whether to start a new session.

## Heartbeat And Watchdog

Heartbeat belongs in runtime sessions because they own the live WebSocket.

Runtime sessions should:

- send periodic WebSocket ping frames
- refresh read deadlines on pong or data
- close the connection and return `ExitTransient` when heartbeat times out
- cancel the whole session when any read/write/process pump exits unexpectedly

Supervisor slots should observe the resulting classified exit and apply normal
backoff/retry policy. They should not access the WebSocket handle directly.

## ACP Pipeline And Journal

`AgentTunnelSession` owns the pipeline for one live attempt:

```text
local harness stdout -> transport journal -> ACP tunnel WebSocket
ACP tunnel WebSocket -> transport journal -> local harness stdin
```

The transport journal owns replay and duplicate suppression. Runtime should
record outbound frames before sending, mark them sent after a successful
WebSocket write, mark them acked only after manager ACK, and replay unacked
frames after reconnect. Runtime should record inbound frames before dispatching
to stdin, avoid dispatching duplicate frames, and replay `received` but not
`applied` inbound frames when a new local process/session starts.

`connection_id` is the paxd-owned durable local identity for a desired
`agent_connection`. It should be the primary journal identity. `cloud_agent_id`
or `agent_id` is remote manager metadata for routing/debugging, not the local
runtime slot identity.

## E2EE bridge

When a 32-byte root key is configured, `AgentTunnelSession` recognizes version-1
encrypted command envelopes without exposing plaintext to the Manager. The
runtime derives separate command and event keys with HKDF-SHA-256, authenticates
the routing metadata as AES-256-GCM AAD, decrypts immediately before local ACP
dispatch, and encrypts attributable ACP output before it enters the reliable
outbound journal.

Business IDs are independent from reliable transport sequence numbers:

- `command_id` is persisted in `e2ee_command_receipts`. An incomplete receipt is
  retried after a dispatch error or restart; a completed receipt is ACKed without
  dispatching the ACP command again.
- `local_id` is generated once for each encrypted event handed to the reliable
  journal. Reliable replay preserves that envelope and ID for Manager-side
  deduplication.
- `connection_epoch` fences commands from old Manager WebSocket connections.

Streaming `session/update` text/thought deltas are collected per canonical PAX
session and flushed after 75 ms or 16 KiB. Turn boundaries, tool calls,
permission requests, completion responses, and errors flush immediately. A
failed reliable-journal write restores the batch for retry. This first rollout
uses bounded in-memory plaintext batching; a process crash can therefore lose
at most the current batching window. A future SQLite plaintext-frame to
encrypted-outbox transaction can remove that bounded gap.

The runtime projector completes a prompt only after handing its terminal ACP
response to the attached output sink. This keeps the active-turn snapshot
running while preceding streaming frames are flushed. A failed handoff is still
returned to the caller, while the reliable output layer retains its own retry
responsibility. A matching slot process exit remains an authoritative terminal
condition.

Outputs that cannot be attributed to an E2EE session retain the legacy raw ACP
path so staged and legacy sessions can coexist during migration.

## Primary Interfaces

Implementation note: the directory is named `runtime`, but Go files may use a package name such as `runtimes` or import aliases to avoid confusion with the standard library `runtime` package.

Shared exit model:

```go
type ExitClass string

const (
    ExitTransient ExitClass = "transient"
    ExitAuth      ExitClass = "auth"
    ExitConfig    ExitClass = "config"
    ExitTerminal  ExitClass = "terminal"
)

type Exit struct {
    Class   ExitClass
    Code    string
    Message string
    Details map[string]string
}
```

One-shot session contract:

```go
type Session interface {
    Run(ctx context.Context) Exit
}
```

Remote node-control spec:

```go
type RemoteSpec struct {
    RemoteID     string
    Name         string
    CloudAPIURL  string
    NodeID       string
    Generation   int64
    RestartNonce int64
}
```

Agent tunnel spec:

```go
type AgentConnectionSpec struct {
    ConnectionID string
    RemoteID     string
    CloudAPIURL  string
    CloudAgentID string
    InstanceID   string
    AgentType    string
    Harness      string
    Command      []string
    WorkingDir   string
    TunnelPath   string
    Env          map[string]string
    Generation   int64
    RestartNonce int64
}
```

Runtime dependencies should be injected:

```go
type WebSocketDialer interface {
    Dial(ctx context.Context, url string, header http.Header) (WebSocketConn, *http.Response, error)
}

type WebSocketConn interface {
    ReadMessage() (messageType int, payload []byte, err error)
    WriteMessage(messageType int, payload []byte) error
    Close() error
}

type LocalACPProcessRunner interface {
    Start(ctx context.Context, spec LocalACPProcessSpec) (LocalACPProcess, error)
}

type LocalACPProcess interface {
    Stdin() io.WriteCloser
    Stdout() io.Reader
    Stderr() io.Reader
    Wait() error
    Terminate(ctx context.Context) error
}
```

Agent tunnel sessions depend on a `ReliableEngineFactory`, not a raw transport
journal. The session creates sender/dispatcher functions from the live
WebSocket and process stdin for each concrete attempt, then asks the factory for
a `reliablemq.Engine`-compatible value:

```go
type ReliableEngine interface {
    Send(ctx context.Context, msg reliablemq.OutboundMessage) (reliablemq.Frame, error)
    Receive(ctx context.Context, env reliablemq.Envelope) error
    ReplayInbound(ctx context.Context, queueID string, stream reliablemq.Stream, limit int) error
    ReplayOutbound(ctx context.Context, queueID string, stream reliablemq.Stream, limit int) error
}

type ReliableEngineFactory interface {
    NewReliableEngine(sender reliablemq.Sender, dispatcher reliablemq.Dispatcher) ReliableEngine
}
```

The default production factory can be built from a `reliablemq.DurableStore`,
but the session itself should talk to the engine abstraction.

Runtime sessions should emit observation events upward:

```go
type SessionEventSink interface {
    OnSessionEvent(event SessionEvent)
}
```

Events describe observed runtime phases such as `connecting`, `connected`,
`starting`, `running`, and `stopping`. Supervisors decide whether to persist
them, using generation/restart nonce guards.

The concrete session constructors can be shaped as:

```go
type RemoteControlSessionFactory interface {
    NewRemoteControlSession(spec RemoteSpec) Session
}

type AgentTunnelSessionFactory interface {
    NewAgentTunnelSession(spec AgentConnectionSpec) Session
}
```
