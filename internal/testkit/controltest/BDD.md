# internal/testkit/controltest BDD test cases

These scenarios define the expected behavior of the shared control test fixtures and mocks.

## JSON-first fixtures

### Scenario: loads a command request from canonical JSON

Given `testdata/golden/control/command_agent_connection_create_request.json` exists  
When a test calls `LoadCommandRequest(t, "agent_connection_create")`  
Then the loader decodes it into a typed `control.Command`  
And the command validates successfully  
And no duplicate Go literal fixture is required

### Scenario: loads a command response from canonical JSON

Given `testdata/golden/control/command_agent_connection_create_response.json` exists  
When a test calls `LoadCommandResponse(t, "agent_connection_create")`  
Then the loader decodes it into a typed `control.CommandAck`  
And the ack command id matches the request fixture command id

### Scenario: loads a query request from canonical JSON

Given `testdata/golden/control/query_remotes_list_request.json` exists  
When a test calls `LoadQueryRequest(t, "remotes_list")`  
Then the loader decodes it into a typed `control.Query`  
And the query validates successfully

### Scenario: fails fast on invalid fixture

Given a golden fixture contains invalid JSON  
When a loader reads that fixture  
Then the loader fails the test immediately  
And the failure message includes the fixture path

### Scenario: fails fast when request and response names drift

Given a command request fixture has command id `cmd_create_conn_1`  
And the matching response fixture has command id `cmd_other`  
When the loader or fixture validation checks the pair  
Then validation fails  
And the failure explains the command id mismatch

## Transport fixture envelopes

### Scenario: localapi fixture wraps HTTP-specific input

Given `testdata/golden/localapi/post_agent_connections_create_request.json` exists  
When a test loads the localapi request fixture  
Then the fixture includes method, path, headers, and body  
And the body can be mapped to the canonical control command

### Scenario: controlws fixture wraps WebSocket-specific input

Given `testdata/golden/controlws/command_agent_connection_create_frame_request.json` exists  
When a test loads the controlws frame fixture  
Then the fixture includes the WebSocket frame kind  
And the frame contains or references the canonical control command payload

## Mock control service

### Scenario: mock service verifies expected command

Given a mock service expects the canonical agent connection create command  
And returns the canonical received ack  
When a transport adapter calls `HandleCommand` with the expected command  
Then the mock returns the configured ack  
And the test passes

### Scenario: mock service rejects unexpected command

Given a mock service expects the canonical agent connection create command  
When a transport adapter calls `HandleCommand` with a different command  
Then the mock fails the test  
And reports the expected and actual command values

### Scenario: mock service verifies no unexpected calls

Given a mock service has no expected command calls  
When a transport adapter handles malformed JSON  
Then the adapter should not call `HandleCommand`  
And the mock verifies no unexpected calls occurred

## Cross-transport consistency

### Scenario: localapi and controlws share the same canonical command fixture

Given the canonical command fixture `agent_connection_create`  
When localapi tests load their HTTP request fixture  
And controlws tests load their WebSocket frame fixture  
Then both adapters should call the mock service with the same typed `control.Command`

### Scenario: localapi and controlws share the same canonical response fixture

Given the canonical response fixture `agent_connection_create`  
When the mock service returns that command ack  
Then localapi encodes it as the expected HTTP response  
And controlws encodes it as the expected WebSocket ACK frame
