# internal/testkit/controltest

`internal/testkit/controltest` provides shared test fixtures and mocks for control-plane modules.

The fixtures are JSON-first. Do not maintain duplicate Go literal fixtures for the same canonical command/query. Loaders should decode JSON into typed control structs and run validation.

This package is test support only. It must not be used in production paths.

## Golden Dataset Layout

Use explicit request/response suffixes.

```text
internal/testkit/controltest/
  README.md
  BDD.md
  loader.go
  mock_service.go
  testdata/golden/
    control/
      command_agent_connection_create_request.json
      command_agent_connection_create_response.json
      command_agent_connection_restart_request.json
      command_agent_connection_restart_response.json
      command_remote_create_request.json
      command_remote_create_response.json
      command_remote_auth_configure_request.json
      command_remote_auth_configure_response.json
      query_remotes_list_request.json
      query_remotes_list_response.json
      query_harnesses_discover_request.json
      query_harnesses_discover_response.json

    localapi/
      post_agent_connections_create_request.json
      post_agent_connections_create_response.json
      patch_agent_connection_enable_false_request.json
      patch_agent_connection_enable_false_response.json

    controlws/
      command_agent_connection_create_frame_request.json
      command_agent_connection_create_frame_response.json
      query_remotes_list_frame_request.json
      query_remotes_list_frame_response.json
```

Naming convention:

```text
*_request.json   input into the component under test
*_response.json  expected output from the component under test
```

## Fixture Layers

`control/` fixtures are canonical, transport-independent payloads:

- command request: `control.Command`
- command response: `control.CommandAck`
- query request: `control.Query`
- query response: `control.QueryResult`

`localapi/` fixtures are HTTP-specific envelopes, such as method, path, headers, and body.

`controlws/` fixtures are WebSocket-specific frames.

Transport fixtures may wrap canonical control payloads, but they should not redefine business fields unless the test is intentionally covering malformed transport input.

## Loader Shape

Expected helper shape:

```go
func LoadCommandRequest(t testing.TB, name string) control.Command
func LoadCommandResponse(t testing.TB, name string) control.CommandAck
func LoadQueryRequest(t testing.TB, name string) control.Query
func LoadQueryResponse(t testing.TB, name string) control.QueryResult

func LoadLocalAPIRequest(t testing.TB, name string) LocalAPIRequest
func LoadLocalAPIResponse(t testing.TB, name string) LocalAPIResponse

func LoadControlWSFrameRequest(t testing.TB, name string) ControlWSFrame
func LoadControlWSFrameResponse(t testing.TB, name string) ControlWSFrame
```

For example:

```go
cmd := controltest.LoadCommandRequest(t, "agent_connection_create")
ack := controltest.LoadCommandResponse(t, "agent_connection_create")
```

The loader should resolve those names to:

```text
testdata/golden/control/command_agent_connection_create_request.json
testdata/golden/control/command_agent_connection_create_response.json
```

## Mock Service

`mock_service.go` should provide a mock `control.Service` for transport tests.

It should support expectations such as:

```go
controltest.NewMockService(t).
    ExpectCommand(controltest.LoadCommandRequest(t, "agent_connection_create")).
    ReturnCommandAck(controltest.LoadCommandResponse(t, "agent_connection_create"))
```

Transport tests should assert that:

- protocol input maps to the expected typed control request
- mock service output maps to the expected protocol response
- unexpected service calls fail the test

Transport tests should not assert database state.
