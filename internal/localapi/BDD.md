# internal/localapi BDD test cases

These scenarios define the expected behavior of the local API transport adapter.

`localapi` is a protocol adapter over `control.Service`. Tests in this module should use a mock control service. They should assert request parsing, command/query construction, response encoding, and local source attribution. They should not assert database state.

## Module boundaries

Upstream callers:

- Unix socket listener for paxctl, TUI, and local programs.
- Optional `127.0.0.1` debug HTTP listener for curl, Postman, and browser debugging.

Downstream dependency:

- `control.Service` is the only business dependency.

What to mock in `localapi` tests:

- Mock `control.Service` with `internal/testkit/controltest`.
- Use `httptest` against `localapi.NewHandler`.
- Do not use a real `daemonstore`, `supervisor`, `auth.HeaderProvider`, `harnessregistry`, or `localsessions`.

Who uses `localapi` test helpers/fixtures:

- `localapi` tests use `controltest` canonical fixtures and HTTP-specific fixtures.
- TUI and paxctl tests may reuse localapi HTTP fixtures when they test client-side request formation.

Boundary rule:

- If a test wants to assert HTTP route parsing, command id headers, or response status/body, it belongs here.
- If a test wants to assert desired-state mutation, it belongs in `internal/control`.
- If a test wants to assert Unix listener creation or debug HTTP bind address, it belongs in daemon/bootstrap tests unless `localapi` owns that listener setup.

## Handler sharing

### Scenario: Unix socket and debug HTTP use the same handler

Given `localapi.NewHandler(controlService)` returns an HTTP handler  
When paxd serves that handler on a Unix socket listener  
And paxd serves the same handler on an optional `127.0.0.1` debug HTTP listener  
Then both transports expose the same routes  
And both transports call the same mock control service methods for the same request

### Scenario: debug HTTP listener is localhost-only

Given debug HTTP is enabled  
When paxd creates the debug HTTP listener  
Then it binds to `127.0.0.1` or another explicit loopback address  
And it does not bind to `0.0.0.0` by default

## Query routes

### Scenario: GET /v1/status maps to status query

Given a mock control service expects a `status.get` query  
When the client sends `GET /v1/status`  
Then `localapi` calls `HandleQuery` with `Source{Kind: local}`  
And the query type is `status.get`  
And the HTTP response encodes the mock query result

### Scenario: GET /v1/remotes maps to remotes list query

Given a mock control service expects a `remotes.list` query  
When the client sends `GET /v1/remotes?include_disabled=true`  
Then `localapi` calls `HandleQuery` with `ListRemotes.IncludeDisabled=true`  
And the HTTP response encodes the mock remotes result

### Scenario: GET /v1/agent-connections maps to agent connections list query

Given a mock control service expects an `agent_connections.list` query  
When the client sends `GET /v1/agent-connections?remote_id=remote_prod&include_disabled=true`  
Then `localapi` calls `HandleQuery` with the remote filter and disabled flag  
And the HTTP response encodes the mock agent connections result

### Scenario: POST /v1/harnesses/discover maps to discover harnesses query

Given a mock control service expects a `harnesses.discover` query  
When the client sends `POST /v1/harnesses/discover` with `{"probe":true}`  
Then `localapi` calls `HandleQuery` with `DiscoverHarnesses.Probe=true`  
And it does not call `HandleCommand`

### Scenario: POST /v1/local/sessions/sync maps to local session sync query

Given a mock control service expects a `local_sessions.sync` query  
When the client sends `POST /v1/local/sessions/sync` with agent and limit fields  
Then `localapi` calls `HandleQuery` with the typed sync query  
And it does not call `HandleCommand`

## Command routes

### Scenario: POST /v1/remotes maps to remote create command

Given a mock control service expects a `remote.create` command  
When the client sends `POST /v1/remotes` with a valid create remote body  
Then `localapi` builds a typed `CreateRemote` command  
And `localapi` calls `HandleCommand` with `Source{Kind: local}`  
And the HTTP response encodes the mock command ack

### Scenario: PATCH /v1/remotes/{id} maps to remote update command

Given a mock control service expects a `remote.update` command  
When the client sends `PATCH /v1/remotes/remote_prod` with update fields  
Then `localapi` builds a typed `UpdateRemote` command with id `remote_prod`  
And omitted update fields remain nil  
And the HTTP response encodes the mock command ack

### Scenario: POST /v1/remotes/{id}/restart maps to remote restart command

Given a mock control service expects a `remote.restart` command  
When the client sends `POST /v1/remotes/remote_prod/restart`  
Then `localapi` builds a typed `RestartRemote` command with id `remote_prod`  
And the HTTP response encodes the mock command ack

### Scenario: POST /v1/agent-connections maps to agent connection create command

Given a mock control service expects an `agent_connection.create` command  
When the client sends `POST /v1/agent-connections` with remote id, harness, and command fields  
Then `localapi` builds a typed `CreateAgentConnection` command  
And the command slice is decoded as `[]string`, not raw JSON bytes  
And the HTTP response encodes the mock command ack

### Scenario: PATCH /v1/agent-connections/{id} preserves omitted fields as nil

Given a mock control service expects an `agent_connection.update` command  
When the client sends `PATCH /v1/agent-connections/conn_codex` with only `enabled=false`  
Then `localapi` builds a typed `UpdateAgentConnection` command with id `conn_codex`  
And `Enabled` is a non-nil pointer to false  
And omitted fields such as command and working directory are nil

### Scenario: DELETE /v1/agent-connections/{id} maps to delete command

Given a mock control service expects an `agent_connection.delete` command  
When the client sends `DELETE /v1/agent-connections/conn_codex?deregister=true`  
Then `localapi` builds a typed delete command with `Deregister=true`  
And the HTTP response encodes the mock command ack

## Command ids

### Scenario: uses client-provided command id

Given a request contains header `X-Pax-Command-Id: cmd_123`  
When the request maps to a mutating command  
Then `localapi` sends command id `cmd_123` to `control.Service`

### Scenario: generates command id when missing

Given a mutating request does not contain a command id  
When `localapi` maps the request to a command  
Then it generates a non-empty command id  
And returns that command id in the response

## Error handling

### Scenario: malformed JSON returns bad request

Given a request body is malformed JSON  
When the client sends a command request  
Then `localapi` returns HTTP 400  
And it does not call `control.Service`

### Scenario: control validation rejection maps to client error

Given the mock control service returns a rejected command ack  
When the client sends a command request  
Then `localapi` returns a client error status according to API policy  
And the response includes the control error code and message

### Scenario: control service internal error maps to server error

Given the mock control service returns an unexpected error  
When the client sends a request  
Then `localapi` returns HTTP 500  
And the response does not expose secrets or internal stack traces

### Scenario: unknown route returns not found

Given no route matches the request path  
When the client sends the request  
Then `localapi` returns HTTP 404  
And it does not call `control.Service`

## Secret handling

### Scenario: debug responses do not expose resolved secrets

Given the mock control service returns a view containing remote auth metadata  
When `localapi` encodes the response  
Then the response contains only safe auth metadata  
And it does not contain resolved secret values

### Scenario: request logs do not include secret material

Given a request configures remote auth with a secret ref  
When `localapi` logs the request  
Then logs contain route and command id information  
And logs do not contain resolved secret values
