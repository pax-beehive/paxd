# pax-conversation MCP command localization plan

## Problem

`pax-manager` currently builds `session/new` requests with a
`pax-conversation` MCP server entry. That entry includes a `command` used by the
local ACP adapter, such as `codex-acp`, to start the MCP server.

This is machine-local state, but it is currently decided by `pax-manager`.
Examples seen in the code/docs include absolute paths such as:

```json
{
  "name": "pax-conversation",
  "command": "/Users/gengcongkai/.local/bin/paxd",
  "args": ["mcp", "conversation", "serve"]
}
```

On a Linux server where `paxd` is installed at `/home/kk/.local/bin/paxd`, the
ACP adapter cannot start that MCP server and reports:

```text
MCP server pax-conversation failed to start: No such file or directory
```

`paxd` can serve the MCP server via:

```bash
paxd mcp conversation serve
```

The failure is caused by the wrong executable path being forwarded to the local
ACP adapter.

## Direction

Keep `pax-manager` responsible for the semantic MCP server request, and make
`paxd` responsible for localizing the command to the actual `paxd` executable
on the target machine.

`pax-manager` should send a portable MCP server entry:

```json
{
  "name": "pax-conversation",
  "command": "paxd",
  "args": ["mcp", "conversation", "serve"],
  "env": [
    {"name": "PAX_AGENT_ID", "value": "..."},
    {"name": "PAX_SESSION_ID", "value": "..."}
  ]
}
```

Before forwarding the frame to the ACP slot, `paxd` should rewrite the reserved
`pax-conversation` MCP server command to its own local executable path, resolved
from the currently running process.

## Implementation Plan

1. Update `pax-manager`

   Change `agentConversationMCPServers` so it no longer emits a developer
   machine absolute path. Use `command: "paxd"` as the portable fallback while
   keeping the existing args and env.

2. Add a paxd executable resolver

   Add an injectable resolver to the ACP router path:

   ```go
   func() (string, error)
   ```

   The production default should call `os.Executable()`. Tests can inject a
   stable fake path such as `/home/kk/.local/bin/paxd`.

3. Rewrite manager frames before slot forwarding

   In `paxd/internal/runtime/acp_router.go`, rewrite `session/new` params before
   sending the payload to a slot:

   - Parse params as an object.
   - Read `mcpServers` or `mcp_servers`.
   - If the field is absent, preserve the current default behavior of
     `mcpServers: []`.
   - For each MCP server where `name == "pax-conversation"`:
     - set `command` to the resolved paxd executable path;
     - normalize `args` to `["mcp", "conversation", "serve"]`;
     - preserve existing env entries.
   - Leave all other MCP servers untouched.

4. Persist localized resume descriptors

   `resumeDescriptorFromSessionNewParams` persists the session lifecycle
   descriptor used for cold resume. It should store the localized
   `pax-conversation` command so future resumes do not replay a bad manager-side
   path.

5. Rewrite cold resume paths too

   When building `session/resume` params from a stored descriptor, localize
   `pax-conversation` again. This protects old sessions whose descriptors already
   contain `/usr/local/bin/paxd`, `/Users/.../paxd`, or any other stale path.

6. Tests

   Add focused tests in `paxd/internal/runtime/acp_router_test.go`:

   - `session/new` with `pax-conversation` rewrites `command` to the injected
     executable path.
   - Non-`pax-conversation` MCP servers are unchanged.
   - `mcp_servers` snake_case input is accepted and stored as `mcpServers`.
   - Cold resume rewrites a stale stored command to the injected executable path.
   - Existing behavior for missing `mcpServers` still defaults to an empty list.

7. Validation

   Run paxd tests:

   ```bash
   GOCACHE=/private/tmp/paxd-go-cache go test ./...
   ```

## Expected Result

`pax-manager` no longer needs to know where `paxd` is installed on each host.

On the target machine, if the running daemon is:

```bash
/home/kk/.local/bin/paxd
```

then the frame ultimately delivered to `codex-acp` should contain:

```json
{
  "name": "pax-conversation",
  "command": "/home/kk/.local/bin/paxd",
  "args": ["mcp", "conversation", "serve"]
}
```

That lets `codex-acp` start the MCP server without depending on PATH order,
install location conventions, or developer machine paths.
