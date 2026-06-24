# internal/runtime BDD test cases

These scenarios define the expected behavior of one-shot runtime sessions.

`runtime` owns shared runtime specs, classified exits, and session interfaces. A session represents one concrete connection attempt. It must not own infinite reconnect loops; retry/backoff decisions belong to supervisor slots.

## Module boundaries

Upstream callers:

- `supervisor` creates and runs `RemoteControlSession` and `AgentTunnelSession` instances through factories.
- Runtime slots provide context cancellation and desired specs.

Downstream dependencies:

- `auth.HeaderProvider` for remote auth headers.
- WebSocket dialer abstraction for node-control and ACP tunnel sessions.
- local process runner abstraction for agent tunnel sessions.
- `controlws` adapter for node-control frame handling.
- ACP bridge/forwarding implementation for agent tunnel traffic.
- transport journal port for ACP tunnel replay, ACKs, and duplicate suppression.

What to mock in `runtime` tests:

- Mock WebSocket dialers or use in-memory/test WebSocket servers.
- Mock `auth.HeaderProvider`.
- Mock local process runner for agent tunnel sessions.
- Mock `controlws` where tests are about session lifecycle rather than frame behavior.
- Mock transport journal operations for agent tunnel session tests.
- Do not use real supervisors, real daemonstore, real pax-manager, or real agent binaries.

Who uses `runtime` test helpers:

- `supervisor` tests use fake sessions instead of real runtime sessions.
- `runtime` tests may provide reusable fake dialers/processes for session-level behavior.

Boundary rule:

- If a test asserts one concrete session attempt, handle cleanup, auth header injection, or exit classification, it belongs here.
- If a test asserts retry/backoff after a session exits, it belongs in `supervisor`.
- If a test asserts node-control frame mapping, it belongs in `controlws`.

## Session ownership

### Scenario: session owns live handles only during Run

Given a runtime session starts  
When it successfully dials a WebSocket and starts any required process  
Then the session owns those live handles until `Run` returns  
And after `Run` returns, the handles are closed or released

### Scenario: session returns classified exit to slot

Given a runtime session exits due to a network error  
When `Run` returns  
Then it returns an exit with class `transient`  
And it does not schedule its own retry

### Scenario: session does not reconnect internally

Given the WebSocket closes during a session  
When the session observes the close  
Then `Run` returns a classified exit  
And the session does not sleep and dial again

### Scenario: heartbeat timeout returns transient exit

Given a runtime session has an open WebSocket  
And no data or pong is received before the heartbeat deadline  
When the heartbeat watchdog fires  
Then the session closes the WebSocket  
And returns an exit with class `transient`  
And the exit code identifies heartbeat timeout

### Scenario: data frames keep heartbeat alive

Given a runtime session has an open WebSocket  
When the session receives data frames before the heartbeat deadline  
Then it refreshes the read deadline  
And does not return heartbeat timeout

### Scenario: pump failure cancels the whole session

Given a runtime session has read, write, and process pumps running  
When one pump returns an unexpected error  
Then the session cancels the other pumps  
And closes all live handles before returning

## RemoteControlSession

### Scenario: dials node-control WebSocket with remote auth headers

Given a remote spec includes remote id and cloud API URL  
And `auth.HeaderProvider` returns `X-Pax-Key` and optional CF Access headers  
When `RemoteControlSession.Run` starts  
Then it dials the node-control WebSocket URL  
And includes the auth headers from the provider

### Scenario: auth header provider failure returns auth exit

Given `auth.HeaderProvider` fails because credentials are missing or denied  
When `RemoteControlSession.Run` starts  
Then it returns an exit with class `auth` or `config` according to error code policy  
And it does not dial the WebSocket

### Scenario: WebSocket handshake unauthorized returns auth exit

Given the WebSocket server rejects handshake with 401 or 403  
When `RemoteControlSession.Run` dials  
Then it returns exit class `auth`  
And the exit code identifies unauthorized or access denied

### Scenario: temporary network dial failure returns transient exit

Given the WebSocket dialer returns a timeout or temporary network error  
When `RemoteControlSession.Run` dials  
Then it returns exit class `transient`  
And the supervisor slot decides backoff

### Scenario: successful connection delegates frames to controlws

Given the WebSocket connection succeeds  
When `RemoteControlSession.Run` starts handling the connection  
Then it delegates frame handling to `controlws`  
And uses the correct remote source attribution

### Scenario: controlws returns normally after peer close

Given `controlws` returns because the peer closed the connection  
When `RemoteControlSession.Run` receives that return  
Then it returns exit class `transient`  
And closes the WebSocket handle

### Scenario: context cancellation stops remote control session

Given `RemoteControlSession.Run` is active  
When the context is canceled by the slot  
Then it closes the WebSocket  
And returns exit class `terminal` or canceled according to runtime policy

## AgentTunnelSession

### Scenario: dials ACP tunnel WebSocket with remote auth headers

Given an agent connection spec includes remote id, cloud agent id, instance id, cloud API URL, and tunnel path  
And `auth.HeaderProvider` returns node and optional CF Access headers  
When `AgentTunnelSession.Run` starts  
Then it dials the ACP tunnel URL  
And includes `agent_id` and `instance_id` query parameters  
And includes auth headers from the provider

### Scenario: missing cloud agent id returns config exit

Given an agent connection spec has no cloud agent id  
When `AgentTunnelSession.Run` starts  
Then it returns exit class `config`  
And it does not dial the WebSocket

### Scenario: missing command returns config exit

Given an agent connection spec has an empty command  
When `AgentTunnelSession.Run` starts  
Then it returns exit class `config`  
And it does not start a local process

### Scenario: command executable not found returns config exit

Given an agent connection spec command points to a missing executable  
When `AgentTunnelSession.Run` starts the process  
Then it returns exit class `config`  
And the exit code identifies command not found

### Scenario: working directory missing returns config exit

Given an agent connection spec working directory does not exist  
When `AgentTunnelSession.Run` starts  
Then it returns exit class `config`  
And it does not dial or start process according to setup order policy

### Scenario: successful session bridges until one side exits

Given the ACP tunnel WebSocket connects  
And the local ACP process starts  
When traffic bridge runs  
Then the session continues until WebSocket closes, process exits, or context cancels  
And `Run` returns a classified exit

### Scenario: connection id is the local journal identity

Given an agent connection spec includes `ConnectionID=conn_codex` and `CloudAgentID=agent_remote`  
When `AgentTunnelSession.Run` records transport frames  
Then journal operations use `conn_codex` as the primary identity  
And may include `agent_remote` only as remote routing/debug metadata

### Scenario: outbound payload is journaled before WebSocket send

Given the local ACP process writes a JSON-RPC line to stdout  
When the outbound pump reads the line  
Then it allocates the next sequence for `connection_id + acp + outbound`  
And inserts a `pending` journal frame before writing to the WebSocket  
And marks the frame `sent` only after the WebSocket write succeeds

### Scenario: outbound ACK marks frames complete

Given outbound frames through sequence 10 are pending or sent  
When the manager sends an ACK for stream `acp` through sequence 10  
Then the session marks those outbound frames `acked`  
And does not mark frames above sequence 10 complete

### Scenario: reconnect replays unacked outbound frames

Given a previous tunnel session ended with outbound frames in `pending` or `sent` state  
When a new `AgentTunnelSession` starts for the same connection id  
Then it replays those frames to the manager in sequence order  
And leaves them replayable until manager ACK marks them `acked`

### Scenario: inbound duplicate is ACKed but not dispatched twice

Given an inbound manager frame with connection id, stream, sequence, and direction already exists in the journal  
When the manager sends the same frame again  
Then the session sends an ACK for that sequence  
And does not write the payload to ACP stdin a second time

### Scenario: inbound received frame is applied before ACK policy completion

Given the manager sends a new inbound data frame  
When the session receives it  
Then it records the frame as `received` before writing to ACP stdin  
And marks it `applied` after a complete stdin write  
And sends an ACK according to the tunnel ACK policy

### Scenario: received but unapplied inbound frame is replayed to stdin

Given a previous session recorded an inbound frame as `received`  
And it did not mark the frame `applied` before exiting  
When a new `AgentTunnelSession` starts for the same connection id  
Then it dispatches the frame to ACP stdin in sequence order  
And marks it `applied` after a complete write

### Scenario: partial stdin write does not mark inbound applied

Given the session is dispatching an inbound frame to ACP stdin  
When the stdin write returns a short write or error  
Then the frame remains not `applied`  
And `Run` returns a classified exit that lets the supervisor recreate the session

### Scenario: WebSocket disconnect returns transient exit

Given the local ACP process is still running  
When the ACP tunnel WebSocket disconnects unexpectedly  
Then `AgentTunnelSession.Run` terminates the local process according to cleanup policy  
And returns exit class `transient`

### Scenario: local process exits immediately with startup error

Given the ACP process exits before the tunnel is usable  
When `AgentTunnelSession.Run` observes the exit  
Then it returns exit class `config` or `transient` according to exit classification policy  
And includes safe process exit diagnostics

### Scenario: context cancellation kills or waits for local process

Given `AgentTunnelSession.Run` has started a local ACP process  
When the slot cancels context  
Then the session closes the WebSocket  
And requests process termination  
And waits up to the configured cleanup timeout  
And returns a terminal/canceled exit

### Scenario: cleanup timeout returns terminal exit with diagnostic

Given the local process does not exit after cancellation  
When cleanup timeout expires  
Then the session force-kills or reports cleanup failure according to process runner policy  
And returns a terminal/canceled exit with safe diagnostic details

## Secret handling

### Scenario: auth headers are not logged

Given auth headers contain Pax key and CF Access secret  
When a runtime session logs dial metadata  
Then logs include remote id and redacted URL  
And do not include raw auth header values

### Scenario: exit messages do not include resolved secrets

Given auth or process setup fails  
When the session returns an exit  
Then exit code/message are safe for status tables  
And do not include resolved secrets or raw headers
