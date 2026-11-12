# Pax Conversation MCP Design

This document records the target design for Pax agent-to-agent conversation MCP.
The current one-shot CLI commands are useful for smoke tests, but the MCP shape
should be session-stable: register the server once at `session/new`, keep the
session identity fixed, and avoid per-invocation environment updates.

## Session Identity

The MCP server should be started with stable session identity only:

```text
PAX_AGENT_ID=agent_runtime_x
PAX_REPRESENTATIVE_AGENT_ID=rep_runtime_x
PAX_SESSION_ID=sess_x
```

Do not use `PAX_INVOCATION_ID` as part of the target MCP server design. An MCP
server may live for the whole session, and ACP `session/update` cannot be relied
on to update already-registered tools or process environment.

## Registration

Register one conversation MCP server at `session/new`. It exposes both `ask` and
`reply` tools.

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "method": "session/new",
  "params": {
    "cwd": "/Users/gengcongkai/pax_workspace",
    "env": {
      "PAX_AGENT_ID": "agent_runtime_x",
      "PAX_REPRESENTATIVE_AGENT_ID": "rep_runtime_x",
      "PAX_SESSION_ID": "sess_x"
    },
    "mcpServers": [
      {
        "name": "pax-conversation",
        "command": "/usr/local/bin/paxd",
        "args": ["mcp", "conversation", "serve"],
        "env": [
          { "name": "PAX_AGENT_ID", "value": "agent_runtime_x" },
          { "name": "PAX_REPRESENTATIVE_AGENT_ID", "value": "rep_runtime_x" },
          { "name": "PAX_SESSION_ID", "value": "sess_x" }
        ]
      }
    ]
  }
}
```

## Ask Tool

`ask` starts an invocation to another representative agent. On success, it returns a
receipt token for audit/status lookup. The receipt token is not required for the
target agent to reply.

Tool input:

```json
{
  "to_representative_agent_id": "rep_target",
  "text": "nihao",
  "include_message": false
}
```

Alternative file input:

```json
{
  "to_representative_agent_id": "rep_target",
  "input_file": "./question.md",
  "include_message": true
}
```

paxd sends manager request:

```json
{
  "source": {
    "agent_id": "$PAX_AGENT_ID",
    "representative_agent_id": "$PAX_REPRESENTATIVE_AGENT_ID",
    "session_id": "$PAX_SESSION_ID"
  },
  "target": {
    "kind": "representative",
    "representative_agent_id": "rep_target"
  },
  "context": {
    "latest_response": false
  },
  "instruction": "nihao"
}
```

Successful MCP result:

```json
{
  "ok": true,
  "status": "sent",
  "receipt_token": "rcpt_abc",
  "message": "Invocation sent."
}
```

Human-readable CLI-style summary:

```text
sent receipt=rcpt_abc
```

Failed MCP result:

```json
{
  "ok": false,
  "error": "target session already has an active invocation"
}
```

## Reply Tool

`reply` sends a response for the current session's only active invocation. It does
not take a reply token, invocation id, target agent id, target session id, or
conversation id.

Tool input:

```json
{
  "text": "wozhidaodaan",
  "include_message": false
}
```

Alternative file input:

```json
{
  "input_file": "./answer.md",
  "include_message": true
}
```

paxd sends manager request:

```json
{
  "source": {
    "agent_id": "$PAX_AGENT_ID",
    "representative_agent_id": "$PAX_REPRESENTATIVE_AGENT_ID",
    "session_id": "$PAX_SESSION_ID"
  },
  "target": {
    "kind": "active_invocation"
  },
  "context": {
    "latest_response": false
  },
  "instruction": "wozhidaodaan"
}
```

Manager behavior:

1. Find the active invocation for `source.agent_id + source.session_id`.
2. If none exists, return `no active invocation for session`.
3. If more than one exists, return `ambiguous active invocation`.
4. Verify the current source is the original invocation target.
5. Write the reply as a conversation message from the current source session.
6. Mark the active invocation completed and release the target session lock.

Successful MCP result:

```json
{
  "ok": true,
  "status": "sent",
  "message": "Reply sent."
}
```

Human-readable CLI-style summary:

```text
sent
```

## Manager State

For v1, do not add a separate inquiries table. Treat each
`conversation_agent_invocations` row as one active unit of work for the target
session. The existing invocation `status` carries the lifecycle state.

```text
conversation_agent_invocations
- receipt_token_hash
- status: active/completed/cancelled/expired
- expires_at
```

Required invariant:

```sql
CREATE UNIQUE INDEX idx_active_invocation_target_session
  ON conversation_agent_invocations (target_runtime_agent_id, target_session_id)
  WHERE status = 'active';
```

The receipt token is returned only to the ask caller. Store only a hash in the
database. The token can later support a `status` tool, audit lookup, cancellation,
or timeout diagnostics.

Ask write path:

1. Resolve the target representative to one target runtime/session.
2. Create the ask invocation with `status = 'active'`.
3. Store `receipt_token_hash`, not the raw receipt token.
4. Return the raw receipt token to the caller.

Reply write path:

1. Find the active invocation where the current session is the target.
2. Write the reply message in the conversation.
3. Mark the invocation `status = 'completed'`.

Multiturn should create a new invocation explicitly. A future `reply_and_ask`
operation can atomically complete the current invocation and create the next
one, but plain `reply` does not create a child invocation.

Create a dedicated task or target table later only if the model needs multiple
parallel targets, group fanout, or richer task lifecycle that becomes awkward on
the invocation table.

## Compatibility Notes

The existing one-shot CLI is still useful for local smoke tests:

```sh
paxd mcp conversation ask --to-representative-agent-id rep_123 "nihao"
paxd mcp conversation reply "wozhidaodaan"
```

However, the target ACP integration should use `paxd mcp conversation serve` as a
stdio MCP server registered once at `session/new`.

## Turn-scoped workspace search

Ordinary sessions expose `workspace_search`, `workspace_request_access`,
`workspace_access_status`, and `workspace_cancel_access`.
Default permission covers all user-owned agents and sessions on the caller's
node. Every search must declare node, agent, and session scope, for example:

```json
{"query":"cicd","scope":{"node_ids":["self"],"agent_ids":["*"],"session_ids":["*"]}}
```

This default also applies under PAX auto approve. Starting a turn never grants
cross-node access automatically. PAX auto approve only automatically approves
explicit `workspace_request_access` calls; user-provided turn preauthorization
can separately authorize the requested expansion.

`self` in `node_ids` resolves to the caller's node; `self` in `agent_ids` still
resolves to the calling agent. A sole `*` in `node_ids` selects all nodes owned
by the user and requires an approved expansion. A sole `*` in `agent_ids`
selects all user-owned agents within the declared node scope; a sole `*` in
`session_ids` selects all sessions within the requested node and agent scope.
Each list can instead contain exact IDs. Missing, empty, or mixed-wildcard scopes are
rejected. Top-level `session_ids` is not accepted by ordinary search. Declare the
whole intended scope: Manager rejects unauthorized scope instead of silently
narrowing it to the caller's own node. Successful responses include the
scope resolved by Manager.

Request an expansion separately before searching other nodes, unless the user
preauthorized it for the turn. The request accepts optional `queries`,
`node_ids`, `agent_ids`, `session_ids`, `reason`, and `timeout_seconds`:

```json
{"queries":["cicd"],"node_ids":["*"],"agent_ids":["*"],"session_ids":["*"],"reason":"Find previous CI decisions across nodes","timeout_seconds":600}
```

All four request lists default to `["*"]`. A sole `*` allows any complete
query, node, agent, or session owned by the authenticated user; it cannot be combined
with other list items. Other query values are complete-query allowlist entries,
not substring patterns. Each list has at most 50 entries. Query values have at
most 256 Unicode characters and IDs at most 4,096; the optional reason has at
most 4,096. Request `node_ids` accepts `self` for the caller's node and
`agent_ids` accepts `self` for the calling agent.

The request timeout is an integer from 0 through 86,400 seconds and defaults to
600 when omitted. An explicit `timeout_seconds: 0` means no time deadline; it is
preserved separately from omission. A request without a deadline still expires
at the turn boundary. The timeout applies while pending; approval creates a
turn-bound grant with its own deadline. It never keeps the turn alive or
authorizes another turn.

The helper posts `{session_id, tool, arguments_json}` to
`/api/v1/node/agents/:agent_id/workspace/tools` using stored node credentials.
Caller agent/session identity comes from the injected MCP environment. Manager
resolves the current active turn for new requests and searches and checks their
explicit scope and authorization.
The helper never sends user-selected owner, turn, or capability fields, and does
not cache authorization across calls or turns.

Access requests return `status` (`pending`, `approved`, `denied`, `expired`, or
`cancelled`), `approval_id`, `turn_id`, and `scope`, with optional `expires_at`
and `poll_after_ms`. PAX auto approve can approve explicit requests. A pending
response returns immediately so the agent can continue other work while the
user decides. Preserve `approval_id` and query it separately:

```json
{"name":"workspace_access_status","arguments":{"approval_id":"approval_123"}}
```

Use the returned `poll_after_ms` between status checks (normally 2,000 ms).
Status checks are immediate and never poll or wait inside the helper. When
abandoning the request, call `workspace_cancel_access` with the same
`{"approval_id":"approval_123"}` argument. If approval won the race, cancellation
also revokes that grant. Status and cancellation responses
are passed through unchanged, including terminal states and deadline metadata.
Manager validates the caller's node, agent, and session. New requests and
searches require an active turn; status can report an old request as expired
without reviving its turn. Cancellation does not require an active turn: it can
close this session's abandoned request or revoke its grant, but never restores
access or revives a turn. There is no wait
tool, approval notification injection, or automatic agent resume. An agent that
still needs approval must keep its turn open; finishing the turn ends its
authorization even if the request had no time deadline.

Search itself never creates an approval request.
A search rejected with HTTP 403 returns the safe MCP error code
`authorization_required` with the default node boundary and instructions to
request an expansion or use the Console's turn preauthorization before retrying.
Search never creates an access request, retries at a narrower scope, or falls
back to a different data set. Expanded authorization expires when its turn ends.
Ordinary sessions do not expose workspace read, expand, history, or summary tools.

Deploy a compatible Manager before enabling these tools. A Manager without the
workspace endpoint returns the safe `unavailable` error; an older workspace
endpoint can reject the node scope, timeout field, or lifecycle operations as
`invalid_request`. The helper never retries with weaker semantics. Arbitrary
response bodies and node credentials are never included in error output.

The new MCP search schema requires explicit `scope.node_ids`. For compatibility,
Manager interprets omission by an older HTTP client as `["*"]`, preserving its
declared all-nodes scope and requiring expansion authorization. It never silently
substitutes the caller's node. Restart an existing MCP helper after upgrading so
its client discovers the three-dimensional schema and the new default example.

## Optional Secretary workspace lookups

Manager-created Secretary executions expose only read-only
`workspace_search`, `workspace_read`, `workspace_expand`, `workspace_history`,
and `workspace_summaries`
through this same stdio server when `PAX_SECRETARY_CAPABILITY` is injected.
Their schemas retain the Secretary contract, including optional top-level
`session_ids` on search and no ordinary `scope` or required `node_ids` field.
The ordinary discovery/messaging/artifact and access-request/status/cancel tools are
unavailable inside the Secretary context, including direct
calls. The helper uses existing local node
credentials; Manager validates the execution grant and current owner on every
call. No user API key or local MCP configuration is needed. See
[Secretary workspace tools](secretary_workspace_mcp.md) for schemas, bounds,
version references, authentication and compatibility.
