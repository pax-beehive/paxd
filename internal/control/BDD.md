# internal/control BDD test cases

These scenarios define the expected behavior of `control.Service`.

`control.Service` is the only business-layer entrypoint for local transports and remote node-control WebSocket. Tests in this module should use test stores and fake supervisor/harness/session ports. They should assert business effects, not HTTP or WebSocket wire encoding.

## Module boundaries

Upstream callers:

- `internal/localapi` calls `HandleQuery` and `HandleCommand` for Unix socket and debug HTTP requests.
- `internal/controlws` calls `HandleQuery` and `HandleCommand` for remote node-control WebSocket frames.
- Future local clients or transports must enter through the same service boundary.

Downstream dependencies:

- `daemonstore` port for desired-state, status, and command records.
- supervisor wake ports for remote and agent connection reconciliation.
- `harnessregistry` port for harness discovery/cache queries.
- `localsessions` port for local session observation queries.

What to mock in `control` tests:

- Use real in-memory/test `daemonstore` implementations or focused fakes when testing transactions.
- Use fake supervisor wake ports that record wake calls.
- Use fake `harnessregistry` and `localsessions` ports for query orchestration tests.
- Do not use real HTTP handlers, WebSocket adapters, or live runtimes.

Who uses `control` test helpers/fixtures:

- Transport tests should use `internal/testkit/controltest` mock service instead of importing control internals.
- `control` tests can use the same JSON-first canonical fixtures from `internal/testkit/controltest` to construct commands and queries.

Boundary rule:

- If a test wants to assert command validation, idempotency, desired-state mutation, command audit, or supervisor wakeup, it belongs here.
- If a test wants to assert route parsing or frame encoding, it belongs in `localapi` or `controlws`.
- If a test wants to assert actual runtime start/stop/backoff, it belongs in `supervisor` or runtime session modules.

## Command validation

### Scenario: accepts a command with exactly one matching payload

Given a command with type `agent_connection.restart`  
And the command has only `RestartAgentConnection` populated  
And the restart payload has a connection id  
When `HandleCommand` receives the command  
Then validation succeeds  
And the command is processed

### Scenario: rejects a command with no payload

Given a command with type `agent_connection.restart`  
And no payload field is populated  
When `HandleCommand` receives the command  
Then it returns a rejected command ack  
And no desired state is changed  
And no supervisor is woken

### Scenario: rejects a command with multiple payloads

Given a command with type `agent_connection.restart`  
And both `RestartAgentConnection` and `UpdateAgentConnection` are populated  
When `HandleCommand` receives the command  
Then it returns a rejected command ack  
And no desired state is changed  
And no supervisor is woken

### Scenario: rejects a command whose payload does not match type

Given a command with type `agent_connection.restart`  
And only `UpdateAgentConnection` is populated  
When `HandleCommand` receives the command  
Then it returns a rejected command ack  
And no desired state is changed

### Scenario: rejects a mutating command without command id

Given a valid mutating command payload  
And the command id is empty  
When `HandleCommand` receives the command  
Then it returns a rejected command ack  
And no desired state is changed

## Command idempotency

### Scenario: returns existing received command for duplicate command id

Given `control_command` already contains command id `cmd_1` with status `received`  
When `HandleCommand` receives the same command id again  
Then it returns the existing command ack  
And it does not apply the mutation again  
And it does not increment generation again

### Scenario: returns existing completed command for duplicate command id

Given `control_command` already contains command id `cmd_1` with status `applied`  
When `HandleCommand` receives the same command id again  
Then it returns the existing command result summary  
And it does not apply the mutation again

### Scenario: rejects duplicate command id with different payload

Given `control_command` already contains command id `cmd_1` for `agent_connection.restart`  
When `HandleCommand` receives command id `cmd_1` for `agent_connection.delete`  
Then it returns a rejected command ack  
And it does not apply the new mutation

## Remote commands

### Scenario: session runtime reset compares the active turn

Given the authenticated runtime connection owns the requested cloud agent
And its projector contains the native session and expected turn instance
When `session_runtime.reset` is handled
Then the projector suppresses only that matching projection
And the command acknowledgement reports the suppression result immediately
And every reset outcome advances the projection revision so a complete
snapshot is republished

### Scenario: session runtime reset rejects a mismatched binding

Given the command names an agent or connection outside the active runtime
binding
When `session_runtime.reset` is handled
Then the command is rejected
And no session projection is mutated

### Scenario: creates a remote

Given no remote exists for `https://api.example.test`  
When `HandleCommand` receives `remote.create`  
Then a remote row is created  
And the remote has generation `1`  
And the command is recorded as `received`  
And `RemoteSupervisor` is woken

### Scenario: updates a runtime-affecting remote field

Given an enabled remote exists  
When `HandleCommand` receives `remote.update` with a new cloud API URL  
Then the remote is updated  
And its generation is incremented  
And `RemoteSupervisor` is woken

### Scenario: restarts a remote without changing configuration

Given an enabled remote exists with restart nonce `3`  
When `HandleCommand` receives `remote.restart`  
Then restart nonce becomes `4`  
And generation is unchanged  
And `RemoteSupervisor` is woken

### Scenario: deletes a remote

Given an enabled remote exists  
When `HandleCommand` receives `remote.delete`  
Then the remote is disabled or marked deleted according to store policy  
And generation is incremented  
And `RemoteSupervisor` is woken  
And agent connection desired state for that remote is not silently deleted unless the command explicitly requests cascading behavior

## Remote auth commands

### Scenario: configures Cloudflare Access auth with a secret ref

Given a remote exists  
When `HandleCommand` receives `remote_auth.configure` with Cloudflare Access client id and secret ref  
Then `remote_auth` is upserted for the remote  
And resolved secret values are not stored in `control_command.payload_json`  
And `RemoteSupervisor` is woken  
And `AgentConnectionSupervisor` is woken for connections under that remote

### Scenario: clears remote auth

Given a remote has `remote_auth` configured  
When `HandleCommand` receives `remote_auth.clear`  
Then `remote_auth` is set to `none` or removed according to store policy  
And remote and agent runtimes for that remote are woken for reconnect

## Agent connection commands

### Scenario: creates an agent connection

Given a remote exists  
And no agent connection with the requested name exists under that remote  
When `HandleCommand` receives `agent_connection.create`  
Then an `agent_connection` row is created  
And generation is `1`  
And restart nonce is `0`  
And the command is recorded as `received`  
And `AgentConnectionSupervisor` is woken

### Scenario: rejects agent connection creation for missing remote

Given no remote exists for the requested remote id  
When `HandleCommand` receives `agent_connection.create`  
Then it returns a rejected command ack  
And no agent connection row is created  
And no supervisor is woken

### Scenario: updates a runtime-affecting agent connection field

Given an agent connection exists  
When `HandleCommand` receives `agent_connection.update` with a new command or working directory  
Then the connection is updated  
And generation is incremented  
And restart nonce is unchanged  
And `AgentConnectionSupervisor` is woken

### Scenario: updates a non-runtime display field

Given an agent connection exists  
When `HandleCommand` receives `agent_connection.update` with only a display name change  
Then the connection is updated  
And generation behavior follows the store policy for display-only changes  
And `AgentConnectionSupervisor` is woken only if that policy requires reconcile

### Scenario: restarts an agent connection

Given an agent connection exists with restart nonce `7`  
When `HandleCommand` receives `agent_connection.restart`  
Then restart nonce becomes `8`  
And generation is unchanged  
And `AgentConnectionSupervisor` is woken

### Scenario: deletes an agent connection

Given an agent connection exists and is running  
When `HandleCommand` receives `agent_connection.delete`  
Then desired state becomes `deleted` or disabled according to store policy  
And generation is incremented  
And `AgentConnectionSupervisor` is woken

## Query handling

### Scenario: lists remotes

Given the store has remotes  
When `HandleQuery` receives `remotes.list`  
Then it returns remote views  
And it does not write `control_command`  
And it does not wake supervisors

### Scenario: discovers harnesses

Given the harness registry can discover local harnesses  
When `HandleQuery` receives `harnesses.discover`  
Then it returns discovered harness views  
And it may refresh `harness_inventory` cache  
And it does not write `control_command`

### Scenario: syncs local sessions

Given local sessions can be scanned  
When `HandleQuery` receives `local_sessions.sync`  
Then it syncs local session cache  
And returns a sync result  
And it does not write `control_command`  
And it does not mutate remotes or agent connections

### Scenario: gets command status

Given `control_command` contains command id `cmd_1`  
When `HandleQuery` receives `command.get` for `cmd_1`  
Then it returns the command view  
And it does not re-run the command

## Secret handling

### Scenario: command audit does not persist resolved secrets

Given a remote auth configure command contains a secret ref  
And the secret resolver can resolve it to a secret value  
When `HandleCommand` stores the command audit row  
Then `payload_json` contains only the secret ref  
And `payload_json` does not contain the resolved secret value

### Scenario: query views do not expose resolved secrets

Given remote auth is configured  
When `HandleQuery` returns remote or daemon status views  
Then the result does not contain resolved secret values  
And debug-friendly views expose at most masked refs or auth kind
