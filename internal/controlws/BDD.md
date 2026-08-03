# internal/controlws BDD test cases

These scenarios define the expected behavior of the remote node-control WebSocket adapter.

`controlws` is a protocol adapter over `control.Service`. Tests in this module should use a mock control service from `internal/testkit/controltest`. They should assert frame parsing, source attribution, command ACK/result encoding, query response encoding, and error handling. They should not assert database state.

## Module boundaries

Upstream caller:

- `RemoteControlSession` owns the live node-control WebSocket and calls `controlws.Run`.
- pax-manager is the remote peer that sends and receives WebSocket frames.

Downstream dependency:

- `control.Service` is the only business dependency.

What to mock in `controlws` tests:

- Mock `control.Service` with `internal/testkit/controltest`.
- Use in-memory/fake WebSocket pairs or a test WebSocket server for frame I/O.
- Do not use a real `daemonstore`, `supervisor`, `auth.HeaderProvider`, or ACP tunnel runtime.

Who uses `controlws` test helpers/fixtures:

- `controlws` tests use `controltest` canonical fixtures.
- `RemoteControlSession` tests may mock or fake `controlws` to assert that session lifecycle passes frames to the adapter and classifies adapter exits.

Boundary rule:

- If a test wants to assert desired-state mutation, it belongs in `internal/control`.
- If a test wants to assert reconnect/backoff after `controlws.Run` exits, it belongs in `internal/supervisor` or `internal/runtime`.
- If a test wants to assert frame-to-command mapping, it belongs here.

## Connection scope

### Scenario: attributes frames to the owning remote

Given a node-control WebSocket session is started for `remote_prod`  
And a mock control service expects source kind `remote` and remote id `remote_prod`  
When the manager sends a valid command frame  
Then `controlws` calls `HandleCommand` with `Source{Kind: remote, RemoteID: remote_prod}`

### Scenario: does not handle ACP data frames

Given a node-control WebSocket session is active  
When the manager sends a frame that belongs to ACP tunnel data-plane  
Then `controlws` rejects the frame or returns a protocol error  
And it does not call `control.Service`

## Command frames

### Scenario: command frame maps to typed control command

Given the canonical `agent_connection_create` command fixture exists  
And the WebSocket frame wraps that command fixture  
When `controlws` receives the command frame  
Then it decodes the frame into a typed `control.Command`  
And calls `HandleCommand` with that command  
And does not pass raw JSON bytes across the service boundary

### Scenario: accepted command returns ACK frame

Given the mock control service returns the canonical accepted command ack  
When `controlws` handles the command frame  
Then it writes an ACK frame containing the command id  
And the ACK frame indicates accepted status  
And the ACK frame does not claim runtime completion

### Scenario: rejected command returns ACK frame with error

Given the mock control service returns a rejected command ack  
When `controlws` handles the command frame  
Then it writes an ACK frame containing accepted=false or rejected status according to protocol policy  
And the frame includes the control error code and message  
And no command result frame is required

### Scenario: malformed command frame returns protocol error

Given the manager sends malformed JSON  
When `controlws` tries to decode the frame  
Then it writes a protocol error frame or closes with a protocol error according to policy  
And it does not call `control.Service`

### Scenario: command frame missing command id is rejected

Given the manager sends a mutating command frame without command id  
When `controlws` handles the frame  
Then it rejects the frame before calling `control.Service`  
And it writes an error response explaining that command id is required for remote commands

## Query frames

### Scenario: query frame maps to typed control query

Given the canonical `remotes_list` query fixture exists  
And the WebSocket frame wraps that query fixture  
When `controlws` receives the query frame  
Then it decodes the frame into a typed `control.Query`  
And calls `HandleQuery` with remote source attribution

### Scenario: query response is encoded as response frame

Given the mock control service returns the canonical `remotes_list` query result  
When `controlws` handles the query frame  
Then it writes a query response frame  
And the response frame includes the request correlation id  
And the response body matches the mock query result

### Scenario: query handler error returns error frame

Given the mock control service returns an unexpected query error  
When `controlws` handles the query frame  
Then it writes an error frame  
And the frame includes a safe error code and message  
And it does not expose stack traces or secrets

## Command results

### Scenario: sends best-effort command result when available

Given a command was accepted  
And the control service or result stream later reports command status `applied`  
When the node-control WebSocket is still open  
Then `controlws` writes a `command_result` frame with the command id and status

### Scenario: does not require command result delivery for correctness

Given a command was accepted  
And the node-control WebSocket disconnects before a command result is available  
When the command later completes  
Then correctness relies on command/status polling, not result frame delivery  
And `controlws` does not retry result delivery by itself unless a higher-level protocol requires it

## Ordering and backpressure

### Scenario: one writer pumps complete runtime snapshots

Given the control WebSocket is connected
When initial state, projector changes, periodic refresh, or reset triggers a
session runtime report
Then one report pump serializes complete `active_turns` snapshots
And allocates a strictly increasing connection-scoped sequence to each emitted
snapshot
And a racing change cannot be lost beyond the next emitted snapshot

### Scenario: reconnect begins with a complete snapshot

Given node control reconnects while turns are active
When the new connection report pump starts
Then it subscribes before reading the initial projector snapshot
And sends that complete snapshot without treating disconnect as turn
completion

### Scenario: serializes writes to the WebSocket

Given multiple goroutines may attempt to write ACK, query response, and command result frames  
When frames are written  
Then `controlws` serializes writes through one writer path or write mutex  
And WebSocket writes do not interleave

### Scenario: slow manager does not block desired-state commit

Given `control.Service` accepts a command  
And the manager is slow to read ACK frames  
When `controlws` writes the ACK  
Then desired-state commit has already happened  
And a slow write only affects the transport session, not the control transaction

## Disconnects

### Scenario: read failure ends the WebSocket session

Given the manager closes the node-control WebSocket  
When `controlws` reads from the connection  
Then `Run` returns a classified transient exit to the remote control session  
And retry/backoff is left to the runtime slot

### Scenario: context cancellation closes the WebSocket session

Given the remote runtime slot cancels the session context  
When `controlws` is running  
Then it stops reading and writing  
And returns a terminal or canceled exit according to runtime policy

## Secret handling

### Scenario: does not log resolved secrets from frames

Given a remote auth configure command contains a secret ref  
When `controlws` logs frame metadata  
Then logs include safe metadata such as frame kind and command id  
And logs do not include resolved secret values

### Scenario: does not echo resolved secrets in error frames

Given `control.Service` returns an auth-related error  
When `controlws` encodes the error frame  
Then the frame includes a safe error code and message  
And it does not include resolved auth headers or secret values

## Golden fixtures

### Scenario: shares canonical command fixture with localapi

Given `internal/testkit/controltest` has canonical `agent_connection_create` command request and response fixtures  
When `controlws` tests load their WebSocket frame fixtures  
Then the decoded frame command matches the canonical control command  
And the encoded ACK corresponds to the canonical command response

### Scenario: shares canonical query fixture with localapi

Given `internal/testkit/controltest` has canonical `remotes_list` query request and response fixtures  
When `controlws` tests load their WebSocket frame fixtures  
Then the decoded frame query matches the canonical control query  
And the encoded response corresponds to the canonical query response
