# Local session MCP launch descriptions

paxd reads optional `~/.paxd/mcp.json` on session/new and explicit
session/resume. `PAXD_LOCAL_MCP_CONFIG` overrides the filename. E2EE commands
are expanded after decryption. Manager changes are not required.

```json
{
  "mcpServers": [{
    "name": "browser",
    "command": "/absolute/path/to/agent-browser/mcp-stdio.sh",
    "args": [],
    "env": [{
      "name": "AGENT_BROWSER_SESSION_KEY",
      "value": "${PAX_SESSION_KEY}"
    }]
  }]
}
```

Only environment values are substituted. Available placeholders are
`${PAX_AGENT_ID}`, `${PAX_SESSION_ID}`, and `${PAX_SESSION_KEY}`. The key is
SHA-256 of the JSON pair [agent ID, manager session ID]. It is a resource
identifier, not a credential. The command must be locally installed.

Expanded descriptions are retained by the existing ACP route store for cold
resume. Slots do not own the identity. No browser APIs, tables, or lifecycle
management are added to paxd. Existing client MCP entries are preserved except
an exact generated-name match, which is replaced by local configuration.

BDD: two sessions get different keys; re-expansion is idempotent; prompt frames
are unchanged; missing configuration is a no-op. Tests are in local_mcp_test.go.
The sibling agent-browser-runtime/IMPLEMENTATION_PLAN.md describes the runtime
BDD/TDD plan. Existing resume-configuration work is verified in the same branch.
