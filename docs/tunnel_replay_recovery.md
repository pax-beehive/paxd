# Tunnel replay and ACP worker recovery

The November 2026 tunnel incident requires changes in paxd, paxkit, and
pax-manager. Updating Manager alone does not update clients' embedded transport
engine or fix their ACP worker lifecycle.

## Failure and recovery behavior

- The production daemon and standalone ACP forwarder wrap SQLite transport
  storage in `ProducerWriteBehindStore`. The wrapper must expose the durable
  inbound cursor so replay stops at a failed frame and preserves sequence order.
- Worker stdout/stderr must be drained while replay writes stdin. Waiting until
  replay finishes can deadlock two full pipes. Every recovery failure must reap
  the started process, and process completion must support multiple waiters.
- A `session/new` request remains pending until its response's native session ID
  is durably bound. Requests from the worker cannot consume that response
  correlation even if their JSON-RPC IDs collide. The pool retains the boundary
  across websocket reconnects while its workers remain alive.
- Inconsistent durable history returns `transport_history_inconsistent` and
  preserves both SQLite databases. It must not reconnect indefinitely, rotate
  the queue, advance cursors, ACK new input, delete rows, or replay old permission
  responses automatically. Operators must inspect delivery state first.

## Regression coverage

The runtime and forwarder tests exercise real SQLite through the production
store factory, a 1,010-frame failed backlog, restart and ordered retry,
out-of-order input, native-binding write failure, request/response ID collision,
websocket reconnect, full worker pipes, child cleanup, and inconsistent history.
The unsafe behaviors were reproduced with the previous shared dependency.

The Manager repository additionally has an opt-in test,
`TestActualPaxdReplayAcrossReconnectIntegration`, that runs the actual compiled
daemon with fresh local state and a mock ACP worker. A controllable websocket
proxy disconnects the daemon after the early command catalog and before the
creation result. The test checks canonical command synchronization, a working
prompt after reconnect, and exactly one worker invocation per user request.

Run product checks with `GOWORK=off go test ./...`, targeted runtime/forwarder
race tests, and `GOWORK=off go vet ./...`. Build Linux clients with CGO enabled:
the SQLite driver in a `CGO_ENABLED=0` binary is a nonfunctional stub.

## Known limitation

The pool's worker-output path does not retry a failed output callback. An
`ACPSlotEventFrame` calls `ACPPool.HandleSlotFrame`, which calls the attached
session boundary and transport sender. If binding the native session ID fails
temporarily, or the sender rejects the output before journal acceptance, the
slot event handler logs `route frame failed` and discards that worker event.
There is no durable output record to replay in that case. This predates the
inbound-replay changes and remains a separate output-durability gap.

The binding-failure regression verifies that calling the boundary again with
the same response preserves its manager-session correlation; it does not assert
that the production slot event handler performs that retry. Retaining the
boundary across reconnects likewise preserves responses arriving after a
disconnect, rather than recovering callbacks already rejected before journal
acceptance. Retrying the original manager command could repeat worker side
effects, so this change does not add that fallback.

## Rollout state

Source fixes and paired acceptance tests do not upgrade already installed
clients. Inspect each installation's journal read-only and back up the daemon
and transport databases before an upgrade. Do not repair a cursor by guessing
whether its associated command or permission response was already executed.

The incident audit found five old received permission responses below the
applied cursor and two queues with applied cursors above receipt cursors on the
Mac installation. Those records were not modified or replayed. The Ubuntu
installation and Manager did not have these checkpoint inconsistencies at audit
time. Keep that Mac installation out of automatic rollout until the affected
queues have an explicit recovery decision that preserves their original data.
